package github

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/fagerbergj/quack-extensions/sdk"
)

// githubContext is the loaded GitHub state for one dispatch (#459).
type githubContext struct {
	snap               Snapshot
	delta              *Delta // nil on first load (#666), set on resume
	firstLoad          bool
	contextUnavailable bool             // fetchSnapshot's meta call failed — label-triggered work aborts (#467)
	newCommits         []snapshotCommit // PR commits for incremental review scope; nil = review everything
	checks             []checkRunView   // current head-commit check runs (PR only); nil = not a PR or the fetch failed
}

// pendingRun is what dispatch stores so RunEnded (arriving later, out of
// band, keyed only by chatID + RunOutcome) can finish the job dispatch used
// to do inline. Host.Dispatch is fire-and-forget - see webhook.go's dispatch
// doc comment.
type pendingRun struct {
	sessionID      string
	owner, repo    string
	number         int
	isPR           bool
	login          string
	gh             githubContext
	isPlan         bool
	isLabelTrigger bool

	// nudged is set once this chat's one-shot "you answered without running
	// anything" retry has fired, so finalize is never re-entered as a nudge
	// twice for the same primary dispatch.
	nudged bool
}

// RunEnded correlates a dispatched run's outcome back to the pendingRun
// dispatch stored it under. The nudge-if-no-plan retry (webhook.go's former
// synchronous e.drive(runNudge) call) becomes a second Dispatch from here -
// design doc's answer for RunOutcome.PlanRan - and finalize does everything
// the old dispatch()'s tail did once a chat's chain of dispatches is done.
func (e *Extension) RunEnded(chatID string, outcome sdk.RunOutcome) {
	v, ok := e.pending.Load(chatID)
	if !ok {
		return // not one of ours, or already finalized (defensive - should not happen)
	}
	pr := v.(*pendingRun)

	if !pr.nudged && !outcome.PlanRan && pr.isLabelTrigger && outcome.Status != sdk.RunCancelled {
		pr.nudged = true
		nudgeReq := sdk.DispatchRequest{
			Chat: sdk.ChatRef{LocalID: pr.sessionID, User: pr.login},
			Ask:  sdk.Ask{Message: runNudge},
			Run:  sdk.RunConfig{Timeout: e.runTimeout},
		}
		e.host.Log.Warn("github: work request produced no plan; nudging it to run the work once",
			"repo", pr.owner+"/"+pr.repo, "issue", pr.number)
		if err := e.host.Dispatch(context.Background(), nudgeReq); err != nil {
			e.host.Log.Error("github: nudge dispatch failed; finalizing with what we have",
				"repo", pr.owner+"/"+pr.repo, "issue", pr.number, "err", err)
			e.finalize(chatID, pr, outcome)
		}
		return // wait for the nudge's own RunEnded
	}
	e.finalize(chatID, pr, outcome)
}

