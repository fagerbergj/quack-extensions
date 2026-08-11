package github

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fagerbergj/quack-extensions/sdk"
)

const testSecret = "shhh-webhook-secret"

func testKeyPEM(t *testing.T) (string, *rsa.PublicKey) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("gen key: %v", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	return string(pemBytes), &key.PublicKey
}

// fakeDispatchHost records every Host.Dispatch call this test's Extension
// makes. notify fires (non-blocking) after each recorded call so a test that
// went through handleWebhook - which dispatches from a goroutine - can wait
// deterministically instead of polling calls()/sleeping.
type fakeDispatchHost struct {
	mu          sync.Mutex
	dispatches  []sdk.DispatchRequest
	dispatchErr error
	notify      chan sdk.DispatchRequest
	// block, if non-nil, is waited on before a call is recorded - simulates a
	// slow Host.Dispatch to prove handleWebhook's `go e.dispatch(...)` returns
	// without waiting on it.
	block chan struct{}
}

func (f *fakeDispatchHost) dispatch(ctx context.Context, req sdk.DispatchRequest) error {
	if f.block != nil {
		<-f.block
	}
	f.mu.Lock()
	f.dispatches = append(f.dispatches, req)
	f.mu.Unlock()
	if f.notify != nil {
		select {
		case f.notify <- req:
		default:
		}
	}
	return f.dispatchErr
}

func (f *fakeDispatchHost) calls() []sdk.DispatchRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]sdk.DispatchRequest, len(f.dispatches))
	copy(out, f.dispatches)
	return out
}

// waitForDispatch blocks for one Host.Dispatch call, failing the test if none
// arrives within timeout - the async-dispatch replacement for the original's
// runner.gotMessage channel read.
func (f *fakeDispatchHost) waitForDispatch(t *testing.T, timeout time.Duration) sdk.DispatchRequest {
	t.Helper()
	select {
	case req := <-f.notify:
		return req
	case <-time.After(timeout):
		t.Fatal("no Host.Dispatch call within timeout")
		return sdk.DispatchRequest{}
	}
}

