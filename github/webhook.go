package github

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/fagerbergj/quack-extensions/sdk"
)

// maxWebhookBody bounds a hostile/oversized request.
const maxWebhookBody = 5 << 20

// issueCommentPayload is the subset of GitHub's issue_comment webhook we use.
type issueCommentPayload struct {
	Action  string `json:"action"`
	Comment struct {
		ID   int64  `json:"id"`
		Body string `json:"body"`
		User struct {
			Login string `json:"login"`
		} `json:"user"`
	} `json:"comment"`
	Issue struct {
		Number int    `json:"number"`
		Title  string `json:"title"`
		Body   string `json:"body"`
		State  string `json:"state"`
		// Present only when the issue is a PR.
		PullRequest *struct{} `json:"pull_request"`
	} `json:"issue"`
	Repository struct {
		Name  string `json:"name"`
		Owner struct {
			Login string `json:"login"`
		} `json:"owner"`
		CloneURL      string `json:"clone_url"`
		DefaultBranch string `json:"default_branch"`
	} `json:"repository"`
	Installation struct {
		ID int64 `json:"id"`
	} `json:"installation"`

	// Synthetic payload fields — not part of the GitHub webhook.
	planOnly        bool            // label-driven plan: produce a plan, touch no code.
	isLabelTrigger  bool            // label/pr_opened trigger vs @mention (T4 session reset).
	deliverableHint string          // fixed deliverable for synthetic triggers (CI auto-heal, own-PR).
	rawEvent        json.RawMessage // originating webhook JSON → envelope's <event> block.
	eventName       string          // originating webhook dotted name.
	checkSHA        string          // CI commit: dump check-runs.json. "" = plan/review/mention run.
	// issueDeliverableCache memoizes classifyIssueDeliverable for one dispatch:
	// shared by pointer across every copy of p passed to
	// buildEnvelope/buildWorkerAsk/deliverableIsPlan, so a live classifier
	// call happens at most once regardless of how many of them need the
	// answer. nil when a caller (e.g. a test) invokes one of those directly.
	issueDeliverableCache *issueDeliverableResult
}

// issuesPayload is the issues webhook subset for the label-driven issue workflow.
type issuesPayload struct {
	Action string `json:"action"`
	Issue  struct {
		Number int    `json:"number"`
		Title  string `json:"title"`
		Body   string `json:"body"`
		State  string `json:"state"`
		// Present when the "issue" is actually a PR.
		PullRequest *struct{} `json:"pull_request"`
	} `json:"issue"`
	Label struct {
		Name string `json:"name"`
	} `json:"label"`
	Repository struct {
		Name  string `json:"name"`
		Owner struct {
			Login string `json:"login"`
		} `json:"owner"`
		CloneURL      string `json:"clone_url"`
		DefaultBranch string `json:"default_branch"`
	} `json:"repository"`
	Installation struct {
		ID int64 `json:"id"`
	} `json:"installation"`
	Sender struct {
		Login string `json:"login"`
	} `json:"sender"`
}

// pullRequestPayload is the PR webhook subset for opened/labeled/closed/reopened actions.
type pullRequestPayload struct {
	Action      string `json:"action"`
	Number      int    `json:"number"`
	PullRequest struct {
		Title string `json:"title"`
		Head  struct {
			SHA string `json:"sha"`
		} `json:"head"`
		State  string `json:"state"`
		Merged bool   `json:"merged"` // set on "closed" - distinguishes a merge from a plain close
	} `json:"pull_request"`
	Label struct {
		Name string `json:"name"` // present on the "labeled" action
	} `json:"label"`
	Repository struct {
		Name  string `json:"name"`
		Owner struct {
			Login string `json:"login"`
		} `json:"owner"`
		CloneURL      string `json:"clone_url"`
		DefaultBranch string `json:"default_branch"`
	} `json:"repository"`
	Installation struct {
		ID int64 `json:"id"`
	} `json:"installation"`
	Sender struct {
		Login string `json:"login"`
	} `json:"sender"`
}

// autoReviewTask for a pr_opened/label-triggered auto-review.
const autoReviewTask = "Review this pull request and post your findings as inline review comments and a verdict."

// autoReviewUser is the synthetic commenter for an auto-review run.
const autoReviewUser = "quack-auto-review"

// handleWebhook verifies HMAC signature, dispatches by event type, and returns
// fast — the run happens in a goroutine (GitHub enforces ~10s webhook timeout).
func (e *Extension) handleWebhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxWebhookBody))
	if err != nil {
		http.Error(w, "cannot read body", http.StatusBadRequest)
		return
	}
	if !verifySignature(e.secret, body, r.Header.Get("X-Hub-Signature-256")) {
		slog.Warn("github webhook: signature verification failed", "component", "github")
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}

	switch r.Header.Get("X-GitHub-Event") {
	case "issue_comment":
		e.handleIssueComment(w, body)
	case "pull_request":
		e.handlePullRequest(w, body)
	case "pull_request_review":
		e.handlePullRequestReview(w, body)
	case "issues":
		e.handleIssues(w, body)
	case "workflow_run":
		e.handleWorkflowRun(w, body)
	default:
		w.WriteHeader(http.StatusOK) // unhandled event type: no-op ack
	}
}

