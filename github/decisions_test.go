package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fagerbergj/quack-extensions/sdk"
)

const decisionsPatch = "@@ -1,2 +1,2 @@\n ctx\n-a\n+b\n@@ -42,2 +42,2 @@\n-old\n+new\n+newer"

// decisionsApp serves one PR (#7, authored by author) and records the posted review body.
func decisionsApp(t *testing.T, author string, posted *[]byte) *App {
	var mu sync.Mutex
	return newDeliveryApp(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/app"):
			io.WriteString(w, `{"slug":"quack"}`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/pulls/7"):
			io.WriteString(w, `{"user":{"login":"`+author+`"},"title":"Add widget","body":"Adds the widget route.","head":{"sha":"abc"}}`)
		case strings.HasSuffix(r.URL.Path, "/pulls/7/files"):
			io.WriteString(w, `[{"filename":"main.go","patch":`+jsonString(decisionsPatch)+`}]`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/reviews"):
			io.WriteString(w, `[]`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/pulls/7/reviews"):
			mu.Lock()
			*posted, _ = io.ReadAll(r.Body)
			mu.Unlock()
			io.WriteString(w, `{"id":9,"html_url":"https://github.com/acme/widgets/pull/7#pullrequestreview-9"}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	})
}

func jsonString(s string) string {
	return `"` + strings.ReplaceAll(s, "\n", `\n`) + `"`
}

func decisionsDelivery(event string) sdk.DeliveryContext {
	blocking := sdk.ReviewComment{Path: "main.go", Line: 42, Body: "**blocking:** route shadowed: /health is unreachable"}
	return sdk.DeliveryContext{
		GatePassed: true, ChatID: "github-acme-widgets-7", CloneURL: "https://github.com/acme/widgets.git", IssueNumber: 7,
		Items: []sdk.StagedDelivery{{Kind: "review", Event: event,
			Body: "**Verdict: request changes** · 1 blocking · 1 nit\n\nThe route table shadows health checks.\n\nVERDICT: request_changes",
			Comments: []sdk.ReviewComment{
				blocking,
				blocking, // exact duplicate: asked once
				{Path: "./main.go", Line: 43, Body: "(carried over, unchanged since a previous review - f2) nit: rename newer"},
				{Path: "main.go", Line: 7, Body: "unlabeled aside"},
			}}},
	}
}

// recorder is a fake Host.Decide; done closes once the want-th call to last returns.
type recorder struct {
	mu         sync.Mutex
	reqs       []sdk.DecideRequest
	deadlines  []bool
	done       chan struct{}
	answer     func(ctx context.Context) error
	last       string
	want, seen int
}

func newRecorder(answer func(ctx context.Context) error) *recorder {
	return &recorder{done: make(chan struct{}), answer: answer, last: "review.verdict", want: 1}
}

func (r *recorder) until(point string, n int) *recorder {
	r.last, r.want = point, n
	return r
}

func (r *recorder) decide(ctx context.Context, req sdk.DecideRequest) (sdk.Decision, error) {
	_, hasDeadline := ctx.Deadline()
	r.mu.Lock()
	r.reqs = append(r.reqs, req)
	r.deadlines = append(r.deadlines, hasDeadline)
	r.mu.Unlock()
	var err error
	if r.answer != nil {
		err = r.answer(ctx)
	}
	r.mu.Lock()
	if req.Point == r.last {
		if r.seen++; r.seen == r.want {
			close(r.done)
		}
	}
	r.mu.Unlock()
	return sdk.Decision{Top: "approve", TopP: 1, Outcome: "observe"}, err
}

func (r *recorder) wait(t *testing.T) []sdk.DecideRequest {
	t.Helper()
	select {
	case <-r.done:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s was never asked %d times", r.last, r.want)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, ok := range r.deadlines {
		if !ok {
			t.Errorf("%s was asked without a deadline", r.reqs[i].Point)
		}
	}
	return r.reqs
}

func TestReviewDecisionPoints(t *testing.T) {
	var posted []byte
	app := decisionsApp(t, "alice", &posted)
	rec := newRecorder(nil)
	app.decide = rec.decide
	if _, err := app.Deliver(context.Background(), decisionsDelivery("REQUEST_CHANGES")); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	reqs := rec.wait(t)

	type want struct{ point, baseline string }
	wants := []want{
		{"finding.severity", "blocking"},
		{"finding.blocking", "true"},
		{"finding.severity", "nit"},
		{"finding.blocking", "false"},
		{"finding.blocking", "false"}, // unlabeled: no severity baseline to compare
		{"review.verdict", "request_changes"},
	}
	if len(reqs) != len(wants) {
		t.Fatalf("got %d Decide calls, want %d: %+v", len(reqs), len(wants), reqs)
	}
	for i, w := range wants {
		r := reqs[i]
		if r.Point != w.point || r.Baseline != w.baseline || r.Questions != nil || r.Primary != "" || r.Restrictive != nil {
			t.Errorf("call %d = %+v; want a slim %+v", i, r, w)
		}
	}

	first := reqs[0].State.(findingState)
	if first.Path != "main.go" || first.Line != 42 || first.Finding != "route shadowed: /health is unreachable" {
		t.Errorf("finding state = %+v; want the finding without its label", first)
	}
	if first.Hunk != "@@ -42,2 +42,2 @@\n-old\n+new\n+newer" {
		t.Errorf("hunk = %q; want only the hunk covering line 42", first.Hunk)
	}
	if carried := reqs[2].State.(findingState); carried.Finding != "rename newer" || carried.Hunk == "" {
		t.Errorf("carried-over finding state = %+v; want its text without the prefix or label, with its hunk", carried)
	}
	if aside := reqs[4].State.(findingState); aside.Hunk != "" || aside.Finding != "unlabeled aside" {
		t.Errorf("out-of-diff finding state = %+v; want no hunk", aside)
	}

	v := reqs[5].State.(verdictState)
	if v.Title != "Add widget" || v.Body != "Adds the widget route." || len(v.Findings) != 3 {
		t.Fatalf("verdict state = %+v", v)
	}
	if v.Findings[0].Severity != "blocking" || v.Findings[1].Severity != "nit" || v.Findings[2].Severity != "" {
		t.Errorf("verdict findings = %+v; want their severities", v.Findings)
	}
	if v.Rationale != "The route table shadows health checks." {
		t.Errorf("rationale = %q; want the reviewer's prose without the verdict line or tail", v.Rationale)
	}
}

func TestOwnPRVerdictBaselineIsTheIntendedVerdict(t *testing.T) {
	var posted []byte
	app := decisionsApp(t, "quack[bot]", &posted)
	rec := newRecorder(nil)
	app.decide = rec.decide
	if _, err := app.Deliver(context.Background(), decisionsDelivery("approve")); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	reqs := rec.wait(t)
	if last := reqs[len(reqs)-1]; last.Baseline != "approve" {
		t.Errorf("own-PR verdict baseline = %q; want the staged approve, not the COMMENT it posts as", last.Baseline)
	}
}

func TestReviewDecisionsChangeNothing(t *testing.T) {
	var plain, failing []byte
	if _, err := decisionsApp(t, "alice", &plain).Deliver(context.Background(), decisionsDelivery("request_changes")); err != nil {
		t.Fatalf("Deliver with nil Decide: %v", err)
	}

	app := decisionsApp(t, "alice", &failing)
	rec := newRecorder(func(context.Context) error { return errors.New("handler down") })
	app.decide = rec.decide
	if _, err := app.Deliver(context.Background(), decisionsDelivery("request_changes")); err != nil {
		t.Fatalf("Deliver with a failing Decide: %v", err)
	}
	rec.wait(t)
	if len(plain) == 0 || string(plain) != string(failing) {
		t.Errorf("posted review differs:\nnil Decide:     %s\nfailing Decide: %s", plain, failing)
	}
}

func TestSlowDecideNeverDelaysTheReview(t *testing.T) {
	var posted []byte
	app := decisionsApp(t, "alice", &posted)
	release := make(chan struct{})
	rec := newRecorder(func(context.Context) error { <-release; return nil })
	app.decide = rec.decide

	delivered := make(chan error, 1)
	go func() {
		_, err := app.Deliver(context.Background(), decisionsDelivery("request_changes"))
		delivered <- err
	}()
	select {
	case err := <-delivered:
		if err != nil || len(posted) == 0 {
			t.Errorf("Deliver = %v, posted %d bytes; want the review posted", err, len(posted))
		}
	case <-time.After(5 * time.Second):
		t.Error("Deliver waited on a blocked Decide")
	}
	close(release)
	rec.wait(t)
}

func TestHungDecideIsBounded(t *testing.T) {
	old := decideTimeout
	decideTimeout = 20 * time.Millisecond
	t.Cleanup(func() { decideTimeout = old })
	var posted []byte
	app := decisionsApp(t, "alice", &posted)
	rec := newRecorder(func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() })
	app.decide = rec.decide
	if _, err := app.Deliver(context.Background(), decisionsDelivery("request_changes")); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	rec.wait(t)
}

func TestHostSeverityBeatsBodyLabel(t *testing.T) {
	var posted []byte
	app := decisionsApp(t, "alice", &posted)
	rec := newRecorder(nil)
	app.decide = rec.decide
	dc := decisionsDelivery("REQUEST_CHANGES")
	dc.Items[0].Comments = []sdk.ReviewComment{{Path: "main.go", Line: 42, Body: "Shadowed route: /health is unreachable", Severity: "Blocking"}}
	if _, err := app.Deliver(context.Background(), dc); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	reqs := rec.wait(t)
	if len(reqs) != 3 || reqs[0].Point != "finding.severity" || reqs[0].Baseline != "blocking" ||
		reqs[1].Point != "finding.blocking" || reqs[1].Baseline != "true" {
		t.Errorf("calls = %+v; want severity=blocking then blocking=true from the host's Severity", reqs)
	}
}

func TestDecisionPointsDeclareTheAskedPoints(t *testing.T) {
	type want struct{ primary, restrictive string }
	wants := map[string]want{
		"finding.severity": {"severity", "blocking"},
		"finding.blocking": {"blocking", "true"},
		"review.verdict":   {"verdict", "comment,request_changes"},
		"intent":           {"write", "false"},
		"ci.flaky":         {"flaky", ""},
	}
	points := (&Extension{}).DecisionPoints()
	if len(points) != len(wants) {
		t.Fatalf("declared %d points, want %d", len(points), len(wants))
	}
	for _, p := range points {
		w, ok := wants[p.Name]
		if !ok || p.Primary != w.primary || strings.Join(p.Restrictive, ",") != w.restrictive || strings.Join(p.Modes, ",") != "observe" {
			t.Errorf("%s = primary %q restrictive %v modes %v; want %+v, observe only", p.Name, p.Primary, p.Restrictive, p.Modes, w)
		}
		if _, ok := p.Questions[p.Primary]; !ok {
			t.Errorf("%s: primary %q is not among its questions", p.Name, p.Primary)
		}
	}
	if opts := points[0].Questions["severity"].Criteria.(map[string]string); len(opts) != 4 || opts["question"] == "" {
		t.Errorf("severity options = %v; want blocking/suggestion/nit/question", opts)
	}
}

func mentionBody(comment string) []byte {
	return []byte(fmt.Sprintf(`{
		"action":"created",
		"comment":{"id":999,"body":%q,"user":{"login":"alice"},"author_association":"OWNER"},
		"issue":{"number":7,"title":"Widget crashes on start"},
		"repository":{"name":"widgets","owner":{"login":"acme"},"clone_url":"https://github.com/acme/widgets.git","default_branch":"main"},
		"installation":{"id":5}
	}`, comment))
}

// mentionDispatch sends one issue mention through the webhook and returns its dispatch.
func mentionDispatch(t *testing.T, decide func(context.Context, sdk.DecideRequest) (sdk.Decision, error)) (*Extension, sdk.DispatchRequest) {
	t.Helper()
	srv := stubGitHub(t, make(chan string, 1))
	t.Cleanup(srv.Close)
	ext, fh := newTestExtension(t, srv.URL, nil)
	ext.app.decide = decide
	ext.handleWebhook(httptest.NewRecorder(), signedRequest("issue_comment", mentionBody("@quack add a feature")))
	return ext, fh.waitForDispatch(t, 2*time.Second)
}

func TestIntentPointObservesAMention(t *testing.T) {
	rec := newRecorder(nil).until("intent", 1)
	_, dispatched := mentionDispatch(t, rec.decide)
	reqs := rec.wait(t)
	if len(reqs) != 1 {
		t.Fatalf("got %d Decide calls, want 1: %+v", len(reqs), reqs)
	}
	r := reqs[0]
	if r.Point != "intent" || r.Baseline != "false" || r.ChatID != globalChatID(dispatched.Chat.LocalID) {
		t.Errorf("request = %+v; want intent, baseline false (a reply), the dispatch's chat", r)
	}
	want := intentState{Subject: "issue", Title: "Widget crashes on start", Sender: "alice", Association: "OWNER", Comment: "@quack add a feature"}
	if st := r.State.(intentState); st != want {
		t.Errorf("state = %+v; want %+v", st, want)
	}
}

func TestIntentDecisionsChangeNothing(t *testing.T) {
	_, plain := mentionDispatch(t, nil)
	rec := newRecorder(func(context.Context) error { return errors.New("handler down") }).until("intent", 1)
	_, failing := mentionDispatch(t, rec.decide)
	rec.wait(t)
	if plain.Ask.Message != failing.Ask.Message || plain.Ask.NodeContext != failing.Ask.NodeContext {
		t.Errorf("dispatch differs with a failing Decide:\nnil:     %s\nfailing: %s", plain.Ask.Message, failing.Ask.Message)
	}

	assertOutlivesTrigger(t, "intent", func(decide func(context.Context, sdk.DecideRequest) (sdk.Decision, error)) *Extension {
		ext, _ := mentionDispatch(t, decide)
		return ext
	})
}

// assertOutlivesTrigger blocks Decide until the trigger has dispatched and returned,
// then checks the call still had a live context.
func assertOutlivesTrigger(t *testing.T, point string, trigger func(func(context.Context, sdk.DecideRequest) (sdk.Decision, error)) *Extension) {
	t.Helper()
	release := make(chan struct{})
	var ctxErr error
	slow := newRecorder(func(ctx context.Context) error { <-release; ctxErr = ctx.Err(); return nil }).until(point, 1)
	trigger(slow.decide).Wait() // the dispatch went out while Decide was blocked, and its ctx is cancelled
	close(release)
	slow.wait(t)
	if ctxErr != nil {
		t.Errorf("%s Decide ran on the trigger's cancelled context: %v", point, ctxErr)
	}
}

func TestHungIntentDecideIsBounded(t *testing.T) {
	old := decideTimeout
	decideTimeout = 20 * time.Millisecond
	t.Cleanup(func() { decideTimeout = old })
	rec := newRecorder(func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }).until("intent", 1)
	mentionDispatch(t, rec.decide)
	rec.wait(t)
}

func TestObserveIntentBaseline(t *testing.T) {
	for _, tc := range []struct {
		kind, want   string
		labelTrigger bool
	}{
		{kind: "commit", want: "true"},
		{kind: "pull_request", want: "true"},
		{kind: "review", want: "false"},
		{kind: "reply", want: "false"},
		{kind: "", want: ""},                                 // synthetic or command trigger: not asked
		{kind: "pull_request", want: "", labelTrigger: true}, // label trigger: nothing classified
	} {
		ext, _ := newTestExtension(t, "http://unused", nil)
		rec := newRecorder(nil).until("intent", 1)
		ext.app.decide = rec.decide
		var p issueCommentPayload
		p.isLabelTrigger = tc.labelTrigger
		ext.observeIntent(context.Background(), p, "ext:github:c", []string{"pull_request", "comment"}, tc.kind)
		if tc.want == "" {
			time.Sleep(20 * time.Millisecond)
			rec.mu.Lock()
			n := len(rec.reqs)
			rec.mu.Unlock()
			if n != 0 {
				t.Errorf("kind %q label %v: asked %d times; want no call", tc.kind, tc.labelTrigger, n)
			}
			continue
		}
		r := rec.wait(t)[0]
		if r.Baseline != tc.want || r.State.(intentState).Grant != "pull_request, comment" {
			t.Errorf("kind %q: baseline %q grant %q; want %q", tc.kind, r.Baseline, r.State.(intentState).Grant, tc.want)
		}
	}
}

func TestEnvelopeRecordsTheDeliverableKind(t *testing.T) {
	var pr, issue issueCommentPayload
	if err := json.Unmarshal(pullCommentBody("@quack x"), &pr); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(issueCommentBody("@quack x"), &issue); err != nil {
		t.Fatal(err)
	}
	planned, labelled, hinted := issue, issue, pr
	planned.planOnly, planned.isLabelTrigger = true, true
	labelled.isLabelTrigger = true
	hinted.deliverableHint = "commits that make CI pass"
	pushable := []string{"review", "pull_request"}
	for _, tc := range []struct {
		name  string
		p     issueCommentPayload
		task  string
		kinds []string
		cls   fakeIntentClassifier
		want  string
	}{
		{"granted commit", pr, "fix it", pushable, fakeIntentClassifier{grantedDeliverable: "COMMIT"}, "commit"},
		{"granted review", pr, "look again", pushable, fakeIntentClassifier{grantedDeliverable: "REVIEW"}, "review"},
		{"granted reply", pr, "why?", pushable, fakeIntentClassifier{grantedDeliverable: "REPLY"}, "reply"},
		{"conversational PR", pr, "thanks", nil, fakeIntentClassifier{verdict: "CONVERSATIONAL"}, "reply"},
		{"review-only grant", pr, "address these", []string{"review"}, fakeIntentClassifier{verdict: "WORK"}, "review"},
		{"ungranted read", pr, "look at the auth path", nil, fakeIntentClassifier{verdict: "WORK"}, "review"},
		{"ungranted change", pr, "fix the bug and push a commit", nil, fakeIntentClassifier{verdict: "WORK"}, "commit"},
		{"issue implement", issue, "build it", []string{"pull_request"}, fakeIntentClassifier{issueDeliverable: "IMPLEMENT"}, "pull_request"},
		{"issue comment", issue, "thoughts?", []string{"pull_request"}, fakeIntentClassifier{issueDeliverable: "COMMENT"}, "reply"},
		{"issue fallback implement", issue, "implement this and open a PR", []string{"pull_request"}, fakeIntentClassifier{issueDeliverableErr: errors.New("down")}, "pull_request"},
		{"issue fallback reply", issue, "hello", nil, fakeIntentClassifier{issueDeliverableErr: errors.New("down")}, "reply"},
		{"plan label", planned, "plan", nil, fakeIntentClassifier{}, "plan"},
		{"implement label", labelled, "build", []string{"pull_request"}, fakeIntentClassifier{}, "pull_request"},
		{"synthetic hint", hinted, "fix CI", pushable, fakeIntentClassifier{}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ext, _ := newTestExtension(t, "http://unused", nil)
			ext.intentClassifier = &tc.cls
			p := tc.p
			p.deliverableKind = new(string)
			ext.buildEnvelope(context.Background(), p, tc.task, seedGC(Snapshot{IsPR: p.Issue.PullRequest != nil}, 0), tc.kinds, nil)
			if *p.deliverableKind != tc.want {
				t.Errorf("kind = %q; want %q", *p.deliverableKind, tc.want)
			}
		})
	}

	ext, _ := newTestExtension(t, "http://unused", nil)
	ext.intentClassifier = &fakeIntentClassifier{grantedDeliverable: "COMMIT"}
	pr.deliverableKind = new(string)
	ext.buildEnvelope(context.Background(), pr, "fix it", seedGC(Snapshot{IsPR: true}, 0), pushable, nil)
	ext.intentClassifier = &fakeIntentClassifier{grantedDeliverable: "REPLY"}
	ext.buildWorkerAsk(context.Background(), pr, "fix it", seedGC(Snapshot{IsPR: true}, 0), pushable, nil)
	if *pr.deliverableKind != "commit" {
		t.Errorf("kind = %q after a disagreeing second call; want the envelope's commit", *pr.deliverableKind)
	}
}

// ciFlakyGitHub is stubFixGitHubFull with the PR's changed files served.
func ciFlakyGitHub(t *testing.T) *httptest.Server {
	inner := stubFixGitHubFull(t, make(chan string, 4), []string{"quack:fix"}, true, "", "someone-else")
	t.Cleanup(inner.Close)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/pulls/7/files") {
			io.WriteString(w, `[{"filename":"internal/foo.go"},{"filename":"README.md"}]`)
			return
		}
		inner.Config.Handler.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func fixLabelEvent() []byte {
	return []byte(`{"action":"labeled","number":7,"pull_request":{"title":"Test PR","head":{"sha":"headsha1"}},"label":{"name":"quack:fix"},
		"repository":{"name":"widgets","owner":{"login":"acme"},"clone_url":"https://github.com/acme/widgets.git","default_branch":"main"},
		"installation":{"id":5},"sender":{"login":"alice"}}`)
}

// ciDispatch sends a CI failure (workflow_run or the fix label) through the webhook and returns its fix dispatch.
func ciDispatch(t *testing.T, event string, decide func(context.Context, sdk.DecideRequest) (sdk.Decision, error)) (*Extension, sdk.DispatchRequest) {
	t.Helper()
	ext, fh := newTestExtension(t, ciFlakyGitHub(t).URL, []string{"ci_fix"})
	ext.app.decide = decide
	body := workflowRunBody("completed", "failure", "sha1", 7)
	if event == "pull_request" {
		body = fixLabelEvent()
	}
	ext.handleWebhook(httptest.NewRecorder(), signedRequest(event, body))
	return ext, fh.waitForDispatch(t, 2*time.Second)
}

func TestCIFlakyPointObservesEachFailingCheck(t *testing.T) {
	for event, sha := range map[string]string{"workflow_run": "sha1", "pull_request": "headsha1"} {
		t.Run(event, func(t *testing.T) {
			rec := newRecorder(nil).until("ci.flaky", 1)
			ciDispatch(t, event, rec.decide)
			reqs := rec.wait(t)
			if len(reqs) != 1 {
				t.Fatalf("got %d Decide calls, want one per failing check (go-test): %+v", len(reqs), reqs)
			}
			r := reqs[0]
			if r.Point != "ci.flaky" || r.Baseline != "" || r.ChatID != "ext:github:github-acme-widgets-7" {
				t.Errorf("request = %+v; want ci.flaky, no baseline, the PR's chat", r)
			}
			st := r.State.(ciFailureState)
			if st.Repo != "acme/widgets" || st.PR != 7 || st.HeadSHA != sha || st.Check != "go-test" ||
				!strings.Contains(st.Failure, "TestFoo failed") || strings.Join(st.ChangedFiles, ",") != "internal/foo.go,README.md" {
				t.Errorf("state = %+v; want go-test's failure on %s with the PR's files", st, sha)
			}
		})
	}
}

func TestCIFlakyDecisionsChangeNothing(t *testing.T) {
	_, plain := ciDispatch(t, "workflow_run", nil)
	rec := newRecorder(func(context.Context) error { return errors.New("handler down") }).until("ci.flaky", 1)
	_, failing := ciDispatch(t, "workflow_run", rec.decide)
	rec.wait(t)
	if plain.Ask.Message != failing.Ask.Message {
		t.Errorf("fix dispatch differs with a failing Decide:\nnil:     %s\nfailing: %s", plain.Ask.Message, failing.Ask.Message)
	}

	assertOutlivesTrigger(t, "ci.flaky", func(decide func(context.Context, sdk.DecideRequest) (sdk.Decision, error)) *Extension {
		ext, _ := ciDispatch(t, "workflow_run", decide)
		return ext
	})
}

func TestHungCIFlakyDecideIsBounded(t *testing.T) {
	old := decideTimeout
	decideTimeout = 20 * time.Millisecond
	t.Cleanup(func() { decideTimeout = old })
	rec := newRecorder(func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }).until("ci.flaky", 1)
	ciDispatch(t, "workflow_run", rec.decide)
	rec.wait(t)
}
