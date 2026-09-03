package github

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
)

// pullCommentBody is issueCommentBody-shaped but the issue is a pull request
// (GitHub marks PR comments with a non-null issue.pull_request).
func pullCommentBody(commentBody string) []byte {
	return []byte(fmt.Sprintf(`{
		"action":"created",
		"comment":{"id":999,"body":%q,"user":{"login":"alice"}},
		"issue":{"number":7,"pull_request":{}},
		"repository":{"name":"widgets","owner":{"login":"acme"},"clone_url":"https://github.com/acme/widgets.git","default_branch":"main"},
		"installation":{"id":5}
	}`, commentBody))
}

// seedGC builds a first-load githubContext from a snapshot - buildEnvelope's
// most common test fixture (no store, no prior snapshot to diff against, so
// delta stays nil and the envelope seeds everything).
func seedGC(snap Snapshot, excludeCommentID int64) githubContext {
	_ = excludeCommentID // exclusion is now the caller's issueCommentPayload.Comment.ID, read by commentsBlock
	return githubContext{snap: snap, firstLoad: true}
}

// fakeIntentClassifier is a fixed-verdict IntentClassifier double: tests set
// verdict directly instead of tuning prose to trip a regex. errAlways
// simulates the classifier failing outright. The three prompts it answers
// (isWorkRequest's WORK/CONVERSATIONAL, classifyGrantedPRDeliverable's
// REPLY/REVIEW/COMMIT, and classifyIssueDeliverable's IMPLEMENT/COMMENT) are
// distinguished by content; grantedDeliverable/grantedDeliverableErr and
// issueDeliverable/issueDeliverableErr let a test degrade one classifier
// independently of the others. (The original's fourth prompt,
// classifyPRDeliverable's REVIEW/COMMIT, is gone in this port -
// classifyPRDeliverable no longer calls a model at all, see intent.go.)
type fakeIntentClassifier struct {
	verdict               string // "WORK" or "CONVERSATIONAL", or any other/blank to test the unparseable path
	grantedDeliverable    string // "REPLY", "REVIEW", or "COMMIT", or any other/blank to test the unparseable path
	issueDeliverable      string // "IMPLEMENT" or "COMMENT", or any other/blank to test the unparseable path
	errAlways             error
	grantedDeliverableErr error
	issueDeliverableErr   error
	calls                 int32
}

func (f *fakeIntentClassifier) Classify(_ context.Context, prompt string) (string, error) {
	atomic.AddInt32(&f.calls, 1)
	if f.errAlways != nil {
		return "", f.errAlways
	}
	if strings.Contains(prompt, "REPLY, REVIEW, or COMMIT") {
		if f.grantedDeliverableErr != nil {
			return "", f.grantedDeliverableErr
		}
		return f.grantedDeliverable, nil
	}
	if strings.Contains(prompt, "IMPLEMENT or COMMENT") {
		if f.issueDeliverableErr != nil {
			return "", f.issueDeliverableErr
		}
		return f.issueDeliverable, nil
	}
	return f.verdict, nil
}

// TestBuildEnvelopeLargeIssueBodyReachesIntact pins #666's test case 1: a
// 12,000-char issue body (well under seedCap) reaches the envelope whole, no
// ellipsis anywhere.
func TestBuildEnvelopeLargeIssueBodyReachesIntact(t *testing.T) {
	ext, _ := newTestExtension(t, "http://unused", nil)
	body := strings.Repeat("a", 12000)
	var issue issueCommentPayload
	issue.Issue.Number = 42
	issue.Repository.Name, issue.Repository.Owner.Login = "widgets", "acme"

	env := ext.buildEnvelope(context.Background(), issue, "add a feature", seedGC(Snapshot{Body: body}, 0), nil, nil)

	if !strings.Contains(env, body) {
		t.Fatalf("12000-char issue body did not reach the envelope intact:\n%s", truncateForLog(env))
	}
	if strings.Contains(env, "…") || strings.Contains(env, "TRUNCATED") {
		t.Errorf("envelope should carry no truncation marker for a body under the seed cap:\n%s", truncateForLog(env))
	}
}

