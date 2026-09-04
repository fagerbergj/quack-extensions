package github

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
)

// IntentClassifier: single model round-trip (interface so tests can fake it).
type IntentClassifier interface {
	Classify(ctx context.Context, prompt string) (string, error)
}

// Bounds classification (runs inline in webhook dispatch).
const intentClassifierTimeout = 5 * time.Second

// intentClassifierRetryTimeout: one retry with a longer deadline before
// falling back (#1172) - a cold worker model on llm-swap can take longer
// than intentClassifierTimeout to swap in right after a restart.
const intentClassifierRetryTimeout = 30 * time.Second

// bareReReviewRe: only a mention that is NOTHING but the re-run phrase is unambiguous
// enough to hardcode; anything with extra wording still goes through the model (#1172).
var bareReReviewRe = regexp.MustCompile(`(?i)^\s*(re-review|review again)[.!]?\s*$`)

// intentClassifierPrompt: replaces regex classifier; handles quoted code, declines, corrections.
const intentClassifierPrompt = `You classify a single GitHub comment as WORK or CONVERSATIONAL.

WORK means the user is asking for review or implementation work to be done now - e.g. "review this PR", "focus on the auth path", "please fix the lint errors", "implement this and push a branch".

CONVERSATIONAL means anything else - a question, a clarification, a correction, an opinion, or small talk. In particular, treat these as CONVERSATIONAL:
- Code quoted or referenced in the message (e.g. "it.migrate(connection) throws here") - a method call inside a code snippet is not an instruction to the reader.
- A message declining or deferring work ("no need to re-review that", "don't bother re-running it").
- A correction to something already said ("that finding was wrong", "you misread the diff") - this is feedback about the past, not a new request.

Reply with exactly one word: WORK or CONVERSATIONAL. No punctuation, no explanation.

Message:
%s`

// isWorkRequest: PR mention → work or conversational. When the classifier fails, a PR
// already carrying the review label or a bare re-run phrase defaults to review, not
// conversational (#1172), and the fallback is announced on the PR instead of log-only.
func (e *Extension) isWorkRequest(ctx context.Context, p issueCommentPayload, task string) bool {
	toReview := e.prHasReviewLabel(p) || bareReReviewRe.MatchString(task)
	if e.intentClassifier == nil {
		return toReview // no classifier configured is not a failure worth a PR comment
	}
	prompt := fmt.Sprintf(intentClassifierPrompt, task)
	var lastErr error
	for _, timeout := range []time.Duration{intentClassifierTimeout, intentClassifierRetryTimeout} {
		cctx, cancel := context.WithTimeout(ctx, timeout)
		answer, err := e.intentClassifier.Classify(cctx, prompt)
		cancel()
		if err != nil {
			lastErr = err
			continue // retry once with a longer deadline before falling back
		}
		// Substring match (small models often wrap output in ** or punctuation).
		switch up := strings.ToUpper(strings.TrimSpace(answer)); {
		case strings.Contains(up, "CONVERSATIONAL"):
			return false
		case strings.Contains(up, "WORK"):
			return true
		default:
			return e.fallbackWorkRequest(p, toReview, fmt.Sprintf("unparseable classifier answer %q", answer))
		}
	}
	return e.fallbackWorkRequest(p, toReview, fmt.Sprintf("classifier failed twice: %v", lastErr))
}

// fallbackWorkRequest announces a failed classification on the PR (best effort) and
// returns the review-vs-conversational default the caller already resolved (#1172).
func (e *Extension) fallbackWorkRequest(p issueCommentPayload, toReview bool, reason string) bool {
	msg := "couldn't classify; treating as conversational"
	if toReview {
		msg = "couldn't classify; treating as review"
	}
	e.host.Log.Warn("github: intent classifier fallback", "reason", reason, "as_review", toReview)
	ctx, cancel := context.WithTimeout(context.Background(), reactionTimeout)
	defer cancel()
	owner, repo := p.Repository.Owner.Login, p.Repository.Name
	if err := e.app.postIssueComment(ctx, owner, repo, p.Issue.Number, msg); err != nil {
		e.host.Log.Warn("github: intent classifier fallback comment failed", "repo", owner+"/"+repo, "issue", p.Issue.Number, "err", err)
	}
	return toReview
}

