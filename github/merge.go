package github

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strings"
	"time"
)

// mergeTimeout bounds one merge evaluation (a few API calls).
const mergeTimeout = 2 * time.Minute

// requiredCheckFailingRe matches GitHub's merge-block message for one named
// required status check, e.g. `Required status check "go-test" is failing.`
var requiredCheckFailingRe = regexp.MustCompile(`(?i)required status check "([^"]+)" is failing`)

// mergePendingRe matches GitHub refusing a merge only because a required
// check has not finished yet - not a failure, the next check event retries.
var mergePendingRe = regexp.MustCompile(`(?i)required status check.*\b(in progress|expected|pending|queued)\b`)

// mergeAPIErrorMessage extracts the "message" field from a wrapped merge API
// error's trailing JSON body so the review never carries raw JSON - full
// detail stays in the slog entry that logged err.
func mergeAPIErrorMessage(err error) string {
	msg := err.Error()
	if i := strings.IndexByte(msg, '{'); i >= 0 {
		var body struct {
			Message string `json:"message"`
		}
		if jerr := json.Unmarshal([]byte(msg[i:]), &body); jerr == nil && body.Message != "" {
			return body.Message
		}
	}
	return msg
}

// isHeadBranchModified reports GitHub's "the tip advanced mid-merge" error (#1142).
func isHeadBranchModified(err error) bool {
	return strings.Contains(mergeAPIErrorMessage(err), "Head branch was modified")
}

func isMergePending(err error) bool {
	return mergePendingRe.MatchString(mergeAPIErrorMessage(err))
}

// mergeFailureLine turns a merge API error into the one line appended to the
// approving review. A named failing required check gets the actionable form.
func mergeFailureLine(err error, fixLabel string) string {
	msg := strings.TrimSuffix(mergeAPIErrorMessage(err), ".")
	if m := requiredCheckFailingRe.FindStringSubmatch(msg); m != nil {
		return fmt.Sprintf("Merge blocked: required check `%s` is failing. Apply `%s` or push a fix; re-review follows.", m[1], fixLabel)
	}
	return "Merge failed: " + msg + "."
}

// mergeOutcome is what one tryMerge evaluation found.
type mergeOutcome int

const (
	mergeNoIntent     mergeOutcome = iota // no standing authorization, or the PR is already closed
	mergeUnreviewed                       // quack has no verdict on this PR yet
	mergeNotApproved                      // latest verdict is request_changes/comment
	mergeStale                            // approval is for an older head
	mergePending                          // no check runs yet, or one on the head isn't green
	mergeHumanBlocked                     // a non-dismissed CHANGES_REQUESTED from another reviewer stands on the current head
	mergeFailed                           // GitHub refused for a real reason (conflict, protection)
	mergeDone
)

// greenConclusions are check-run conclusions that count as passing.
var greenConclusions = map[string]bool{"success": true, "neutral": true, "skipped": true}

// allChecksGreen reports whether the head has actually finished CI clean. An
// EMPTY list is not green: pull_request_review.submitted can arrive before
// any workflow has even queued (event ordering), and treating "nothing
// reported yet" as "nothing to wait for" merges a PR CI hasn't touched.
func allChecksGreen(checks []checkRunView) bool {
	if len(checks) == 0 {
		return false
	}
	for _, c := range checks {
		if c.Status != "completed" || !greenConclusions[c.Conclusion] {
			return false
		}
	}
	return true
}

// ciSuiteState is what headHasPendingCI found among the head's check
// suites, beyond the empty check-run list that triggered the lookup.
type ciSuiteState int

const (
	ciClear   ciSuiteState = iota // nothing left that could ever post a run: attempt the merge
	ciWaiting                     // a suite that can still produce a run hasn't finished
	ciFailed                      // a suite finished with a non-green conclusion and posted no runs
)