// TestBuildEnvelopeTruncatesOversizedDescription pins the seed ceiling
// (#666): a description over seedCap is marked truncated and points at the
// untruncated file - the ONE sanctioned truncation in the trigger path.
func TestBuildEnvelopeTruncatesOversizedDescription(t *testing.T) {
	ext, _ := newTestExtension(t, "http://unused", nil)
	body := strings.Repeat("x", seedCap+5000)
	var issue issueCommentPayload
	issue.Issue.Number = 7

	env := ext.buildEnvelope(context.Background(), issue, "task", seedGC(Snapshot{Body: body}, 0), nil, nil)

	if !strings.Contains(env, "TRUNCATED") {
		t.Errorf("an oversized description should be marked truncated:\n%s", truncateForLog(env))
	}
	if !strings.Contains(env, "issue.json") {
		t.Errorf("a truncated issue description should point at issue.json:\n%s", truncateForLog(env))
	}
	if strings.Contains(env, body) {
		t.Errorf("an oversized description should not appear in full")
	}
}

// TestBuildEnvelopeTruncatesOversizedPRDescription pins the PR half of the
// same ceiling - it must point at pull.json, not issue.json.
func TestBuildEnvelopeTruncatesOversizedPRDescription(t *testing.T) {
	ext, _ := newTestExtension(t, "http://unused", nil)
	ext.intentClassifier = &fakeIntentClassifier{verdict: "WORK"}
	body := strings.Repeat("x", seedCap+5000)
	var pr issueCommentPayload
	if err := json.Unmarshal(pullCommentBody("review this"), &pr); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	env := ext.buildEnvelope(context.Background(), pr, "review this", seedGC(Snapshot{IsPR: true, Body: body}, 0), nil, nil)

	if !strings.Contains(env, "pull.json") {
		t.Errorf("a truncated PR description should point at pull.json:\n%s", truncateForLog(env))
	}
}

// TestBuildEnvelopeCommentDeletionVisibleInDelta pins #666's test case: a
// comment deleted between two dispatches is visible in the delta - computed
// by the existing snapshot diff (diffSnapshots), never GitHub's ?since=,
// which can express no signal for a deletion at all.
func TestBuildEnvelopeCommentDeletionVisibleInDelta(t *testing.T) {
	ext, _ := newTestExtension(t, "http://unused", nil)
	old := Snapshot{Comments: []snapshotComment{{ID: 1, User: "bob", Body: "will be deleted", CreatedAt: "t0"}}}
	cur := Snapshot{Comments: []snapshotComment{}}
	delta := diffSnapshots(old, cur, 0)
	gh := githubContext{snap: cur, delta: &delta}

	var issue issueCommentPayload
	issue.Issue.Number = 7

	env := ext.buildEnvelope(context.Background(), issue, "what changed?", gh, nil, nil)

	if !strings.Contains(env, `deleted="1"`) {
		t.Errorf("envelope missing the deleted=1 comment-delta marker:\n%s", truncateForLog(env))
	}
	if !strings.Contains(env, "will be deleted") {
		t.Errorf("a deleted comment's last-known body should still be visible in the delta:\n%s", truncateForLog(env))
	}
}

// TestBuildEnvelopeTitleChangeMarking pins #666: an edited title re-seeds
// marked as changed, quoting the old value; an unchanged title is not
// re-marked.
func TestBuildEnvelopeTitleChangeMarking(t *testing.T) {
	ext, _ := newTestExtension(t, "http://unused", nil)
	var issue issueCommentPayload
	issue.Issue.Number = 7

	old := Snapshot{Title: "Old title"}
	cur := Snapshot{Title: "New title"}
	changed := diffSnapshots(old, cur, 0)
	gh := githubContext{snap: cur, delta: &changed}

	env := ext.buildEnvelope(context.Background(), issue, "task", gh, nil, nil)
	if !strings.Contains(env, `New title (changed from "Old title")`) {
		t.Errorf("a changed title should be marked and quote the old value:\n%s", truncateForLog(env))
	}

	unchanged := diffSnapshots(cur, cur, 0)
	ghSame := githubContext{snap: cur, delta: &unchanged}
	envSame := ext.buildEnvelope(context.Background(), issue, "task", ghSame, nil, nil)
	if strings.Contains(envSame, "changed from") {
		t.Errorf("an unchanged title should not be marked changed:\n%s", truncateForLog(envSame))
	}
}