func (e *Extension) handleIssueComment(w http.ResponseWriter, body []byte) {
	var p issueCommentPayload
	if err := json.Unmarshal(body, &p); err != nil {
		http.Error(w, "invalid payload", http.StatusBadRequest)
		return
	}
	// Never act on another bot's comments.
	if strings.HasSuffix(p.Comment.User.Login, "[bot]") {
		w.WriteHeader(http.StatusOK)
		return
	}
	task, ok := e.triggerTask(p)
	if !ok {
		w.WriteHeader(http.StatusOK) // not a mention we act on: no-op ack
		return
	}
	p.rawEvent = json.RawMessage(body)
	p.eventName = "issue_comment." + p.Action
	if !e.isInvokerAllowed(p.Comment.User.Login) {
		slog.Warn("github webhook: invoker not in allowed_users; ignoring", "component", "github",
			"repo", p.Repository.Owner.Login+"/"+p.Repository.Name, "issue", p.Issue.Number, "user", p.Comment.User.Login)
		w.WriteHeader(http.StatusOK)
		return
	}

	slog.Info("github webhook received", "component", "github",
		"repo", p.Repository.Owner.Login+"/"+p.Repository.Name, "issue", p.Issue.Number,
		"user", p.Comment.User.Login, "installation", p.Installation.ID)
	go e.ackReaction(p) // instant 👀 "quack saw it", independent of the model run
	go e.dispatch(p, task)
	w.WriteHeader(http.StatusAccepted)
}

// handlePullRequest fires an auto-review on "opened" or "labeled" with the configured auto_review_label,
// and refreshes the sidebar badge on close/merge/reopen.
func (e *Extension) handlePullRequest(w http.ResponseWriter, body []byte) {
	var p pullRequestPayload
	if err := json.Unmarshal(body, &p); err != nil {
		http.Error(w, "invalid payload", http.StatusBadRequest)
		return
	}

	if p.Action == "closed" || p.Action == "reopened" {
		badge := "open"
		switch {
		case p.Action == "closed" && p.PullRequest.Merged:
			badge = "merged"
		case p.Action == "closed":
			badge = "closed"
		}
		e.refreshChatOrigin(p.Repository.Owner.Login, p.Repository.Name, true, p.Number, badge)
		w.WriteHeader(http.StatusOK)
		return
	}

	// The merge label is a human authorization — checks quack's verdict and merges (or explains why not).
	if p.Action == "labeled" && e.triggers["merge"] && p.Label.Name == e.labels.Merge &&
		!strings.HasSuffix(p.Sender.Login, "[bot]") {
		if !e.isInvokerAllowed(p.Sender.Login) {
			slog.Warn("github webhook: invoker not in allowed_users; ignoring", "component", "github",
				"repo", p.Repository.Owner.Login+"/"+p.Repository.Name, "pr", p.Number,
				"label", p.Label.Name, "user", p.Sender.Login)
			w.WriteHeader(http.StatusOK)
			return
		}
		slog.Info("github webhook received", "component", "github",
			"repo", p.Repository.Owner.Login+"/"+p.Repository.Name, "pr", p.Number,
			"label", p.Label.Name, "user", p.Sender.Login, "installation", p.Installation.ID)
		go e.mergeIfApproved(p, body)
		w.WriteHeader(http.StatusAccepted)
		return
	}

	// quack:fix is a persistent capability flag (#656) — re-arms auto-heal; fixes CI if currently failing.
	if p.Action == "labeled" && e.triggers["ci_fix"] && p.Label.Name == e.labels.Fix &&
		!strings.HasSuffix(p.Sender.Login, "[bot]") {
		if !e.isInvokerAllowed(p.Sender.Login) {
			slog.Warn("github webhook: invoker not in allowed_users; ignoring", "component", "github",
				"repo", p.Repository.Owner.Login+"/"+p.Repository.Name, "pr", p.Number,
				"label", p.Label.Name, "user", p.Sender.Login)
			w.WriteHeader(http.StatusOK)
			return
		}
		slog.Info("github webhook received", "component", "github",
			"repo", p.Repository.Owner.Login+"/"+p.Repository.Name, "pr", p.Number,
			"label", p.Label.Name, "user", p.Sender.Login, "installation", p.Installation.ID)
		go e.fixLabelApplied(p, body)
		w.WriteHeader(http.StatusAccepted)
		return
	}

	fires := (p.Action == "opened" && e.triggers["pr_opened"]) ||
		(p.Action == "labeled" && e.triggers["label"] && p.Label.Name == e.labels.Review)
	if !fires {
		w.WriteHeader(http.StatusOK)
		return
	}

	slog.Info("github webhook received", "component", "github",
		"repo", p.Repository.Owner.Login+"/"+p.Repository.Name, "pr", p.Number,
		"action", p.Action, "installation", p.Installation.ID)
	go e.dispatch(autoReviewPayload(p, body), autoReviewTask)
	w.WriteHeader(http.StatusAccepted)
}

// autoReviewPayload shapes a PR event as an issueCommentPayload so the mention path's dispatch/envelope builder handles it.
func autoReviewPayload(p pullRequestPayload, rawBody []byte) issueCommentPayload {
	synthetic := issueCommentPayload{Action: "created"}
	synthetic.Issue.Number = p.Number
	synthetic.Issue.Title = p.PullRequest.Title
	synthetic.Issue.PullRequest = &struct{}{}
	synthetic.Comment.User.Login = autoReviewUser
	synthetic.Repository.Name = p.Repository.Name
	synthetic.Repository.Owner.Login = p.Repository.Owner.Login
	synthetic.Repository.CloneURL = p.Repository.CloneURL
	synthetic.Repository.DefaultBranch = p.Repository.DefaultBranch
	synthetic.Installation.ID = p.Installation.ID
	synthetic.isLabelTrigger = true // auto-review, never a mention (T4)
	synthetic.rawEvent = json.RawMessage(rawBody)
	synthetic.eventName = "pull_request." + p.Action
	return synthetic
}