// headHasPendingCI classifies the head's check suites. Called only when
// listCheckRuns came back empty, to tell apart "CI queued, no run reported
// yet" (wait), "a suite already failed without ever posting a run" (stop -
// GitHub's own required-check refusal would read as merely "expected" and
// never resolve), and "no CI on this head at all" (nothing to wait for). A
// suite is only pending while it hasn't reached "completed": a terminal
// suite - success or failure - will never fire another event, so treating
// it as pending would stall the merge forever with no re-evaluation.
func (e *Extension) headHasPendingCI(ctx context.Context, owner, repo, sha string) (ciSuiteState, error) {
	suites, err := e.app.listCheckSuites(ctx, owner, repo, sha)
	if err != nil {
		return ciClear, err
	}
	state := ciClear
	for _, s := range suites {
		if !s.producesRuns() {
			continue
		}
		if s.Status != "completed" {
			return ciWaiting, nil
		}
		if !greenConclusions[s.Conclusion] {
			state = ciFailed
		}
	}
	return state, nil
}

// blockingHumanReviewer reports the first reviewer (any login, quack's own
// included) whose latest review AGAINST THE CURRENT HEAD is a standing
// CHANGES_REQUESTED (5): the quack:merge label authorizes merging a green,
// approved PR, never overriding a live human objection. A review against an
// older head is stale the same way an old approval is (#71) - a push clears
// it; only a later approval or an explicit dismissal (both show up as a
// newer, or state-changed, review from that same login) clears it otherwise.
func blockingHumanReviewer(reviews []prReview, headSHA string) (login string, blocked bool) {
	type latest struct {
		state string
		at    time.Time
	}
	byUser := make(map[string]latest, len(reviews))
	for _, r := range reviews {
		if r.CommitID != headSHA {
			continue
		}
		at, _ := time.Parse(time.RFC3339, r.SubmittedAt)
		if prev, ok := byUser[r.User.Login]; ok && !at.After(prev.at) {
			continue
		}
		byUser[r.User.Login] = latest{state: r.State, at: at}
	}
	for user, l := range byUser {
		if l.state == "CHANGES_REQUESTED" {
			return user, true
		}
	}
	return "", false
}

// reviewVerdictMarkerRe extracts quack's verdict from the hidden marker in an own-PR review comment (GitHub forbids self-review).
var reviewVerdictMarkerRe = regexp.MustCompile(`<!-- quack:delivery:review:(approve|request_changes|comment) -->`)

// reviewHeadMarkerRe extracts the head SHA an own-PR verdict was pinned
// against - embedded alongside the verdict marker since a plain issue
// comment (unlike a formal review) has no commit_id of its own to compare.
var reviewHeadMarkerRe = regexp.MustCompile(`<!-- quack:delivery:head:(\S+) -->`)

// formalReviewVerdicts maps GitHub review states to the same vocabulary as reviewVerdictMarkerRe.
var formalReviewVerdicts = map[string]string{
	"APPROVED":          "approve",
	"CHANGES_REQUESTED": "request_changes",
	"COMMENTED":         "comment",
}

// verdictRef is quack's latest verdict on a PR plus where it lives, so a
// merge outcome can be appended to that same body. commitID is empty when no
// head SHA is available: an issue-comment marker predating
// quack:delivery:head, or a review whose GitHub-reported commit_id is
// somehow empty and whose body has no head marker either.
type verdictRef struct {
	verdict   string
	commitID  string
	reviewID  int64
	commentID int64
	body      string
	at        time.Time
}