// stubGitHub serves the REST endpoints dispatch touches (installation
// resolve, token mint, comment post) and signals postedComment when a
// comment lands.
func stubGitHub(t *testing.T, postedComment chan<- string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/installation"):
			fmt.Fprint(w, `{"id":5}`)
		case strings.HasSuffix(r.URL.Path, "/access_tokens"):
			fmt.Fprintf(w, `{"token":"ghs_x","expires_at":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339))
		case strings.HasSuffix(r.URL.Path, "/reactions"):
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"id":1}`)
		case strings.HasSuffix(r.URL.Path, "/app"):
			fmt.Fprint(w, `{"slug":"quack"}`)
		case strings.HasSuffix(r.URL.Path, "/files"):
			fmt.Fprint(w, `[]`)
		case strings.HasSuffix(r.URL.Path, "/reviews"):
			fmt.Fprint(w, `[]`)
		case strings.HasSuffix(r.URL.Path, "/commits"):
			fmt.Fprint(w, `[]`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments"):
			body, _ := io.ReadAll(r.Body)
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{}`)
			postedComment <- string(body)
		case strings.HasSuffix(r.URL.Path, "/comments"):
			fmt.Fprint(w, `[]`)
		case strings.Contains(r.URL.Path, "/pulls/"):
			fmt.Fprint(w, `{"title":"Test PR","body":"A test PR.","state":"open","head":{"ref":"feature-branch","sha":"headsha1"},"base":{"ref":"main"}}`)
		case isIssueMetaPath(r.URL.Path):
			fmt.Fprint(w, `{"title":"Test issue","body":"A test issue.","state":"open"}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
}

func isIssueMetaPath(path string) bool {
	parts := strings.Split(strings.TrimRight(path, "/"), "/")
	if len(parts) < 2 || parts[len(parts)-2] != "issues" {
		return false
	}
	_, err := strconv.Atoi(parts[len(parts)-1])
	return err == nil
}

func issueCommentPayloadFor(owner, repo string, number int, login, body string, apiURL string) issueCommentPayload {
	var p issueCommentPayload
	p.Action = "created"
	p.Comment.ID = 999
	p.Comment.Body = body
	p.Comment.User.Login = login
	p.Issue.Number = number
	p.Repository.Name = repo
	p.Repository.Owner.Login = owner
	p.Repository.CloneURL = "https://github.com/" + owner + "/" + repo + ".git"
	p.Repository.DefaultBranch = "main"
	p.Installation.ID = 5
	return p
}

// issueCommentBody is the raw issue_comment webhook JSON handleWebhook parses.
func issueCommentBody(commentBody string) []byte {
	return []byte(fmt.Sprintf(`{
		"action":"created",
		"comment":{"id":999,"body":%q,"user":{"login":"alice"}},
		"issue":{"number":7},
		"repository":{"name":"widgets","owner":{"login":"acme"},"clone_url":"https://github.com/acme/widgets.git","default_branch":"main"},
		"installation":{"id":5}
	}`, commentBody))
}

// pullRequestBody is the raw pull_request webhook JSON for opened/labeled actions.
func pullRequestBody(action, labelName string) []byte {
	return []byte(fmt.Sprintf(`{
		"action":%q,
		"number":7,
		"label":{"name":%q},
		"repository":{"name":"widgets","owner":{"login":"acme"},"clone_url":"https://github.com/acme/widgets.git","default_branch":"main"},
		"installation":{"id":5},
		"sender":{"login":"alice"}
	}`, action, labelName))
}

// issuesBody is the raw issues webhook JSON for the label-driven issue workflow.
func issuesBody(action, labelName, sender string, isPR bool) []byte {
	pr := ""
	if isPR {
		pr = `"pull_request":{},`
	}
	return []byte(fmt.Sprintf(`{
		"action":%q,
		"issue":{"number":7,"title":"Add widget cache","body":"Widgets are refetched on every request.",%s"labels":[]},
		"label":{"name":%q},
		"repository":{"name":"widgets","owner":{"login":"acme"},"clone_url":"https://github.com/acme/widgets.git","default_branch":"main"},
		"installation":{"id":5},
		"sender":{"login":%q}
	}`, action, pr, labelName, sender))
}

// pullRequestReviewBody is the pull_request_review webhook payload for a submitted review.
func pullRequestReviewBody(state, reviewer string, prNumber int) []byte {
	return []byte(fmt.Sprintf(`{
		"action":"submitted",
		"review":{"state":%q,"user":{"login":%q}},
		"pull_request":{"number":%d},
		"repository":{"name":"widgets","owner":{"login":"acme"},"clone_url":"https://github.com/acme/widgets.git","default_branch":"main"},
		"installation":{"id":5}
	}`, state, reviewer, prNumber))
}

// newTestExtension builds an Extension wired to apiBase (a stubGitHub
// server) and a fakeDispatchHost, bypassing the sdk.Factory/YAML path -
// these tests exercise the webhook/dispatch/RunEnded machinery directly.
func newTestExtension(t *testing.T, apiBase string, triggers []string) (*Extension, *fakeDispatchHost) {
	t.Helper()
	keyPEM, _ := testKeyPEM(t)
	app, err := NewApp("1", keyPEM)
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	app.apiBase = apiBase

	st, err := openStore(t.TempDir())
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	fh := &fakeDispatchHost{notify: make(chan sdk.DispatchRequest, 8)}
	host := sdk.Host{
		Dispatch: fh.dispatch,
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		DataDir:  t.TempDir(),
	}

	if len(triggers) == 0 {
		triggers = []string{"mention"}
	}
	tmap := make(map[string]bool, len(triggers))
	for _, tr := range triggers {
		tmap[tr] = true
	}

	e := &Extension{
		app:      app,
		host:     host,
		store:    st,
		secret:   []byte(testSecret),
		mention:  "@quack",
		triggers: tmap,
		labels: Labels{
			Plan: "quack:plan", Implement: "quack:implement", Review: "quack-auto-review",
			Merge: "quack:merge", PartialFix: "quack:partial-fix", Fix: "quack:fix",
		},
		allowedUsers: map[string]bool{"alice": true},
		runTimeout:   time.Hour,
	}
	return e, fh
}

func signedRequest(event string, body []byte) *http.Request {
	mac := hmac.New(sha256.New, []byte(testSecret))
	mac.Write(body)
	sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	req := httptest.NewRequest(http.MethodPost, webhookPath, strings.NewReader(string(body)))
	req.Header.Set("X-GitHub-Event", event)
	req.Header.Set("X-Hub-Signature-256", sig)
	return req
}

func TestVerifySignature(t *testing.T) {
	secret := []byte(testSecret)
	body := []byte(`{"hello":"world"}`)
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	valid := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	tests := []struct {
		name   string
		secret []byte
		body   []byte
		header string
		want   bool
	}{
		{"valid", secret, body, valid, true},
		{"missing header", secret, body, "", false},
		{"no prefix", secret, body, strings.TrimPrefix(valid, "sha256="), false},
		{"tampered body", secret, append(body, '!'), valid, false},
		{"wrong secret", []byte("other"), body, valid, false},
		{"empty secret", nil, body, valid, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := verifySignature(tt.secret, tt.body, tt.header); got != tt.want {
				t.Errorf("verifySignature = %v; want %v", got, tt.want)
			}
		})
	}
}

func TestTriggerTaskLineStart(t *testing.T) {
	ext := &Extension{mention: "/quack", triggers: map[string]bool{"mention": true}}
	tests := []struct {
		name     string
		body     string
		wantTask string
		wantOK   bool
	}{
		{"line-start token dispatches", "/quack address finding 1", "address finding 1", true},
		{"leading spaces still count as line start", "  /quack fix the typo", "fix the typo", true},
		{"prose containing the bare word does not dispatch", "quack's gate did not pass", "", false},
		{"prose mentioning the token mid-sentence does not dispatch", "please run /quack fix this", "", false},
		{"a quoted reply does not re-fire", "> /quack address finding 1\n\nlooks good", "", false},
		{"a quoted reply followed by a real request still dispatches from its own line", "> /quack old request\n\n/quack new request", "new request", true},
		{"a longer word sharing the prefix does not match", "/quackers is not a command", "", false},
		{"empty task after the token does not dispatch", "/quack", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := issueCommentPayload{Action: "created"}
			p.Comment.Body = tt.body
			task, ok := ext.triggerTask(p)
			if ok != tt.wantOK || task != tt.wantTask {
				t.Errorf("triggerTask(%q) = (%q, %v); want (%q, %v)", tt.body, task, ok, tt.wantTask, tt.wantOK)
			}
		})
	}
}