// prHasReviewLabel reports whether the PR being commented on already carries
// the configured review label (#1172) - mirrors isReviewCommand's label scan.
func (e *Extension) prHasReviewLabel(p issueCommentPayload) bool {
	for _, l := range p.Issue.Labels {
		if l.Name == e.labels.Review {
			return true
		}
	}
	return false
}

// classifyPRDeliverable resolves review-vs-nothing for a PR mention when the grant does NOT
// carry push_commits_to_pr (that case runs through classifyGrantedPRDeliverable instead - #760,
// which also retired this function's old COMMIT branch: with no push permission "commit" was
// never a legal answer here, so the remaining question is bounded entirely by post_review and
// decided from the grant alone - no model call needed, ctx/task kept for a stable signature
// alongside classifyGrantedPRDeliverable/classifyIssueDeliverable).
func (e *Extension) classifyPRDeliverable(_ context.Context, _ string, allowedKinds []string) (kind string, ok bool) {
	if slices.Contains(allowedKinds, "review") {
		return "review", true
	}
	return "", false
}

// grantedPRPrompt: reply, review, or commit? Only reachable when the grant already carries
// push_commits_to_pr (#760) - the permission question is answered deterministically by the
// grant, so this asks only what the comment wants, never whether quack may act.
const grantedPRPrompt = `You classify a single GitHub pull request comment as REPLY, REVIEW, or COMMIT.

REPLY means the asker wants a response, clarification, or discussion right now - a question, an FYI, or feedback about something already said that does not end in an ask to act on it.

REVIEW means the asker wants the code assessed - e.g. "review this", "take another look", "double check the auth path".

COMMIT means the asker wants a specific code change made and pushed now - e.g. "fix this", "change X to Y", a one-line "use qwen3-embed instead", or a numbered list of defects with the exact replacement values to use. A comment that also corrects something said earlier is still COMMIT, not just REPLY, if it ends in a specific requested change.

Reply with exactly one word: REPLY, REVIEW, or COMMIT. No punctuation, no explanation.

Message:
%s`

// classifyGrantedPRDeliverable picks reply vs review vs commit for a PR comment when the grant
// already carries push_commits_to_pr (#760): computeGrant already decided, from labels and
// authorship, that quack MAY push here - unlike isWorkRequest/classifyPRDeliverable below, this
// never asks a model to re-derive that permission, only what the comment is asking for. REPLY is
// always legal; REVIEW is legal only when the grant also carries post_review. ok=false
// (classifier nil/erroring/unparseable) leaves the caller to fail safe to reply - never guess
// commit. A live answer that names an ungranted option (REVIEW without post_review) degrades to
// reply rather than being discarded outright, so a real ask for engagement isn't silently dropped.
func (e *Extension) classifyGrantedPRDeliverable(ctx context.Context, task string, allowedKinds []string) (kind string, ok bool) {
	if e.intentClassifier == nil {
		return "", false
	}
	ctx, cancel := context.WithTimeout(ctx, intentClassifierTimeout)
	defer cancel()
	answer, err := e.intentClassifier.Classify(ctx, fmt.Sprintf(grantedPRPrompt, task))
	if err != nil {
		e.host.Log.Warn("github: granted-PR deliverable classifier failed; falling back to reply", "err", err)
		return "", false
	}
	switch up := strings.ToUpper(strings.TrimSpace(answer)); {
	case strings.Contains(up, "COMMIT"):
		return "commit", true
	case strings.Contains(up, "REVIEW"):
		if slices.Contains(allowedKinds, "review") {
			return "review", true
		}
		return "reply", true // review asked for but not granted here - never escalate to commit instead
	case strings.Contains(up, "REPLY"):
		return "reply", true
	default:
		e.host.Log.Warn("github: granted-PR deliverable classifier returned an unparseable answer; falling back to reply", "answer", answer)
		return "", false
	}
}

// implementPrompt: implement vs comment? Only reachable when the grant permits open_pr (#713).
const implementPrompt = `You classify a single GitHub issue comment as IMPLEMENT or COMMENT.

IMPLEMENT means the asker wants code written and a pull request opened now - e.g. "implement this", "go ahead and build it", "the deliverable is a pull request, not a plan", "commit and stage the PR".

COMMENT means the asker wants a reply, discussion, or a plan - e.g. "what do you think", "can you clarify this", "draft a plan first", or a correction about a prior run.

Reply with exactly one word: IMPLEMENT or COMMENT. No punctuation, no explanation.

Message:
%s`