// TestFilterGitHubJSONDropsNoiseKeepsShape pins #659's core promise: the
// filter is a DROP-LIST, never a rename - GitHub's own field names and
// nesting survive exactly (pull_request.head.ref stays put), while node_id,
// the *_url family, avatar_url and bare "url" are gone at EVERY nesting
// level, not just the top one.
func TestFilterGitHubJSONDropsNoiseKeepsShape(t *testing.T) {
	raw := []byte(`{
		"action":"opened",
		"pull_request":{
			"number":97,"node_id":"PR_abc","url":"https://api.github.com/x",
			"head":{"ref":"feat/x","sha":"abc123","repo":{"full_name":"acme/widgets"}},
			"base":{"ref":"main"},
			"user":{"login":"alice","avatar_url":"https://x","node_id":"U_1"}
		},
		"repository":{"full_name":"acme/widgets","html_url":"https://x"},
		"sender":{"login":"alice","avatar_url":"https://x"},
		"reactions":{"total_count":0},
		"performed_via_github_app":null
	}`)
	filtered := filterGitHubJSON(raw)

	var v map[string]any
	if err := json.Unmarshal([]byte(filtered), &v); err != nil {
		t.Fatalf("filtered output is not valid JSON: %v\n%s", err, filtered)
	}
	pr, ok := v["pull_request"].(map[string]any)
	if !ok {
		t.Fatalf("pull_request missing from filtered output:\n%s", filtered)
	}
	head, ok := pr["head"].(map[string]any)
	if !ok || head["ref"] != "feat/x" || head["sha"] != "abc123" {
		t.Errorf("pull_request.head.ref/sha not preserved with GitHub's own names/nesting:\n%s", filtered)
	}
	if base, ok := pr["base"].(map[string]any); !ok || base["ref"] != "main" {
		t.Errorf("pull_request.base.ref not preserved:\n%s", filtered)
	}
	for _, dropped := range []string{`"node_id"`, `"avatar_url"`, `"html_url"`, `"reactions"`, `"performed_via_github_app"`} {
		if strings.Contains(filtered, dropped) {
			t.Errorf("filtered event still contains dropped key %s:\n%s", dropped, filtered)
		}
	}
	// A bare "url" key is dropped too, everywhere - but "full_name" (which
	// merely CONTAINS no "url" substring at a word boundary) must survive.
	if _, ok := pr["url"]; ok {
		t.Errorf("pull_request.url should have been dropped:\n%s", filtered)
	}
	if repo, ok := v["repository"].(map[string]any); !ok || repo["full_name"] != "acme/widgets" {
		t.Errorf("repository.full_name not preserved:\n%s", filtered)
	}
}

// TestFilterGitHubJSONUnknownFieldSurvives pins the drop-list-not-allow-list
// promise: adding an unknown field to a fixture payload does not break the
// filter, and the field is NOT dropped (only the named drop-list is) - a
// drop-list needs no maintenance when GitHub adds a field.
func TestFilterGitHubJSONUnknownFieldSurvives(t *testing.T) {
	raw := []byte(`{"action":"opened","brand_new_field_from_github":"some value"}`)
	filtered := filterGitHubJSON(raw)
	if !strings.Contains(filtered, "brand_new_field_from_github") {
		t.Errorf("an unrecognised GitHub field should survive the drop-list filter unchanged:\n%s", filtered)
	}
}

// TestPermissionsTextRendersAllowedKinds pins that <permissions> states
// EXACTLY the closed vocabulary (pull_request, review, comment) staged
// delivery and the trust gate's allowlist use - the port's replacement for
// vetting.Grant with a flat allowedKinds slice (computeGrant, grant.go), so
// permissionsText is now a straight join rather than a field-by-field
// translation.
func TestPermissionsTextRendersAllowedKinds(t *testing.T) {
	for _, tt := range []struct {
		name  string
		kinds []string
		want  string
	}{
		{"review-only", []string{"review", "comment"}, "review, comment"},
		{"issue-plan", []string{"comment"}, "comment"},
		{"fix", []string{"pull_request", "comment"}, "pull_request, comment"},
		{"none", nil, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := permissionsText(tt.kinds); got != tt.want {
				t.Errorf("permissionsText(%+v) = %q, want %q", tt.kinds, got, tt.want)
			}
		})
	}
}

