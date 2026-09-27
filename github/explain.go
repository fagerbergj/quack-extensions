package github

import (
	"context"
	"fmt"
	"strings"
)

// explainSessionID keys an /explain chat per requester, apart from the PR's
// review session: a quiz is personal, and a review's ResetHistory must not wipe it.
func explainSessionID(owner, repo string, number int, login string) string {
	return fmt.Sprintf("github-%s-%s-%d-explain-%s", owner, repo, number, strings.ToLower(login))
}

// shapeExplain marks p as an /explain run and returns its task. The task
// opens the deliverable so the orchestrator routes it to the PR tutor.
func shapeExplain(p *issueCommentPayload) string {
	task := fmt.Sprintf("Walk me through and quiz me on PR %s/%s#%d", p.Repository.Owner.Login, p.Repository.Name, p.Issue.Number)
	p.explain = true
	p.deliverableHint = task + ": an interactive walkthrough and quiz rendered in quack's chat UI. Post nothing to GitHub - no review, no comment, no commit."
	p.Issue.Title = "Explain: " + strings.TrimSpace(p.Issue.Title)
	return task
}

// postExplainLink replies on the PR with where the walkthrough lives - the
// only thing an /explain run posts. Best effort.
func (e *Extension) postExplainLink(ctx context.Context, owner, repo string, number int, login, chatID string) {
	where := "chat `" + chatID + "` (set quack's `server.public_url` to get a link here)"
	if e.app.publicURL != "" {
		where = e.app.publicURL + "/chat/" + chatID
	}
	body := fmt.Sprintf("@%s your walkthrough and quiz for this PR is in quack: %s", login, where)
	if err := e.app.postIssueComment(ctx, owner, repo, number, body); err != nil {
		e.host.Log.Warn("github: explain link comment failed", "repo", owner+"/"+repo, "pr", number, "err", err)
	}
}
