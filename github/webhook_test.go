package github

import (
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

// fakeDispatchHost records every Host.Dispatch call this test's Extension makes.
type fakeDispatchHost struct {
	mu          sync.Mutex
	dispatches  []sdk.DispatchRequest
	dispatchErr error
}

func (f *fakeDispatchHost) dispatch(ctx context.Context, req sdk.DispatchRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dispatches = append(f.dispatches, req)
	return f.dispatchErr
}

func (f *fakeDispatchHost) calls() []sdk.DispatchRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]sdk.DispatchRequest, len(f.dispatches))
	copy(out, f.dispatches)
	return out
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

	fh := &fakeDispatchHost{}
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