// TestDispatchBuildsRequestAndStoresPending proves the new dispatch() path:
// it shapes an sdk.DispatchRequest (permissions/deliverable text in
// Ask.Message, the deterministic Setup) and stores a pendingRun BEFORE
// calling Host.Dispatch, rather than driving a run to completion itself.
func TestDispatchBuildsRequestAndStoresPending(t *testing.T) {
	posted := make(chan string, 1)
	srv := stubGitHub(t, posted)
	defer srv.Close()
	e, fh := newTestExtension(t, srv.URL, nil)

	p := issueCommentPayloadFor("acme", "widgets", 7, "alice", "@quack take a look", srv.URL)
	e.dispatch(p, "take a look")

	calls := fh.calls()
	if len(calls) != 1 {
		t.Fatalf("Dispatch calls = %d, want 1", len(calls))
	}
	req := calls[0]
	if req.Chat.LocalID != "github-acme-widgets-7" {
		t.Errorf("LocalID = %q, want github-acme-widgets-7", req.Chat.LocalID)
	}
	if req.Chat.User != "alice" {
		t.Errorf("User = %q, want alice", req.Chat.User)
	}
	if !strings.Contains(req.Ask.Message, "<permissions>") {
		t.Errorf("Ask.Message missing <permissions> block: %q", req.Ask.Message)
	}
	if req.Run.Setup == nil || req.Run.Setup.Repo != p.Repository.CloneURL {
		t.Errorf("Run.Setup = %+v, want Repo=%q", req.Run.Setup, p.Repository.CloneURL)
	}

	chatID := globalChatID("github-acme-widgets-7")
	if _, ok := e.pending.Load(chatID); !ok {
		t.Errorf("pending[%q] not stored before Dispatch returned", chatID)
	}
}

// TestDispatchDedupDropsSecondTrigger proves the inflight guard still works
// unchanged: a second trigger for a session already dispatched is dropped,
// not queued, and never reaches Host.Dispatch.
func TestDispatchDedupDropsSecondTrigger(t *testing.T) {
	posted := make(chan string, 1)
	srv := stubGitHub(t, posted)
	defer srv.Close()
	e, fh := newTestExtension(t, srv.URL, nil)

	p := issueCommentPayloadFor("acme", "widgets", 7, "alice", "@quack go", srv.URL)
	e.dispatch(p, "go")
	e.dispatch(p, "go again") // same session - must be deduped

	if calls := fh.calls(); len(calls) != 1 {
		t.Fatalf("Dispatch calls = %d, want 1 (second trigger should dedup)", len(calls))
	}
}