// TestBuildEnvelopeReviewOnlyPermissionsNeverNameStagePR pins #659's test
// case 3: the envelope for a review-only run never grants pull_request -
// permissions state only what THIS run is allowed, not what a broader
// trigger on the same thread might be.
func TestBuildEnvelopeReviewOnlyPermissionsNeverNameStagePR(t *testing.T) {
	ext, _ := newTestExtension(t, "http://unused", nil)
	allowedKinds := []string{"review", "comment"} // PR-scoped: post_review + join_pr_conversation, no push
	var pr issueCommentPayload
	if err := json.Unmarshal(pullCommentBody("unused"), &pr); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	pr.isLabelTrigger = true

	env := ext.buildEnvelope(context.Background(), pr, autoReviewTask, seedGC(Snapshot{IsPR: true}, 0), allowedKinds, nil)
	permLine := env[strings.Index(env, "<permissions>") : strings.Index(env, "</permissions>")+len("</permissions>")]
	if strings.Contains(permLine, "pull_request") {
		t.Errorf("review-only envelope must not grant the pull_request kind:\n%s", permLine)
	}
	if !strings.Contains(permLine, "review") {
		t.Errorf("review-only envelope missing its own granted permission:\n%s", truncateForLog(env))
	}
}

// TestBuildEnvelopeArtifactsManifestListsEntries pins #1010's wiring into the
// envelope: buildEnvelope renders one <artifact> line per input artifact
// this dispatch wrote, naming its revision and new/unchanged status.
func TestBuildEnvelopeArtifactsManifestListsEntries(t *testing.T) {
	ext, _ := newTestExtension(t, "http://unused", nil)
	var issue issueCommentPayload
	issue.Issue.Number = 7

	manifest := []artifactEntry{
		{Name: "comments", Revision: 4, Changed: true, Note: "47 items"},
		{Name: "event", Revision: 4, Changed: false, Note: "issues.labeled"},
	}
	env := ext.buildEnvelope(context.Background(), issue, "task", seedGC(Snapshot{}, 0), nil, manifest)

	if !strings.Contains(env, `<artifact id="comments" revision="4" status="new">47 items</artifact>`) {
		t.Errorf("envelope missing the comments manifest entry:\n%s", truncateForLog(env))
	}
	if !strings.Contains(env, `<artifact id="event" revision="4" status="unchanged">issues.labeled</artifact>`) {
		t.Errorf("envelope missing the event manifest entry:\n%s", truncateForLog(env))
	}
}

// TestBuildEnvelopeNoArtifactsBlockWithoutEntries pins the degrade path: a
// dispatch with no input artifacts written gets no <artifacts> block at all,
// rather than an empty or malformed one.
func TestBuildEnvelopeNoArtifactsBlockWithoutEntries(t *testing.T) {
	ext, _ := newTestExtension(t, "http://unused", nil)
	var issue issueCommentPayload
	issue.Issue.Number = 7
	env := ext.buildEnvelope(context.Background(), issue, "task", seedGC(Snapshot{}, 0), nil, nil)
	if strings.Contains(env, "<artifacts") {
		t.Errorf("envelope should have no <artifacts> block when nothing was written:\n%s", truncateForLog(env))
	}
}