// latestQuackVerdict reads both formal reviews and own-PR comment markers.
func (e *Extension) latestQuackVerdict(ctx context.Context, owner, repo string, number int) (verdictRef, error) {
	bot, err := e.app.botLogin(ctx)
	if err != nil {
		return verdictRef{}, err
	}
	var verdicts []verdictRef

	reviews, err := e.app.listReviews(ctx, owner, repo, number)
	if err != nil {
		return verdictRef{}, err
	}
	for _, r := range reviews {
		if r.User.Login != bot {
			continue
		}
		at, _ := time.Parse(time.RFC3339, r.SubmittedAt)
		// Marker first: an own-PR review always submits as state COMMENTED
		// (GitHub disallows approve/request_changes on your own PR) but carries
		// the REAL verdict in the marker - the state alone would read as "comment".
		if m := reviewVerdictMarkerRe.FindStringSubmatch(r.Body); m != nil {
			commitID := r.CommitID
			if commitID == "" {
				if hm := reviewHeadMarkerRe.FindStringSubmatch(r.Body); hm != nil {
					commitID = hm[1]
				}
			}
			verdicts = append(verdicts, verdictRef{verdict: m[1], commitID: commitID, reviewID: r.ID, body: r.Body, at: at})
			continue
		}
		if v := formalReviewVerdicts[r.State]; v != "" {
			verdicts = append(verdicts, verdictRef{verdict: v, commitID: r.CommitID, reviewID: r.ID, body: r.Body, at: at})
		}
	}

	comments, err := e.app.listIssueComments(ctx, owner, repo, number)
	if err != nil {
		return verdictRef{}, err
	}
	for _, c := range comments {
		if c.User != bot {
			continue
		}
		m := reviewVerdictMarkerRe.FindStringSubmatch(c.Body)
		if m == nil {
			continue
		}
		at, _ := time.Parse(time.RFC3339, c.CreatedAt)
		commitID := ""
		if hm := reviewHeadMarkerRe.FindStringSubmatch(c.Body); hm != nil {
			commitID = hm[1]
		}
		verdicts = append(verdicts, verdictRef{verdict: m[1], commitID: commitID, commentID: c.ID, body: c.Body, at: at})
	}

	if len(verdicts) == 0 {
		return verdictRef{}, nil
	}
	sort.SliceStable(verdicts, func(i, j int) bool { return verdicts[i].at.Before(verdicts[j].at) })
	return verdicts[len(verdicts)-1], nil
}

// appendToVerdict appends line to the review (or own-PR comment) carrying
// the verdict, once. Only when there is nothing to edit does the line become
// a comment of its own - still the single message for this outcome.
func (e *Extension) appendToVerdict(ctx context.Context, owner, repo string, number int, ref verdictRef, line string) {
	body, changed := appendLine(ref.body, line)
	if !changed {
		return
	}
	var err error
	switch {
	case ref.reviewID != 0:
		err = e.app.updateReview(ctx, owner, repo, number, ref.reviewID, body)
	case ref.commentID != 0:
		err = e.app.editIssueComment(ctx, owner, repo, ref.commentID, body)
	default:
		err = fmt.Errorf("no review to edit")
	}
	if err == nil {
		return
	}
	slog.Warn("github: appending the merge outcome to the review failed; posting it as a comment",
		"component", "github", "repo", owner+"/"+repo, "pr", number, "err", err)
	if perr := e.app.postIssueComment(ctx, owner, repo, number, line); perr != nil {
		slog.Error("github: merge outcome comment failed", "component", "github", "repo", owner+"/"+repo, "pr", number, "err", perr)
	}
}