// pullRequestReviewPayload handles request_changes on a PR quack authored (#656).
type pullRequestReviewPayload struct {
	Action string `json:"action"`
	Review struct {
		State string `json:"state"` // "approved" | "changes_requested" | "commented"
		User  struct {
			Login string `json:"login"`
		} `json:"user"`
	} `json:"review"`
	PullRequest struct {
		Number int `json:"number"`
	} `json:"pull_request"`
	Repository struct {
		Name  string `json:"name"`
		Owner struct {
			Login string `json:"login"`
		} `json:"owner"`
		CloneURL      string `json:"clone_url"`
		DefaultBranch string `json:"default_branch"`
	} `json:"repository"`
	Installation struct {
		ID int64 `json:"id"`
	} `json:"installation"`
}

// handlePullRequestReview engages only on request_changes to a PR quack authored — gated on ci_fix.
func (e *Extension) handlePullRequestReview(w http.ResponseWriter, body []byte) {
	var p pullRequestReviewPayload
	if err := json.Unmarshal(body, &p); err != nil {
		http.Error(w, "invalid payload", http.StatusBadRequest)
		return
	}
	if p.Action != "submitted" || p.Review.State != "changes_requested" || !e.triggers["ci_fix"] {
		w.WriteHeader(http.StatusOK)
		return
	}
	if strings.HasSuffix(p.Review.User.Login, "[bot]") {
		w.WriteHeader(http.StatusOK)
		return
	}
	if !e.isInvokerAllowed(p.Review.User.Login) {
		slog.Warn("github webhook: invoker not in allowed_users; ignoring", "component", "github",
			"repo", p.Repository.Owner.Login+"/"+p.Repository.Name, "pr", p.PullRequest.Number, "user", p.Review.User.Login)
		w.WriteHeader(http.StatusOK)
		return
	}
	slog.Info("github webhook received", "component", "github",
		"repo", p.Repository.Owner.Login+"/"+p.Repository.Name, "pr", p.PullRequest.Number,
		"user", p.Review.User.Login, "installation", p.Installation.ID)
	go e.engageOwnPRReview(p, body)
	w.WriteHeader(http.StatusAccepted)
}

// engageOwnPRReview dispatches a fix-the-findings run on the PR's existing session.
func (e *Extension) engageOwnPRReview(p pullRequestReviewPayload, rawBody []byte) {
	owner, repo, number := p.Repository.Owner.Login, p.Repository.Name, p.PullRequest.Number
	ctx, cancel := context.WithTimeout(context.Background(), fixContextTimeout)
	defer cancel()

	authored, err := e.authoredByQuack(ctx, owner, repo, number)
	if err != nil {
		slog.Warn("github: own-PR authorship check failed; not engaging", "component", "github",
			"repo", owner+"/"+repo, "pr", number, "err", err)
		return
	}
	if !authored {
		return // not quack's PR - the label/mention triggers already cover it
	}

	sessionID := fmt.Sprintf("github-%s-%s-%d", owner, repo, number)
	login := p.Review.User.Login
	if login == "" || e.host.ChatUser == nil {
		// fall through with whatever login we have
	} else if u, ok := e.host.ChatUser(globalChatID(sessionID)); ok && u != "" {
		login = u
	}

	synthetic := issueCommentPayload{Action: "created"}
	synthetic.Issue.Number = number
	synthetic.Issue.PullRequest = &struct{}{}
	synthetic.Comment.User.Login = login
	synthetic.Repository.Name = repo
	synthetic.Repository.Owner.Login = owner
	synthetic.Repository.CloneURL = p.Repository.CloneURL
	synthetic.Repository.DefaultBranch = p.Repository.DefaultBranch
	synthetic.Installation.ID = p.Installation.ID
	// isLabelTrigger stays false: this continues the PR's existing session.
	synthetic.rawEvent = json.RawMessage(rawBody)
	synthetic.eventName = "pull_request_review." + p.Action
	synthetic.deliverableHint = "a commit addressing every finding in the review that requested changes"

	slog.Info("github: engaging own PR after requested changes", "component", "github", "repo", owner+"/"+repo, "pr", number)
	e.dispatch(synthetic, fmt.Sprintf(
		"@%s requested changes on this pull request, which you authored. Address every finding: read the review comments and the current diff, make the fix, run the repo's own checks to verify, and commit the change on this PR's existing head branch.",
		p.Review.User.Login))
}