// TestBuildEnvelopeFortyFilePRHasChurnAndFullListNoContentButFullDescription
// pins #664's test case 1: the orchestrator's envelope for a large PR carries
// the churn summary and the full file list (names only, no patch content -
// changedFile has no patch field to begin with), while the PR description
// still reaches it in full - the split applies to evidence, never the ask.
func TestBuildEnvelopeFortyFilePRHasChurnAndFullListNoContentButFullDescription(t *testing.T) {
	ext, _ := newTestExtension(t, "http://unused", nil)
	body := strings.Repeat("this PR description matters. ", 50)
	files := make([]changedFile, 40)
	for i := range files {
		files[i] = changedFile{Filename: fmt.Sprintf("internal/pkg%d/file.go", i), Additions: 10, Deletions: 5, Status: "modified"}
	}
	var pr issueCommentPayload
	if err := json.Unmarshal(pullCommentBody("review this"), &pr); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	env := ext.buildEnvelope(context.Background(), pr, "review this", seedGC(Snapshot{IsPR: true, Body: body, Files: files}, 0), nil, nil)

	if !strings.Contains(env, `<changed_files count="40" additions="400" deletions="200">`) {
		t.Errorf("envelope missing the churn summary for a 40-file PR:\n%s", truncateForLog(env))
	}
	for i := range files {
		if !strings.Contains(env, fmt.Sprintf("internal/pkg%d/file.go", i)) {
			t.Fatalf("envelope missing changed file %d of 40 - the orchestrator needs the FULL list to cluster by subsystem:\n%s", i, truncateForLog(env))
		}
	}
	if !strings.Contains(env, body) {
		t.Errorf("envelope missing the full PR description:\n%s", truncateForLog(env))
	}
}

// TestChecksBlockRendersStatusAndSummary pins #876/#880/#882: a reviewer must
// see CI status as compact lines (name: status[, conclusion]) plus a
// failing/pending/passing summary line.
func TestChecksBlockRendersStatusAndSummary(t *testing.T) {
	checks := []checkRunView{
		{Name: "go-test", Status: "completed", Conclusion: "failure"},
		{Name: "docker-build", Status: "in_progress"},
		{Name: "lint", Status: "completed", Conclusion: "success"},
	}
	got := checksBlock(checks)
	for _, want := range []string{
		`<checks count="3" summary="1 failing, 1 pending, 1 passing">`,
		"go-test: completed failure",
		"docker-build: in_progress",
		"lint: completed success",
		"</checks>",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("checksBlock missing %q:\n%s", want, got)
		}
	}
}

