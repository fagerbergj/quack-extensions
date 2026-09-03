package github

import (
	"context"
	"strings"
	"testing"
)

// TestBuildEnvelopeSizeShrinksOrderOfMagnitude pins issue #1010's measured
// target: a #1006-shaped dispatch (a long-running issue with a large
// comment thread and a large raw webhook payload) built ~48.8k chars before
// this change. With comments/timeline/event moved to input artifacts and
// only the steady-state delta inlined, the envelope for the SAME underlying
// data should be an order of magnitude smaller - this asserts a bound, not
// the exact number (which depends on fixture sizes chosen here).
func TestBuildEnvelopeSizeShrinksOrderOfMagnitude(t *testing.T) {
	ext, _ := newTestExtension(t, "http://unused", nil)

	// A #1006-shaped thread: ~50 long-ish comments (the seeded snapshot),
	// none of them new/edited/deleted since the last dispatch (steady state -
	// "the case the issue says gets worse over time").
	comments := make([]snapshotComment, 0, 50)
	for i := 0; i < 50; i++ {
		comments = append(comments, snapshotComment{
			ID: int64(i + 1), User: "alice", CreatedAt: "2026-01-01T00:00:00Z",
			Body: strings.Repeat("this comment discusses the plan in detail. ", 20),
		})
	}
	snap := Snapshot{Title: "Long-running issue", Body: strings.Repeat("issue body text. ", 50), Comments: comments}
	delta := Delta{} // nothing new/edited/deleted - the steady-state re-dispatch

	var issue issueCommentPayload
	issue.Issue.Number = 1006
	issue.Action = "created"
	// A large raw webhook payload (repo metadata, gravatar_id, etc. - the
	// #1010 issue's own measured ~10.6k chunk) - no longer inlined at all.
	issue.rawEvent = []byte(`{"action":"created","repository":{` + strings.Repeat(`"field":"noise",`, 400) + `"last":"x"}}`)
	issue.eventName = "issues.labeled"

	gh := githubContext{snap: snap, delta: &delta}
	env := ext.buildEnvelope(context.Background(), issue, "task", gh, nil, nil)

	inputSize := len(snap.Body)
	for _, c := range comments {
		inputSize += len(c.Body)
	}
	inputSize += len(issue.rawEvent)

	t.Logf("input=%d env=%d", inputSize, len(env))
	if len(env) >= inputSize/5 {
		t.Errorf("envelope size %d did not shrink by an order of magnitude vs. the %d bytes of underlying evidence", len(env), inputSize)
	}
	if strings.Contains(env, "gravatar_id") || strings.Contains(env, `"field":"noise"`) {
		t.Errorf("envelope still inlines raw webhook payload noise:\n%s", truncateForLog(env))
	}
}