// TestRunEndedNudgesOnceThenFinalizes exercises the async nudge chain that
// replaces the old synchronous e.drive(runNudge) call: a label-triggered
// dispatch whose outcome carries PlanRan=false gets ONE follow-up Dispatch
// with runNudge as the message; that follow-up's own RunEnded then finalizes
// (posts the answer as a comment) rather than nudging again.
func TestRunEndedNudgesOnceThenFinalizes(t *testing.T) {
	posted := make(chan string, 1)
	srv := stubGitHub(t, posted)
	defer srv.Close()
	e, fh := newTestExtension(t, srv.URL, []string{"issue_plan"})

	sessionID := "github-acme-widgets-7"
	chatID := globalChatID(sessionID)
	e.pending.Store(chatID, &pendingRun{
		sessionID: sessionID, owner: "acme", repo: "widgets", number: 7,
		login: "alice", isLabelTrigger: true,
	})
	e.inflight.Store(sessionID, struct{}{})

	e.RunEnded(chatID, sdk.RunOutcome{Status: sdk.RunDone, PlanRan: false})

	calls := fh.calls()
	if len(calls) != 1 {
		t.Fatalf("nudge Dispatch calls = %d, want 1", len(calls))
	}
	if calls[0].Ask.Message != runNudge {
		t.Errorf("nudge message = %q, want runNudge", calls[0].Ask.Message)
	}
	if _, ok := e.pending.Load(chatID); !ok {
		t.Fatalf("pending entry removed after nudge - RunEnded must keep it for the nudge's own callback")
	}

	// The nudge's own RunEnded now fires with a real answer - finalize, not another nudge.
	e.RunEnded(chatID, sdk.RunOutcome{Status: sdk.RunDone, PlanRan: true, Answer: "done, see the fix"})

	select {
	case body := <-posted:
		if !strings.Contains(body, "done, see the fix") {
			t.Errorf("posted comment = %q, want it to contain the answer", body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no comment posted after the nudge's own RunEnded")
	}
	if _, ok := e.pending.Load(chatID); ok {
		t.Errorf("pending entry still present after finalize")
	}
	if _, ok := e.inflight.Load(sessionID); ok {
		t.Errorf("inflight entry still present after finalize")
	}
}

// TestFinalizeSkipsSummaryWhenDeliveryVerified proves finalize's
// "commitDelivery already posted the review/PR" short-circuit still holds:
// when takeDeliveryDetail reports a verified delivery, finalize must not
// ALSO post the run's answer as a duplicate comment.
func TestFinalizeSkipsSummaryWhenDeliveryVerified(t *testing.T) {
	posted := make(chan string, 1)
	srv := stubGitHub(t, posted)
	defer srv.Close()
	e, _ := newTestExtension(t, srv.URL, nil)

	sessionID := "github-acme-widgets-7"
	chatID := globalChatID(sessionID)
	pr := &pendingRun{sessionID: sessionID, owner: "acme", repo: "widgets", number: 7, login: "alice"}
	e.pending.Store(chatID, pr)
	e.inflight.Store(sessionID, struct{}{})
	recordDelivery(chatID, deliveryOutcome{prNumber: 42, prURL: "https://github.com/acme/widgets/pull/42"})

	e.RunEnded(chatID, sdk.RunOutcome{Status: sdk.RunDone, PlanRan: true, Answer: "I opened a PR"})

	select {
	case body := <-posted:
		t.Fatalf("unexpected summary comment posted: %q", body)
	case <-time.After(300 * time.Millisecond):
		// expected: no comment
	}
	if _, ok := e.pending.Load(chatID); ok {
		t.Errorf("pending entry still present after finalize")
	}
}

// ---- Batch A: trigger/label-routing/dispatch-shape (ported from quack's internal/github/webhook_test.go) ----

// TestHandleWebhookPROpenedTrigger pins the pr_opened trigger gate: it fires
// a dispatch only when "pr_opened" is in the configured trigger set.
func TestHandleWebhookPROpenedTrigger(t *testing.T) {
	tests := []struct {
		name     string
		triggers []string
		wantRun  bool
	}{
		{"pr_opened enabled fires", []string{"pr_opened"}, true},
		{"mention only is a no-op", []string{"mention"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := stubGitHub(t, make(chan string, 1))
			defer srv.Close()
			ext, fh := newTestExtension(t, srv.URL, tt.triggers)

			rec := httptest.NewRecorder()
			ext.handleWebhook(rec, signedRequest("pull_request", pullRequestBody("opened", "")))
			if rec.Code != http.StatusAccepted && rec.Code != http.StatusOK {
				t.Fatalf("status = %d", rec.Code)
			}

			if tt.wantRun {
				req := fh.waitForDispatch(t, 2*time.Second)
				if !strings.Contains(req.Ask.Message, `<pull_request number="7">`) || !strings.Contains(req.Ask.Message, `"name":"widgets"`) {
					t.Errorf("dispatch message missing the hoisted PR ask / repo event: %q", req.Ask.Message)
				}
			} else {
				select {
				case <-fh.notify:
					t.Error("pr_opened should not fire when only mention is configured")
				case <-time.After(200 * time.Millisecond):
				}
			}
		})
	}
}

// TestHandleWebhookLabelTrigger pins the "label" trigger: it fires only for
// the configured review label, and only when "label" is in the trigger set.
func TestHandleWebhookLabelTrigger(t *testing.T) {
	tests := []struct {
		name      string
		triggers  []string
		labelName string
		wantRun   bool
	}{
		{"matching label + label trigger fires", []string{"label"}, "quack-auto-review", true},
		{"non-matching label is a no-op", []string{"label"}, "other-label", false},
		{"matching label but trigger not enabled is a no-op", []string{"mention"}, "quack-auto-review", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := stubGitHub(t, make(chan string, 1))
			defer srv.Close()
			ext, fh := newTestExtension(t, srv.URL, tt.triggers)

			rec := httptest.NewRecorder()
			ext.handleWebhook(rec, signedRequest("pull_request", pullRequestBody("labeled", tt.labelName)))
			if rec.Code != http.StatusAccepted && rec.Code != http.StatusOK {
				t.Fatalf("status = %d", rec.Code)
			}

			if tt.wantRun {
				fh.waitForDispatch(t, 2*time.Second)
			} else {
				select {
				case <-fh.notify:
					t.Errorf("%s: should not have dispatched a run", tt.name)
				case <-time.After(200 * time.Millisecond):
				}
			}
		})
	}
}

// TestHandleWebhookAutoReviewUsesPRSessionID pins that a pr_opened auto-review
// dispatches on the PR's own session id, not a synthetic one.
func TestHandleWebhookAutoReviewUsesPRSessionID(t *testing.T) {
	srv := stubGitHub(t, make(chan string, 1))
	defer srv.Close()
	ext, fh := newTestExtension(t, srv.URL, []string{"pr_opened"})

	rec := httptest.NewRecorder()
	ext.handleWebhook(rec, signedRequest("pull_request", pullRequestBody("opened", "")))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d; want 202", rec.Code)
	}

	req := fh.waitForDispatch(t, 2*time.Second)
	if req.Chat.LocalID != "github-acme-widgets-7" {
		t.Errorf("session id = %q; want github-acme-widgets-7", req.Chat.LocalID)
	}
}

// TestHandleWebhookMentionRespectsTriggerSet pins that a mention only
// dispatches when "mention" is in the configured trigger set.
func TestHandleWebhookMentionRespectsTriggerSet(t *testing.T) {
	tests := []struct {
		name     string
		triggers []string
		wantRun  bool
	}{
		{"mention enabled fires", []string{"mention"}, true},
		{"mention not configured is a no-op", []string{"pr_opened"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := stubGitHub(t, make(chan string, 1))
			defer srv.Close()
			ext, fh := newTestExtension(t, srv.URL, tt.triggers)

			rec := httptest.NewRecorder()
			ext.handleWebhook(rec, signedRequest("issue_comment", issueCommentBody("@quack add a feature")))
			if rec.Code != http.StatusAccepted && rec.Code != http.StatusOK {
				t.Fatalf("status = %d", rec.Code)
			}

			if tt.wantRun {
				fh.waitForDispatch(t, 2*time.Second)
			} else {
				select {
				case <-fh.notify:
					t.Error("mention should not fire when not in the trigger set")
				case <-time.After(200 * time.Millisecond):
				}
			}
		})
	}
}