// finalize does everything the old synchronous dispatch()'s tail did once a
// run (or its one nudge follow-up) is done: check the verified delivery
// outcome, post a HITL question, or post the run's answer as a comment.
func (e *Extension) finalize(chatID string, pr *pendingRun, outcome sdk.RunOutcome) {
	defer e.inflight.Delete(pr.sessionID)
	defer e.pending.Delete(chatID)
	owner, repo, number := pr.owner, pr.repo, pr.number

	// Only post a summary when nothing was delivered — commitDelivery already
	// posted the review/PR. A push that landed but left the head at the SHA it
	// already was (a fix run that correctly found nothing to fix, #876/#880/
	// #882) is NOT delivered work: GitHub shows no trace of the run, so the
	// answer is the only place its analysis survives - fall through and post it.
	if d, ok := takeDeliveryDetail(chatID); ok {
		if d.err != nil {
			// A worker's own report can't be trusted here (#714) — it may claim success it never had.
			e.host.Log.Error("github: staged delivery failed", "repo", owner+"/"+repo, "issue", number, "err", d.err)
			e.postDeliveryFailure(owner, repo, number, d)
			return
		}
		e.host.Log.Info("github: delivery verified against GitHub", "repo", owner+"/"+repo, "issue", number,
			"pr_number", d.prNumber, "pr_url", d.prURL, "pushed_sha", d.pushedSHA)
		headUnchanged := d.pushedSHA != "" && d.pushedSHA == pr.gh.snap.HeadSHA
		if d.reviewDelivered || !headUnchanged {
			if d.reviewDelivered {
				baselineCtx, baselineCancel := context.WithTimeout(context.Background(), 10*time.Second)
				e.advanceReviewBaseline(baselineCtx, chatID, pr.gh.snap.Commits)
				baselineCancel()

				mergeCtx, mergeCancel := context.WithTimeout(context.Background(), mergeTimeout)
				e.tryMergeStandingIntent(mergeCtx, owner, repo, number, chatID, pr.gh.snap.HeadSHA)
				mergeCancel()
			}
			e.persistGithubSnapshot(chatID, pr.gh)
			e.host.Log.Info("github: work delivered on the PR; skipping the duplicate summary comment", "repo", owner+"/"+repo, "issue", number)
			return
		}
		e.host.Log.Info("github: push left the PR head unchanged and no review was posted; posting the run's answer instead of a silent no-op",
			"repo", owner+"/"+repo, "issue", number, "pushed_sha", d.pushedSHA)
	}

	// User cancelled: Answer is mid-thought, not a finished product - post
	// nothing (no comment, no nudge re-dispatch above), just settle records
	// like a normal completion.
	if outcome.Status == sdk.RunCancelled {
		e.persistGithubSnapshot(chatID, pr.gh)
		e.host.Log.Info("github: run cancelled by user; no comment posted", "repo", owner+"/"+repo, "issue", number)
		return
	}

	// HITL pause: post the question as a comment; the reply resumes the paused node.
	if outcome.Status == sdk.RunNeedsInput {
		comment := fmt.Sprintf("⏸️ quack has a question before proceeding:\n\n**%s**\n\n%s", outcome.NodeID, outcome.Question)
		hitlCtx, hitlCancel := context.WithTimeout(context.Background(), time.Minute)
		defer hitlCancel()
		if err := e.app.postIssueComment(hitlCtx, owner, repo, number, comment); err != nil {
			e.host.Log.Error("github: HITL question comment post failed", "repo", owner+"/"+repo, "issue", number, "err", err)
		} else {
			e.host.Log.Info("github: HITL question posted", "repo", owner+"/"+repo, "issue", number, "node", outcome.NodeID)
		}
		return
	}

	answer := strings.TrimSpace(outcome.Answer)
	switch {
	case outcome.TimedOut:
		answer = fmt.Sprintf("⚠️ quack hit its run deadline before finishing; nothing was delivered. Re-apply the label to retry.\n\nLast progress:\n\n%s", answer)
	case answer == "":
		// Silent-gap (#568) — run finished (or failed) with nothing to say.
		e.host.Log.Warn("github: run completed with no final answer", "repo", owner+"/"+repo, "issue", number, "status", outcome.Status)
		answer = "⚠️ quack finished this run but produced no answer - no error, no failed node, nothing delivered. " +
			"That's a silent-gap failure, not a run with nothing to say. Re-apply the label to retry."
	case pr.isPlan:
		e.app.collapsePriorComments(context.Background(), owner, repo, number, "plan")
		answer += "\n\n" + deliveryMarker("plan")
	}

	tailCtx, tailCancel := context.WithTimeout(context.Background(), time.Minute)
	defer tailCancel()
	if err := e.app.postIssueComment(tailCtx, owner, repo, number, answer); err != nil {
		e.host.Log.Error("github comment post failed", "repo", owner+"/"+repo, "issue", number, "err", err)
		return
	}
	if !outcome.TimedOut {
		e.persistGithubSnapshot(chatID, pr.gh)
	}
	e.host.Log.Info("github comment posted", "repo", owner+"/"+repo, "issue", number, "timed_out", outcome.TimedOut)
}