// tryMerge is the one merge decision, re-run on every event that can change
// its inputs: merge iff a standing intent (the label) exists, quack's latest
// verdict approves the CURRENT head, every check run on that head is green
// (at least one exists, and none is missing/running/failed), and no other
// reviewer has a standing CHANGES_REQUESTED on that head. Anything short of
// that is silent - the next event re-evaluates, with no timer and no
// polling. A non-nil error is an infrastructure failure (GitHub/store
// unreadable), never a merge refusal. Serialized per PR (e.mergeMu).
func (e *Extension) tryMerge(ctx context.Context, owner, repo string, number int) (mergeOutcome, error) {
	sessionID := fmt.Sprintf("github-%s-%s-%d", owner, repo, number)
	chatID := globalChatID(sessionID)
	unlock := e.mergeMu.Lock(chatID)
	defer unlock()

	intent, err := e.store.GetMergeIntent(ctx, chatID)
	if err != nil {
		return mergeNoIntent, fmt.Errorf("merge-intent lookup: %w", err)
	}
	if intent == nil {
		return mergeNoIntent, nil
	}
	ref, err := e.latestQuackVerdict(ctx, owner, repo, number)
	if err != nil {
		return mergeNoIntent, fmt.Errorf("review lookup: %w", err)
	}
	switch ref.verdict {
	case "":
		return mergeUnreviewed, nil
	case "approve":
	default:
		return mergeNotApproved, nil
	}
	m, err := e.app.pullMeta(ctx, owner, repo, number)
	if err != nil {
		return mergeNoIntent, fmt.Errorf("pull lookup: %w", err)
	}
	if m.Merged || m.State == "closed" {
		e.clearMergeIntent(ctx, chatID)
		return mergeNoIntent, nil
	}
	// #71: the approval must be for the PR's actual current head; a push
	// after it invalidates it. An own-PR marker comment carries no commit_id
	// and is trusted as-is.
	if ref.commitID != "" && ref.commitID != m.HeadSHA {
		return mergeStale, nil
	}
	allReviews, rerr := e.app.listReviews(ctx, owner, repo, number)
	if rerr != nil {
		return mergeNoIntent, fmt.Errorf("review lookup: %w", rerr)
	}
	if login, blocked := blockingHumanReviewer(allReviews, m.HeadSHA); blocked {
		slog.Debug("github: merge blocked by a standing human objection", "component", "github", "repo", owner+"/"+repo, "pr", number, "reviewer", login)
		return mergeHumanBlocked, nil
	}
	if checks, cerr := e.app.listCheckRuns(ctx, owner, repo, m.HeadSHA); cerr != nil {
		slog.Warn("github: check-runs lookup failed before merge; letting GitHub decide", "component", "github", "repo", owner+"/"+repo, "pr", number, "err", cerr)
	} else if len(checks) == 0 {
		// Zero runs is ambiguous: pull_request_review.submitted can beat every
		// workflow's queue (event ordering), this head may never get a run at
		// all (docs-only PR under path filters, no CI), or a suite already
		// failed without posting one. GitHub creates a check suite for every
		// workflow a push triggers before any run exists, so consult that to
		// tell the three apart - no CI ever waits forever for an event that
		// never arrives.
		if state, serr := e.headHasPendingCI(ctx, owner, repo, m.HeadSHA); serr != nil {
			slog.Warn("github: check-suites lookup failed before merge; letting GitHub decide", "component", "github", "repo", owner+"/"+repo, "pr", number, "err", serr)
		} else if state == ciWaiting {
			return mergePending, nil
		} else if state == ciFailed {
			e.appendToVerdict(ctx, owner, repo, number, ref, "Merge blocked: CI failed on this head and posted no check run to retry.")
			return mergeFailed, nil
		}
		// ciClear (or the lookup failed): no relevant suite - attempt the
		// merge and let GitHub's own required-check refusal (mergePendingRe)
		// be the guard wherever branch protection is actually configured.
	} else if !allChecksGreen(checks) {
		return mergePending, nil
	}
	sha, err := e.app.mergePR(ctx, owner, repo, number, m.HeadSHA)
	if err != nil {
		switch {
		case isHeadBranchModified(err):
			return mergeStale, nil
		case isMergePending(err):
			return mergePending, nil
		}
		slog.Error("github merge failed", "component", "github", "repo", owner+"/"+repo, "pr", number, "err", err)
		e.appendToVerdict(ctx, owner, repo, number, ref, mergeFailureLine(err, e.labels.Fix))
		return mergeFailed, nil
	}
	slog.Info("github pr merged", "component", "github", "repo", owner+"/"+repo, "pr", number, "user", intent.RequestedBy, "sha", sha)
	if e.autoArchiveOnMerge && e.host.ArchiveChat != nil {
		if derr := e.host.ArchiveChat(chatID); derr != nil {
			slog.Warn("github: auto-archive on merge failed", "component", "github", "repo", owner+"/"+repo, "pr", number, "err", derr)
		}
	}
	// Clear the intent BEFORE announcing: once the line is visible the intent
	// must already be gone, not racing whoever reads it next.
	e.clearMergeIntent(ctx, chatID)
	e.appendToVerdict(ctx, owner, repo, number, ref, "Merged as "+shortSHA(sha)+".")
	return mergeDone, nil
}

func (e *Extension) clearMergeIntent(ctx context.Context, chatID string) {
	if err := e.store.DeleteMergeIntent(ctx, chatID); err != nil {
		slog.Warn("github: merge-intent cleanup failed", "component", "github", "chat", chatID, "err", err)
	}
}