// handleIssues drives the label-driven issue workflow (plan/implement labels)
// and refreshes the sidebar badge on close/reopen.
func (e *Extension) handleIssues(w http.ResponseWriter, body []byte) {
	var p issuesPayload
	if err := json.Unmarshal(body, &p); err != nil {
		http.Error(w, "invalid payload", http.StatusBadRequest)
		return
	}

	// A real issue's own state change - not the label-driven work triggers below.
	if p.Issue.PullRequest == nil && (p.Action == "closed" || p.Action == "reopened") {
		badge := "open"
		if p.Action == "closed" {
			badge = "closed"
		}
		e.refreshChatOrigin(p.Repository.Owner.Login, p.Repository.Name, false, p.Issue.Number, badge)
		w.WriteHeader(http.StatusOK)
		return
	}

	// Only human-applied labels on real issues.
	if p.Action != "labeled" || p.Issue.PullRequest != nil ||
		strings.HasSuffix(p.Sender.Login, "[bot]") {
		w.WriteHeader(http.StatusOK)
		return
	}
	if !e.isInvokerAllowed(p.Sender.Login) {
		slog.Warn("github webhook: invoker not in allowed_users; ignoring", "component", "github",
			"repo", p.Repository.Owner.Login+"/"+p.Repository.Name, "issue", p.Issue.Number,
			"label", p.Label.Name, "user", p.Sender.Login)
		w.WriteHeader(http.StatusOK)
		return
	}
	synthetic := issueCommentPayload{Action: "created"}
	synthetic.Issue.Number = p.Issue.Number
	synthetic.Issue.Title = p.Issue.Title
	synthetic.Comment.User.Login = p.Sender.Login
	synthetic.Repository.Name = p.Repository.Name
	synthetic.Repository.Owner.Login = p.Repository.Owner.Login
	synthetic.Repository.CloneURL = p.Repository.CloneURL
	synthetic.Repository.DefaultBranch = p.Repository.DefaultBranch
	synthetic.Installation.ID = p.Installation.ID
	synthetic.isLabelTrigger = true // quack:plan/quack:implement, never a mention (T4)
	synthetic.rawEvent = json.RawMessage(body)
	synthetic.eventName = "issues.labeled"

	switch {
	case e.triggers["issue_plan"] && p.Label.Name == e.labels.Plan:
		synthetic.planOnly = true
		go e.ackLabelReaction(p) // instant 👀 on the issue - the label path's equivalent of ackReaction
		go e.dispatch(synthetic, planTask(p))
	case e.triggers["issue_implement"] && p.Label.Name == e.labels.Implement:
		go e.ackLabelReaction(p)
		go e.runImplement(p, synthetic)
	default:
		w.WriteHeader(http.StatusOK)
		return
	}
	slog.Info("github webhook received", "component", "github",
		"repo", p.Repository.Owner.Login+"/"+p.Repository.Name, "issue", p.Issue.Number,
		"label", p.Label.Name, "user", p.Sender.Login, "installation", p.Installation.ID)
	w.WriteHeader(http.StatusAccepted)
}

// runImplement dispatches the implementation run on the issue's session - the
// same session the planning run used, so the plan is also in the model's own
// history. Fetches current labels to wire a contextual closing signal into the
// task prompt: if the issue carries the partial-fix label the implementer
// skips the Closes keyword; otherwise it's instructed to close the issue.
func (e *Extension) runImplement(p issuesPayload, synthetic issueCommentPayload) {
	owner, repo, number := p.Repository.Owner.Login, p.Repository.Name, p.Issue.Number

	ctx, cancel := context.WithTimeout(context.Background(), reactionTimeout)
	defer cancel()

	_, _, _, labels, _, err := e.app.issueMeta(ctx, owner, repo, number)
	if err != nil {
		slog.Warn("github: fetch issue labels failed; running without partial-fix signal", "component", "github",
			"repo", owner+"/"+repo, "issue", number, "err", err)
	}
	e.dispatch(synthetic, implementTask(p, labels, e.labels.PartialFix))
}

// hasPartialFix reports whether names includes the configured partial-fix label.
func hasPartialFix(partialFixLabel string, names []string) bool {
	return hasLabel(names, partialFixLabel)
}

// issueImplementDeliverable is the PR-implementing deliverable text, shared by
// the label trigger and a comment classified/heuristically read as implement.
func issueImplementDeliverable(partialFixLabel string, labels []string, issueNumber int) string {
	if hasPartialFix(partialFixLabel, labels) {
		return "a pull request implementing the changes, without a Closes keyword (this is a partial fix)"
	}
	return fmt.Sprintf("a pull request implementing the approved plan, body containing `Closes #%d`", issueNumber)
}

// implementTask synthesizes the implement classification signal (fed to implementationIntent).
func implementTask(p issuesPayload, labels []string, partialFixLabel string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Implement issue #%d: %s\n", p.Issue.Number, strings.TrimSpace(p.Issue.Title))

	if body := strings.TrimSpace(p.Issue.Body); body != "" {
		fmt.Fprintf(&b, "\nIssue description (may be incomplete - see discussion below):\n%s\n", truncate(body, 4000))
	}

	isPartial := hasPartialFix(partialFixLabel, labels)
	if isPartial {
		b.WriteString("\nA maintainer approved this for implementation (see the approved plan in the discussion below). This is a partial fix: implement the changes, commit locally, and call stage_pr. Do NOT use a Closes keyword - the issue will not be fully closed by this PR.")
	} else {
		b.WriteString("\nA maintainer approved this for implementation (see the approved plan in the discussion below). Implement it per the plan, commit your work locally, " +
			"then call stage_pr with a title and a body that includes `Closes #" + fmt.Sprintf("%d", p.Issue.Number) + "` - you do not push or open the pull request yourself; " +
			"the pull request is opened for you once your work passes review")
	}
	b.WriteString("\nNever merge anything - merging is a human decision.")
	return b.String()
}

// planTask synthesizes the planning request for a plan-labeled issue (for implementationIntent and chat-title fallback).
func planTask(p issuesPayload) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Produce an implementation plan for issue #%d: %s\n", p.Issue.Number, strings.TrimSpace(p.Issue.Title))
	b.WriteString("\nInvestigate the repository first, then lay out a concrete plan: the approach, the files to change, and how to verify it. A maintainer will review the plan before any implementation happens.")
	return b.String()
}

// mergeTimeout bounds the deterministic merge-label handler (a few API calls).
const mergeTimeout = 2 * time.Minute

