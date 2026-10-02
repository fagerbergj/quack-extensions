package github

import (
	"context"
	"errors"
	"io"
	"net/http"
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

// recorder is a fake Host.Decide; done closes once review.verdict, the last call, returns.
type recorder struct {
	mu        sync.Mutex
	reqs      []sdk.DecideRequest
	deadlines []bool
	done      chan struct{}
	answer    func(ctx context.Context) error
}

func newRecorder(answer func(ctx context.Context) error) *recorder {
	return &recorder{done: make(chan struct{}), answer: answer}
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
	if req.Point == "review.verdict" {
		close(r.done)
	}
	return sdk.Decision{Top: "approve", TopP: 1, Outcome: "observe"}, err
}

func (r *recorder) wait(t *testing.T) []sdk.DecideRequest {
	t.Helper()
	select {
	case <-r.done:
	case <-time.After(5 * time.Second):
		t.Fatal("review.verdict was never asked")
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

	type want struct{ point, baseline, primary, restrictive string }
	wants := []want{
		{"finding.severity", "blocking", "severity", "blocking"},
		{"finding.blocking", "true", "blocking", "true"},
		{"finding.severity", "nit", "severity", "blocking"},
		{"finding.blocking", "false", "blocking", "true"},
		{"finding.blocking", "false", "blocking", "true"}, // unlabeled: no severity baseline to compare
		{"review.verdict", "request_changes", "verdict", "comment,request_changes"},
	}
	if len(reqs) != len(wants) {
		t.Fatalf("got %d Decide calls, want %d: %+v", len(reqs), len(wants), reqs)
	}
	for i, w := range wants {
		r := reqs[i]
		if r.Point != w.point || r.Baseline != w.baseline || r.Primary != w.primary || strings.Join(r.Restrictive, ",") != w.restrictive {
			t.Errorf("call %d = %s baseline=%q primary=%q restrictive=%v; want %+v", i, r.Point, r.Baseline, r.Primary, r.Restrictive, w)
		}
		if _, ok := r.Questions[r.Primary]; !ok {
			t.Errorf("call %d: primary %q is not among its questions", i, r.Primary)
		}
	}
	if opts := reqs[0].Questions["severity"].Criteria.(map[string]string); len(opts) != 4 || opts["question"] == "" {
		t.Errorf("severity options = %v; want blocking/suggestion/nit/question", opts)
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