// classifyIssueDeliverable picks implement-vs-comment for an issue comment,
// mirroring classifyPRDeliverable (#691) on the issue side (#713): a comment
// is always a legal answer, but "implement" is legal only when the grant
// carries open_pr (quack:implement) - the classifier picks within that bound.
// ok=false falls back to implementationIntent's wording heuristic, never
// straight to conversational (a cold/erroring classifier must not silently
// invert an implementation request).
func (e *Extension) classifyIssueDeliverable(ctx context.Context, task string, allowedKinds []string) (kind string, ok bool) {
	if !slices.Contains(allowedKinds, "pull_request") || e.intentClassifier == nil {
		return "", false
	}
	ctx, cancel := context.WithTimeout(ctx, intentClassifierTimeout)
	defer cancel()
	answer, err := e.intentClassifier.Classify(ctx, fmt.Sprintf(implementPrompt, task))
	if err != nil {
		e.host.Log.Warn("github: issue deliverable classifier failed; falling back to the implement wording heuristic", "err", err)
		return "", false
	}
	switch up := strings.ToUpper(strings.TrimSpace(answer)); {
	case strings.Contains(up, "IMPLEMENT"):
		return "implement", true
	case strings.Contains(up, "COMMENT"):
		return "comment", true
	default:
		e.host.Log.Warn("github: issue deliverable classifier returned an unparseable answer; falling back to the implement wording heuristic", "answer", answer)
		return "", false
	}
}

// issueDeliverableResult memoizes one classifyIssueDeliverable call (#731).
type issueDeliverableResult struct {
	kind string
	ok   bool
	done bool
}

// classifyIssueDeliverableCached calls classifyIssueDeliverable at most once
// per dispatch: deliverableText (two callers) and deliverableIsPlan all need
// this same answer for the same run, and a second live call could disagree
// with the first, telling the worker to produce a plan while the tail
// decides it wasn't one. p.issueDeliverableCache is nil for a caller that
// invokes this outside a dispatch (e.g. a test calling buildEnvelope
// directly) - falls back to one uncached call, preserving prior behaviour.
func (e *Extension) classifyIssueDeliverableCached(ctx context.Context, p issueCommentPayload, task string, allowedKinds []string) (kind string, ok bool) {
	c := p.issueDeliverableCache
	if c == nil {
		return e.classifyIssueDeliverable(ctx, task, allowedKinds)
	}
	if !c.done {
		c.kind, c.ok = e.classifyIssueDeliverable(ctx, task, allowedKinds)
		c.done = true
	}
	return c.kind, c.ok
}

// planIntentRe: did the human's OWN request mention planning? Mirrors
// implementationIntent's contract - read what was ASKED, never what the
// model wrote back, so a rephrased answer can't silently change the
// outcome. "comment" (deliverableText's non-implement issue bucket) also
// covers plain conversational replies; without this, every one of them
// would wrongly get marked and collapsed as a plan.
var planIntentRe = regexp.MustCompile(`(?i)\bplan(s|ning)?\b`)

// deliverableIsPlan reports whether this run's deliverable belongs to the
// issue's plan family (#731) - mirrors deliverableText's own branching so
// mark-and-collapse is keyed on the SAME classification the answer was asked
// for, never on how the run was triggered or what the answer says. True for
// the quack:plan label, and for a comment-triggered issue ask that mentions
// planning and the classifier (or its fallback heuristic) doesn't read as
// IMPLEMENT. Always false for a PR and for the quack:implement label.
func (e *Extension) deliverableIsPlan(ctx context.Context, p issueCommentPayload, task string, allowedKinds []string, isPR bool) bool {
	switch {
	case p.planOnly:
		return true
	case isPR, p.isLabelTrigger:
		return false
	case !planIntentRe.MatchString(task):
		return false
	}
	if kind, ok := e.classifyIssueDeliverableCached(ctx, p, task, allowedKinds); ok {
		return kind != "implement"
	}
	// Not PR-scoped here (isPR already false above): "pull_request" means open-a-new-PR.
	return !(slices.Contains(allowedKinds, "pull_request") && ImplementationIntent(task))
}