// mergeIfApproved handles the merge label: records the human's standing
// authorization, acks with a reaction, and evaluates the merge. A PR quack
// has not reviewed at its current head gets a review dispatched so the label
// never silently waits for one.
func (e *Extension) mergeIfApproved(p pullRequestPayload, rawBody []byte) {
	owner, repo, number := p.Repository.Owner.Login, p.Repository.Name, p.Number
	sessionID := fmt.Sprintf("github-%s-%s-%d", owner, repo, number)
	chatID := globalChatID(sessionID)
	ctx, cancel := context.WithTimeout(context.Background(), mergeTimeout)
	defer cancel()

	// Recorded BEFORE anything else - fail CLOSED, an unrecorded intent must
	// never be acted on as one.
	if err := e.store.SetMergeIntent(ctx, chatID, p.Sender.Login); err != nil {
		slog.Error("github: merge-intent persist failed", "component", "github", "repo", owner+"/"+repo, "pr", number, "err", err)
		e.comment(ctx, owner, repo, number, fmt.Sprintf("Could not record the merge request: %v. Re-apply `%s` to retry.", err, e.labels.Merge))
		return
	}
	e.ackIssue(owner, repo, number)

	outcome, err := e.tryMerge(ctx, owner, repo, number)
	if err != nil {
		slog.Error("github: merge-label evaluation failed", "component", "github", "repo", owner+"/"+repo, "pr", number, "err", err)
		e.comment(ctx, owner, repo, number, fmt.Sprintf("Could not evaluate the merge: %v. Re-apply `%s` to retry.", err, e.labels.Merge))
		return
	}
	if outcome != mergeUnreviewed && outcome != mergeStale {
		return
	}
	if _, live := e.inflightActive(sessionID); live {
		return // the running review's delivery re-evaluates the merge
	}
	go e.dispatch(autoReviewPayload(p, rawBody), autoReviewTask)
}

// reviewOnMovedHeadUnderIntent dispatches a fresh review when a push moves
// the head of a PR under a standing quack:merge intent (#1277): otherwise an
// approved-then-pushed PR sits waiting for a human to comment /review, since
// nothing else re-reviews a head that moved after delivery already
// finished. Guarded by the head SHA recorded on the intent row itself -
// concurrent or repeated synchronize events for the same head dispatch once.
func (e *Extension) reviewOnMovedHeadUnderIntent(p pullRequestPayload, rawBody []byte) {
	if !e.triggers["merge"] {
		return
	}
	owner, repo, number := p.Repository.Owner.Login, p.Repository.Name, p.Number
	head := p.PullRequest.Head.SHA
	if head == "" {
		return
	}
	sessionID := fmt.Sprintf("github-%s-%s-%d", owner, repo, number)
	chatID := globalChatID(sessionID)
	unlock := e.mergeMu.Lock(chatID)
	defer unlock()

	ctx, cancel := context.WithTimeout(context.Background(), reactionTimeout)
	defer cancel()
	intent, err := e.store.GetMergeIntent(ctx, chatID)
	if err != nil {
		slog.Warn("github: merge-intent lookup for push re-review failed", "component", "github", "repo", owner+"/"+repo, "pr", number, "err", err)
		return
	}
	if intent == nil || intent.DispatchedHead == head {
		return
	}
	if err := e.store.SetMergeIntentDispatchedHead(ctx, chatID, head); err != nil {
		slog.Warn("github: recording the push re-review's head failed", "component", "github", "repo", owner+"/"+repo, "pr", number, "err", err)
		return
	}
	go e.dispatch(autoReviewPayload(p, rawBody), autoReviewTask)
}

// mergeOnEvent re-evaluates the merge after a state-changing webhook (check
// completed, head pushed, review submitted). Silent by design: nothing to
// say until the PR is mergeable, and the outcome then lands on the review.
func (e *Extension) mergeOnEvent(owner, repo string, number int, event string) {
	if !e.triggers["merge"] {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), mergeTimeout)
	defer cancel()
	outcome, err := e.tryMerge(ctx, owner, repo, number)
	if err != nil {
		slog.Warn("github: merge re-evaluation failed", "component", "github", "repo", owner+"/"+repo, "pr", number, "event", event, "err", err)
		return
	}
	slog.Debug("github: merge re-evaluated", "component", "github", "repo", owner+"/"+repo, "pr", number, "event", event, "outcome", outcome)
}

func (e *Extension) comment(ctx context.Context, owner, repo string, number int, text string) {
	if err := e.app.postIssueComment(ctx, owner, repo, number, text); err != nil {
		slog.Error("github: comment failed", "component", "github", "repo", owner+"/"+repo, "issue", number, "err", err)
	}
}