// mergeIfApproved merges only at the intersection of a human's merge label and quack's own approving verdict.
// A non-approving verdict records a standing intent — merge fires when a later review approves.
func (e *Extension) mergeIfApproved(p pullRequestPayload, rawBody []byte) {
	owner, repo, number := p.Repository.Owner.Login, p.Repository.Name, p.Number
	sessionID := fmt.Sprintf("github-%s-%s-%d", owner, repo, number)
	chatID := globalChatID(sessionID)
	ctx, cancel := context.WithTimeout(context.Background(), mergeTimeout)
	defer cancel()

	comment := func(text string) {
		if err := e.app.postIssueComment(ctx, owner, repo, number, text); err != nil {
			slog.Error("github: merge-label comment failed", "component", "github",
				"repo", owner+"/"+repo, "pr", number, "err", err)
		}
	}

	verdict, err := e.latestQuackVerdict(ctx, owner, repo, number)
	if err != nil {
		slog.Error("github: merge-label review lookup failed", "component", "github",
			"repo", owner+"/"+repo, "pr", number, "err", err)
		comment(fmt.Sprintf("Not merging: I could not read this PR's reviews (%v). Re-apply the `%s` label to retry.", err, e.labels.Merge))
		return
	}
	unlock := e.mergeMu.Lock(chatID)
	defer unlock()
	if verdict == "approve" {
		if err := e.app.mergePR(ctx, owner, repo, number, ""); err != nil {
			slog.Error("github merge failed", "component", "github", "repo", owner+"/"+repo, "pr", number, "err", err)
			comment(fmt.Sprintf("Merge failed: %v", err))
			return
		}
		slog.Info("github pr merged", "component", "github", "repo", owner+"/"+repo, "pr", number, "user", p.Sender.Login)
		if e.autoArchiveOnMerge && e.host.ArchiveChat != nil {
			if derr := e.host.ArchiveChat(chatID); derr != nil {
				slog.Warn("github: auto-archive on merge failed", "component", "github", "repo", owner+"/"+repo, "pr", number, "session", sessionID, "err", derr)
			}
		}
		if derr := e.store.DeleteMergeIntent(ctx, chatID); derr != nil {
			slog.Warn("github: stale merge-intent cleanup failed", "component", "github", "repo", owner+"/"+repo, "pr", number, "err", derr)
		}
		comment(fmt.Sprintf("Merged - my review approved this PR and @%s authorized the merge via the `%s` label.", p.Sender.Login, e.labels.Merge))
		return
	}

	// Not approved yet: record the standing intent BEFORE saying anything is
	// queued - fail CLOSED, an unrecorded intent must never be reported as one.
	if err := e.store.SetMergeIntent(ctx, chatID, p.Sender.Login); err != nil {
		slog.Error("github: merge-intent persist failed", "component", "github", "repo", owner+"/"+repo, "pr", number, "err", err)
		comment(fmt.Sprintf("Not merging: I could not record your merge request (%v) - not queued. Re-apply `%s` to retry.", err, e.labels.Merge))
		return
	}

	switch verdict {
	case "":
		msg := fmt.Sprintf("Queued: I have not reviewed this PR yet. @%s's `%s` label authorizes the merge once I approve.", p.Sender.Login, e.labels.Merge)
		if _, inflight := e.inflight.Load(sessionID); inflight {
			msg += " A review is already in progress - I'll merge automatically once it lands, if it approves."
		} else {
			msg += " Reviewing it now."
			go e.dispatch(autoReviewPayload(p, rawBody), autoReviewTask)
		}
		comment(msg)
	default: // "request_changes" or "comment": already reviewed, just not approving
		comment(fmt.Sprintf("Standing by: my latest review is %s, not an approval, so I'm not merging yet. @%s's `%s` label stands as authorization - I'll merge automatically the next time a review from me approves.",
			verdict, p.Sender.Login, e.labels.Merge))
	}
}

// reviewVerdictMarkerRe extracts quack's verdict from the hidden marker in an own-PR review comment (GitHub forbids self-review).
var reviewVerdictMarkerRe = regexp.MustCompile(`<!-- quack:delivery:review:(approve|request_changes|comment) -->`)

// formalReviewVerdicts maps GitHub review states to the same vocabulary as reviewVerdictMarkerRe.
var formalReviewVerdicts = map[string]string{
	"APPROVED":          "approve",
	"CHANGES_REQUESTED": "request_changes",
	"COMMENTED":         "comment",
}

// latestQuackVerdict returns quack's most recent review verdict — reads both formal reviews and own-PR comment markers.
func (e *Extension) latestQuackVerdict(ctx context.Context, owner, repo string, number int) (string, error) {
	bot, err := e.app.botLogin(ctx)
	if err != nil {
		return "", err
	}
	type dated struct {
		at      time.Time
		verdict string
	}
	var verdicts []dated

	reviews, err := e.app.listReviews(ctx, owner, repo, number)
	if err != nil {
		return "", err
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
			verdicts = append(verdicts, dated{at, m[1]})
			continue
		}
		if v := formalReviewVerdicts[r.State]; v != "" {
			verdicts = append(verdicts, dated{at, v})
		}
	}

	comments, err := e.app.listIssueComments(ctx, owner, repo, number)
	if err != nil {
		return "", err
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
		verdicts = append(verdicts, dated{at, m[1]})
	}

	if len(verdicts) == 0 {
		return "", nil
	}
	sort.Slice(verdicts, func(i, j int) bool { return verdicts[i].at.Before(verdicts[j].at) })
	return verdicts[len(verdicts)-1].verdict, nil
}