// TestChecksBlockRendersWhyLinesForFailures pins that a failing check's
// enriched detail (annotations or output title) reaches the envelope as
// indented why-lines, so the agent sees what broke, not just that it did.
func TestChecksBlockRendersWhyLinesForFailures(t *testing.T) {
	checks := []checkRunView{
		{Name: "go-test", Status: "completed", Conclusion: "failure",
			Why: []string{"internal/dag/executor.go:42 TestFoo: got 2, want 1", "build failed"}},
		{Name: "lint", Status: "completed", Conclusion: "success"},
	}
	got := checksBlock(checks)
	for _, want := range []string{
		"go-test: completed failure",
		"  why: internal/dag/executor.go:42 TestFoo: got 2, want 1",
		"  why: build failed",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("checksBlock missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "lint: completed success\n  why:") {
		t.Error("a passing check must not carry why-lines")
	}
}

// TestChecksBlockEmptyWhenNoChecks pins the degrade path: a PR with no check
// runs yet (or a fetch that failed upstream) gets no <checks> block at all.
func TestChecksBlockEmptyWhenNoChecks(t *testing.T) {
	if got := checksBlock(nil); got != "" {
		t.Errorf("checksBlock(nil) = %q, want empty", got)
	}
}

// TestChecksBlockTruncatesAtCap pins the ~20-check ceiling: a PR with a huge
// matrix build still gets a bounded envelope, marked truncated, while the
// summary line still counts every check, not just the shown ones.
func TestChecksBlockTruncatesAtCap(t *testing.T) {
	checks := make([]checkRunView, 35)
	for i := range checks {
		checks[i] = checkRunView{Name: fmt.Sprintf("job-%d", i), Status: "completed", Conclusion: "success"}
	}
	checks[30] = checkRunView{Name: "go-test", Status: "completed", Conclusion: "failure"}

	got := checksBlock(checks)
	if !strings.Contains(got, `count="35"`) {
		t.Errorf("checksBlock should still report the true total count:\n%s", got)
	}
	if !strings.Contains(got, "1 failing, 0 pending, 34 passing") {
		t.Errorf("checksBlock summary should count every check, not just the shown ones:\n%s", got)
	}
	if !strings.Contains(got, `truncated="showing 20 of 35"`) {
		t.Errorf("checksBlock missing a truncation marker for 35 checks:\n%s", got)
	}
	if strings.Contains(got, "job-25") {
		t.Errorf("checksBlock should not render a check past the cap:\n%s", got)
	}
	if !strings.Contains(got, "job-19") {
		t.Errorf("checksBlock should render every check up to the cap:\n%s", got)
	}
}

// TestBuildEnvelopeIncludesChecksSection pins the orchestrator-envelope half
// of #876/#880/#882's fix: a PR's check runs reach <checks>, positioned right
// after <changed_files>. A non-PR envelope never carries the section.
func TestBuildEnvelopeIncludesChecksSection(t *testing.T) {
	ext, _ := newTestExtension(t, "http://unused", nil)
	checks := []checkRunView{{Name: "go-test", Status: "completed", Conclusion: "failure"}}
	var pr issueCommentPayload
	if err := json.Unmarshal(pullCommentBody("review this"), &pr); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	gh := githubContext{snap: Snapshot{IsPR: true}, firstLoad: true, checks: checks}
	env := ext.buildEnvelope(context.Background(), pr, "review this", gh, nil, nil)
	if !strings.Contains(env, "go-test: completed failure") {
		t.Errorf("PR envelope missing the <checks> section:\n%s", truncateForLog(env))
	}
	if idx := strings.Index(env, "<changed_files"); idx == -1 || !strings.Contains(env[idx:], "<checks") {
		t.Errorf("<checks> should follow <changed_files>:\n%s", truncateForLog(env))
	}

	var issue issueCommentPayload
	issue.Issue.Number = 7
	issueEnv := ext.buildEnvelope(context.Background(), issue, "task", seedGC(Snapshot{}, 0), nil, nil)
	if strings.Contains(issueEnv, "<checks") {
		t.Errorf("an issue (non-PR) envelope must never carry a <checks> section:\n%s", truncateForLog(issueEnv))
	}
}

// TestBuildWorkerAskIncludesChecksSection pins the node-ask half: unlike
// changed_files (deliberately withheld, see TestBuildWorkerAskOmitsEvidenceKeepsAsk),
// CI status is compact enough that a review node needs it directly rather
// than crowding out its own task.
func TestBuildWorkerAskIncludesChecksSection(t *testing.T) {
	ext, _ := newTestExtension(t, "http://unused", nil)
	checks := []checkRunView{{Name: "go-test", Status: "completed", Conclusion: "failure"}}
	var pr issueCommentPayload
	if err := json.Unmarshal(pullCommentBody("review this"), &pr); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	allowedKinds := []string{"review", "comment"}

	gh := githubContext{snap: Snapshot{IsPR: true}, firstLoad: true, checks: checks}
	ask := ext.buildWorkerAsk(context.Background(), pr, "review this", gh, allowedKinds, nil)
	if !strings.Contains(ask, "go-test: completed failure") {
		t.Errorf("worker ask missing the <checks> section a reviewer needs to avoid approving red CI:\n%s", truncateForLog(ask))
	}
}

// TestBuildWorkerAskOmitsEvidenceKeepsAsk pins #664's other half: a node's
// background (buildWorkerAsk) carries permissions, deliverable and the ask in
// full, but never the orchestrator's changed_files evidence.
func TestBuildWorkerAskOmitsEvidenceKeepsAsk(t *testing.T) {
	ext, _ := newTestExtension(t, "http://unused", nil)
	body := "the worker still needs the full description"
	files := []changedFile{{Filename: "a.go", Additions: 1, Deletions: 1, Status: "modified"}}
	var pr issueCommentPayload
	if err := json.Unmarshal(pullCommentBody("review this"), &pr); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	allowedKinds := []string{"review", "comment"}

	ask := ext.buildWorkerAsk(context.Background(), pr, "review this", seedGC(Snapshot{IsPR: true, Body: body, Files: files}, 0), allowedKinds, nil)

	if strings.Contains(ask, "changed_files") {
		t.Errorf("worker ask must not carry the orchestrator's changed_files evidence:\n%s", truncateForLog(ask))
	}
	if !strings.Contains(ask, body) {
		t.Errorf("worker ask missing the full description:\n%s", truncateForLog(ask))
	}
	if !strings.Contains(ask, "review") {
		t.Errorf("worker ask missing its permissions:\n%s", truncateForLog(ask))
	}
}

// truncateForLog keeps a failed test's diagnostic readable.
func truncateForLog(s string) string {
	const max = 4000
	if len(s) <= max {
		return s
	}
	return s[:max] + "…(truncated for log)"
}

// A deleted comment's body reads exactly like a live one, so the delta must
// mark each item rather than leaving the split positional: miscount the
// attribute counts once and a RETRACTED statement is treated as current.
func TestCommentsBlockMarksEachDeltaItem(t *testing.T) {
	gh := githubContext{delta: &Delta{
		CommentsAdded:   []snapshotComment{{ID: 1, User: "a", Body: "new one"}},
		CommentsEdited:  []snapshotComment{{ID: 2, User: "b", Body: "edited one"}},
		CommentsDeleted: []snapshotComment{{ID: 3, User: "c", Body: "retracted one"}},
	}}
	got := commentsBlock(gh, 0)
	for _, want := range []string{
		`"id":1,`, `"quack_status":"new"`,
		`"id":2,`, `"quack_status":"edited"`,
		`"id":3,`, `"quack_status":"deleted"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("commentsBlock missing %q:\n%s", want, got)
		}
	}
}

// Full-seed mode has no delta, so no item carries a status - the field is
// omitempty precisely so a seed stays GitHub's own shape.
func TestCommentsBlockSeedHasNoStatus(t *testing.T) {
	gh := githubContext{snap: Snapshot{Comments: []snapshotComment{{ID: 1, User: "a", Body: "hi"}}}}
	if got := commentsBlock(gh, 0); strings.Contains(got, "quack_status") {
		t.Errorf("full seed must not mark items:\n%s", got)
	}
}

// TestNoTriggerOutputNamesSkillOrToolMechanics pins #662: constant
// instructions - which skill to load, how stage_pr/stage_review work - are
// bundle-prompt content now, never trigger prose. Covers every deliverableText
// branch (plan-only, label-implement, review-only, conversational reply).
func TestNoTriggerOutputNamesSkillOrToolMechanics(t *testing.T) {
	ext, _ := newTestExtension(t, "http://unused", nil)
	ext.intentClassifier = &fakeIntentClassifier{verdict: "WORK"}

	var issue issueCommentPayload
	issue.Issue.Number = 7
	issue.Repository.Name, issue.Repository.Owner.Login = "widgets", "acme"

	var planOnly issueCommentPayload
	planOnly.Issue.Number = 7
	planOnly.planOnly = true
	planOnly.isLabelTrigger = true

	var implement issueCommentPayload
	implement.Issue.Number = 7
	implement.isLabelTrigger = true

	var pr issueCommentPayload
	if err := json.Unmarshal(pullCommentBody("please review this PR"), &pr); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	cases := map[string]string{
		"plan-only":      ext.buildEnvelope(context.Background(), planOnly, "plan it", seedGC(Snapshot{}, 0), nil, nil),
		"implement":      ext.buildEnvelope(context.Background(), implement, "implement it", seedGC(Snapshot{}, 0), nil, nil),
		"review-only":    ext.buildEnvelope(context.Background(), pr, "please review this PR", seedGC(Snapshot{IsPR: true, HeadRef: "x"}, 0), nil, nil),
		"conversational": ext.buildEnvelope(context.Background(), issue, "what changed?", seedGC(Snapshot{}, 0), nil, nil),
	}
	for name, env := range cases {
		for _, banned := range []string{"present-coding-plan", "stage_pr", "stage_review", "github_add_review_comment"} {
			if strings.Contains(env, banned) {
				t.Errorf("%s envelope names %q - tool/skill mechanics belong in the agent bundle prompt, not the trigger:\n%s", name, banned, truncateForLog(env))
			}
		}
	}
}

// The original's TestOrchestratorPromptCarriesMovedPlanOnlyCautions pinned
// agents/orchestrator/prompt.md, a quack-repo file this module never owns or
// ships (this package never imports quack, and quack-extensions carries no
// agents/ bundles) - genuinely inapplicable here, not ported.
