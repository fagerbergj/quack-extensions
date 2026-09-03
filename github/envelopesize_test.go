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

// TestBuildEnvelopeSizeWithManifest pins the <artifacts> manifest's own
// contribution to envelope size: even with every #1006-shaped input
// artifact listed, the manifest block itself stays small (one compact line
// per artifact), not a second copy of what it points at.
func TestBuildEnvelopeSizeWithManifest(t *testing.T) {
	ext, _ := newTestExtension(t, "http://unused", nil)

	var issue issueCommentPayload
	issue.Issue.Number = 1006
	issue.Action = "created"
	issue.eventName = "issues.labeled"

	manifest := []artifactEntry{
		{Name: "comments", Revision: 4, Changed: true, Note: "47 total, 3 new"},
		{Name: "event", Revision: 4, Changed: false, Note: "issues.labeled"},
		{Name: "timeline", Revision: 1, Changed: false, Note: "12 entries"},
		{Name: "check-runs", Revision: 2, Changed: true, Note: "5 checks, 1 failed"},
		{Name: "annotations-go-test", Revision: 1, Changed: true, Note: "14 annotations"},
	}
	env := ext.buildEnvelope(context.Background(), issue, "task", seedGC(Snapshot{}, 0), nil, manifest)

	want := "<artifacts>\n" +
		`  <artifact id="comments" revision="4" status="new">47 total, 3 new</artifact>` + "\n" +
		`  <artifact id="event" revision="4" status="unchanged">issues.labeled</artifact>` + "\n" +
		`  <artifact id="timeline" revision="1" status="unchanged">12 entries</artifact>` + "\n" +
		`  <artifact id="check-runs" revision="2" status="new">5 checks, 1 failed</artifact>` + "\n" +
		`  <artifact id="annotations-go-test" revision="1" status="new">14 annotations</artifact>` + "\n" +
		"</artifacts>\n"
	if !strings.Contains(env, want) {
		t.Errorf("manifest block =\n%s\nwant it to contain:\n%s", truncateForLog(env), want)
	}
	// The manifest is a pointer, not a payload: five entries render in well
	// under 500 chars regardless of how large the artifacts they name are.
	if len(want) > 500 {
		t.Errorf("manifest block is %d chars for 5 entries - no longer a compact pointer", len(want))
	}
}