// tryMergeStandingIntent consumes a merge intent after a review is actually posted.
// headSHA pins the merge to the commit the review was against. Serialized
// against mergeIfApproved per chat (e.mergeMu) - both read the intent, check
// the live verdict, and act on it, which used to lean on Postgres's shared
// connection for incidental ordering; SQLite gives this extension no such
// guarantee, so the lock closes the gap explicitly (design doc Risk 2).
func (e *Extension) tryMergeStandingIntent(ctx context.Context, owner, repo string, number int, chatID, headSHA string) {
	unlock := e.mergeMu.Lock(chatID)
	defer unlock()

	intent, err := e.store.GetMergeIntent(ctx, chatID)
	if err != nil {
		slog.Warn("github: merge-intent lookup failed", "component", "github", "repo", owner+"/"+repo, "pr", number, "err", err)
		return
	}
	if intent == nil {
		return // no standing authorization on this PR
	}
	verdict, err := e.latestQuackVerdict(ctx, owner, repo, number)
	if err != nil {
		slog.Warn("github: merge-intent verdict lookup failed", "component", "github", "repo", owner+"/"+repo, "pr", number, "err", err)
		return
	}
	if verdict != "approve" {
		return // still not approved; the intent stands for a later review
	}

	comment := func(text string) {
		if err := e.app.postIssueComment(ctx, owner, repo, number, text); err != nil {
			slog.Error("github: merge-intent comment failed", "component", "github", "repo", owner+"/"+repo, "pr", number, "err", err)
		}
	}
	if err := e.app.mergePR(ctx, owner, repo, number, headSHA); err != nil {
		slog.Error("github: standing-intent merge failed", "component", "github", "repo", owner+"/"+repo, "pr", number, "err", err)
		comment(fmt.Sprintf("Merge failed: %v. @%s's standing `%s` authorization still stands - I'll retry the next time a review from me approves.", err, intent.RequestedBy, e.labels.Merge))
		return
	}
	slog.Info("github pr merged", "component", "github", "repo", owner+"/"+repo, "pr", number, "user", intent.RequestedBy)
	if e.autoArchiveOnMerge && e.host.ArchiveChat != nil {
		if derr := e.host.ArchiveChat(chatID); derr != nil {
			slog.Warn("github: auto-archive on merge failed", "component", "github", "repo", owner+"/"+repo, "pr", number, "chat", chatID, "err", derr)
		}
	}
	// Clear the intent BEFORE announcing the merge: once the comment is visible
	// the intent must already be gone, not racing whoever reads it next.
	if derr := e.store.DeleteMergeIntent(ctx, chatID); derr != nil {
		slog.Warn("github: merge-intent cleanup failed", "component", "github", "repo", owner+"/"+repo, "pr", number, "err", derr)
	}
	comment(fmt.Sprintf("Merged - my review approved this PR, on the standing authorization @%s gave via the `%s` label.", intent.RequestedBy, e.labels.Merge))
}

// ackReaction posts a 👀 reaction on the mentioning comment — instant code-level acknowledgment, best effort.
func (e *Extension) ackReaction(p issueCommentPayload) {
	if p.Comment.ID == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), reactionTimeout)
	defer cancel()
	owner, repo := p.Repository.Owner.Login, p.Repository.Name
	if _, err := e.app.reactToComment(ctx, owner, repo, "issues", p.Comment.ID, "eyes"); err != nil {
		slog.Warn("github ack reaction failed", "component", "github",
			"repo", owner+"/"+repo, "comment", p.Comment.ID, "err", err)
	}
}

// ackLabelReaction posts a 👀 reaction on the issue (no comment ID on a label event). Best effort.
func (e *Extension) ackLabelReaction(p issuesPayload) {
	ctx, cancel := context.WithTimeout(context.Background(), reactionTimeout)
	defer cancel()
	owner, repo := p.Repository.Owner.Login, p.Repository.Name
	if _, err := e.app.reactToIssue(ctx, owner, repo, p.Issue.Number, "eyes"); err != nil {
		slog.Warn("github label ack reaction failed", "component", "github",
			"repo", owner+"/"+repo, "issue", p.Issue.Number, "err", err)
	}
}

// ackDedup fires a 👀 reaction when a dispatch is dropped (run already in-flight). Best effort.
func (e *Extension) ackDedup(owner, repo string, number int) {
	ctx, cancel := context.WithTimeout(context.Background(), reactionTimeout)
	defer cancel()
	// reactToIssue works for both plain issues and PRs.
	if _, err := e.app.reactToIssue(ctx, owner, repo, number, "eyes"); err != nil {
		slog.Warn("github dedup ack reaction failed", "component", "github",
			"repo", owner+"/"+repo, "issue", number, "err", err)
	}
}

// triggerTask extracts the task from a mention at the START OF A LINE (leading spaces/tabs only) — makes quote-reply safe.
func (e *Extension) triggerTask(p issueCommentPayload) (string, bool) {
	if !e.triggers["mention"] {
		return "", false
	}
	if p.Action != "created" {
		return "", false
	}
	lines := strings.Split(p.Comment.Body, "\n")
	mentionLower := strings.ToLower(e.mention)
	for i, line := range lines {
		trimmed := strings.TrimLeft(line, " \t")
		if len(trimmed) < len(e.mention) || !strings.HasPrefix(strings.ToLower(trimmed), mentionLower) {
			continue
		}
		// A word boundary right after the token: "/quackers" is not "/quack".
		if len(trimmed) > len(e.mention) && isTokenRune(trimmed[len(e.mention)]) {
			continue
		}
		task := strings.TrimSpace(trimmed[len(e.mention):])
		if rest := strings.TrimSpace(strings.Join(lines[i+1:], "\n")); rest != "" {
			if task != "" {
				task += "\n" + rest
			} else {
				task = rest
			}
		}
		if task == "" {
			return "", false
		}
		return task, true
	}
	return "", false
}