// postDeliveryFailure reports a failed delivery on GitHub, so a pushed-but-unopened branch is recoverable by hand instead of sitting silently invisible (#714).
func (e *Extension) postDeliveryFailure(owner, repo string, number int, d deliveryOutcome) {
	msg := fmt.Sprintf("⚠️ delivery failed: %s", d.err)
	if d.branch != "" {
		msg += fmt.Sprintf("\n\nBranch `%s` was not delivered — recover it by hand.", d.branch)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := e.app.postIssueComment(ctx, owner, repo, number, msg); err != nil {
		e.host.Log.Error("github: delivery-failure comment post failed", "repo", owner+"/"+repo, "issue", number, "err", err)
	}
}

// loadGithubContext fetches current GitHub state and diffs it against the stored snapshot.
// Does NOT persist — persistGithubSnapshot runs on completion.
func (e *Extension) loadGithubContext(ctx context.Context, chatID, owner, repo string, number int, isPR bool, triggerCommentID int64, forceReseed bool) githubContext {
	snap, err := e.fetchSnapshot(ctx, owner, repo, number, isPR)
	if err != nil {
		// The required meta call (issueMeta/pullMeta, already retried at the
		// HTTP layer for transient failures) still failed - this is NOT a
		// legitimately empty issue, it's GitHub unreachable. Flag it so a
		// label-triggered work request can refuse to run blind rather than
		// silently treating the empty snapshot as "no discussion yet" (#467).
		e.host.Log.Warn("github: fetchSnapshot failed; this turn has no usable GitHub context",
			"repo", owner+"/"+repo, "number", number, "err", err)
		return githubContext{snap: snap, firstLoad: true, contextUnavailable: true}
	}

	var prevJSON string
	var hasPrev bool
	if !forceReseed {
		prevJSON, hasPrev, err = e.store.GetSnapshot(ctx, chatID)
		if err != nil {
			e.host.Log.Warn("github: GetSnapshot failed; treating this as a first load", "chat", chatID, "err", err)
			hasPrev = false
		}
	}

	gh := githubContext{snap: snap}
	if !hasPrev {
		gh.firstLoad = true
	} else {
		prev, uerr := unmarshalSnapshot(prevJSON)
		if uerr != nil {
			e.host.Log.Warn("github: stored snapshot did not decode; treating this as a first load", "chat", chatID, "err", uerr)
			gh.firstLoad = true
		} else {
			delta := diffSnapshots(prev, snap, triggerCommentID)
			gh.delta = &delta
		}
	}
	// The incremental-review scope is DELIBERATELY not delta.NewCommits above:
	// that delta advances on every dispatch (comment/label/etc. included), so
	// scoping a review off it would under-scope whenever a conversational
	// dispatch landed between two reviews. reviewScope reads a SEPARATE
	// baseline that only a delivered review advances (see advanceReviewBaseline).
	if isPR {
		gh.newCommits = e.reviewScope(ctx, chatID, snap)
		// #876/#880/#882: a review that never sees CI status can approve a PR
		// with a failing required check. Best-effort - a fetch failure leaves
		// the envelope silent on CI rather than aborting the run.
		if snap.HeadSHA != "" {
			if checks, cerr := e.app.listCheckRuns(ctx, owner, repo, snap.HeadSHA); cerr != nil {
				e.host.Log.Warn("github: check-runs fetch failed; envelope carries no CI status", "repo", owner+"/"+repo, "pr", number, "err", cerr)
			} else {
				e.app.enrichFailingChecks(ctx, owner, repo, checks)
				gh.checks = checks
			}
		}
	}
	return gh
}

// persistGithubSnapshot upserts the pre-run snapshot as the new watermark — only on genuine completion.
func (e *Extension) persistGithubSnapshot(chatID string, gh githubContext) {
	if gh.contextUnavailable {
		return
	}
	j, err := marshalSnapshot(gh.snap)
	if err != nil {
		e.host.Log.Warn("github: marshal snapshot failed; not persisted", "chat", chatID, "err", err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := e.store.SetSnapshot(ctx, chatID, j); err != nil {
		e.host.Log.Warn("github: SetSnapshot failed; next resume may re-see this turn's changes", "chat", chatID, "err", err)
	}
}

// reviewScope returns commits not yet covered by quack's last delivered review. Falls back to nil (review everything).
func (e *Extension) reviewScope(ctx context.Context, chatID string, snap Snapshot) []snapshotCommit {
	raw, ok, err := e.store.GetReviewBaseline(ctx, chatID)
	if err != nil {
		e.host.Log.Warn("github: GetReviewBaseline failed; reviewing everything this run", "chat", chatID, "err", err)
		return nil
	}
	if !ok {
		return nil
	}
	ids, err := unmarshalPatchIDs(raw)
	if err != nil {
		e.host.Log.Warn("github: stored review baseline did not decode; reviewing everything this run", "chat", chatID, "err", err)
		return nil
	}
	reviewed := make(map[string]bool, len(ids))
	for _, id := range ids {
		reviewed[id] = true
	}
	return newCommitsAgainstBaseline(snap.Commits, reviewed)
}

// advanceReviewBaseline persists current PR commits' patch-ids — only after a review is actually delivered.
func (e *Extension) advanceReviewBaseline(ctx context.Context, chatID string, commits []snapshotCommit) {
	ids := make([]string, 0, len(commits))
	for _, c := range commits {
		if c.PatchID != "" {
			ids = append(ids, c.PatchID)
		}
	}
	j, err := marshalPatchIDs(ids)
	if err != nil {
		e.host.Log.Warn("github: marshal review baseline failed; not persisted", "chat", chatID, "err", err)
		return
	}
	if err := e.store.SetReviewBaseline(ctx, chatID, j); err != nil {
		e.host.Log.Warn("github: SetReviewBaseline failed; the next review may under-scope", "chat", chatID, "err", err)
	}
}