// TestHandleWebhookMentionTriggersRun pins the mention path end to end: the
// dispatch carries the task/repo, and RunEnded's finalize posts the run's
// answer as a comment.
func TestHandleWebhookMentionTriggersRun(t *testing.T) {
	posted := make(chan string, 1)
	srv := stubGitHub(t, posted)
	defer srv.Close()
	ext, fh := newTestExtension(t, srv.URL, nil)

	rec := httptest.NewRecorder()
	ext.handleWebhook(rec, signedRequest("issue_comment", issueCommentBody("@quack add a feature")))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d; want 202", rec.Code)
	}

	req := fh.waitForDispatch(t, 2*time.Second)
	if !strings.Contains(req.Ask.Message, "add a feature") || !strings.Contains(req.Ask.Message, `"name":"widgets"`) {
		t.Errorf("dispatch message missing task or repo: %q", req.Ask.Message)
	}

	chatID := globalChatID("github-acme-widgets-7")
	ext.RunEnded(chatID, sdk.RunOutcome{Status: sdk.RunDone, PlanRan: true, Answer: "done - opened PR #12"})

	select {
	case body := <-posted:
		if !strings.Contains(body, "opened PR #12") {
			t.Errorf("posted comment missing answer: %q", body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no comment posted back")
	}
}

// TestHandleWebhookNoMentionIsNoop pins that triggerTask's decision is made
// synchronously, before any dispatch goroutine is spawned.
func TestHandleWebhookNoMentionIsNoop(t *testing.T) {
	ext, fh := newTestExtension(t, "http://unused", nil)

	rec := httptest.NewRecorder()
	ext.handleWebhook(rec, signedRequest("issue_comment", issueCommentBody("just chatting, no mention")))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rec.Code)
	}
	if calls := fh.calls(); len(calls) != 0 {
		t.Error("orchestrator should not run without a mention")
	}
}

// TestHandleWebhookUnhandledEventIsNoop pins handleWebhook's default case for
// an event type this extension does not act on.
func TestHandleWebhookUnhandledEventIsNoop(t *testing.T) {
	ext, fh := newTestExtension(t, "http://unused", nil)

	rec := httptest.NewRecorder()
	ext.handleWebhook(rec, signedRequest("star", []byte(`{"action":"created"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rec.Code)
	}
	if calls := fh.calls(); len(calls) != 0 {
		t.Error("orchestrator should not run for an unhandled event")
	}
}

// TestHandleWebhookBadSignature pins that a tampered body fails HMAC
// verification before any dispatch happens.
func TestHandleWebhookBadSignature(t *testing.T) {
	ext, fh := newTestExtension(t, "http://unused", nil)

	body := issueCommentBody("@quack do it")
	req := signedRequest("issue_comment", body)
	// Tamper with the body AFTER signing so the signature no longer matches.
	req.Body = io.NopCloser(bytes.NewReader(append(body, ' ')))

	rec := httptest.NewRecorder()
	ext.handleWebhook(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d; want 401", rec.Code)
	}
	if calls := fh.calls(); len(calls) != 0 {
		t.Error("a bad signature must not trigger a run")
	}
}

// TestHandleWebhookAcksBeforeRunFinishes pins that handleWebhook returns fast
// even when Host.Dispatch is slow - the dispatch happens in a goroutine
// (`go e.dispatch(...)`), never inline on the request path.
func TestHandleWebhookAcksBeforeRunFinishes(t *testing.T) {
	ext, fh := newTestExtension(t, "http://unused", nil)
	fh.block = make(chan struct{})

	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		ext.handleWebhook(rec, signedRequest("issue_comment", issueCommentBody("@quack slow task")))
		close(done)
	}()
	select {
	case <-done:
		if rec.Code != http.StatusAccepted {
			t.Fatalf("status = %d; want 202", rec.Code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("handler blocked on the dispatch instead of acking fast")
	}
	close(fh.block) // let the blocked dispatch goroutine finish
}

// TestHandleWebhookMentionPostsEyesReaction pins the instant 👀 ack, independent of the model run.
func TestHandleWebhookMentionPostsEyesReaction(t *testing.T) {
	reacted := make(chan string, 1) // path + body of the reaction POST
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/installation"):
			fmt.Fprint(w, `{"id":5}`)
		case strings.HasSuffix(r.URL.Path, "/access_tokens"):
			fmt.Fprintf(w, `{"token":"ghs_x","expires_at":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339))
		case strings.HasSuffix(r.URL.Path, "/reactions"):
			body, _ := io.ReadAll(r.Body)
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"id":1}`)
			reacted <- r.URL.Path + " " + string(body)
		default:
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{}`)
		}
	}))
	defer srv.Close()

	ext, _ := newTestExtension(t, srv.URL, nil)

	rec := httptest.NewRecorder()
	ext.handleWebhook(rec, signedRequest("issue_comment", issueCommentBody("@quack take a look")))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d; want 202", rec.Code)
	}

	select {
	case got := <-reacted:
		if !strings.Contains(got, "/repos/acme/widgets/issues/comments/999/reactions") {
			t.Errorf("reaction hit wrong endpoint: %q", got)
		}
		if !strings.Contains(got, `"content":"eyes"`) {
			t.Errorf("reaction content should be eyes; got %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no 👀 reaction was posted on the mention")
	}
}