// isTokenRune rejects a mention match that's a prefix of a longer word.
func isTokenRune(b byte) bool {
	return b == '_' || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

// verifySignature checks GitHub's X-Hub-Signature-256 using constant-time compare — the trust boundary.
func verifySignature(secret, body []byte, header string) bool {
	if len(secret) == 0 || !strings.HasPrefix(header, "sha256=") {
		return false
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	expected := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	// hmac.Equal is constant time.
	return hmac.Equal([]byte(header), []byte(expected))
}

// runNudge is delivered when a webhook run answered without running a plan - a
// firm instruction to actually do the work rather than narrate intent.
const runNudge = "You answered without running anything. Do NOT reply in prose: use the plan and execute tools NOW to actually clone the repo, read the change, and carry out the review (or the requested change). Nothing has run yet and the user is waiting."

// dispatch shapes and sends one Host.Dispatch call for a webhook trigger.
// Unlike quack-core's former synchronous dispatch (which drove the run to
// completion inline via a Runner it owned), Host.Dispatch is fire-and-forget:
// this function returns once the request is accepted, and pendingRun +
// RunEnded (run.go) pick up where it left off - the nudge-if-no-plan retry
// becomes a second Dispatch call from inside RunEnded (design doc's answer
// for RunOutcome.PlanRan), never a raw event stream this extension drives
// itself.
func (e *Extension) dispatch(p issueCommentPayload, task string) {
	owner, repo, number := p.Repository.Owner.Login, p.Repository.Name, p.Issue.Number

	// Key by commenter's login so sessions are partitioned per-person (#262).
	login := p.Comment.User.Login
	if login == "" {
		login = runUserID
	}

	// Dedup: one run per session — second trigger is dropped, not queued (#665, #668).
	sessionID := fmt.Sprintf("github-%s-%s-%d", owner, repo, number)
	chatID := globalChatID(sessionID)
	if _, inflight := e.inflight.LoadOrStore(sessionID, struct{}{}); inflight {
		slog.Info("deduplicated trigger", "sessionID", sessionID)
		go e.ackDedup(owner, repo, number)
		return
	}
	// clearInflight is called exactly once, either here (immediate failure) or
	// from finalize (after RunEnded settles the whole chain).
	clearInflight := func() { e.inflight.Delete(sessionID) }

	ctx, cancel := context.WithTimeout(context.Background(), reactionTimeout)
	defer cancel()

	isPR := p.Issue.PullRequest != nil

	gh := e.loadGithubContext(ctx, chatID, owner, repo, number, isPR, p.Comment.ID, false)

	// Label-driven work starts a fresh session — must not inherit prior events.
	resetSession := p.isLabelTrigger
	if resetSession {
		gh = e.loadGithubContext(ctx, chatID, owner, repo, number, isPR, p.Comment.ID, true)
	}

	// Never run a label-triggered work request blind — #467 failure mode.
	if p.isLabelTrigger && gh.contextUnavailable {
		slog.Warn("github: label-triggered work request has no usable GitHub context; aborting rather than running blind",
			"component", "github", "repo", owner+"/"+repo, "issue", number)
		abortCtx, abortCancel := context.WithTimeout(context.Background(), reactionTimeout)
		abortMsg := "Couldn't load this issue's plan and discussion from GitHub (a transient error fetching it) - not running blind. Re-apply the label to retry."
		if err := e.app.postIssueComment(abortCtx, owner, repo, number, abortMsg); err != nil {
			slog.Warn("github: abort comment failed", "component", "github", "repo", owner+"/"+repo, "issue", number, "err", err)
		}
		abortCancel()
		clearInflight()
		return
	}

	// Compute permission grant once — authorship-check failure denies rather than grants.
	authored := false
	if isPR {
		if a, aerr := e.authoredByQuack(ctx, owner, repo, number); aerr != nil {
			slog.Warn("github: authorship check failed computing this run's grant; treating as not-authored",
				"component", "github", "repo", owner+"/"+repo, "issue", number, "err", aerr)
		} else {
			authored = a
		}
	}
	allowedKinds := computeGrant(e.labels, gh.snap.Labels, isPR, authored, gh.snap.Fork)

	p.issueDeliverableCache = &issueDeliverableResult{}
	isPlan := e.deliverableIsPlan(ctx, p, task, allowedKinds, isPR)

	// Context directory: best-effort, skipped when Host has no capability wired.
	var ctxDir string
	var ctxFiles []ContextFile
	if e.host.EnsureContextDir != nil {
		if dir, derr := e.host.EnsureContextDir(login, chatID); derr != nil {
			slog.Warn("github: context dir setup failed; running without one", "component", "github",
				"repo", owner+"/"+repo, "issue", number, "err", derr)
		} else {
			ctxDir = dir
			if werr := e.app.WriteContextDir(ctx, ctxDir, ContextRequest{
				Owner: owner, Repo: repo, Number: number, IsPR: isPR, CheckSHA: p.checkSHA,
			}); werr != nil {
				slog.Warn("github: context dir write failed; running with a partial or empty one", "component", "github",
					"repo", owner+"/"+repo, "issue", number, "err", werr)
			}
			ctxFiles = contextDirFiles(ctxDir, owner, repo, number, p.checkSHA)
		}
	}

	message := e.buildEnvelope(ctx, p, task, gh, allowedKinds, ctxDir, ctxFiles)
	workerAsk := e.buildWorkerAsk(ctx, p, task, gh, allowedKinds, ctxDir)
	var contextItems []sdk.NamedContext
	if p.checkSHA != "" {
		if checks, cerr := e.failingChecks(ctx, owner, repo, p.checkSHA); cerr != nil {
			slog.Warn("github: CI-check fetch for node-scoped detail failed; nodes get none", "component", "github",
				"repo", owner+"/"+repo, "issue", number, "err", cerr)
		} else {
			contextItems = ciChecksForNodes(checks)
		}
	}

	title := strings.TrimSpace(p.Issue.Title)
	if title == "" {
		title = truncate(task, 80)
	}

	setup := &sdk.Setup{Repo: p.Repository.CloneURL, BaseRef: setupBaseRef(p, gh), WorkBranch: fmt.Sprintf("quack/issue-%d", number)}
	if isPR && gh.snap.HeadRef != "" {
		// The PR's real head branch, not the deterministic default - dag.
		// OverrideExistingPRHead's job in the old ctx-stamped world.
		setup.ExistingHeadRef = gh.snap.HeadRef
	}

	state := gh.snap.State
	if isPR && gh.snap.Merged {
		state = "merged"
	} else if isPR && gh.snap.Draft {
		state = "draft"
	}
	o := chatOrigin(owner, repo, isPR, number, state)
	origin := &o

	req := sdk.DispatchRequest{
		Chat: sdk.ChatRef{
			LocalID:      sessionID,
			User:         login,
			Title:        title,
			Origin:       origin,
			ResetHistory: resetSession,
		},
		Ask: sdk.Ask{
			Message:      message,
			NodeContext:  workerAsk,
			ContextItems: contextItems,
		},
		Run: sdk.RunConfig{
			Setup:    setup,
			ReadOnly: p.planOnly,
			Timeout:  e.runTimeout,
		},
		Delivery: sdk.DeliveryAuthority{AllowedKinds: sdkDeliveryKinds(allowedKinds)},
	}

	// Stored BEFORE Dispatch: RunEnded can fire as soon as Dispatch returns.
	e.pending.Store(chatID, &pendingRun{
		sessionID: sessionID, owner: owner, repo: repo, number: number,
		isPR: isPR, login: login, gh: gh, isPlan: isPlan, isLabelTrigger: p.isLabelTrigger,
	})

	slog.Info("github run dispatched", "component", "github", "repo", owner+"/"+repo, "issue", number)
	if err := e.host.Dispatch(ctx, req); err != nil {
		slog.Error("github: dispatch failed", "component", "github", "repo", owner+"/"+repo, "issue", number, "err", err)
		e.pending.Delete(chatID)
		clearInflight()
	}
}

// sdkDeliveryKinds converts the plain-string allowlist computeGrant emits to
// the SDK's typed vocabulary (design doc: the seam carries a closed
// vocabulary, quack-core stages delivery items). nil stays nil (unrestricted).
func sdkDeliveryKinds(kinds []string) []sdk.DeliveryKind {
	if kinds == nil {
		return nil
	}
	out := make([]sdk.DeliveryKind, len(kinds))
	for i, k := range kinds {
		out[i] = sdk.DeliveryKind(k)
	}
	return out
}

// chatOrigin builds the sidebar provenance chip for an issue/PR chat -
// shared by dispatch (initial stamp) and refreshChatOrigin (badge-only
// updates on later state-change webhooks).
func chatOrigin(owner, repo string, isPR bool, number int, badge string) sdk.ChatOrigin {
	kind := "issues"
	if isPR {
		kind = "pull"
	}
	return sdk.ChatOrigin{
		Extension: extensionName,
		Label:     fmt.Sprintf("%s/%s#%d", owner, repo, number),
		Kind:      kind,
		Href:      fmt.Sprintf("https://github.com/%s/%s/%s/%d", owner, repo, kind, number),
		Badge:     badge,
		Labels:    map[string][]sdk.LabelValue{"repo": {{Value: owner + "/" + repo, Href: fmt.Sprintf("https://github.com/%s/%s", owner, repo)}}},
	}
}

// refreshChatOrigin advances the sidebar badge after a state-change webhook -
// Label/Kind/Href/Labels stay exactly what dispatch stamped, only Badge
// moves. Most issues/PRs never had a chat dispatched (no mention, no label),
// so ErrUnknownChat is the expected, common outcome here - swallowed at
// Debug rather than Warn.
func (e *Extension) refreshChatOrigin(owner, repo string, isPR bool, number int, badge string) {
	if e.host.UpdateChatOrigin == nil {
		return
	}
	sessionID := fmt.Sprintf("github-%s-%s-%d", owner, repo, number)
	err := e.host.UpdateChatOrigin(sessionID, chatOrigin(owner, repo, isPR, number, badge))
	switch {
	case err == nil:
	case errors.Is(err, sdk.ErrUnknownChat):
		e.host.Log.Debug("github: origin refresh skipped; no chat dispatched for this issue/PR",
			"repo", owner+"/"+repo, "number", number)
	default:
		e.host.Log.Warn("github: origin refresh failed", "repo", owner+"/"+repo, "number", number, "err", err)
	}
}

// setupBaseRef returns the PR's base branch, or the repo's default branch for an issue run (#661).
func setupBaseRef(p issueCommentPayload, gh githubContext) string {
	if gh.snap.BaseRef != "" {
		return gh.snap.BaseRef
	}
	if p.Repository.DefaultBranch != "" {
		return p.Repository.DefaultBranch
	}
	return "main"
}

// truncate shortens s to at most n runes.
func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