// ackIssue posts a 👀 reaction on the issue/PR itself - the ack for a trigger
// that has no comment to react to (a label, a CI event, a deduped run).
func (e *Extension) ackIssue(owner, repo string, number int) {
	ctx, cancel := context.WithTimeout(context.Background(), reactionTimeout)
	defer cancel()
	if _, err := e.app.reactToIssue(ctx, owner, repo, number, "eyes"); err != nil {
		slog.Warn("github ack reaction failed", "component", "github", "repo", owner+"/"+repo, "issue", number, "err", err)
	}
}

// reReviewMovedHead builds the auto-review payload for a head that moved
// under an approving review (#1142): the standing intent is left untouched
// and merges once THIS review approves the new head. No comment - the fresh
// review is the visible outcome.
func (e *Extension) reReviewMovedHead(ctx context.Context, pr *pendingRun) *issueCommentPayload {
	owner, repo, number := pr.owner, pr.repo, pr.number
	title := ""
	if meta, err := e.app.pullMeta(ctx, owner, repo, number); err == nil {
		title = meta.Title
	}
	cloneURL := ""
	if pr.dispatched.Run.Setup != nil {
		cloneURL = pr.dispatched.Run.Setup.Repo
	}
	p := reReviewPayload(owner, repo, number, title, cloneURL, pr.defaultBranch, pr.installationID)
	return &p
}

// reReviewPayload synthesizes an issueCommentPayload for a re-review that
// quack itself triggers (no webhook event backs it) - same auto-review path
// a label trigger dispatches.
func reReviewPayload(owner, repo string, number int, title, cloneURL, defaultBranch string, installationID int64) issueCommentPayload {
	synthetic := issueCommentPayload{Action: "created"}
	synthetic.Issue.Number = number
	synthetic.Issue.Title = title
	synthetic.Issue.PullRequest = &struct{}{}
	synthetic.Comment.User.Login = autoReviewUser
	synthetic.Repository.Name = repo
	synthetic.Repository.Owner.Login = owner
	synthetic.Repository.CloneURL = cloneURL
	synthetic.Repository.DefaultBranch = defaultBranch
	synthetic.Installation.ID = installationID
	synthetic.isLabelTrigger = true // auto-review, never a mention (T4)
	synthetic.rawEvent = json.RawMessage(`{}`)
	synthetic.eventName = "github.head_branch_modified_rereview"
	return synthetic
}

// checkEventPayload is the shared subset of check_suite / check_run /
// workflow_run webhooks: which PRs the completed CI object belongs to.
type checkEventPayload struct {
	Action      string  `json:"action"`
	CheckSuite  *ciHead `json:"check_suite"`
	CheckRun    *ciHead `json:"check_run"`
	WorkflowRun *ciHead `json:"workflow_run"`
	Repository  struct {
		Name  string `json:"name"`
		Owner struct {
			Login string `json:"login"`
		} `json:"owner"`
	} `json:"repository"`
}

type ciHead struct {
	PullRequests []struct {
		Number int `json:"number"`
	} `json:"pull_requests"`
}

// mergeOnCheckEvent fans a completed CI event out to tryMerge for every PR it
// belongs to. Cheap when nothing is labeled: tryMerge stops at the intent
// lookup before any GitHub call.
//
// Required topology: pull_requests is populated only when the check's head
// AND base both live in p.Repository (GitHub's own scoping) - so for a fork
// PR whose CI runs in the fork's own Actions, this array (and this fan-out)
// is already empty; there is no base-repo PR number here to resolve it
// from. Checks must run where the base repo can see them (the app installed
// on the fork, or a pull_request_target-style workflow) or the merge stalls
// silently the same way an all-empty check-suite list would.
func (e *Extension) mergeOnCheckEvent(event string, body []byte) {
	var p checkEventPayload
	if json.Unmarshal(body, &p) != nil || p.Action != "completed" {
		return
	}
	head := p.CheckSuite
	if head == nil {
		head = p.CheckRun
	}
	if head == nil {
		head = p.WorkflowRun
	}
	if head == nil {
		return
	}
	for _, pr := range head.PullRequests {
		go e.mergeOnEvent(p.Repository.Owner.Login, p.Repository.Name, pr.Number, event)
	}
}