// TestAckReactionFailureDoesNotBlockRun pins that a failed 👀 reaction must not stop the dispatch.
func TestAckReactionFailureDoesNotBlockRun(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/installation"):
			fmt.Fprint(w, `{"id":5}`)
		case strings.HasSuffix(r.URL.Path, "/access_tokens"):
			fmt.Fprintf(w, `{"token":"ghs_x","expires_at":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339))
		case strings.HasSuffix(r.URL.Path, "/reactions"):
			http.Error(w, "boom", http.StatusInternalServerError) // reaction fails
		case isIssueMetaPath(r.URL.Path):
			fmt.Fprint(w, `{"title":"Test issue","body":"","state":"open"}`)
		default:
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, `[]`)
		}
	}))
	defer srv.Close()

	ext, fh := newTestExtension(t, srv.URL, nil)

	rec := httptest.NewRecorder()
	ext.handleWebhook(rec, signedRequest("issue_comment", issueCommentBody("@quack do it")))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d; want 202", rec.Code)
	}
	// The run must still be dispatched despite the failed reaction.
	fh.waitForDispatch(t, 2*time.Second)
}

// TestHandleWebhookBotCommentIgnored pins the bot-sender guard: quack's own
// posted comments (and any other bot's) must never trigger a run.
func TestHandleWebhookBotCommentIgnored(t *testing.T) {
	ext, fh := newTestExtension(t, "http://unused", nil)

	body := []byte(`{
		"action":"created",
		"comment":{"id":999,"body":"@quack review this","user":{"login":"quack[bot]"}},
		"issue":{"number":7},
		"repository":{"name":"widgets","owner":{"login":"acme"},"clone_url":"https://github.com/acme/widgets.git","default_branch":"main"},
		"installation":{"id":5}
	}`)
	rec := httptest.NewRecorder()
	ext.handleWebhook(rec, signedRequest("issue_comment", body))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 no-op ack", rec.Code)
	}
	if calls := fh.calls(); len(calls) != 0 {
		t.Error("a bot-authored mention must not dispatch a run")
	}
}

// TestHandleWebhookInvokerAllowlist pins issue #357: a mention only
// dispatches when its commenter is in allowed_users (case-insensitive), and
// an empty allowlist is a secure DENY-ALL default rather than allow-all.
func TestHandleWebhookInvokerAllowlist(t *testing.T) {
	tests := []struct {
		name         string
		allowedUsers []string
		invoker      string
		wantRun      bool
	}{
		{"allowed invoker dispatches", []string{"alice"}, "alice", true},
		{"allowed invoker matches case-insensitively", []string{"Alice"}, "alice", true},
		{"disallowed invoker does not dispatch", []string{"alice"}, "mallory", false},
		{"empty allowlist denies everyone", nil, "alice", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := stubGitHub(t, make(chan string, 1))
			defer srv.Close()
			ext, fh := newTestExtension(t, srv.URL, nil)
			ext.allowedUsers = make(map[string]bool, len(tt.allowedUsers))
			for _, u := range tt.allowedUsers {
				ext.allowedUsers[strings.ToLower(u)] = true
			}

			body := []byte(fmt.Sprintf(`{
				"action":"created",
				"comment":{"id":999,"body":"@quack add a feature","user":{"login":%q}},
				"issue":{"number":7},
				"repository":{"name":"widgets","owner":{"login":"acme"},"clone_url":"https://github.com/acme/widgets.git","default_branch":"main"},
				"installation":{"id":5}
			}`, tt.invoker))

			rec := httptest.NewRecorder()
			ext.handleWebhook(rec, signedRequest("issue_comment", body))
			if rec.Code != http.StatusAccepted && rec.Code != http.StatusOK {
				t.Fatalf("status = %d", rec.Code)
			}
			if tt.wantRun {
				fh.waitForDispatch(t, 2*time.Second)
			} else {
				select {
				case <-fh.notify:
					t.Errorf("%s: must not dispatch a run", tt.name)
				case <-time.After(200 * time.Millisecond):
				}
			}
		})
	}
}

// TestHandleWebhookIssueLabelRespectsAllowlist pins the issues-labeled
// (quack:plan/quack:implement) enforcement point: a sender outside
// allowed_users never dispatches, even though the label itself required repo
// write access.
func TestHandleWebhookIssueLabelRespectsAllowlist(t *testing.T) {
	ext, fh := newTestExtension(t, "http://unused", []string{"issue_plan"})

	rec := httptest.NewRecorder()
	ext.handleWebhook(rec, signedRequest("issues", issuesBody("labeled", "quack:plan", "mallory", false)))
	if rec.Code != http.StatusAccepted && rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if calls := fh.calls(); len(calls) != 0 {
		t.Error("issue-labeled sender not in allowed_users must not dispatch")
	}
}

// TestHandleWebhookAutoReviewIgnoresAllowlist pins that the synthetic
// pr_opened auto-review has no human invoker and fires regardless of
// allowed_users (including empty/deny-all) - the allowlist gates
// human-invoked triggers only.
func TestHandleWebhookAutoReviewIgnoresAllowlist(t *testing.T) {
	srv := stubGitHub(t, make(chan string, 1))
	defer srv.Close()
	ext, fh := newTestExtension(t, srv.URL, []string{"pr_opened"})
	ext.allowedUsers = map[string]bool{} // deny-all for human triggers; must not block auto-review

	rec := httptest.NewRecorder()
	ext.handleWebhook(rec, signedRequest("pull_request", pullRequestBody("opened", "")))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d; want 202", rec.Code)
	}
	fh.waitForDispatch(t, 2*time.Second)
}

// TestHandleWebhookNoAnswerFailsLoudly guards #568: a run that neither hits
// its deadline nor gets cancelled, but persists no final answer, must post an
// explicit failure - not the old "quack finished but produced no answer."
// placeholder, which read identically to a run that legitimately had nothing
// to say.
func TestHandleWebhookNoAnswerFailsLoudly(t *testing.T) {
	posted := make(chan string, 1)
	srv := stubGitHub(t, posted)
	defer srv.Close()
	ext, fh := newTestExtension(t, srv.URL, nil)

	rec := httptest.NewRecorder()
	ext.handleWebhook(rec, signedRequest("issue_comment", issueCommentBody("@quack summarize this issue")))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d; want 202", rec.Code)
	}
	fh.waitForDispatch(t, 2*time.Second)

	chatID := globalChatID("github-acme-widgets-7")
	ext.RunEnded(chatID, sdk.RunOutcome{Status: sdk.RunDone, PlanRan: true, Answer: ""})

	var body string
	select {
	case body = <-posted:
	case <-time.After(2 * time.Second):
		t.Fatal("no comment posted back")
	}

	if strings.Contains(body, "quack finished but produced no answer.") {
		t.Errorf("posted the old silent placeholder verbatim: %q", body)
	}
	if !strings.Contains(body, "Re-apply the label to retry") {
		t.Errorf("comment does not say what to do next: %q", body)
	}
	if !strings.Contains(strings.ToLower(body), "no error") {
		t.Errorf("comment does not describe what actually happened: %q", body)
	}
}

// TestHandleWebhookSubmittedReviewSkipsSummaryComment pins that when the run
// submitted a formal review (recordDelivery's reviewDelivered), the review IS
// the deliverable on the PR - finalize must NOT also post the run's text
// summary as a duplicate top-level comment.
func TestHandleWebhookSubmittedReviewSkipsSummaryComment(t *testing.T) {
	posted := make(chan string, 1)
	srv := stubGitHub(t, posted)
	defer srv.Close()
	ext, fh := newTestExtension(t, srv.URL, nil)

	rec := httptest.NewRecorder()
	ext.handleWebhook(rec, signedRequest("issue_comment", pullCommentBody("@quack review this PR")))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d; want 202", rec.Code)
	}
	fh.waitForDispatch(t, 2*time.Second)

	chatID := globalChatID("github-acme-widgets-7")
	recordDelivery(chatID, deliveryOutcome{reviewDelivered: true})
	ext.RunEnded(chatID, sdk.RunOutcome{Status: sdk.RunDone, PlanRan: true, Answer: "I reviewed it."})

	select {
	case body := <-posted:
		t.Errorf("a duplicate summary comment was posted despite a submitted review: %q", body)
	case <-time.After(300 * time.Millisecond):
	}
}

// TestHandleWebhookFailedDeliveryStillComments pins #714/#286: a FAILED
// delivery must not count as delivered, and must not fall back to the
// worker's own self-reported answer (which can claim success it never had) -
// the comment must be the actual delivery error, with the branch so the work
// is recoverable by hand.
func TestHandleWebhookFailedDeliveryStillComments(t *testing.T) {
	posted := make(chan string, 1)
	srv := stubGitHub(t, posted)
	defer srv.Close()
	ext, fh := newTestExtension(t, srv.URL, nil)

	rec := httptest.NewRecorder()
	ext.handleWebhook(rec, signedRequest("issue_comment", pullCommentBody("@quack implement this")))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d; want 202", rec.Code)
	}
	fh.waitForDispatch(t, 2*time.Second)

	chatID := globalChatID("github-acme-widgets-7")
	recordDelivery(chatID, deliveryOutcome{err: fmt.Errorf("github_pull_request: branch not pushed"), branch: "quack/issue-66"})
	ext.RunEnded(chatID, sdk.RunOutcome{Status: sdk.RunDone, PlanRan: true, Answer: "partial progress"})

	select {
	case body := <-posted:
		if strings.Contains(body, "partial progress") {
			t.Errorf("failure comment = %q; must not use the worker's own self-report", body)
		}
		if !strings.Contains(body, "branch not pushed") {
			t.Errorf("failure comment = %q; want the delivery error", body)
		}
		if !strings.Contains(body, "quack/issue-66") {
			t.Errorf("failure comment = %q; want the branch name so the work is recoverable", body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no comment posted after a FAILED delivery - the silent-death bug")
	}
}

// TestDispatchDedupNearSimultaneousVerifiesTheInflightGuard pins the inflight
// guard's full lifecycle through handleWebhook: a second trigger on the same
// session, while the first is still awaiting RunEnded, is dropped; once
// RunEnded finalizes the first, a third trigger on the same session succeeds.
func TestDispatchDedupNearSimultaneousVerifiesTheInflightGuard(t *testing.T) {
	posted := make(chan string, 2)
	srv := stubGitHub(t, posted)
	defer srv.Close()
	ext, fh := newTestExtension(t, srv.URL, nil)

	rec1 := httptest.NewRecorder()
	ext.handleWebhook(rec1, signedRequest("issue_comment", issueCommentBody("@quack first")))
	if rec1.Code != http.StatusAccepted {
		t.Fatalf("first dispatch status = %d; want 202", rec1.Code)
	}
	fh.waitForDispatch(t, 2*time.Second)

	// Second trigger on the SAME session while the first is still in flight (no
	// RunEnded yet) - must be dropped, not queued.
	rec2 := httptest.NewRecorder()
	ext.handleWebhook(rec2, signedRequest("issue_comment", issueCommentBody("@quack second")))
	if rec2.Code != http.StatusAccepted {
		t.Fatalf("second dispatch status = %d; want 202 (handler acks even if deduped)", rec2.Code)
	}
	select {
	case <-fh.notify:
		t.Fatal("second concurrent trigger on same sessionID should have been deduplicated")
	case <-time.After(200 * time.Millisecond):
	}

	// Finalize the first - releases the inflight claim.
	chatID := globalChatID("github-acme-widgets-7")
	ext.RunEnded(chatID, sdk.RunOutcome{Status: sdk.RunDone, PlanRan: true, Answer: "ok"})
	select {
	case <-posted:
	case <-time.After(2 * time.Second):
		t.Fatal("first dispatch never posted its answer")
	}

	// A third dispatch on the same sessionID must now succeed.
	rec3 := httptest.NewRecorder()
	ext.handleWebhook(rec3, signedRequest("issue_comment", issueCommentBody("@quack third")))
	if rec3.Code != http.StatusAccepted {
		t.Fatalf("third dispatch status = %d; want 202", rec3.Code)
	}
	fh.waitForDispatch(t, 2*time.Second)

	if calls := fh.calls(); len(calls) != 2 {
		t.Errorf("Host.Dispatch calls = %d, want 2 (first + third; second was deduplicated)", len(calls))
	}
}

// TestDispatchDedupDifferentSessionsAllowsConcurrent verifies that dispatches
// on DIFFERENT sessions (different issues/PRs) all proceed - the inflight
// guard only blocks duplicate sessionIDs.
func TestDispatchDedupDifferentSessionsAllowsConcurrent(t *testing.T) {
	ext, fh := newTestExtension(t, "http://unused", nil)

	for _, issueNum := range []int{7, 8} {
		body := fmt.Sprintf(`{
			"action":"created",
			"comment":{"id":999,"body":"@quack task %d","user":{"login":"alice"}},
			"issue":{"number":%d},
			"repository":{"name":"widgets","owner":{"login":"acme"},"clone_url":"https://github.com/acme/widgets.git","default_branch":"main"},
			"installation":{"id":5}
		}`, issueNum, issueNum)
		rec := httptest.NewRecorder()
		ext.handleWebhook(rec, signedRequest("issue_comment", []byte(body)))
		if rec.Code != http.StatusAccepted {
			t.Fatalf("issue #%d dispatch status = %d; want 202", issueNum, rec.Code)
		}
	}

	received := make(map[string]bool)
	for i := 0; i < 2; i++ {
		req := fh.waitForDispatch(t, 2*time.Second)
		received[req.Chat.LocalID] = true
	}
	if !received["github-acme-widgets-7"] {
		t.Error("missing sessionID for issue #7")
	}
	if !received["github-acme-widgets-8"] {
		t.Error("missing sessionID for issue #8")
	}
}

// TestDispatchPostsHITLCommentOnPause pins the HITL pause path: finalize
// posts the paused node's question as a comment, framed distinctly from a
// "produced no answer" tail.
func TestDispatchPostsHITLCommentOnPause(t *testing.T) {
	posted := make(chan string, 1)
	srv := stubGitHub(t, posted)
	defer srv.Close()
	ext, fh := newTestExtension(t, srv.URL, nil)

	rec := httptest.NewRecorder()
	ext.handleWebhook(rec, signedRequest("issue_comment", issueCommentBody("@quack research and advise")))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d; want 202", rec.Code)
	}
	fh.waitForDispatch(t, 2*time.Second)

	chatID := globalChatID("github-acme-widgets-7")
	// PlanRan true: a real paused run has already executed a plan by the time
	// it hits a HITL node - see TestDispatchSkipsNudgeOnPause for the negative.
	ext.RunEnded(chatID, sdk.RunOutcome{Status: sdk.RunNeedsInput, PlanRan: true, NodeID: "node-1", Question: "What version of Go should we target?"})

	select {
	case body := <-posted:
		if !strings.Contains(body, "quack has a question before proceeding") {
			t.Errorf("posted comment missing HITL framing: %s", body)
		}
		if !strings.Contains(body, "version of Go") {
			t.Errorf("HITL comment should carry the question: %s", body)
		}
		if strings.Contains(body, "produced no answer") {
			t.Errorf("HITL pause should NOT post the 'produced no answer' tail; got: %s", body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no HITL comment posted after the pause")
	}
}

// TestDispatchSkipsNudgeOnPause verifies that RunEnded does NOT nudge a run
// that hit a HITL pause with a plan already run - the nudge is only for runs
// that produced no plan but were otherwise complete (not paused).
func TestDispatchSkipsNudgeOnPause(t *testing.T) {
	posted := make(chan string, 1)
	srv := stubGitHub(t, posted)
	defer srv.Close()
	ext, fh := newTestExtension(t, srv.URL, []string{"issue_implement"})

	rec := httptest.NewRecorder()
	ext.handleWebhook(rec, signedRequest("issues", issuesBody("labeled", "quack:implement", "alice", false)))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d; want 202", rec.Code)
	}
	fh.waitForDispatch(t, 2*time.Second)

	chatID := globalChatID("github-acme-widgets-7")
	ext.RunEnded(chatID, sdk.RunOutcome{Status: sdk.RunNeedsInput, PlanRan: true, NodeID: "node-1", Question: "which approach?"})

	select {
	case <-posted:
	case <-time.After(2 * time.Second):
		t.Fatal("no comment posted back")
	}
	if calls := fh.calls(); len(calls) != 1 {
		t.Errorf("Host.Dispatch calls = %d, want 1 (HITL pause must not trigger the nudge)", len(calls))
	}
}
