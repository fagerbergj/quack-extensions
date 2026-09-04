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
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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

	// originUpdates records every Host.UpdateChatOrigin call. originErr, keyed
	// by localID, lets a test simulate sdk.ErrUnknownChat or any other failure.
	originUpdates []originUpdate
	originErr     map[string]error

	// invalidated records every Host.InvalidateSetup call.
	invalidated []string
}

func (f *fakeDispatchHost) invalidateSetup(chatID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.invalidated = append(f.invalidated, chatID)
	return nil
}

func (f *fakeDispatchHost) invalidateCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.invalidated...)
}

type originUpdate struct {
	localID string
	origin  sdk.ChatOrigin
}

func (f *fakeDispatchHost) updateChatOrigin(localID string, origin sdk.ChatOrigin) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.originUpdates = append(f.originUpdates, originUpdate{localID, origin})
	return f.originErr[localID]
}

func (f *fakeDispatchHost) originCalls() []originUpdate {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]originUpdate, len(f.originUpdates))
	copy(out, f.originUpdates)
	return out
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
		case strings.Contains(r.URL.Path, "/check-runs"):
			fmt.Fprint(w, `{"check_runs":[]}`)
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

// pullRequestStateBody is the raw pull_request webhook JSON for closed/reopened actions.
func pullRequestStateBody(action string, merged bool) []byte {
	return []byte(fmt.Sprintf(`{
		"action":%q,
		"number":7,
		"pull_request":{"merged":%t},
		"repository":{"name":"widgets","owner":{"login":"acme"},"clone_url":"https://github.com/acme/widgets.git","default_branch":"main"},
		"installation":{"id":5},
		"sender":{"login":"alice"}
	}`, action, merged))
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
		Dispatch:         fh.dispatch,
		UpdateChatOrigin: fh.updateChatOrigin,
		InvalidateSetup:  fh.invalidateSetup,
		Log:              slog.New(slog.NewTextHandler(io.Discard, nil)),
		DataDir:          t.TempDir(),
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

// claimInflightFor puts chatID's already-stored pendingRun into the state a
// real dispatch leaves behind: a live inflight lease whose token the pendingRun
// carries, so finalize's compare-and-delete releases it.
func claimInflightFor(t *testing.T, e *Extension, chatID, sessionID string) {
	t.Helper()
	claimedAt, _, claimed := e.claimInflight(sessionID)
	if !claimed {
		t.Fatalf("claimInflight(%q) refused a free slot", sessionID)
	}
	v, ok := e.pending.Load(chatID)
	if !ok {
		t.Fatalf("no pendingRun stored for %q", chatID)
	}
	v.(*pendingRun).claimedAt = claimedAt
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

// TestDispatchTakesOverExpiredInflightClaim is #29: a run that dies without
// settling never reaches finalize's delete, so its claim outlives it. Before the
// lease that wedged the session for the life of the process - every later
// trigger, of every type, took the dedup branch and returned. A claim older than
// the lease must be taken over so the next trigger actually dispatches.
func TestDispatchTakesOverExpiredInflightClaim(t *testing.T) {
	posted := make(chan string, 1)
	srv := stubGitHub(t, posted)
	defer srv.Close()
	e, fh := newTestExtension(t, srv.URL, nil)

	sessionID := "github-acme-widgets-7"
	// The owning run never settles: its claim just sits there, aged past the lease.
	e.inflight.Store(sessionID, time.Now().Add(-e.inflightLease()-time.Minute))

	e.dispatch(issueCommentPayloadFor("acme", "widgets", 7, "alice", "@quack go", srv.URL), "go")

	if calls := fh.calls(); len(calls) != 1 {
		t.Fatalf("Dispatch calls = %d, want 1 (an expired claim must not suppress a trigger)", len(calls))
	}
	if _, live := e.inflightActive(sessionID); !live {
		t.Error("takeover left no live claim; the new run is unprotected from its own duplicates")
	}
}

// TestDispatchDedupHoldsWithinLease is the other half of #29: taking over
// EXPIRED claims must not weaken the real guard. A second trigger arriving while
// a claim is still within its lease is still dropped.
func TestDispatchDedupHoldsWithinLease(t *testing.T) {
	posted := make(chan string, 1)
	srv := stubGitHub(t, posted)
	defer srv.Close()
	e, fh := newTestExtension(t, srv.URL, nil)

	sessionID := "github-acme-widgets-7"
	// Old, but not old enough - a genuinely long-running review.
	e.inflight.Store(sessionID, time.Now().Add(-e.inflightLease()+time.Minute))

	e.dispatch(issueCommentPayloadFor("acme", "widgets", 7, "alice", "@quack go", srv.URL), "go")

	if calls := fh.calls(); len(calls) != 0 {
		t.Fatalf("Dispatch calls = %d, want 0 (a live claim must still dedup)", len(calls))
	}
}

// TestStragglerFinalizeKeepsTakeoverClaim pins the compare-and-delete in
// finalize: when a straggler from the displaced run finally settles, it must
// release its OWN claim only - deleting the takeover's claim would let a third
// trigger start a concurrent run on a session already running one.
func TestStragglerFinalizeKeepsTakeoverClaim(t *testing.T) {
	posted := make(chan string, 1)
	srv := stubGitHub(t, posted)
	defer srv.Close()
	e, _ := newTestExtension(t, srv.URL, nil)

	sessionID := "github-acme-widgets-7"
	chatID := globalChatID(sessionID)
	straggler := &pendingRun{
		sessionID: sessionID, claimedAt: time.Now().Add(-e.inflightLease() - time.Minute),
		owner: "acme", repo: "widgets", number: 7, login: "alice",
	}
	e.inflight.Store(sessionID, straggler.claimedAt)

	takeoverAt, _, claimed := e.claimInflight(sessionID)
	if !claimed {
		t.Fatal("takeover did not claim the expired slot")
	}

	e.finalize(chatID, straggler, sdk.RunOutcome{Status: sdk.RunDone, Answer: "late"})

	v, ok := e.inflight.Load(sessionID)
	if !ok {
		t.Fatal("straggler's finalize deleted the takeover's claim")
	}
	if v.(time.Time) != takeoverAt {
		t.Errorf("claim = %v, want the takeover's %v", v, takeoverAt)
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
	claimInflightFor(t, e, chatID, sessionID)

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
	claimInflightFor(t, e, chatID, sessionID)
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

// TestFinalizePostsAnswerWhenPushLeftHeadUnchanged pins #876/#880/#882: a
// ci_fix run that correctly finds nothing to fix still pushes (a no-op) and
// still records a delivery outcome - if that alone suppressed the summary,
// the run's entire analysis would post nowhere. pushedSHA equal to the PR's
// pre-run head must fall through to the answer comment.
func TestFinalizePostsAnswerWhenPushLeftHeadUnchanged(t *testing.T) {
	posted := make(chan string, 1)
	srv := stubGitHub(t, posted)
	defer srv.Close()
	e, _ := newTestExtension(t, srv.URL, nil)

	sessionID := "github-acme-widgets-7"
	chatID := globalChatID(sessionID)
	pr := &pendingRun{
		sessionID: sessionID, owner: "acme", repo: "widgets", number: 7, login: "alice", isPR: true,
		gh: githubContext{snap: Snapshot{IsPR: true, HeadSHA: "sha-before"}},
	}
	e.pending.Store(chatID, pr)
	claimInflightFor(t, e, chatID, sessionID)
	recordDelivery(chatID, deliveryOutcome{prNumber: 42, prURL: "https://github.com/acme/widgets/pull/42", pushedSHA: "sha-before"})

	e.RunEnded(chatID, sdk.RunOutcome{Status: sdk.RunDone, PlanRan: true, Answer: "nothing needed fixing - CI was flaky, re-run it"})

	select {
	case body := <-posted:
		if !strings.Contains(body, "nothing needed fixing") {
			t.Errorf("posted comment missing the run's answer: %q", body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no summary comment posted for a no-op push - the run's analysis was silently dropped")
	}
}

// TestFinalizeSkipsSummaryWhenPushActuallyMovedHead pins the complement: a
// push whose SHA differs from the PR's pre-run head is real delivered work,
// so the existing skip-the-duplicate-comment behavior must still hold.
func TestFinalizeSkipsSummaryWhenPushActuallyMovedHead(t *testing.T) {
	posted := make(chan string, 1)
	srv := stubGitHub(t, posted)
	defer srv.Close()
	e, _ := newTestExtension(t, srv.URL, nil)

	sessionID := "github-acme-widgets-7"
	chatID := globalChatID(sessionID)
	pr := &pendingRun{
		sessionID: sessionID, owner: "acme", repo: "widgets", number: 7, login: "alice", isPR: true,
		gh: githubContext{snap: Snapshot{IsPR: true, HeadSHA: "sha-before"}},
	}
	e.pending.Store(chatID, pr)
	claimInflightFor(t, e, chatID, sessionID)
	recordDelivery(chatID, deliveryOutcome{prNumber: 42, prURL: "https://github.com/acme/widgets/pull/42", pushedSHA: "sha-after-fix"})

	e.RunEnded(chatID, sdk.RunOutcome{Status: sdk.RunDone, PlanRan: true, Answer: "fixed the failing test"})

	select {
	case body := <-posted:
		t.Fatalf("unexpected summary comment posted for a push that actually moved the head: %q", body)
	case <-time.After(300 * time.Millisecond):
		// expected: no comment - the push itself is the delivered record
	}
}

// TestRunEndedCancelledPostsNoCommentAndDoesNotNudge pins the fix for the
// live incident (quack#879): a user-cancelled run's Answer is mid-thought,
// not a finished product, so finalize must post nothing - and a
// label-triggered cancelled run must not get the no-plan nudge re-dispatch
// either, since a human cancellation is not a silent no-op to retry.
func TestRunEndedCancelledPostsNoCommentAndDoesNotNudge(t *testing.T) {
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
	claimInflightFor(t, e, chatID, sessionID)

	e.RunEnded(chatID, sdk.RunOutcome{Status: sdk.RunCancelled, PlanRan: false, Answer: "halfway through drafting a reply, the user hit stop"})

	if calls := fh.calls(); len(calls) != 0 {
		t.Fatalf("Dispatch calls = %d, want 0 (a cancelled run must not be nudged)", len(calls))
	}
	select {
	case body := <-posted:
		t.Fatalf("unexpected comment posted for a cancelled run: %q", body)
	case <-time.After(300 * time.Millisecond):
		// expected: no comment
	}
	if _, ok := e.pending.Load(chatID); ok {
		t.Errorf("pending entry still present after finalize")
	}
	if _, ok := e.inflight.Load(sessionID); ok {
		t.Errorf("inflight entry still present after finalize")
	}
}

// TestRunEndedDoneStillPostsComment is the RunCancelled test's contrast case:
// an ordinary RunDone outcome must keep posting its answer as a comment,
// unchanged by the new cancelled handling.
func TestRunEndedDoneStillPostsComment(t *testing.T) {
	posted := make(chan string, 1)
	srv := stubGitHub(t, posted)
	defer srv.Close()
	e, _ := newTestExtension(t, srv.URL, nil)

	sessionID := "github-acme-widgets-7"
	chatID := globalChatID(sessionID)
	e.pending.Store(chatID, &pendingRun{
		sessionID: sessionID, owner: "acme", repo: "widgets", number: 7, login: "alice",
	})
	claimInflightFor(t, e, chatID, sessionID)

	e.RunEnded(chatID, sdk.RunOutcome{Status: sdk.RunDone, PlanRan: true, Answer: "finished the task"})

	select {
	case body := <-posted:
		if !strings.Contains(body, "finished the task") {
			t.Errorf("posted comment = %q, want it to contain the answer", body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no comment posted for a RunDone outcome")
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
				if !strings.Contains(req.Ask.Message, `<pull_request number="7">`) {
					t.Errorf("dispatch message missing the hoisted PR ask: %q", req.Ask.Message)
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

// TestChatOriginDistinguishesIssueFromPR pins #31: issue and PR numbers share
// one sequence per repo, so the same number must still be tellable apart from
// the sidebar chip alone - while Href keeps GitHub's own URL spelling.
func TestChatOriginDistinguishesIssueFromPR(t *testing.T) {
	issue := chatOrigin("acme", "widgets", false, 42, "open", sdk.SubjectOpen)
	pr := chatOrigin("acme", "widgets", true, 42, "draft", sdk.SubjectOpen)

	if issue.Label == pr.Label {
		t.Errorf("issue and PR #42 share label %q; must be distinguishable without opening either", issue.Label)
	}
	if issue.Label != "acme/widgets issue#42" || issue.Kind != "issue" {
		t.Errorf("issue: label=%q kind=%q, want acme/widgets issue#42 / issue", issue.Label, issue.Kind)
	}
	if pr.Label != "acme/widgets pr#42" || pr.Kind != "pr" {
		t.Errorf("pr: label=%q kind=%q, want acme/widgets pr#42 / pr", pr.Label, pr.Kind)
	}
	if issue.Href != "https://github.com/acme/widgets/issues/42" {
		t.Errorf("issue href = %q, want .../issues/42", issue.Href)
	}
	if pr.Href != "https://github.com/acme/widgets/pull/42" {
		t.Errorf("pr href = %q, want .../pull/42", pr.Href)
	}
	// Badge/State stay subject status only - the type lives in Kind/Label.
	if pr.Badge != "draft" || pr.State != sdk.SubjectOpen {
		t.Errorf("pr badge/state = %q/%q, want draft/open", pr.Badge, pr.State)
	}
}

// TestHandleWebhookIssueStateChangeRefreshesOrigin pins the event→badge
// mapping for a plain issue's own close/reopen - #844.
func TestHandleWebhookIssueStateChangeRefreshesOrigin(t *testing.T) {
	tests := []struct {
		action    string
		wantBadge string
		wantState sdk.SubjectState
	}{
		{"closed", "closed", sdk.SubjectClosed},
		{"reopened", "open", sdk.SubjectOpen},
	}
	for _, tt := range tests {
		t.Run(tt.action, func(t *testing.T) {
			srv := stubGitHub(t, make(chan string, 1))
			defer srv.Close()
			ext, fh := newTestExtension(t, srv.URL, nil)

			rec := httptest.NewRecorder()
			ext.handleWebhook(rec, signedRequest("issues", issuesBody(tt.action, "", "alice", false)))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
			}

			calls := fh.originCalls()
			if len(calls) != 1 {
				t.Fatalf("UpdateChatOrigin calls = %d, want 1", len(calls))
			}
			if calls[0].localID != "github-acme-widgets-7" {
				t.Errorf("localID = %q, want github-acme-widgets-7 (mirrors dispatch's sessionID)", calls[0].localID)
			}
			if calls[0].origin.Badge != tt.wantBadge {
				t.Errorf("badge = %q, want %q", calls[0].origin.Badge, tt.wantBadge)
			}
			if calls[0].origin.State != tt.wantState {
				t.Errorf("state = %q, want %q", calls[0].origin.State, tt.wantState)
			}
			// A badge-only refresh rebuilds the whole origin - the type must survive it.
			if calls[0].origin.Kind != "issue" {
				t.Errorf("kind = %q, want issue", calls[0].origin.Kind)
			}
			if calls[0].origin.Label != "acme/widgets issue#7" {
				t.Errorf("label = %q, want acme/widgets issue#7", calls[0].origin.Label)
			}
			if calls[0].origin.Href != "https://github.com/acme/widgets/issues/7" {
				t.Errorf("href = %q, want .../issues/7", calls[0].origin.Href)
			}

			// A pure state change never triggers work.
			select {
			case <-fh.notify:
				t.Error("issue close/reopen should never dispatch a run")
			case <-time.After(100 * time.Millisecond):
			}
		})
	}
}

// TestHandleWebhookPullRequestStateChangeRefreshesOrigin pins the
// merged-vs-plain-closed distinction (#844): a PR's "closed" action carries
// merged=true/false in the same payload, badge must reflect it.
func TestHandleWebhookPullRequestStateChangeRefreshesOrigin(t *testing.T) {
	tests := []struct {
		name      string
		action    string
		merged    bool
		wantBadge string
		wantState sdk.SubjectState
	}{
		{"merged", "closed", true, "merged", sdk.SubjectMerged},
		{"closed without merging", "closed", false, "closed", sdk.SubjectClosed},
		{"reopened", "reopened", false, "open", sdk.SubjectOpen},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := stubGitHub(t, make(chan string, 1))
			defer srv.Close()
			ext, fh := newTestExtension(t, srv.URL, nil)

			rec := httptest.NewRecorder()
			ext.handleWebhook(rec, signedRequest("pull_request", pullRequestStateBody(tt.action, tt.merged)))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
			}

			calls := fh.originCalls()
			if len(calls) != 1 {
				t.Fatalf("UpdateChatOrigin calls = %d, want 1", len(calls))
			}
			if calls[0].localID != "github-acme-widgets-7" {
				t.Errorf("localID = %q, want github-acme-widgets-7", calls[0].localID)
			}
			if calls[0].origin.Badge != tt.wantBadge {
				t.Errorf("badge = %q, want %q", calls[0].origin.Badge, tt.wantBadge)
			}
			if calls[0].origin.State != tt.wantState {
				t.Errorf("state = %q, want %q", calls[0].origin.State, tt.wantState)
			}
			if calls[0].origin.Kind != "pr" {
				t.Errorf("kind = %q, want pr", calls[0].origin.Kind)
			}
			if calls[0].origin.Label != "acme/widgets pr#7" {
				t.Errorf("label = %q, want acme/widgets pr#7", calls[0].origin.Label)
			}
			if calls[0].origin.Href != "https://github.com/acme/widgets/pull/7" {
				t.Errorf("href = %q, want .../pull/7", calls[0].origin.Href)
			}

			select {
			case <-fh.notify:
				t.Error("pull_request close/merge/reopen should never dispatch a run")
			case <-time.After(100 * time.Millisecond):
			}
		})
	}
}

// TestRefreshChatOriginUnknownChatSwallowedCleanly pins the no-op contract:
// sdk.ErrUnknownChat (the common case - most issues/PRs never had a chat
// dispatched) must not surface as a handler error or block the webhook ack.
func TestRefreshChatOriginUnknownChatSwallowedCleanly(t *testing.T) {
	srv := stubGitHub(t, make(chan string, 1))
	defer srv.Close()
	ext, fh := newTestExtension(t, srv.URL, nil)
	fh.originErr = map[string]error{"github-acme-widgets-7": sdk.ErrUnknownChat}

	rec := httptest.NewRecorder()
	ext.handleWebhook(rec, signedRequest("issues", issuesBody("closed", "", "alice", false)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if len(fh.originCalls()) != 1 {
		t.Fatalf("UpdateChatOrigin calls = %d, want 1 (still attempted)", len(fh.originCalls()))
	}
}

// TestRefreshChatOriginNilHostIsNoop pins that a Host with no UpdateChatOrigin
// wired (the nil-tolerant SDK contract) never panics the webhook handler.
func TestRefreshChatOriginNilHostIsNoop(t *testing.T) {
	srv := stubGitHub(t, make(chan string, 1))
	defer srv.Close()
	ext, _ := newTestExtension(t, srv.URL, nil)
	ext.host.UpdateChatOrigin = nil

	rec := httptest.NewRecorder()
	ext.handleWebhook(rec, signedRequest("issues", issuesBody("closed", "", "alice", false)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
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

	// The repo's own name is no longer inlined (#1010 dropped the raw event
	// payload from the envelope; Setup.Repo already carries it for the run) -
	// only the triggering comment's own text still is.
	req := fh.waitForDispatch(t, 2*time.Second)
	if !strings.Contains(req.Ask.Message, "add a feature") {
		t.Errorf("dispatch message missing the triggering comment's task: %q", req.Ask.Message)
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

// ---- Batch 2: deliverable classification / isWorkRequest / dispatch internals / plan-collapse / title (ported from quack's internal/github/webhook_test.go) ----

func TestBuildEnvelopeDeliverableClassification(t *testing.T) {
	ext, _ := newTestExtension(t, "http://unused", nil)
	ext.intentClassifier = &fakeIntentClassifier{verdict: "WORK"}

	var pr issueCommentPayload
	if err := json.Unmarshal(pullCommentBody("@quack review this, focusing on the auth path"), &pr); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	env := ext.buildEnvelope(context.Background(), pr, "review this, focusing on the auth path", seedGC(Snapshot{IsPR: true}, 0), nil, nil)
	if !strings.Contains(env, "<deliverable>a review with inline comments and a verdict</deliverable>") {
		t.Errorf("review-intent PR envelope missing the review deliverable:\n%s", env)
	}
	if !strings.Contains(env, `<pull_request number="7">`) {
		t.Errorf("envelope missing the hoisted pull_request ask block:\n%s", env)
	}

	// A PR request that DOES ask to change code gets the implement deliverable.
	implEnv := ext.buildEnvelope(context.Background(), pr, "fix the null dereference in the auth path and open a PR", seedGC(Snapshot{IsPR: true}, 0), nil, nil)
	if !strings.Contains(implEnv, "<deliverable>a commit addressing the requested change</deliverable>") {
		t.Errorf("implement-intent PR envelope missing the implement deliverable:\n%s", implEnv)
	}

	// A non-PR issue mention never mentions review, and hoists <issue> not <pull_request>.
	var issue issueCommentPayload
	if err := json.Unmarshal(issueCommentBody("@quack add a feature"), &issue); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	imsg := ext.buildEnvelope(context.Background(), issue, "add a feature", seedGC(Snapshot{}, 0), nil, nil)
	if strings.Contains(imsg, "a review with inline comments") {
		t.Errorf("issue envelope should not mention the review deliverable:\n%s", imsg)
	}
	if !strings.Contains(imsg, `<issue number="7">`) {
		t.Errorf("issue envelope missing the hoisted issue ask block:\n%s", imsg)
	}
}

// TestBuildEnvelopeDeliverableClassifierResolvesFindingsAddress pins #689's
// exact production failure: "please address these findings" has no delivery
// word and its impl verb isn't clause-initial, so ImplementationIntent
// misreads it as review-only. With "pull_request" granted (the real ledger's
// permission set), classifyGrantedPRDeliverable (#760) - not the regex -
// picks the deliverable, and gets this one right.
func TestBuildEnvelopeDeliverableClassifierResolvesFindingsAddress(t *testing.T) {
	ext, _ := newTestExtension(t, "http://unused", nil)
	ext.intentClassifier = &fakeIntentClassifier{grantedDeliverable: "COMMIT"}
	// PRScoped grant{PostReview,PushCommitsToPR} (no JoinPRConversation) →
	// computeGrant's PRScoped branch: postReview→"review", pushCommitsToPR→"pull_request".
	allowedKinds := []string{"review", "pull_request"}

	var pr issueCommentPayload
	if err := json.Unmarshal(pullCommentBody("@quack please address these findings make sure they are valid first"), &pr); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	env := ext.buildEnvelope(context.Background(), pr, "please address these findings make sure they are valid first", seedGC(Snapshot{IsPR: true}, 0), allowedKinds, nil)
	if !strings.Contains(env, "<deliverable>a commit addressing the requested change</deliverable>") {
		t.Errorf("a findings-address request with pull_request granted should get the commit deliverable, not a second review:\n%s", env)
	}

	// Same grant, a genuine review ask still gets the review deliverable.
	ext.intentClassifier = &fakeIntentClassifier{grantedDeliverable: "REVIEW"}
	revEnv := ext.buildEnvelope(context.Background(), pr, "take another look at the auth changes", seedGC(Snapshot{IsPR: true}, 0), allowedKinds, nil)
	if !strings.Contains(revEnv, "<deliverable>a review with inline comments and a verdict</deliverable>") {
		t.Errorf("a genuine review ask should still get the review deliverable:\n%s", revEnv)
	}
}

// TestBuildEnvelopeDeliverableBoundedBySoleGrant pins #689's case 3: when the
// grant permits only "review" (no "pull_request"), that's the deliverable
// regardless of what the message reads like - classifyGrantedPRDeliverable
// is never even consulted (mentionIsWork's pull_request gate fails first),
// so it cannot hand back an ungranted plan.
func TestBuildEnvelopeDeliverableBoundedBySoleGrant(t *testing.T) {
	ext, _ := newTestExtension(t, "http://unused", nil)
	// The original's fakeIntentClassifier also set `deliverable: "COMMIT"` for
	// classifyPRDeliverable's old REVIEW/COMMIT model prompt - that prompt is
	// gone in this port (classifyPRDeliverable is now a pure grant check, see
	// intent.go), so there is nothing left for that field to drive.
	classifier := &fakeIntentClassifier{verdict: "WORK"}
	ext.intentClassifier = classifier
	allowedKinds := []string{"review"} // PRScoped grant{PostReview: true} only, no pull_request

	var pr issueCommentPayload
	if err := json.Unmarshal(pullCommentBody("@quack please address these findings"), &pr); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	env := ext.buildEnvelope(context.Background(), pr, "please address these findings", seedGC(Snapshot{IsPR: true}, 0), allowedKinds, nil)
	if !strings.Contains(env, "<deliverable>a review with inline comments and a verdict</deliverable>") {
		t.Errorf("with only review granted, the deliverable must fall back to review even though the message asks for a fix:\n%s", env)
	}
	// One call total: isWorkRequest's WORK/CONVERSATIONAL check. classifyPRDeliverable
	// makes no model call at all in this port.
	if calls := atomic.LoadInt32(&classifier.calls); calls != 1 {
		t.Errorf("classifier called %d times, want 1 (deliverable choice is bounded to the sole grant, no model call needed)", calls)
	}
}

// TestBuildEnvelopeGrantedPRChangeRequestClassifiesAsCommit pins #760 test
// case 1: home-server#3, a quack-authored PR with "comment"+"pull_request"
// granted, got a comment naming three numbered defects with the exact
// replacement values and "Pick one and say which" - an unambiguous change
// request. classifyGrantedPRDeliverable goes straight to what the comment
// asks for, bounded by the grant.
func TestBuildEnvelopeGrantedPRChangeRequestClassifiesAsCommit(t *testing.T) {
	ext, _ := newTestExtension(t, "http://unused", nil)
	ext.intentClassifier = &fakeIntentClassifier{grantedDeliverable: "COMMIT"}
	// PRScoped grant{JoinPRConversation, PushCommitsToPR}: pushCommitsToPR→"pull_request", joinPRConversation→"comment".
	allowedKinds := []string{"pull_request", "comment"}

	task := "1. EMBEDDING_MODEL should be qwen3-embed, not text-embedding-3-small. " +
		"2. GENERATOR_MODEL should be qwen3.5-9b. 3. The volume paths are wrong. " +
		"Pick one and say which. Do not silently ship a config that indexes three repos while the plan says six."
	var pr issueCommentPayload
	if err := json.Unmarshal(pullCommentBody(task), &pr); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	env := ext.buildEnvelope(context.Background(), pr, task, seedGC(Snapshot{IsPR: true}, 0), allowedKinds, nil)
	if !strings.Contains(env, "<deliverable>a commit addressing the requested change</deliverable>") {
		t.Errorf("a numbered change request on a push-granted PR should classify as commit, not reply:\n%s", env)
	}
}

// TestBuildEnvelopeGrantedPRQuestionStaysReply pins #760 test case 2, the
// regression guard: a genuine question still gets a reply even though
// "pull_request" is granted.
func TestBuildEnvelopeGrantedPRQuestionStaysReply(t *testing.T) {
	ext, _ := newTestExtension(t, "http://unused", nil)
	ext.intentClassifier = &fakeIntentClassifier{grantedDeliverable: "REPLY"}
	allowedKinds := []string{"pull_request", "comment"}

	task := "why did you vendor this instead of pulling the image?"
	var pr issueCommentPayload
	if err := json.Unmarshal(pullCommentBody(task), &pr); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	env := ext.buildEnvelope(context.Background(), pr, task, seedGC(Snapshot{IsPR: true}, 0), allowedKinds, nil)
	if !strings.Contains(env, "<deliverable>a reply to their message") {
		t.Errorf("a genuine question on a push-granted PR must stay a reply, not regress to commit:\n%s", env)
	}
}

// TestBuildEnvelopeGrantedPRDeliverableFailsSafeToReply pins the fail-safe
// direction for #760's gate: a classifier failure here has no other signal
// to fall back on, so it must fail toward reply - never guess commit.
func TestBuildEnvelopeGrantedPRDeliverableFailsSafeToReply(t *testing.T) {
	ext, _ := newTestExtension(t, "http://unused", nil)
	ext.intentClassifier = &fakeIntentClassifier{grantedDeliverableErr: errors.New("model unavailable")}
	allowedKinds := []string{"pull_request", "comment"}

	task := "please address these findings"
	var pr issueCommentPayload
	if err := json.Unmarshal(pullCommentBody(task), &pr); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	env := ext.buildEnvelope(context.Background(), pr, task, seedGC(Snapshot{IsPR: true}, 0), allowedKinds, nil)
	if !strings.Contains(env, "<deliverable>a reply to their message") {
		t.Errorf("a classifier failure on a push-granted PR must fail safe to reply, not guess commit:\n%s", env)
	}
}

// TestBuildEnvelopeGrantedPRDeliverableIgnoresUngrantedReview pins that a
// live COMMIT/REVIEW answer never surfaces an ungranted deliverable: REVIEW
// without "review" granted degrades to reply rather than escalating to commit.
func TestBuildEnvelopeGrantedPRDeliverableIgnoresUngrantedReview(t *testing.T) {
	ext, _ := newTestExtension(t, "http://unused", nil)
	ext.intentClassifier = &fakeIntentClassifier{grantedDeliverable: "REVIEW"}
	allowedKinds := []string{"pull_request", "comment"} // no "review"

	task := "take another look at the auth changes"
	var pr issueCommentPayload
	if err := json.Unmarshal(pullCommentBody(task), &pr); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	env := ext.buildEnvelope(context.Background(), pr, task, seedGC(Snapshot{IsPR: true}, 0), allowedKinds, nil)
	if !strings.Contains(env, "<deliverable>a reply to their message") {
		t.Errorf("a review verdict without review granted must degrade to reply, not surface an ungranted review deliverable:\n%s", env)
	}
}

// TestBuildEnvelopeIssueDeliverableClassification pins #713: an issue comment
// asking for implementation gets the PR deliverable when "pull_request" is
// granted (quack:implement present), but the same comment without that grant
// stays bounded to a plain reply - the label decides what's LEGAL, the
// message decides what's ASKED.
func TestBuildEnvelopeIssueDeliverableClassification(t *testing.T) {
	ext, _ := newTestExtension(t, "http://unused", nil)
	ext.intentClassifier = &fakeIntentClassifier{issueDeliverable: "IMPLEMENT"}

	var issue issueCommentPayload
	if err := json.Unmarshal(issueCommentBody("@quack implement this and open the PR"), &issue); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	granted := []string{"pull_request"} // issue-scoped grant{OpenPR: true}
	env := ext.buildEnvelope(context.Background(), issue, "implement this and open the PR", seedGC(Snapshot{}, 0), granted, nil)
	if !strings.Contains(env, "a pull request implementing the approved plan") {
		t.Errorf("implement request with pull_request granted should get the PR deliverable:\n%s", env)
	}

	// Same message, no quack:implement label: the grant bounds it back to a comment.
	ungranted := ext.buildEnvelope(context.Background(), issue, "implement this and open the PR", seedGC(Snapshot{}, 0), nil, nil)
	if strings.Contains(ungranted, "a pull request implementing") {
		t.Errorf("implement request WITHOUT pull_request granted must not surface the PR deliverable:\n%s", ungranted)
	}
	if !strings.Contains(ungranted, "an answer to their message") {
		t.Errorf("ungranted implement request should fall back to the comment deliverable:\n%s", ungranted)
	}

	// A plain question with the label still present stays a comment - the
	// classifier, not the grant alone, decides what was actually asked.
	ext.intentClassifier = &fakeIntentClassifier{issueDeliverable: "COMMENT"}
	question := ext.buildEnvelope(context.Background(), issue, "what do you think the right approach is here?", seedGC(Snapshot{}, 0), granted, nil)
	if !strings.Contains(question, "an answer to their message") {
		t.Errorf("a plain question should stay a comment even with pull_request granted:\n%s", question)
	}
}

// TestBuildEnvelopeIssueDeliverableClassifierFailureFallsBack pins #713's
// robustness requirement: a classifier failure (error, timeout, or
// unparseable answer) must fall back to ImplementationIntent's wording
// heuristic, never straight to conversational.
func TestBuildEnvelopeIssueDeliverableClassifierFailureFallsBack(t *testing.T) {
	ext, _ := newTestExtension(t, "http://unused", nil)
	ext.intentClassifier = &fakeIntentClassifier{issueDeliverableErr: errors.New("model unavailable")}
	granted := []string{"pull_request"}

	var issue issueCommentPayload
	if err := json.Unmarshal(issueCommentBody("@quack implement this, commit it, and open a PR"), &issue); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// ImplementationIntent("implement this, commit it, and open a PR") is true
	// (implement verb + delivery word) - the classifier failure must still
	// land on the PR deliverable via the heuristic, not silently downgrade it.
	env := ext.buildEnvelope(context.Background(), issue, "implement this, commit it, and open a PR", seedGC(Snapshot{}, 0), granted, nil)
	if !strings.Contains(env, "a pull request implementing the approved plan") {
		t.Errorf("classifier failure should fall back to ImplementationIntent's reading, not conversational:\n%s", env)
	}

	// A message with no delivery wording falls back to the heuristic's negative reading too.
	plain := ext.buildEnvelope(context.Background(), issue, "what do you think?", seedGC(Snapshot{}, 0), granted, nil)
	if !strings.Contains(plain, "an answer to their message") {
		t.Errorf("classifier failure on a non-implement message should still fall back to the comment deliverable:\n%s", plain)
	}
}

// TestBuildEnvelopeSeedsFullOnFirstLoad pins the seed half of #666's session
// model: session creation seeds the whole comment thread as <comments
// count="N">, triggering comment excluded (it's already inside the <event>
// block's own comment.body).
func TestBuildEnvelopeSeedsFullOnFirstLoad(t *testing.T) {
	ext, _ := newTestExtension(t, "http://unused", nil)
	var issue issueCommentPayload
	issue.Issue.Number = 269
	issue.Issue.Title = "Evaluate mem0"
	issue.Comment.ID = 999 // the triggering comment

	snap := Snapshot{
		Body: "We should evaluate mem0 as a memory backend.",
		Comments: []snapshotComment{
			{ID: 100, User: "hegu-1", Body: "The gate should stay the authority.", CreatedAt: "t0"},
			{ID: 200, User: "quack-jason[bot]", Body: "# Implementation Plan: mem0 as a vector store", CreatedAt: "t1"},
			{ID: 999, User: "fagerbergj", Body: "rework it - mem0 is not a store", CreatedAt: "t2"},
		},
	}
	env := ext.buildEnvelope(context.Background(), issue, "rework it - mem0 is not a store", seedGC(snap, issue.Comment.ID), nil, nil)
	if !strings.Contains(env, "evaluate mem0 as a memory backend") {
		t.Errorf("envelope missing the seeded issue body:\n%s", env)
	}
	if !strings.Contains(env, `<comments count="2">`) {
		t.Errorf("envelope missing the full first-load comment seed (2, excluding the trigger):\n%s", env)
	}
	if strings.Contains(env, "hegu-1") == false || strings.Contains(env, "Implementation Plan: mem0 as a vector store") == false {
		t.Errorf("envelope missing seeded comment content:\n%s", env)
	}
	// The triggering comment is quoted once, inside the event block - not
	// duplicated into the seeded comments array too.
	if n := strings.Count(env, "rework it - mem0 is not a store"); n != 0 {
		t.Errorf("triggering comment should not appear in the seeded comments array (n=%d):\n%s", n, env)
	}
}

// TestBuildEnvelopeResumeSeedsOnlyDelta pins the resume half of #666: a
// later run seeds only what changed - new/edited/deleted comments - never
// the whole thread again.
func TestBuildEnvelopeResumeSeedsOnlyDelta(t *testing.T) {
	ext, _ := newTestExtension(t, "http://unused", nil)
	var issue issueCommentPayload
	issue.Issue.Number = 7

	old := Snapshot{Body: "desc", Comments: []snapshotComment{{ID: 1, User: "bob", Body: "first comment", CreatedAt: "t0"}}}
	cur := Snapshot{Body: "desc", Comments: []snapshotComment{
		{ID: 1, User: "bob", Body: "first comment", CreatedAt: "t0"},
		{ID: 2, User: "carol", Body: "a brand new comment", CreatedAt: "t1"},
	}}
	delta := diffSnapshots(old, cur, 0)
	env := ext.buildEnvelope(context.Background(), issue, "what's new?", githubContext{snap: cur, delta: &delta}, nil, nil)
	if !strings.Contains(env, "a brand new comment") {
		t.Errorf("resume envelope missing the new comment:\n%s", env)
	}
	if strings.Contains(env, "first comment") {
		t.Errorf("resume envelope re-injected an UNCHANGED comment (should only carry the delta):\n%s", env)
	}
	if !strings.Contains(env, `<comments new="1" edited="0" deleted="0">`) {
		t.Errorf("resume envelope missing the delta attributes:\n%s", env)
	}

	// An unchanged snapshot: the delta is empty, nothing extra is injected.
	unchanged := diffSnapshots(cur, cur, 0)
	if !unchanged.Empty() {
		t.Fatalf("diffSnapshots(cur, cur) = %+v; want an empty delta", unchanged)
	}
	noopEnv := ext.buildEnvelope(context.Background(), issue, "anything new?", githubContext{snap: cur, delta: &unchanged}, nil, nil)
	if strings.Contains(noopEnv, "a brand new comment") || strings.Contains(noopEnv, "first comment") {
		t.Errorf("an unchanged-snapshot resume should inject no comment content:\n%s", noopEnv)
	}
}

// TestBuildEnvelopeChangedFilesOnPRRuns pins the scope note: <changed_files>
// is seeded on PR runs only, with GitHub's own filename/additions/deletions
// shape (no reshaping needed - changedFile already matches pulls/{n}/files
// field-for-field).
func TestBuildEnvelopeChangedFilesOnPRRuns(t *testing.T) {
	ext, _ := newTestExtension(t, "http://unused", nil)
	ext.intentClassifier = &fakeIntentClassifier{verdict: "WORK"}
	var pr issueCommentPayload
	if err := json.Unmarshal(pullCommentBody("@quack review this"), &pr); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	snap := Snapshot{
		IsPR:  true,
		Files: []changedFile{{Filename: "a.go", Additions: 10, Deletions: 2}, {Filename: "b.go", Additions: 1}},
	}
	env := ext.buildEnvelope(context.Background(), pr, "review this", seedGC(snap, 0), nil, nil)
	if !strings.Contains(env, `<changed_files count="2" additions="11" deletions="2">`) {
		t.Errorf("envelope missing the changed_files summary attributes:\n%s", env)
	}
	if !strings.Contains(env, `"filename":"a.go"`) || !strings.Contains(env, `"additions":10`) {
		t.Errorf("envelope missing per-file churn in GitHub's own field names:\n%s", env)
	}

	var issue issueCommentPayload
	issue.Issue.Number = 7
	issueEnv := ext.buildEnvelope(context.Background(), issue, "task", seedGC(Snapshot{}, 0), nil, nil)
	if strings.Contains(issueEnv, "<changed_files") {
		t.Errorf("an issue-scoped envelope should carry no changed_files block:\n%s", issueEnv)
	}
}

// TestBuildEnvelopeIncrementalReviewScoping pins #459 §5 under the envelope:
// a resume with new commits gets the "what's new" deliverable; a resume with
// none says a full review is not owed either.
func TestBuildEnvelopeIncrementalReviewScoping(t *testing.T) {
	ext, _ := newTestExtension(t, "http://unused", nil)
	ext.intentClassifier = &fakeIntentClassifier{verdict: "WORK"}
	var pr issueCommentPayload
	if err := json.Unmarshal(pullCommentBody("@quack review this"), &pr); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// First-time review: no prior baseline, the full-review deliverable.
	first := ext.buildEnvelope(context.Background(), pr, "review this", seedGC(Snapshot{IsPR: true}, 0), nil, nil)
	if !strings.Contains(first, "<deliverable>a review with inline comments and a verdict</deliverable>") {
		t.Errorf("first-time review should get the full-review deliverable:\n%s", first)
	}

	// Resume with new commits: the incremental deliverable, naming the SHA.
	withNew := ext.buildEnvelope(context.Background(), pr, "review this", githubContext{
		snap:       Snapshot{IsPR: true},
		newCommits: []snapshotCommit{{SHA: "abc1234567", Message: "fix the bug"}},
	}, nil, nil)
	if !strings.Contains(withNew, "a review of what is new since the last one") || !strings.Contains(withNew, "abc1234") {
		t.Errorf("incremental review envelope missing the scoped deliverable naming the new commit:\n%s", withNew)
	}

	// Resume with zero new commits still reads as "scoped to what's new" (a
	// review baseline exists), not the first-time framing.
	noneNew := ext.buildEnvelope(context.Background(), pr, "review this", githubContext{snap: Snapshot{IsPR: true}, newCommits: []snapshotCommit{}}, nil, nil)
	if !strings.Contains(noneNew, "already looked at every commit") {
		t.Errorf("zero-new-commits resume should say there's nothing new, not the first-time framing:\n%s", noneNew)
	}
}

// TestBuildEnvelopeConversationalFollowup pins that a PR mention classified
// CONVERSATIONAL gets the reply deliverable, never review/implement
// language - and a genuine work request still gets the work deliverable.
func TestBuildEnvelopeConversationalFollowup(t *testing.T) {
	ext, _ := newTestExtension(t, "http://unused", nil)
	var pr issueCommentPayload
	if err := json.Unmarshal(pullCommentBody("@quack which finding matters most? No need to re-review."), &pr); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	env := ext.buildEnvelope(context.Background(), pr, "which finding matters most? No need to re-review.", seedGC(Snapshot{IsPR: true}, 0), nil, nil)
	if !strings.Contains(env, "<deliverable>a reply to their message, posted as a comment") {
		t.Errorf("conversational envelope missing the reply deliverable:\n%s", env)
	}

	// A genuine review request, classified as a work request, gets the work deliverable.
	ext.intentClassifier = &fakeIntentClassifier{verdict: "WORK"}
	rev := ext.buildEnvelope(context.Background(), pr, "please review this PR", seedGC(Snapshot{IsPR: true, HeadRef: "x"}, 0), nil, nil)
	if !strings.Contains(rev, "<deliverable>a review with inline comments and a verdict</deliverable>") {
		t.Errorf("a classified work request must still get the review deliverable:\n%s", rev)
	}
}

// TestBuildEnvelopeMentionClassifiedAsWork/Conversational pin that the
// classifier's verdict (not task wording) decides the deliverable.
func TestBuildEnvelopeMentionClassifiedAsWork(t *testing.T) {
	ext, _ := newTestExtension(t, "http://unused", nil)
	ext.intentClassifier = &fakeIntentClassifier{verdict: "WORK"}
	var pr issueCommentPayload
	if err := json.Unmarshal(pullCommentBody("@quack review this, focusing on the auth path"), &pr); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	env := ext.buildEnvelope(context.Background(), pr, "review this, focusing on the auth path", seedGC(Snapshot{IsPR: true}, 0), nil, nil)
	if strings.Contains(env, "a reply to their message") {
		t.Errorf("a mention classified WORK should not get the conversational deliverable:\n%s", env)
	}
}

func TestBuildEnvelopeMentionClassifiedAsConversational(t *testing.T) {
	ext, _ := newTestExtension(t, "http://unused", nil)
	ext.intentClassifier = &fakeIntentClassifier{verdict: "CONVERSATIONAL"}
	var pr issueCommentPayload
	if err := json.Unmarshal(pullCommentBody("@quack what did you mean by that finding?"), &pr); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	env := ext.buildEnvelope(context.Background(), pr, "what did you mean by that finding?", seedGC(Snapshot{IsPR: true}, 0), nil, nil)
	if !strings.Contains(env, "a reply to their message") {
		t.Errorf("a mention classified CONVERSATIONAL should get the reply deliverable:\n%s", env)
	}
}

// TestBuildEnvelopeLabelTriggerNeverClassifies pins rule 1: a label trigger
// is work by construction, so buildEnvelope must never call the classifier
// for it - not even to double-check.
func TestBuildEnvelopeLabelTriggerNeverClassifies(t *testing.T) {
	ext, _ := newTestExtension(t, "http://unused", nil)
	classifier := &fakeIntentClassifier{verdict: "CONVERSATIONAL"} // even a "no" verdict must not flip a label trigger
	ext.intentClassifier = classifier

	var pr issueCommentPayload
	if err := json.Unmarshal(pullCommentBody("@quack review this"), &pr); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	pr.isLabelTrigger = true
	env := ext.buildEnvelope(context.Background(), pr, autoReviewTask, seedGC(Snapshot{IsPR: true}, 0), nil, nil)
	if strings.Contains(env, "a reply to their message") {
		t.Errorf("a label-triggered PR request should never get the conversational deliverable:\n%s", env)
	}
	if calls := atomic.LoadInt32(&classifier.calls); calls != 0 {
		t.Errorf("classifier called %d times for a label trigger, want 0 (work by construction)", calls)
	}
}

// TestBuildEnvelopePartialFixOmitsClosesKeyword pins the partial-fix
// deliverable distinction: quack:partial-fix suppresses the Closes keyword
// language, read off the FRESHLY FETCHED snapshot labels (gh.snap.Labels),
// never a separately-threaded flag.
func TestBuildEnvelopePartialFixOmitsClosesKeyword(t *testing.T) {
	ext, _ := newTestExtension(t, "http://unused", nil)
	ext.labels.PartialFix = "quack:partial-fix"
	var issue issueCommentPayload
	issue.Issue.Number = 42
	issue.isLabelTrigger = true

	full := ext.buildEnvelope(context.Background(), issue, "implement it", seedGC(Snapshot{Labels: []string{"quack:implement"}}, 0), nil, nil)
	if !strings.Contains(full, "Closes #42") {
		t.Errorf("a non-partial implement envelope should ask for a Closes keyword:\n%s", full)
	}

	partial := ext.buildEnvelope(context.Background(), issue, "implement it", seedGC(Snapshot{Labels: []string{"quack:implement", "quack:partial-fix"}}, 0), nil, nil)
	if strings.Contains(partial, "Closes #42") {
		t.Errorf("a partial-fix envelope must not ask for a Closes keyword:\n%s", partial)
	}
	if !strings.Contains(partial, "partial fix") {
		t.Errorf("a partial-fix envelope should say so:\n%s", partial)
	}
}

// TestBuildEnvelopePlanOnlyDeliverable pins the plan-only deliverable and
// that the issue body appears exactly once (planTask never embeds it; only
// the hoisted <issue><description> does - #619's duplicate-body defect).
func TestBuildEnvelopePlanOnlyDeliverable(t *testing.T) {
	ext, _ := newTestExtension(t, "http://unused", nil)
	const body = "Widgets are refetched on every request."
	up := issuesPayload{}
	up.Issue.Number = 7
	up.Issue.Title = "Add widget cache"
	up.Issue.Body = body
	task := planTask(up)

	var synthetic issueCommentPayload
	synthetic.Issue.Number = 7
	synthetic.Comment.User.Login = "alice"
	synthetic.Repository.Name = "widgets"
	synthetic.Repository.Owner.Login = "acme"
	synthetic.planOnly = true
	synthetic.isLabelTrigger = true

	env := ext.buildEnvelope(context.Background(), synthetic, task, seedGC(Snapshot{Body: body}, 0), nil, nil)

	if n := strings.Count(env, body); n != 1 {
		t.Errorf("issue body appears %d times in the plan-only envelope, want exactly 1:\n%s", n, env)
	}
	if !strings.Contains(env, "PLANNING-ONLY") || !strings.Contains(env, "ANSWER TEXT is the plan") {
		t.Errorf("plan-only envelope missing the plan deliverable:\n%s", env)
	}
	for _, banned := range []string{"git_push", "github_pull_request", "create a branch"} {
		if strings.Contains(env, banned) {
			t.Errorf("plan-only envelope contains delivery instruction %q:\n%s", banned, env)
		}
	}
}

// TestIsWorkRequestTolerantOfWrappedVerdict: a small instruct model rarely
// answers with a bare word. Exact matching made "**WORK**" unparseable, which
// fails safe to conversational - so every genuine "@quack review this" would
// have quietly lost the review framing. CONVERSATIONAL must win when both
// appear, since "WORK" is a substring of neither but a hedged answer can name
// both ("not WORK, CONVERSATIONAL").
func TestIsWorkRequestTolerantOfWrappedVerdict(t *testing.T) {
	for _, tt := range []struct {
		answer string
		want   bool
	}{
		{"WORK", true},
		{"**WORK**", true},
		{"WORK.", true},
		{" work \n", true},
		{"CONVERSATIONAL", false},
		{"**CONVERSATIONAL**", false},
		{"not WORK, CONVERSATIONAL", false},
		{"I am unable to classify this", false},
	} {
		t.Run(tt.answer, func(t *testing.T) {
			ext, _ := newTestExtension(t, "http://unused", nil)
			ext.intentClassifier = &fakeIntentClassifier{verdict: tt.answer}
			if got := ext.isWorkRequest(context.Background(), issueCommentPayload{}, "@quack review this"); got != tt.want {
				t.Errorf("isWorkRequest(%q) = %v, want %v", tt.answer, got, tt.want)
			}
		})
	}
}

func TestIsWorkRequestFailsSafe(t *testing.T) {
	ext, _ := newTestExtension(t, "http://unused", nil)

	cases := []struct {
		name       string
		classifier IntentClassifier
	}{
		{"nil classifier", nil},
		{"classifier error", &fakeIntentClassifier{errAlways: errors.New("model unavailable")}},
		{"unparseable answer", &fakeIntentClassifier{verdict: "maybe?"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ext.intentClassifier = c.classifier
			if ext.isWorkRequest(context.Background(), issueCommentPayload{}, "review this PR") {
				t.Errorf("isWorkRequest = true, want false (fail safe to conversational)")
			}
		})
	}
}

// prWithReviewLabel builds a minimal payload carrying the extension's
// configured review label, for the #1172 fallback-to-review tests.
func prWithReviewLabel() issueCommentPayload {
	var p issueCommentPayload
	p.Issue.Labels = []struct {
		Name string `json:"name"`
	}{{Name: "quack-auto-review"}}
	return p
}

// fallbackNoticeServer stubs the GitHub REST calls a fallback comment needs
// (auth + POST .../comments) and records every comment body posted.
func fallbackNoticeServer(t *testing.T) (*httptest.Server, *[]string) {
	t.Helper()
	var posted []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/installation"):
			fmt.Fprint(w, `{"id":5}`)
		case strings.HasSuffix(r.URL.Path, "/access_tokens"):
			fmt.Fprintf(w, `{"token":"ghs_x","expires_at":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments"):
			body, _ := io.ReadAll(r.Body)
			posted = append(posted, string(body))
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &posted
}

// TestIsWorkRequestFallbackDefaultsToReviewOnReviewLabel is #1172 branch (a):
// a classifier failure on a PR that already carries the review label must
// default to work/review, not conversational, and must post a visible
// fallback notice rather than failing silently.
func TestIsWorkRequestFallbackDefaultsToReviewOnReviewLabel(t *testing.T) {
	srv, posted := fallbackNoticeServer(t)
	ext, _ := newTestExtension(t, srv.URL, nil)
	ext.intentClassifier = &fakeIntentClassifier{errAlways: errors.New("model unavailable")}

	p := prWithReviewLabel()
	if !ext.isWorkRequest(context.Background(), p, "what do you think?") {
		t.Error("isWorkRequest = false, want true (review label present, classifier failed)")
	}
	if len(*posted) != 1 || !strings.Contains((*posted)[0], "treating as review") {
		t.Errorf("fallback notice not posted as expected, got %v", *posted)
	}
}

// TestIsWorkRequestFallbackDefaultsToReviewOnBareReRunPhrase is #1172 branch
// (a)'s other trigger: a bare "re-review"/"review again" mention with no
// review label still must not fall back to conversational.
func TestIsWorkRequestFallbackDefaultsToReviewOnBareReRunPhrase(t *testing.T) {
	srv, posted := fallbackNoticeServer(t)
	ext, _ := newTestExtension(t, srv.URL, nil)

	for _, task := range []string{"re-review", "  review again  ", "Re-Review."} {
		t.Run(task, func(t *testing.T) {
			ext.intentClassifier = &fakeIntentClassifier{verdict: "gibberish"} // unparseable
			*posted = nil
			if !ext.isWorkRequest(context.Background(), issueCommentPayload{}, task) {
				t.Errorf("isWorkRequest(%q) = false, want true (bare re-run phrase)", task)
			}
			if len(*posted) != 1 || !strings.Contains((*posted)[0], "treating as review") {
				t.Errorf("fallback notice not posted as expected, got %v", *posted)
			}
		})
	}

	// A re-review ask with extra words is NOT bare - goes through the model, no forced fallback.
	ext.intentClassifier = &fakeIntentClassifier{verdict: "CONVERSATIONAL"}
	*posted = nil
	if ext.isWorkRequest(context.Background(), issueCommentPayload{}, "please re-review this when you get a chance") {
		t.Error("isWorkRequest = true, want false (non-bare phrasing goes through the classifier)")
	}
	if len(*posted) != 0 {
		t.Errorf("no fallback expected when the classifier answered cleanly, got %v", *posted)
	}
}

// TestIsWorkRequestFallbackPostsNoticeWithoutReviewSignal is #1172 branch (c)
// on the plain conversational path: no review label, no bare re-run phrase -
// the fallback still must be announced, not silent.
func TestIsWorkRequestFallbackPostsNoticeWithoutReviewSignal(t *testing.T) {
	srv, posted := fallbackNoticeServer(t)
	ext, _ := newTestExtension(t, srv.URL, nil)
	ext.intentClassifier = &fakeIntentClassifier{verdict: "not sure"}

	if ext.isWorkRequest(context.Background(), issueCommentPayload{}, "what do you think about this?") {
		t.Error("isWorkRequest = true, want false (no review signal present)")
	}
	if len(*posted) != 1 || !strings.Contains((*posted)[0], "treating as conversational") {
		t.Errorf("fallback notice not posted as expected, got %v", *posted)
	}
}

// sequencedIntentClassifier returns errAlways on its first N calls, then verdict.
type sequencedIntentClassifier struct {
	failCalls int32
	verdict   string
	calls     int32
}

func (s *sequencedIntentClassifier) Classify(_ context.Context, _ string) (string, error) {
	n := atomic.AddInt32(&s.calls, 1)
	if n <= s.failCalls {
		return "", errors.New("timed out")
	}
	return s.verdict, nil
}

// TestIsWorkRequestRetriesOnceBeforeFallingBack is #1172 branch (b): a
// classifier call that fails once (e.g. the 5s deadline on a cold model) gets
// one retry with the longer deadline before any fallback kicks in.
func TestIsWorkRequestRetriesOnceBeforeFallingBack(t *testing.T) {
	ext, _ := newTestExtension(t, "http://unused", nil)
	classifier := &sequencedIntentClassifier{failCalls: 1, verdict: "WORK"}
	ext.intentClassifier = classifier

	if !ext.isWorkRequest(context.Background(), issueCommentPayload{}, "review this PR") {
		t.Error("isWorkRequest = false, want true (second attempt succeeded)")
	}
	if calls := atomic.LoadInt32(&classifier.calls); calls != 2 {
		t.Errorf("classifier called %d times, want exactly 2 (one retry)", calls)
	}
}

// blockingIntentClassifier blocks until its ctx is done, then reports the
// ctx's error - simulating a classifier call that hangs past its deadline.
type blockingIntentClassifier struct{}

func (blockingIntentClassifier) Classify(ctx context.Context, _ string) (string, error) {
	<-ctx.Done()
	return "", ctx.Err()
}

// TestIsWorkRequestTimeoutFailsSafe pins the timeout bound: a classifier call
// that hangs past its deadline fails safe to conversational, not work.
func TestIsWorkRequestTimeoutFailsSafe(t *testing.T) {
	ext, _ := newTestExtension(t, "http://unused", nil)
	ext.intentClassifier = blockingIntentClassifier{}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if ext.isWorkRequest(ctx, issueCommentPayload{}, "review this PR") {
		t.Error("isWorkRequest = true on timeout, want false (fail safe to conversational)")
	}
}

// TestBuildEnvelopeQuotedCodeCorrectionNotWorkRequest is the regression test
// for the bug this classifier replaced: a naive verb regex read a method call
// quoted inside code (it.migrate(connection)) as the imperative "migrate",
// which armed the no-plan nudge and forced a whole re-review that discarded
// the reply the model had already written. A real model should call this
// CONVERSATIONAL (a correction, not an instruction); this pins that the
// deliverable follows the classifier's verdict end to end.
func TestBuildEnvelopeQuotedCodeCorrectionNotWorkRequest(t *testing.T) {
	ext, _ := newTestExtension(t, "http://unused", nil)
	classifier := &fakeIntentClassifier{verdict: "CONVERSATIONAL"}
	ext.intentClassifier = classifier

	task := "That finding was wrong - it.migrate(connection) is called during setup, not teardown."
	var pr issueCommentPayload
	if err := json.Unmarshal(pullCommentBody(task), &pr); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	env := ext.buildEnvelope(context.Background(), pr, task, seedGC(Snapshot{IsPR: true}, 0), nil, nil)
	if !strings.Contains(env, "a reply to their message") {
		t.Errorf("a correction quoting code must be conversational, not a work request:\n%s", env)
	}
	if calls := atomic.LoadInt32(&classifier.calls); calls != 1 {
		t.Errorf("classifier called %d times, want exactly 1 for a mention", calls)
	}
}

// TestDispatchFirstLoadSeedsThenResumeInjectsDelta is the end-to-end version
// of #459: dispatch #1 on a fresh session seeds the FULL context (no prior
// snapshot); a comment is added on GitHub between runs; dispatch #2 (a
// resume) injects ONLY that new comment, not the whole thread again. Uses the
// Extension's real (sqlite-backed) store so snapshot persistence itself is
// exercised, not just the in-memory diff function.
func TestDispatchFirstLoadSeedsThenResumeInjectsDelta(t *testing.T) {
	var commentsJSON atomic.Value
	commentsJSON.Store(`[{"id":1,"body":"the original comment","user":{"login":"bob"},"updated_at":"t0"}]`)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/installation"):
			fmt.Fprint(w, `{"id":5}`)
		case strings.HasSuffix(r.URL.Path, "/access_tokens"):
			fmt.Fprintf(w, `{"token":"ghs_x","expires_at":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339))
		case strings.HasSuffix(r.URL.Path, "/reactions"):
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"id":1}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments"):
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{}`)
		case strings.HasSuffix(r.URL.Path, "/comments"):
			fmt.Fprint(w, commentsJSON.Load().(string))
		case isIssueMetaPath(r.URL.Path):
			fmt.Fprint(w, `{"title":"Evaluate widgets","body":"Should we use widgets?","state":"open"}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	ext, fh := newTestExtension(t, srv.URL, nil)

	rec1 := httptest.NewRecorder()
	ext.handleWebhook(rec1, signedRequest("issue_comment", issueCommentBody("@quack what do you think?")))
	if rec1.Code != http.StatusAccepted {
		t.Fatalf("first dispatch status = %d; want 202", rec1.Code)
	}
	firstReq := fh.waitForDispatch(t, 2*time.Second)
	if !strings.Contains(firstReq.Ask.Message, "the original comment") {
		t.Errorf("first (seed) dispatch missing the existing comment:\n%s", firstReq.Ask.Message)
	}
	if !strings.Contains(firstReq.Ask.Message, "Should we use widgets?") {
		t.Errorf("first (seed) dispatch missing the issue body:\n%s", firstReq.Ask.Message)
	}
	chatID := globalChatID("github-acme-widgets-7")
	ext.RunEnded(chatID, sdk.RunOutcome{Status: sdk.RunDone, PlanRan: true, Answer: "ok"})

	// A new comment lands on GitHub between runs.
	commentsJSON.Store(`[
		{"id":1,"body":"the original comment","user":{"login":"bob"},"updated_at":"t0"},
		{"id":2,"body":"a brand-new follow-up","user":{"login":"carol"},"updated_at":"t1"}
	]`)

	rec2 := httptest.NewRecorder()
	ext.handleWebhook(rec2, signedRequest("issue_comment", issueCommentBody("@quack anything new?")))
	if rec2.Code != http.StatusAccepted {
		t.Fatalf("second dispatch status = %d; want 202", rec2.Code)
	}
	secondReq := fh.waitForDispatch(t, 2*time.Second)
	if !strings.Contains(secondReq.Ask.Message, "a brand-new follow-up") {
		t.Errorf("resume dispatch missing the new comment:\n%s", secondReq.Ask.Message)
	}
	if strings.Contains(secondReq.Ask.Message, "the original comment") {
		t.Errorf("resume dispatch re-injected the UNCHANGED comment - should carry only the delta:\n%s", secondReq.Ask.Message)
	}
}

// TestReviewBaselineDecoupledFromGeneralSnapshot is the coordinator-flagged
// fix for #459/#460: the review scope (gh.newCommits) must be keyed off the
// commits quack actually DELIVERED a review at, never off the general
// snapshot (which advances on every dispatch, review or not). Scenario:
// review delivered at [c1] -> c2 pushed -> a CONVERSATIONAL dispatch lands
// (advances the general snapshot to [c1,c2] but must NOT advance the review
// baseline) -> a review request must still see c2 as new -> once that review
// IS delivered, the baseline advances and the NEXT review sees zero new.
func TestReviewBaselineDecoupledFromGeneralSnapshot(t *testing.T) {
	// Two synthetic commits with real, distinct git patch-ids (gitPatchID
	// reads a diff from stdin - no clone needed, see snapshot.go).
	diffs := map[string]string{
		"c1": "diff --git a/f1.txt b/f1.txt\nnew file mode 100644\nindex 0000000..1111111\n--- /dev/null\n+++ b/f1.txt\n@@ -0,0 +1 @@\n+c1\n",
		"c2": "diff --git a/f2.txt b/f2.txt\nnew file mode 100644\nindex 0000000..2222222\n--- /dev/null\n+++ b/f2.txt\n@@ -0,0 +1 @@\n+c2\n",
	}
	var commitsJSON atomic.Value
	commitsJSON.Store(`[{"sha":"c1","commit":{"message":"add f1"}}]`)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/app": // botLogin, called computing this run's permission grant (#662)
			fmt.Fprint(w, `{"slug":"quack"}`)
		case strings.HasSuffix(r.URL.Path, "/installation"):
			fmt.Fprint(w, `{"id":5}`)
		case strings.HasSuffix(r.URL.Path, "/access_tokens"):
			fmt.Fprintf(w, `{"token":"ghs_x","expires_at":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339))
		case strings.HasSuffix(r.URL.Path, "/reactions"):
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"id":1}`)
		case strings.Contains(r.URL.Path, "/pulls/") && strings.HasSuffix(r.URL.Path, "/commits"):
			fmt.Fprint(w, commitsJSON.Load().(string))
		case strings.Contains(r.URL.Path, "/commits/"): // single-commit diff (Accept: v3.diff)
			parts := strings.Split(r.URL.Path, "/")
			sha := parts[len(parts)-1]
			fmt.Fprint(w, diffs[sha])
		case strings.HasSuffix(r.URL.Path, "/files"):
			fmt.Fprint(w, `[]`)
		case strings.HasSuffix(r.URL.Path, "/reviews"):
			fmt.Fprint(w, `[]`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments"):
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{}`)
		case strings.HasSuffix(r.URL.Path, "/comments"):
			fmt.Fprint(w, `[]`)
		case strings.Contains(r.URL.Path, "/pulls/"):
			fmt.Fprint(w, `{"title":"Test PR","body":"","state":"open","head":{"ref":"feature","sha":"headsha"},"base":{"ref":"main"}}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	ext, fh := newTestExtension(t, srv.URL, nil)

	run := func(task, verdict string, reviewDelivered bool) string {
		t.Helper()
		ext.intentClassifier = &fakeIntentClassifier{verdict: verdict}
		rec := httptest.NewRecorder()
		ext.handleWebhook(rec, signedRequest("issue_comment", pullCommentBody("@quack "+task)))
		if rec.Code != http.StatusAccepted {
			t.Fatalf("dispatch status = %d; want 202 (task=%q)", rec.Code, task)
		}
		req := fh.waitForDispatch(t, 2*time.Second)
		chatID := globalChatID("github-acme-widgets-7")
		if reviewDelivered {
			recordDelivery(chatID, deliveryOutcome{reviewDelivered: true})
		}
		ext.RunEnded(chatID, sdk.RunOutcome{Status: sdk.RunDone, PlanRan: true, Answer: "handled"})
		return req.Ask.Message
	}

	// 1. First review ever: full review (no baseline yet), and it DELIVERS -
	// the baseline should advance to just c1's patch-id.
	first := run("review this", "WORK", true)
	if strings.Contains(first, "Focus your review on what's NEW") || strings.Contains(first, "already looked at every commit") {
		t.Errorf("first-ever review should carry no incremental scoping language:\n%s", first)
	}

	// 2. c2 lands on the PR.
	commitsJSON.Store(`[{"sha":"c1","commit":{"message":"add f1"}},{"sha":"c2","commit":{"message":"add f2"}}]`)

	// 3. A CONVERSATIONAL dispatch (no review delivered) - this advances the
	// GENERAL snapshot (comments/commits-as-seen) but must NOT touch the
	// review baseline.
	_ = run("what do you think so far? no need to re-review", "CONVERSATIONAL", false)

	// 4. A review request now MUST still see c2 as new - if the review scope
	// had been keyed off the general snapshot (the bug), c2 would already
	// read as "seen" because step 3 advanced it.
	second := run("review this", "WORK", true)
	if !strings.Contains(second, "Focus your review on what's NEW") || !strings.Contains(second, "c2") {
		t.Errorf("review after a conversational dispatch must still scope to c2:\n%s", second)
	}
	if strings.Contains(second, "already looked at every commit") {
		t.Errorf("review under-scoped itself off the general snapshot instead of the review baseline:\n%s", second)
	}

	// 5. Step 4 DELIVERED a review covering c2 - the baseline now advances,
	// so the NEXT review sees zero new work.
	third := run("review this", "WORK", false)
	if !strings.Contains(third, "already looked at every commit") {
		t.Errorf("after the review in step 4 delivered, the next review should see zero new commits:\n%s", third)
	}
}

// TestLatestQuackVerdictReadsOwnPRReviewMarker pins #513's webhook half: an
// own-PR review submits as a real review (state COMMENTED, since GitHub
// disallows approve/request_changes on your own PR) carrying the actual
// verdict in the hidden marker - latestQuackVerdict must read that marker,
// not the state, or an own-PR approve would be misread as "comment".
func TestLatestQuackVerdictReadsOwnPRReviewMarker(t *testing.T) {
	keyPEM, _ := testKeyPEM(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/app"):
			io.WriteString(w, `{"slug":"quack"}`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/reviews"):
			io.WriteString(w, `[{"state":"COMMENTED","body":"looks good\n\n<!-- quack:delivery:review:approve -->","user":{"login":"quack[bot]"},"submitted_at":"2026-07-20T00:00:00Z"}]`)
		case strings.HasSuffix(r.URL.Path, "/comments"):
			io.WriteString(w, `[]`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	app, err := NewApp("1", keyPEM)
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	app.apiBase = srv.URL
	app.installs["acme/widgets"] = 1
	app.tokens[1] = cachedToken{token: "ghs_x", expires: time.Now().Add(time.Hour)}
	ext := &Extension{app: app}

	verdict, err := ext.latestQuackVerdict(context.Background(), "acme", "widgets", 7)
	if err != nil {
		t.Fatalf("latestQuackVerdict: %v", err)
	}
	if verdict != "approve" {
		t.Errorf("verdict = %q; want %q (from the review body marker, not its COMMENTED state)", verdict, "approve")
	}
}

// TestDispatchAttachesDeterministicGitHubSetup pins that a label-driven
// implement run's Setup is deterministic (repo clone_url, base ref from the
// repo's default branch, a quack/issue-<N> work branch) - carried directly on
// sdk.DispatchRequest.Run.Setup in this port, not stamped onto a context the
// way the old ctx-based Runner design required.
func TestDispatchAttachesDeterministicGitHubSetup(t *testing.T) {
	srv := stubGitHub(t, make(chan string, 4))
	defer srv.Close()
	ext, fh := newTestExtension(t, srv.URL, []string{"issue_implement"})

	rec := httptest.NewRecorder()
	ext.handleWebhook(rec, signedRequest("issues", issuesBody("labeled", "quack:implement", "alice", false)))
	if rec.Code != http.StatusAccepted && rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}

	req := fh.waitForDispatch(t, 2*time.Second)
	setup := req.Run.Setup
	if setup == nil {
		t.Fatal("no deterministic Setup attached to the dispatch request")
	}
	if setup.Repo != "https://github.com/acme/widgets.git" {
		t.Errorf("Repo = %q, want the repository's clone_url", setup.Repo)
	}
	if setup.BaseRef != "main" {
		t.Errorf("BaseRef = %q, want the repository's default_branch", setup.BaseRef)
	}
	if setup.WorkBranch != "quack/issue-7" {
		t.Errorf("WorkBranch = %q, want quack/issue-7 (issue #7, no PR yet)", setup.WorkBranch)
	}
}

// TestDispatchResetsSessionForLabelWorkRequest pins T4 session hygiene: a
// LABEL-driven work request (quack:implement) resets the session before
// running (Chat.ResetHistory), so a new attempt is not poisoned by a prior
// attempt's history - unlike a conversational @mention, which keeps full
// history for continuity (TestDispatchDoesNotResetSessionForMention, below).
func TestDispatchResetsSessionForLabelWorkRequest(t *testing.T) {
	posted := make(chan string, 4)
	srv := stubGitHub(t, posted)
	defer srv.Close()
	ext, fh := newTestExtension(t, srv.URL, []string{"issue_implement"})

	rec := httptest.NewRecorder()
	ext.handleWebhook(rec, signedRequest("issues", issuesBody("labeled", "quack:implement", "alice", false)))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d; want 202", rec.Code)
	}
	req := fh.waitForDispatch(t, 2*time.Second)
	if !req.Chat.ResetHistory {
		t.Error("ResetHistory = false for a label-driven work request; want true")
	}

	chatID := globalChatID("github-acme-widgets-7")
	ext.RunEnded(chatID, sdk.RunOutcome{Status: sdk.RunDone, PlanRan: true, Answer: "done"})
	select {
	case body := <-posted:
		if !strings.Contains(body, "done") {
			t.Errorf("summary comment = %q; want the run's answer", body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no summary comment posted")
	}
}

func TestDispatchDoesNotResetSessionForMention(t *testing.T) {
	posted := make(chan string, 1)
	srv := stubGitHub(t, posted)
	defer srv.Close()
	ext, fh := newTestExtension(t, srv.URL, nil)

	rec := httptest.NewRecorder()
	ext.handleWebhook(rec, signedRequest("issue_comment", issueCommentBody("@quack implement a feature")))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d; want 202", rec.Code)
	}
	req := fh.waitForDispatch(t, 2*time.Second)
	if req.Chat.ResetHistory {
		t.Error("ResetHistory = true for a conversational @mention; want false (needs continuity)")
	}

	chatID := globalChatID("github-acme-widgets-7")
	ext.RunEnded(chatID, sdk.RunOutcome{Status: sdk.RunDone, PlanRan: true, Answer: "done"})
	select {
	case <-posted:
	case <-time.After(2 * time.Second):
		t.Fatal("no comment posted back")
	}
}

// TestFetchSnapshotRequiredMetaFailureSurfacesAsUnusable pins #467's first
// guard: when the required meta call (issueMeta) fails persistently (retries
// at the HTTP layer exhausted), loadGithubContext must flag the context as
// UNAVAILABLE - not silently return an empty-but-"valid" firstLoad snapshot,
// which is indistinguishable from a legitimately empty new issue.
func TestFetchSnapshotRequiredMetaFailureSurfacesAsUnusable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/installation"):
			fmt.Fprint(w, `{"id":5}`)
		case strings.HasSuffix(r.URL.Path, "/access_tokens"):
			fmt.Fprintf(w, `{"token":"ghs_x","expires_at":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339))
		case isIssueMetaPath(r.URL.Path):
			// Persistently unavailable - outlives doJSON's own retry budget.
			http.Error(w, `{"message":"No server is currently available to service your request."}`, http.StatusServiceUnavailable)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	ext, _ := newTestExtension(t, srv.URL, nil)
	got := ext.loadGithubContext(context.Background(), "sess-1", "acme", "widgets", 7, false, 0, false)
	if !got.contextUnavailable {
		t.Error("contextUnavailable = false; want true when the required meta fetch fails")
	}
	if !got.firstLoad {
		t.Error("firstLoad = false; want true (no snapshot to diff against)")
	}
}

// TestDispatchAbortsLabelImplementWhenContextUnavailable pins #467's second
// guard: a label-triggered implement whose GitHub context could not be
// loaded (required fetch failed) must NOT dispatch "implement per the plan"
// to Host.Dispatch - it must abort with an honest comment instead.
func TestDispatchAbortsLabelImplementWhenContextUnavailable(t *testing.T) {
	posted := make(chan string, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/installation"):
			fmt.Fprint(w, `{"id":5}`)
		case strings.HasSuffix(r.URL.Path, "/access_tokens"):
			fmt.Fprintf(w, `{"token":"ghs_x","expires_at":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339))
		case strings.HasSuffix(r.URL.Path, "/reactions"):
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"id":1}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments"):
			body, _ := io.ReadAll(r.Body)
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{}`)
			posted <- string(body)
		case isIssueMetaPath(r.URL.Path):
			// The transient 503 from #467's diagnosis, persisting past the retry budget.
			http.Error(w, `{"message":"No server is currently available to service your request."}`, http.StatusServiceUnavailable)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	ext, fh := newTestExtension(t, srv.URL, []string{"issue_implement"})

	rec := httptest.NewRecorder()
	ext.handleWebhook(rec, signedRequest("issues", issuesBody("labeled", "quack:implement", "alice", false)))
	if rec.Code != http.StatusAccepted && rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}

	var abortComment string
	select {
	case abortComment = <-posted:
	case <-time.After(5 * time.Second):
		t.Fatal("no abort comment posted")
	}
	if !strings.Contains(abortComment, "not running blind") || !strings.Contains(abortComment, "Re-apply the label") {
		t.Errorf("abort comment = %q; want the don't-run-blind message", abortComment)
	}

	// Host.Dispatch must never have been called - must not implement blind.
	select {
	case <-fh.notify:
		t.Error("Host.Dispatch was called; want no dispatch when context is unavailable")
	case <-time.After(200 * time.Millisecond):
	}
	if calls := fh.calls(); len(calls) != 0 {
		t.Errorf("Host.Dispatch calls = %d; want 0 (must not implement blind)", len(calls))
	}
}

// TestDispatchCollapsesPriorPlanComment pins plan half: when a NEW plan
// is posted for an issue, any PRIOR quack plan comment (carrying the plan
// delivery marker) is minimized via GraphQL before the new one lands, so the
// thread shows the current plan, not a pile of dead attempts.
func TestDispatchCollapsesPriorPlanComment(t *testing.T) {
	posted := make(chan string, 1)
	var minimizedID string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/installation"):
			fmt.Fprint(w, `{"id":5}`)
		case strings.HasSuffix(r.URL.Path, "/access_tokens"):
			fmt.Fprintf(w, `{"token":"ghs_x","expires_at":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339))
		case strings.HasSuffix(r.URL.Path, "/app"):
			fmt.Fprint(w, `{"slug":"quack"}`)
		case strings.HasSuffix(r.URL.Path, "/reactions"):
			w.WriteHeader(http.StatusCreated) // the label-triggered 👀 ack (#252); irrelevant here
			fmt.Fprint(w, `{"id":1}`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/comments"):
			fmt.Fprint(w, `[{"id":11,"node_id":"PLAN1","body":"## Old Plan\n\n<!-- quack:delivery:plan -->","user":{"login":"quack[bot]"}}]`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments"):
			body, _ := io.ReadAll(r.Body)
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{}`)
			posted <- string(body)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/graphql"):
			data, _ := io.ReadAll(r.Body)
			var b struct {
				Variables struct {
					ID string `json:"id"`
				} `json:"variables"`
			}
			_ = json.Unmarshal(data, &b)
			minimizedID = b.Variables.ID
			fmt.Fprint(w, `{"data":{"minimizeComment":{"minimizedComment":{"isMinimized":true}}}}`)
		case isIssueMetaPath(r.URL.Path):
			fmt.Fprint(w, `{"title":"Some issue","body":"","state":"open"}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	ext, fh := newTestExtension(t, srv.URL, []string{"issue_plan"})

	rec := httptest.NewRecorder()
	ext.handleWebhook(rec, signedRequest("issues", issuesBody("labeled", "quack:plan", "alice", false)))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d; want 202", rec.Code)
	}
	fh.waitForDispatch(t, 2*time.Second)
	chatID := globalChatID("github-acme-widgets-7")
	ext.RunEnded(chatID, sdk.RunOutcome{Status: sdk.RunDone, PlanRan: true, Answer: "## New Plan\n1. do the thing"})

	select {
	case body := <-posted:
		if !strings.Contains(body, "New Plan") || !strings.Contains(body, "quack:delivery:plan") {
			t.Errorf("posted plan comment = %q; want the new plan carrying its delivery marker", body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no plan comment posted")
	}
	if minimizedID != "PLAN1" {
		t.Errorf("minimizeComment subjectId = %q; want the prior plan comment's node_id PLAN1", minimizedID)
	}
}

// TestDispatchMarksCommentTriggeredPlan pins #731 test case 1: a plan
// requested via a /quack comment (not the quack:plan label) still carries
// the plan delivery marker on its tail comment.
func TestDispatchMarksCommentTriggeredPlan(t *testing.T) {
	posted := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/installation"):
			fmt.Fprint(w, `{"id":5}`)
		case strings.HasSuffix(r.URL.Path, "/access_tokens"):
			fmt.Fprintf(w, `{"token":"ghs_x","expires_at":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339))
		case strings.HasSuffix(r.URL.Path, "/app"):
			fmt.Fprint(w, `{"slug":"quack"}`)
		case strings.HasSuffix(r.URL.Path, "/reactions"):
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"id":1}`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/comments"):
			fmt.Fprint(w, `[]`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments"):
			body, _ := io.ReadAll(r.Body)
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{}`)
			posted <- string(body)
		case isIssueMetaPath(r.URL.Path):
			fmt.Fprint(w, `{"title":"Some issue","body":"","state":"open"}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	ext, fh := newTestExtension(t, srv.URL, nil)

	rec := httptest.NewRecorder()
	ext.handleWebhook(rec, signedRequest("issue_comment", issueCommentBody("@quack plan this issue")))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d; want 202", rec.Code)
	}
	fh.waitForDispatch(t, 2*time.Second)
	chatID := globalChatID("github-acme-widgets-7")
	ext.RunEnded(chatID, sdk.RunOutcome{Status: sdk.RunDone, PlanRan: true, Answer: "## The Plan\n1. do the thing"})

	select {
	case body := <-posted:
		if !strings.Contains(body, "quack:delivery:plan") {
			t.Errorf("comment-triggered plan comment = %q; want it to carry the plan delivery marker", body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no plan comment posted")
	}
}

// TestDispatchClassifiesIssueDeliverableOnce pins the single-call property a
// review of #731 caught: deliverableText (via buildEnvelope AND
// buildWorkerAsk) and deliverableIsPlan all need classifyIssueDeliverable's
// answer for the same run. Without memoization each calls the classifier
// independently, and a live model can disagree with itself between calls -
// the envelope telling the worker to produce a plan while the tail decides
// it wasn't one and skips the marker. quack:implement is on the issue so the
// classifier is actually consulted (never bounded away for free).
func TestDispatchClassifiesIssueDeliverableOnce(t *testing.T) {
	posted := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/installation"):
			fmt.Fprint(w, `{"id":5}`)
		case strings.HasSuffix(r.URL.Path, "/access_tokens"):
			fmt.Fprintf(w, `{"token":"ghs_x","expires_at":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339))
		case strings.HasSuffix(r.URL.Path, "/app"):
			fmt.Fprint(w, `{"slug":"quack"}`)
		case strings.HasSuffix(r.URL.Path, "/reactions"):
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"id":1}`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/comments"):
			fmt.Fprint(w, `[]`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments"):
			body, _ := io.ReadAll(r.Body)
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{}`)
			posted <- string(body)
		case isIssueMetaPath(r.URL.Path):
			fmt.Fprint(w, `{"title":"Some issue","body":"","state":"open","labels":[{"name":"quack:implement"}]}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	classifier := &fakeIntentClassifier{issueDeliverable: "COMMENT"}
	ext, fh := newTestExtension(t, srv.URL, nil)
	ext.intentClassifier = classifier

	rec := httptest.NewRecorder()
	ext.handleWebhook(rec, signedRequest("issue_comment", issueCommentBody("@quack plan this issue")))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d; want 202", rec.Code)
	}
	fh.waitForDispatch(t, 2*time.Second)
	chatID := globalChatID("github-acme-widgets-7")
	ext.RunEnded(chatID, sdk.RunOutcome{Status: sdk.RunDone, PlanRan: true, Answer: "## The Plan\n1. do the thing"})

	select {
	case body := <-posted:
		if !strings.Contains(body, "quack:delivery:plan") {
			t.Errorf("plan comment = %q; want it to carry the plan delivery marker", body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no plan comment posted")
	}
	if calls := atomic.LoadInt32(&classifier.calls); calls != 1 {
		t.Errorf("issue deliverable classifier called %d times; want exactly 1 - buildEnvelope, buildWorkerAsk, and the plan-marker decision must share one answer", calls)
	}
}

// TestDispatchCollapsesPriorCommentTriggeredPlan pins #731 test case 2: two
// successive comment-triggered plan runs - the first is minimized before the
// second posts, exactly like the label-triggered case above.
func TestDispatchCollapsesPriorCommentTriggeredPlan(t *testing.T) {
	var commentsJSON atomic.Value
	commentsJSON.Store(`[]`)
	posted := make(chan string, 2)
	var minimizedID atomic.Value
	minimizedID.Store("")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/installation"):
			fmt.Fprint(w, `{"id":5}`)
		case strings.HasSuffix(r.URL.Path, "/access_tokens"):
			fmt.Fprintf(w, `{"token":"ghs_x","expires_at":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339))
		case strings.HasSuffix(r.URL.Path, "/app"):
			fmt.Fprint(w, `{"slug":"quack"}`)
		case strings.HasSuffix(r.URL.Path, "/reactions"):
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"id":1}`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/comments"):
			fmt.Fprint(w, commentsJSON.Load().(string))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments"):
			body, _ := io.ReadAll(r.Body)
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{}`)
			posted <- string(body)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/graphql"):
			data, _ := io.ReadAll(r.Body)
			var b struct {
				Variables struct {
					ID string `json:"id"`
				} `json:"variables"`
			}
			_ = json.Unmarshal(data, &b)
			minimizedID.Store(b.Variables.ID)
			fmt.Fprint(w, `{"data":{"minimizeComment":{"minimizedComment":{"isMinimized":true}}}}`)
		case isIssueMetaPath(r.URL.Path):
			fmt.Fprint(w, `{"title":"Some issue","body":"","state":"open"}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	ext, fh := newTestExtension(t, srv.URL, nil)
	chatID := globalChatID("github-acme-widgets-7")

	rec1 := httptest.NewRecorder()
	ext.handleWebhook(rec1, signedRequest("issue_comment", issueCommentBody("@quack plan this issue")))
	if rec1.Code != http.StatusAccepted {
		t.Fatalf("first dispatch status = %d; want 202", rec1.Code)
	}
	fh.waitForDispatch(t, 2*time.Second)
	ext.RunEnded(chatID, sdk.RunOutcome{Status: sdk.RunDone, PlanRan: true, Answer: "## First Plan\n1. step one"})
	select {
	case body := <-posted:
		if !strings.Contains(body, "quack:delivery:plan") {
			t.Fatalf("first plan comment = %q; want the plan delivery marker", body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no first plan comment posted")
	}

	// The first plan is now on GitHub, marker and all - the second run's collapse must find it.
	commentsJSON.Store(`[{"id":11,"node_id":"PLAN1","body":"## First Plan\n1. step one\n\n<!-- quack:delivery:plan -->","user":{"login":"quack[bot]"}}]`)

	rec2 := httptest.NewRecorder()
	ext.handleWebhook(rec2, signedRequest("issue_comment", issueCommentBody("@quack plan this again")))
	if rec2.Code != http.StatusAccepted {
		t.Fatalf("second dispatch status = %d; want 202", rec2.Code)
	}
	fh.waitForDispatch(t, 2*time.Second)
	ext.RunEnded(chatID, sdk.RunOutcome{Status: sdk.RunDone, PlanRan: true, Answer: "## Second Plan\n1. step two"})
	select {
	case body := <-posted:
		if !strings.Contains(body, "Second Plan") || !strings.Contains(body, "quack:delivery:plan") {
			t.Errorf("second plan comment = %q; want the new plan carrying its delivery marker", body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no second plan comment posted")
	}
	if got := minimizedID.Load().(string); got != "PLAN1" {
		t.Errorf("minimizeComment subjectId = %q; want the first plan comment's node_id PLAN1", got)
	}
}

// TestDispatchCollapsesCommentTriggeredPlanOnLabelReplan pins #731 test case
// 3 (mixed triggers): a comment-triggered plan, then a label-triggered
// replan - the comment-triggered predecessor must still be minimized, which
// only works because the FIRST run also carried the marker.
func TestDispatchCollapsesCommentTriggeredPlanOnLabelReplan(t *testing.T) {
	var commentsJSON atomic.Value
	commentsJSON.Store(`[]`)
	posted := make(chan string, 2)
	var minimizedID atomic.Value
	minimizedID.Store("")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/installation"):
			fmt.Fprint(w, `{"id":5}`)
		case strings.HasSuffix(r.URL.Path, "/access_tokens"):
			fmt.Fprintf(w, `{"token":"ghs_x","expires_at":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339))
		case strings.HasSuffix(r.URL.Path, "/app"):
			fmt.Fprint(w, `{"slug":"quack"}`)
		case strings.HasSuffix(r.URL.Path, "/reactions"):
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"id":1}`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/comments"):
			fmt.Fprint(w, commentsJSON.Load().(string))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments"):
			body, _ := io.ReadAll(r.Body)
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{}`)
			posted <- string(body)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/graphql"):
			data, _ := io.ReadAll(r.Body)
			var b struct {
				Variables struct {
					ID string `json:"id"`
				} `json:"variables"`
			}
			_ = json.Unmarshal(data, &b)
			minimizedID.Store(b.Variables.ID)
			fmt.Fprint(w, `{"data":{"minimizeComment":{"minimizedComment":{"isMinimized":true}}}}`)
		case isIssueMetaPath(r.URL.Path):
			fmt.Fprint(w, `{"title":"Some issue","body":"","state":"open"}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	ext, fh := newTestExtension(t, srv.URL, []string{"mention", "issue_plan"})
	chatID := globalChatID("github-acme-widgets-7")

	// First: a plain /quack comment asks for a plan.
	rec1 := httptest.NewRecorder()
	ext.handleWebhook(rec1, signedRequest("issue_comment", issueCommentBody("@quack plan this issue")))
	if rec1.Code != http.StatusAccepted {
		t.Fatalf("comment-triggered dispatch status = %d; want 202", rec1.Code)
	}
	fh.waitForDispatch(t, 2*time.Second)
	ext.RunEnded(chatID, sdk.RunOutcome{Status: sdk.RunDone, PlanRan: true, Answer: "## Comment-Triggered Plan\n1. step one"})
	select {
	case body := <-posted:
		if !strings.Contains(body, "quack:delivery:plan") {
			t.Fatalf("comment-triggered plan comment = %q; want the plan delivery marker", body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no comment-triggered plan comment posted")
	}

	commentsJSON.Store(`[{"id":11,"node_id":"PLAN1","body":"## Comment-Triggered Plan\n1. step one\n\n<!-- quack:delivery:plan -->","user":{"login":"quack[bot]"}}]`)

	// Second: a maintainer applies quack:plan to re-plan properly.
	rec2 := httptest.NewRecorder()
	ext.handleWebhook(rec2, signedRequest("issues", issuesBody("labeled", "quack:plan", "alice", false)))
	if rec2.Code != http.StatusAccepted {
		t.Fatalf("label-triggered dispatch status = %d; want 202", rec2.Code)
	}
	fh.waitForDispatch(t, 2*time.Second)
	ext.RunEnded(chatID, sdk.RunOutcome{Status: sdk.RunDone, PlanRan: true, Answer: "## Labeled Plan\n1. step two"})
	select {
	case body := <-posted:
		if !strings.Contains(body, "Labeled Plan") || !strings.Contains(body, "quack:delivery:plan") {
			t.Errorf("label-triggered plan comment = %q; want the new plan carrying its delivery marker", body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no label-triggered plan comment posted")
	}
	if got := minimizedID.Load().(string); got != "PLAN1" {
		t.Errorf("minimizeComment subjectId = %q; want the comment-triggered plan's node_id PLAN1 - the label-triggered replan must collapse it", got)
	}
}

// TestDispatchImplementRunUntouchedByPlanCollapse pins #731 test case 4: a
// non-plan deliverable's tail comment carries no plan marker and triggers no
// collapse.
func TestDispatchImplementRunUntouchedByPlanCollapse(t *testing.T) {
	posted := make(chan string, 1)
	var graphqlCalled int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/installation"):
			fmt.Fprint(w, `{"id":5}`)
		case strings.HasSuffix(r.URL.Path, "/access_tokens"):
			fmt.Fprintf(w, `{"token":"ghs_x","expires_at":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339))
		case strings.HasSuffix(r.URL.Path, "/reactions"):
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"id":1}`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/comments"):
			fmt.Fprint(w, `[]`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments"):
			body, _ := io.ReadAll(r.Body)
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{}`)
			posted <- string(body)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/graphql"):
			atomic.AddInt32(&graphqlCalled, 1)
			fmt.Fprint(w, `{"data":{"minimizeComment":{"minimizedComment":{"isMinimized":true}}}}`)
		case isIssueMetaPath(r.URL.Path):
			fmt.Fprint(w, `{"title":"Add widget cache","body":"","state":"open"}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	ext, fh := newTestExtension(t, srv.URL, []string{"issue_implement"})

	rec := httptest.NewRecorder()
	ext.handleWebhook(rec, signedRequest("issues", issuesBody("labeled", "quack:implement", "alice", false)))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d; want 202", rec.Code)
	}
	fh.waitForDispatch(t, 2*time.Second)
	chatID := globalChatID("github-acme-widgets-7")
	ext.RunEnded(chatID, sdk.RunOutcome{Status: sdk.RunDone, PlanRan: true, Answer: "implemented the change"})

	select {
	case body := <-posted:
		if strings.Contains(body, "quack:delivery:plan") {
			t.Errorf("implement run's tail comment = %q; must not carry the plan marker", body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no tail comment posted")
	}
	if got := atomic.LoadInt32(&graphqlCalled); got != 0 {
		t.Errorf("graphql (minimizeComment) called %d times; want 0 - an implement run must never trigger plan collapse", got)
	}
}

// TestDispatchPostsPlanWhenCollapseFails pins #731 test case 5: collapse
// stays best-effort - a GraphQL minimizeComment failure must not block or
// fail the new plan's delivery.
func TestDispatchPostsPlanWhenCollapseFails(t *testing.T) {
	posted := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/installation"):
			fmt.Fprint(w, `{"id":5}`)
		case strings.HasSuffix(r.URL.Path, "/access_tokens"):
			fmt.Fprintf(w, `{"token":"ghs_x","expires_at":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339))
		case strings.HasSuffix(r.URL.Path, "/app"):
			fmt.Fprint(w, `{"slug":"quack"}`)
		case strings.HasSuffix(r.URL.Path, "/reactions"):
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"id":1}`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/comments"):
			fmt.Fprint(w, `[{"id":11,"node_id":"PLAN1","body":"## Old Plan\n\n<!-- quack:delivery:plan -->","user":{"login":"quack[bot]"}}]`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments"):
			body, _ := io.ReadAll(r.Body)
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{}`)
			posted <- string(body)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/graphql"):
			http.Error(w, `{"errors":[{"message":"internal error"}]}`, http.StatusInternalServerError)
		case isIssueMetaPath(r.URL.Path):
			fmt.Fprint(w, `{"title":"Some issue","body":"","state":"open"}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	ext, fh := newTestExtension(t, srv.URL, nil)

	rec := httptest.NewRecorder()
	ext.handleWebhook(rec, signedRequest("issue_comment", issueCommentBody("@quack plan this issue")))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d; want 202", rec.Code)
	}
	fh.waitForDispatch(t, 2*time.Second)
	chatID := globalChatID("github-acme-widgets-7")
	ext.RunEnded(chatID, sdk.RunOutcome{Status: sdk.RunDone, PlanRan: true, Answer: "## New Plan\n1. do the thing"})

	select {
	case body := <-posted:
		if !strings.Contains(body, "New Plan") || !strings.Contains(body, "quack:delivery:plan") {
			t.Errorf("plan comment = %q; want it posted with its marker despite the collapse failure", body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no plan comment posted - a collapse failure must not block delivery")
	}
}

// TestPlanTaskNoIssueBodyDuplicate pins #619 defect 2: planTask must not
// embed the issue body - the envelope's #459 context block already carries
// it verbatim, so planTask's own copy is a straight duplicate of the same
// text in the same prompt.
func TestPlanTaskNoIssueBodyDuplicate(t *testing.T) {
	var p issuesPayload
	p.Issue.Number = 7
	p.Issue.Title = "Add widget cache"
	p.Issue.Body = "Widgets are refetched on every request."
	msg := planTask(p)
	if !strings.Contains(msg, "Add widget cache") {
		t.Errorf("planTask missing the issue title:\n%s", msg)
	}
	if strings.Contains(msg, p.Issue.Body) {
		t.Errorf("planTask embeds the issue body itself - the envelope's context block already carries it:\n%s", msg)
	}
}

// TestImplementTaskCore pins implementTask's own contribution - the issue
// number/title and the delivery instructions. The discussion (the approved
// plan) is no longer implementTask's job: it arrives via dispatch's unified
// loadGithubContext, the same path every other trigger uses (#459).
func TestImplementTaskCore(t *testing.T) {
	var p issuesPayload
	p.Issue.Number = 7
	p.Issue.Title = "Add widget cache"
	p.Issue.Body = "Widgets are refetched on every request."
	msg := implementTask(p, nil, "quack:partial-fix")
	for _, want := range []string{"Implement issue #7", "Add widget cache", "Closes #7", "stage_pr", "Never merge"} {
		if !strings.Contains(msg, want) {
			t.Errorf("implementTask missing %q:\n%s", want, msg)
		}
	}

	// A CUSTOM configured partial-fix label is what's honoured - not a hardcoded
	// default (the blocking finding on #505).
	custom := implementTask(p, []string{"bug", "my-org:incomplete"}, "my-org:incomplete")
	if strings.Contains(custom, "`Closes #7`") {
		t.Errorf("custom partial-fix label ignored - Closes still present:\n%s", custom)
	}
	// The default string must NOT trigger partial-fix when a custom label is configured.
	notCustom := implementTask(p, []string{"quack:partial-fix"}, "my-org:incomplete")
	if !strings.Contains(notCustom, "`Closes #7`") {
		t.Errorf("non-matching label wrongly suppressed Closes:\n%s", notCustom)
	}

	// Partial-fix: should NOT instruct a Closes keyword.
	partialMsg := implementTask(p, []string{"bug", "quack:partial-fix"}, "quack:partial-fix")
	for _, absent := range []string{"`Closes #7`"} {
		if strings.Contains(partialMsg, absent) {
			t.Errorf("partial-fix task must not instruct closing with the keyword %q:\n%s", absent, partialMsg)
		}
	}
	for _, want := range []string{"part" + "ial fix", "Do NOT use a Closes keyword", "stage_pr"} {
		if !strings.Contains(partialMsg, want) {
			t.Errorf("partial-fix task missing %q:\n%s", want, partialMsg)
		}
	}
}

// mentionCommentBody is issueCommentBody with the issue's title present, as a
// real issue_comment payload carries it - used to pin #380's title backfill.
func mentionCommentBody(commentBody, issueTitle string) []byte {
	return []byte(fmt.Sprintf(`{
		"action":"created",
		"comment":{"id":999,"body":%q,"user":{"login":"alice"}},
		"issue":{"number":7,"title":%q},
		"repository":{"name":"widgets","owner":{"login":"acme"},"clone_url":"https://github.com/acme/widgets.git","default_branch":"main"},
		"installation":{"id":5}
	}`, commentBody, issueTitle))
}

// TestDispatchGeneratesTitle pins #380: a GitHub-webhook-dispatched chat gets
// a real, non-placeholder title derived from the triggering issue - carried
// directly on sdk.DispatchRequest.Chat.Title in this port (applied by the
// host only if the chat has no title yet, per ChatRef.Title's own contract),
// rather than this extension calling UpdateTitle against its own chat store.
func TestDispatchGeneratesTitle(t *testing.T) {
	srv := stubGitHub(t, make(chan string, 1))
	defer srv.Close()
	ext, fh := newTestExtension(t, srv.URL, nil)

	rec := httptest.NewRecorder()
	ext.handleWebhook(rec, signedRequest("issue_comment", mentionCommentBody("@quack review this", "Widgets leak memory")))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d; want 202", rec.Code)
	}

	req := fh.waitForDispatch(t, 2*time.Second)
	if req.Chat.Title == "" || req.Chat.Title == "New chat" {
		t.Errorf("Title = %q; want a real title derived from the issue", req.Chat.Title)
	}
	if req.Chat.Title != "Widgets leak memory" {
		t.Errorf("Title = %q; want the issue title", req.Chat.Title)
	}
}

// TestDispatchTitleFromLabelDrivenIssue pins #380 for the label-driven path
// (quack:plan/quack:implement), which synthesizes its issueCommentPayload from
// an issuesPayload rather than a real webhook comment.
func TestDispatchTitleFromLabelDrivenIssue(t *testing.T) {
	srv := stubGitHub(t, make(chan string, 1))
	defer srv.Close()
	ext, fh := newTestExtension(t, srv.URL, []string{"issue_plan"})

	rec := httptest.NewRecorder()
	ext.handleWebhook(rec, signedRequest("issues", issuesBody("labeled", "quack:plan", "alice", false)))
	if rec.Code != http.StatusAccepted && rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}

	req := fh.waitForDispatch(t, 2*time.Second)
	if req.Chat.Title != "Add widget cache" {
		t.Errorf("Title = %q; want the issue title from issuesBody", req.Chat.Title)
	}
}

// The original's TestDispatchDoesNotOverwriteExistingTitle pinned that a
// SECOND dispatch on an already-titled session preserves the title a prior
// dispatch set - enforced back then by this extension's own chat-store
// UpdateTitle guard. In this port, "applied only if the chat has no title
// yet" is now sdk.ChatRef.Title's own documented contract (sdk/sdk.go),
// enforced by the host quack-core dispatches to - this extension always
// sends the freshly computed title on every dispatch and has no seam left to
// verify a not-overwritten title from; genuinely inapplicable here.

// The original's TestKilledRunPreservesWatermarkDelta pinned that a run
// killed mid-flight via ext.hub.CancelRun (this extension's own Runner-driven
// run registry) must not have persisted the snapshot it fetched. That hub -
// and this extension owning run lifecycle/cancellation at all - is gone in
// this port: Host.Dispatch is fire-and-forget, and RunEnded only ever fires
// once, with a terminal outcome, for a run the host decided was done.
// persistGithubSnapshot only ever runs inside finalize (run.go), reached via
// RunEnded - so a run that is killed before RunEnded fires trivially never
// advances the watermark, by construction; there is no "started but the
// extension must specifically avoid treating it as complete" scenario left
// to construct at this seam. Not ported.

// ---- merge-label / issue-label / request-changes batch (ported from quack's internal/github/webhook_test.go) ----

// newTestExtensionWithStore is newTestExtension but reuses an existing
// *ghStore instead of opening a fresh one - lets a test simulate a process
// restart (a new Extension over the SAME durable state).
func newTestExtensionWithStore(t *testing.T, apiBase string, triggers []string, st *ghStore) (*Extension, *fakeDispatchHost) {
	t.Helper()
	e, fh := newTestExtension(t, apiBase, triggers)
	e.store = st
	return e, fh
}

// mergeStub serves the REST endpoints mergeIfApproved/tryMergeStandingIntent
// touch: reviewsJSON seeds GET .../reviews, commentsJSON seeds GET
// .../comments (own-PR verdict-marker comments), merged fires on the PUT
// .../merge.
func mergeStub(t *testing.T, reviewsJSON, commentsJSON string, posted chan<- string, merged chan<- struct{}) *httptest.Server {
	t.Helper()
	if commentsJSON == "" {
		commentsJSON = "[]"
	}
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
		case strings.HasSuffix(r.URL.Path, "/reviews"):
			fmt.Fprint(w, reviewsJSON)
		case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/merge"):
			merged <- struct{}{}
			fmt.Fprint(w, `{"merged":true}`)
		case strings.HasSuffix(r.URL.Path, "/files"):
			fmt.Fprint(w, `[]`)
		case strings.HasSuffix(r.URL.Path, "/commits"):
			fmt.Fprint(w, `[]`)
		case strings.Contains(r.URL.Path, "/check-runs"):
			fmt.Fprint(w, `{"check_runs":[]}`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/comments"):
			fmt.Fprint(w, commentsJSON)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments"):
			body, _ := io.ReadAll(r.Body)
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{}`)
			posted <- string(body)
		case strings.Contains(r.URL.Path, "/pulls/"):
			fmt.Fprint(w, `{"title":"Test PR","body":"A test PR.","state":"open","head":{"ref":"feature-branch","sha":"headsha1"},"base":{"ref":"main"}}`)
		case isIssueMetaPath(r.URL.Path):
			fmt.Fprint(w, `{"title":"Test issue","body":"A test issue.","state":"open"}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
}

// mergeStubDynamic is mergeStub with reviews served from a mutable value (an
// empty review list to start) so a test can simulate a review landing
// mid-dispatch via the returned setReviews. merged carries the PUT
// .../merge request body so a test can assert the head-sha merge guard.
func mergeStubDynamic(t *testing.T, posted chan<- string, merged chan<- string) (srv *httptest.Server, setReviews func(string)) {
	t.Helper()
	var reviews atomic.Value
	reviews.Store("[]")
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
		case strings.HasSuffix(r.URL.Path, "/reviews"):
			fmt.Fprint(w, reviews.Load().(string))
		case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/merge"):
			body, _ := io.ReadAll(r.Body)
			merged <- string(body)
			fmt.Fprint(w, `{"merged":true}`)
		case strings.HasSuffix(r.URL.Path, "/files"):
			fmt.Fprint(w, `[]`)
		case strings.HasSuffix(r.URL.Path, "/commits"):
			fmt.Fprint(w, `[]`)
		case strings.Contains(r.URL.Path, "/check-runs"):
			fmt.Fprint(w, `{"check_runs":[]}`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/comments"):
			fmt.Fprint(w, `[]`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments"):
			body, _ := io.ReadAll(r.Body)
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{}`)
			posted <- string(body)
		case strings.Contains(r.URL.Path, "/pulls/"):
			fmt.Fprint(w, `{"title":"Test PR","body":"A test PR.","state":"open","head":{"ref":"feature-branch","sha":"headsha1"},"base":{"ref":"main"}}`)
		case isIssueMetaPath(r.URL.Path):
			fmt.Fprint(w, `{"title":"Test issue","body":"A test issue.","state":"open"}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	return srv, func(j string) { reviews.Store(j) }
}

// TestMergeFailureCommentHumanizesRequiredCheckFailure pins the #876/#880/#882
// incident: mergePR's error wraps GitHub's real 405 body verbatim
// ("PUT /repos/.../merge: status 405: {...}") - the comment must name the
// failing check and never leak the raw JSON, and must say what actually
// happens next rather than claim an auto-apply the merge flow never does.
func TestMergeFailureCommentHumanizesRequiredCheckFailure(t *testing.T) {
	// The real 405 body GitHub returned on quack PR #880.
	err := fmt.Errorf(`github: PUT /repos/acme/widgets/pulls/880/merge: status 405: {"message":"Required status check \"go-test\" is failing.","documentation_url":"https://docs.github.com/rest/pulls/pulls#merge-a-pull-request"}`)

	got := mergeFailureComment(err, "quack:fix")
	want := "Merge blocked: required check **go-test** is failing on this PR. Apply the `quack:fix` label to trigger a self-heal, or push a fix yourself - re-review will follow once it pushes."
	if got != want {
		t.Errorf("mergeFailureComment(...) = %q, want %q", got, want)
	}
	if strings.Contains(got, "{") || strings.Contains(got, "documentation_url") {
		t.Errorf("humanized comment leaked the raw JSON body: %q", got)
	}
}

// TestMergeFailureCommentConciseForOtherErrors pins the fallback: any error
// that isn't the named-required-check 405 collapses to a single concise line
// with the raw JSON dropped, never the wrapped-error dump.
func TestMergeFailureCommentConciseForOtherErrors(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "other 405 message, JSON dropped",
			err:  fmt.Errorf(`github: PUT /repos/acme/widgets/pulls/7/merge: status 405: {"message":"Base branch was modified. Review and try the merge again.","documentation_url":"https://docs.github.com/x"}`),
			want: "Merge failed: Base branch was modified. Review and try the merge again.",
		},
		{
			name: "409 conflict, JSON dropped",
			err:  fmt.Errorf(`github: PUT /repos/acme/widgets/pulls/7/merge: status 409: {"message":"Head branch was modified. Review and try the merge again."}`),
			want: "Merge failed: Head branch was modified. Review and try the merge again.",
		},
		{
			name: "no JSON body at all - raw error text kept as-is",
			err:  fmt.Errorf("github: PUT %s: context deadline exceeded", "/repos/acme/widgets/pulls/7/merge"),
			want: "Merge failed: github: PUT /repos/acme/widgets/pulls/7/merge: context deadline exceeded",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := mergeFailureComment(tt.err, "quack:fix"); got != tt.want {
				t.Errorf("mergeFailureComment(...) = %q, want %q", got, tt.want)
			}
		})
	}
}

func mergeLabelBody(sender string) []byte {
	return []byte(fmt.Sprintf(`{
		"action":"labeled",
		"number":7,
		"label":{"name":"quack:merge"},
		"repository":{"name":"widgets","owner":{"login":"acme"},"clone_url":"https://github.com/acme/widgets.git","default_branch":"main"},
		"installation":{"id":5},
		"sender":{"login":%q}
	}`, sender))
}

// TestHandleWebhookMergeLabel covers the cases where the merge label's fate is
// decided WITHOUT needing to dispatch a run: an approving review already
// exists (merges immediately, unchanged), a non-approving verdict already
// exists (refuses and leaves the standing intent recorded so a later approval
// can still merge it), the trigger is off, or the sender is a bot. The "no
// review at all yet" case dispatches a review run and is covered separately
// (TestHandleWebhookMergeLabelQueuesAndDispatchesReview).
func TestHandleWebhookMergeLabel(t *testing.T) {
	approved := `[{"state":"CHANGES_REQUESTED","user":{"login":"quack[bot]"}},{"state":"APPROVED","user":{"login":"quack[bot]"}}]`
	tests := []struct {
		name        string
		triggers    []string
		reviews     string
		comments    string
		sender      string
		wantMerge   bool
		wantComment string
		wantIntent  bool
	}{
		{"approved review merges", []string{"merge"}, approved, "", "alice", true, "Merged", false},
		{"changes-requested stands by with the intent recorded", []string{"merge"},
			`[{"state":"APPROVED","user":{"login":"quack[bot]"}},{"state":"CHANGES_REQUESTED","user":{"login":"quack[bot]"}}]`,
			"", "alice", false, "Standing by: my latest review is request_changes, not an approval", true},
		{"COMMENTED carries no verdict but still stands by without a later approve", []string{"merge"},
			`[{"state":"APPROVED","user":{"login":"quack[bot]"},"submitted_at":"2026-01-01T00:00:00Z"},{"state":"COMMENTED","user":{"login":"quack[bot]"},"submitted_at":"2026-01-02T00:00:00Z"}]`,
			"", "alice", false, "Standing by: my latest review is comment, not an approval", true},
		{"own-PR comment-review marker approves and merges", []string{"merge"}, `[]`,
			`[{"user":{"login":"quack[bot]"},"body":"LGTM\n\n<!-- quack:delivery:review:approve -->","created_at":"2026-01-01T00:00:00Z"}]`,
			"alice", true, "Merged", false},
		{"own-PR comment-review marker request_changes stands by", []string{"merge"}, `[]`,
			`[{"user":{"login":"quack[bot]"},"body":"needs work\n\n<!-- quack:delivery:review:request_changes -->","created_at":"2026-01-01T00:00:00Z"}]`,
			"alice", false, "Standing by: my latest review is request_changes, not an approval", true},
		{"trigger not enabled is a no-op", []string{"mention"}, approved, "", "alice", false, "", false},
		{"bot sender cannot authorize", []string{"merge"}, approved, "", "other[bot]", false, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			posted := make(chan string, 2)
			merged := make(chan struct{}, 1)
			srv := mergeStub(t, tt.reviews, tt.comments, posted, merged)
			defer srv.Close()
			ext, fh := newTestExtension(t, srv.URL, tt.triggers)

			rec := httptest.NewRecorder()
			ext.handleWebhook(rec, signedRequest("pull_request", mergeLabelBody(tt.sender)))
			if rec.Code != http.StatusAccepted && rec.Code != http.StatusOK {
				t.Fatalf("status = %d", rec.Code)
			}

			if tt.wantComment != "" {
				select {
				case c := <-posted:
					if !strings.Contains(c, tt.wantComment) {
						t.Errorf("comment = %q, want substring %q", c, tt.wantComment)
					}
				case <-time.After(2 * time.Second):
					t.Fatal("expected an outcome comment")
				}
			}
			if tt.wantMerge {
				select {
				case <-merged:
				case <-time.After(2 * time.Second):
					t.Fatal("expected a merge PUT")
				}
			} else {
				select {
				case <-merged:
					t.Error("merge must not have been called")
				default:
				}
			}
			// None of these cases dispatches an orchestrator run - either the
			// verdict was already decided, or the trigger/sender gate refused first.
			select {
			case <-fh.notify:
				t.Error("this case must not dispatch an orchestrator run")
			default:
			}

			intent, err := ext.store.GetMergeIntent(context.Background(), globalChatID("github-acme-widgets-7"))
			if err != nil {
				t.Fatalf("GetMergeIntent: %v", err)
			}
			if tt.wantIntent && (intent == nil || intent.RequestedBy != "alice") {
				t.Errorf("intent = %+v; want a recorded standing intent for alice", intent)
			}
			if !tt.wantIntent && intent != nil {
				t.Errorf("intent = %+v; want none recorded", intent)
			}
		})
	}
}

// TestHandleWebhookMergeLabelQueuesAndDispatchesReview covers applying
// quack:merge to a PR quack has never looked at: the label becomes a standing
// intent AND dispatches a review itself - otherwise the label would silently
// do nothing until someone separately asked for a review.
func TestHandleWebhookMergeLabelQueuesAndDispatchesReview(t *testing.T) {
	posted := make(chan string, 4)
	merged := make(chan struct{}, 1)
	srv := mergeStub(t, "[]", "", posted, merged)
	defer srv.Close()
	ext, fh := newTestExtension(t, srv.URL, []string{"merge"})

	rec := httptest.NewRecorder()
	ext.handleWebhook(rec, signedRequest("pull_request", mergeLabelBody("alice")))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d", rec.Code)
	}

	select {
	case c := <-posted:
		if !strings.Contains(c, "Queued") || !strings.Contains(c, "Reviewing it now") {
			t.Errorf("comment = %q; want a queued+reviewing message", c)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no queued comment posted")
	}
	req := fh.waitForDispatch(t, 2*time.Second)
	if !strings.Contains(req.Ask.Message, "<deliverable>a review with inline comments and a verdict</deliverable>") {
		t.Errorf("dispatched envelope = %q; want the auto-review deliverable", req.Ask.Message)
	}

	intent, err := ext.store.GetMergeIntent(context.Background(), globalChatID("github-acme-widgets-7"))
	if err != nil || intent == nil || intent.RequestedBy != "alice" {
		t.Fatalf("GetMergeIntent = %+v, %v; want a recorded intent for alice", intent, err)
	}
}

// TestHandleWebhookMergeLabelWaitsForInFlightReview covers applying
// quack:merge while a review is ALREADY running on the PR (a common race: the
// label lands while a review dispatched moments earlier is still in
// progress) - it must record the intent and wait, never dispatch a SECOND
// concurrent review on the same session. In this port a dispatched run stays
// "in flight" (inflight map held) until its RunEnded arrives - never called
// here - so the label lands while the first is still open, no blocking
// channel needed.
func TestHandleWebhookMergeLabelWaitsForInFlightReview(t *testing.T) {
	posted := make(chan string, 4)
	merged := make(chan struct{}, 1)
	srv := mergeStub(t, "[]", "", posted, merged)
	defer srv.Close()
	ext, fh := newTestExtension(t, srv.URL, []string{"mention", "merge"})

	rec1 := httptest.NewRecorder()
	ext.handleWebhook(rec1, signedRequest("issue_comment", pullCommentBody("@quack review this")))
	if rec1.Code != http.StatusAccepted {
		t.Fatalf("status = %d", rec1.Code)
	}
	fh.waitForDispatch(t, 2*time.Second)

	rec2 := httptest.NewRecorder()
	ext.handleWebhook(rec2, signedRequest("pull_request", mergeLabelBody("alice")))
	if rec2.Code != http.StatusAccepted {
		t.Fatalf("status = %d", rec2.Code)
	}

	select {
	case c := <-posted:
		if !strings.Contains(c, "already in progress") {
			t.Errorf("comment = %q; want it to note a review is already running", c)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no queued comment posted")
	}

	intent, err := ext.store.GetMergeIntent(context.Background(), globalChatID("github-acme-widgets-7"))
	if err != nil || intent == nil {
		t.Fatalf("GetMergeIntent = %+v, %v; want a recorded intent", intent, err)
	}
	if calls := fh.calls(); len(calls) != 1 {
		t.Errorf("Host.Dispatch calls = %d; want 1 (the label must not dispatch a second review while one is in flight)", len(calls))
	}
}

// TestHandleWebhookMergeLabelReviewLandsConsumesIntent covers the standing
// intent's whole point: no review existed when quack:merge was applied, the
// label queued a review AND recorded the intent, and once that review is
// actually delivered with an approving verdict, the PR merges on its own -
// naming the original label-applier.
func TestHandleWebhookMergeLabelReviewLandsConsumesIntent(t *testing.T) {
	posted := make(chan string, 4)
	merged := make(chan string, 1)
	srv, setReviews := mergeStubDynamic(t, posted, merged)
	defer srv.Close()
	ext, fh := newTestExtension(t, srv.URL, []string{"merge"})

	rec := httptest.NewRecorder()
	ext.handleWebhook(rec, signedRequest("pull_request", mergeLabelBody("alice")))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d", rec.Code)
	}

	select {
	case c := <-posted:
		if !strings.Contains(c, "Queued") {
			t.Errorf("comment = %q; want the queued message", c)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no queued comment posted")
	}
	fh.waitForDispatch(t, 2*time.Second)

	// The review "lands" as an approval, then the dispatched review run
	// completes and records its delivery - simulating quack's own review
	// being posted and the worker's RunEnded arriving.
	setReviews(`[{"state":"APPROVED","user":{"login":"quack[bot]"},"submitted_at":"2026-01-01T00:00:00Z"}]`)
	chatID := globalChatID("github-acme-widgets-7")
	recordDelivery(chatID, deliveryOutcome{reviewDelivered: true})
	ext.RunEnded(chatID, sdk.RunOutcome{Status: sdk.RunDone, PlanRan: true, Answer: "reviewed"})

	select {
	case body := <-merged:
		if !strings.Contains(body, `"sha":"headsha1"`) {
			t.Errorf("merge request body = %q; want it pinned to the reviewed head sha", body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("expected a merge PUT once the review landed approving")
	}
	select {
	case c := <-posted:
		if !strings.Contains(c, "Merged") || !strings.Contains(c, "@alice") {
			t.Errorf("comment = %q; want it to name the original authorizer", c)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no merge comment posted")
	}

	intent, err := ext.store.GetMergeIntent(context.Background(), chatID)
	if err != nil || intent != nil {
		t.Errorf("intent = %+v, %v; want it cleared after the merge", intent, err)
	}
}

// TestHandleWebhookMergeLabelRestartSurvival pins that the standing intent
// survives a process restart: a FRESH Extension over the SAME store - with no
// in-memory memory of the label event that recorded it - still honours it
// once a review lands.
func TestHandleWebhookMergeLabelRestartSurvival(t *testing.T) {
	st, err := openStore(t.TempDir())
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	chatID := globalChatID("github-acme-widgets-7")
	if err := st.SetMergeIntent(context.Background(), chatID, "alice"); err != nil {
		t.Fatalf("seed merge intent: %v", err)
	}

	posted := make(chan string, 4)
	merged := make(chan struct{}, 1)
	approved := `[{"state":"APPROVED","user":{"login":"quack[bot]"},"submitted_at":"2026-01-01T00:00:00Z"}]`
	srv := mergeStub(t, approved, "", posted, merged)
	defer srv.Close()
	ext, fh := newTestExtensionWithStore(t, srv.URL, []string{"mention"}, st)

	rec := httptest.NewRecorder()
	ext.handleWebhook(rec, signedRequest("issue_comment", pullCommentBody("@quack review this")))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d", rec.Code)
	}
	fh.waitForDispatch(t, 2*time.Second)
	recordDelivery(chatID, deliveryOutcome{reviewDelivered: true})
	ext.RunEnded(chatID, sdk.RunOutcome{Status: sdk.RunDone, PlanRan: true, Answer: "reviewed"})

	select {
	case <-merged:
	case <-time.After(2 * time.Second):
		t.Fatal("the pre-restart standing intent did not merge once the review landed")
	}
	select {
	case c := <-posted:
		if !strings.Contains(c, "Merged") || !strings.Contains(c, "@alice") {
			t.Errorf("comment = %q; want it to name the pre-restart authorizer", c)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no merge comment posted")
	}

	intent, err := st.GetMergeIntent(context.Background(), chatID)
	if err != nil || intent != nil {
		t.Errorf("intent = %+v, %v; want it cleared after the merge", intent, err)
	}
}

// TestHandleWebhookMergeLabelRespectsAllowlist pins the merge-label
// enforcement point: a sender outside allowed_users can never authorize a
// merge, even with an APPROVED review already on the PR.
func TestHandleWebhookMergeLabelRespectsAllowlist(t *testing.T) {
	approved := `[{"state":"APPROVED","user":{"login":"quack[bot]"}}]`
	posted := make(chan string, 2)
	merged := make(chan struct{}, 1)
	srv := mergeStub(t, approved, "", posted, merged)
	defer srv.Close()
	ext, _ := newTestExtension(t, srv.URL, []string{"merge"})

	rec := httptest.NewRecorder()
	ext.handleWebhook(rec, signedRequest("pull_request", mergeLabelBody("mallory")))
	if rec.Code != http.StatusAccepted && rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	select {
	case <-merged:
		t.Error("merge-label sender not in allowed_users must not authorize a merge")
	default:
	}
}

// TestHandleWebhookPlanLabelPostsPlanEvenWhenDelivered pins the regression
// where a plan-only run silently dropped its plan: a label trigger implies
// work, so a proxy "delivered" signal must never suppress a plan-only run's
// summary comment - that comment IS the deliverable.
func TestHandleWebhookPlanLabelPostsPlanEvenWhenDelivered(t *testing.T) {
	posted := make(chan string, 1)
	srv := stubGitHub(t, posted)
	defer srv.Close()
	ext, fh := newTestExtension(t, srv.URL, []string{"issue_plan"})

	rec := httptest.NewRecorder()
	ext.handleWebhook(rec, signedRequest("issues", issuesBody("labeled", "quack:plan", "alice", false)))
	if rec.Code != http.StatusAccepted && rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	fh.waitForDispatch(t, 2*time.Second)

	chatID := globalChatID("github-acme-widgets-7")
	// No takeDeliveryDetail entry recorded - a plan-only run never delivers
	// anything staged, so finalize must fall through to posting the answer.
	ext.RunEnded(chatID, sdk.RunOutcome{Status: sdk.RunDone, PlanRan: true, Answer: "## Plan\n\nthe plan"})

	select {
	case body := <-posted:
		if !strings.Contains(body, "the plan") {
			t.Errorf("posted comment is not the plan: %q", body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("plan-only run did not post its plan - the delivered-skip dropped it")
	}
}

// TestHandleWebhookLabelPostsEyesReactionOnIssue pins #252: a label-triggered
// run (quack:plan / quack:implement) posts an instant 👀 on the ISSUE - POST
// to /issues/{number}/reactions, NOT the comment-reaction endpoint (a label
// event carries no comment ID, so ackReaction can't be reused).
func TestHandleWebhookLabelPostsEyesReactionOnIssue(t *testing.T) {
	for _, tc := range []struct{ trigger, label string }{
		{"issue_plan", "quack:plan"},
		{"issue_implement", "quack:implement"},
	} {
		t.Run(tc.trigger, func(t *testing.T) {
			reacted := make(chan string, 1)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.HasSuffix(r.URL.Path, "/installation"):
					fmt.Fprint(w, `{"id":5}`)
				case strings.HasSuffix(r.URL.Path, "/access_tokens"):
					fmt.Fprintf(w, `{"token":"ghs_x","expires_at":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339))
				case strings.HasSuffix(r.URL.Path, "/reactions"):
					b, _ := io.ReadAll(r.Body)
					w.WriteHeader(http.StatusCreated)
					fmt.Fprint(w, `{"id":1}`)
					select {
					case reacted <- r.URL.Path + " " + string(b):
					default:
					}
				default:
					w.WriteHeader(http.StatusCreated)
					fmt.Fprint(w, `{}`)
				}
			}))
			defer srv.Close()

			ext, _ := newTestExtension(t, srv.URL, []string{tc.trigger})

			rec := httptest.NewRecorder()
			ext.handleWebhook(rec, signedRequest("issues", issuesBody("labeled", tc.label, "alice", false)))
			if rec.Code != http.StatusAccepted && rec.Code != http.StatusOK {
				t.Fatalf("status = %d", rec.Code)
			}
			select {
			case got := <-reacted:
				if !strings.Contains(got, "/repos/acme/widgets/issues/7/reactions") {
					t.Errorf("reaction hit wrong endpoint: %q (want /issues/7/reactions)", got)
				}
				if !strings.Contains(got, `"content":"eyes"`) {
					t.Errorf("reaction content not eyes: %q", got)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("no 👀 reaction posted on the issue for a label-triggered run")
			}
		})
	}
}

// TestHandleWebhookIssuePlanLabel pins the quack:plan label routing and the
// dispatched plan message's framing.
func TestHandleWebhookIssuePlanLabel(t *testing.T) {
	tests := []struct {
		name     string
		triggers []string
		label    string
		sender   string
		isPR     bool
		wantRun  bool
	}{
		{"plan label + issue_plan trigger fires", []string{"issue_plan"}, "quack:plan", "alice", false, true},
		{"non-matching label is a no-op", []string{"issue_plan"}, "bug", "alice", false, false},
		{"trigger not enabled is a no-op", []string{"mention"}, "quack:plan", "alice", false, false},
		{"bot sender is a no-op", []string{"issue_plan"}, "quack:plan", "quack[bot]", false, false},
		{"PR-shaped issue is a no-op", []string{"issue_plan"}, "quack:plan", "alice", true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := stubGitHub(t, make(chan string, 1))
			defer srv.Close()
			ext, fh := newTestExtension(t, srv.URL, tt.triggers)

			rec := httptest.NewRecorder()
			ext.handleWebhook(rec, signedRequest("issues", issuesBody("labeled", tt.label, tt.sender, tt.isPR)))
			if rec.Code != http.StatusAccepted && rec.Code != http.StatusOK {
				t.Fatalf("status = %d", rec.Code)
			}

			if !tt.wantRun {
				select {
				case <-fh.notify:
					t.Error("issues event should not have dispatched a run")
				case <-time.After(200 * time.Millisecond):
				}
				return
			}
			req := fh.waitForDispatch(t, 2*time.Second)
			msg := req.Ask.Message
			// The issue title/body come from the fetched snapshot (stubGitHub's
			// fixed issue meta), not the raw webhook payload - #1010 moved the
			// latter to the "event" input artifact and out of the inline
			// envelope, so it no longer carries the payload's own title text.
			if !strings.Contains(msg, "Test issue") {
				t.Errorf("plan message missing issue context: %q", msg)
			}
			if !strings.Contains(msg, "PLANNING-ONLY") {
				t.Errorf("plan message not framed planning-only: %q", msg)
			}
			// #569: the plan-only prompt must state that the answer text IS the
			// deliverable, not a pointer to a file the run wrote and discarded.
			if !strings.Contains(msg, "ANSWER TEXT is the plan") {
				t.Errorf("plan message does not state the answer text is the deliverable: %q", msg)
			}
			// #662: the file-path and stale-version cautions are constant, not
			// per-event - they moved to agents/orchestrator/prompt.md (a quack-repo
			// file this module doesn't own/ship), so the trigger itself no longer
			// carries them.
			for _, moved := range []string{"discarded", "current stable"} {
				if strings.Contains(msg, moved) {
					t.Errorf("plan message still carries the %q caution - it should have moved to the orchestrator bundle prompt: %q", moved, msg)
				}
			}
			for _, banned := range []string{"git_push", "github_pull_request", "create a branch"} {
				if strings.Contains(msg, banned) {
					t.Errorf("planning-only message contains delivery instruction %q: %q", banned, msg)
				}
			}
			if req.Chat.LocalID != "github-acme-widgets-7" {
				t.Errorf("LocalID = %q, want issue-tied github-acme-widgets-7", req.Chat.LocalID)
			}
		})
	}
}

// TestHandleWebhookIssueOpenedNoOp pins that non-labeled issue actions are
// ignored - the workflow is label-driven, not event-driven.
func TestHandleWebhookIssueOpenedNoOp(t *testing.T) {
	ext, fh := newTestExtension(t, "http://unused", []string{"issue_plan"})

	rec := httptest.NewRecorder()
	ext.handleWebhook(rec, signedRequest("issues", issuesBody("opened", "", "alice", false)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 no-op ack", rec.Code)
	}
	if calls := fh.calls(); len(calls) != 0 {
		t.Error("issues opened should not dispatch a run")
	}
}

// TestHandleWebhookIssueImplementLabel pins the quack:implement label routing
// and its dispatched task's Closes-trailer framing.
func TestHandleWebhookIssueImplementLabel(t *testing.T) {
	tests := []struct {
		name     string
		triggers []string
		label    string
		wantRun  bool
	}{
		{"implement label + trigger fires", []string{"issue_implement"}, "quack:implement", true},
		{"trigger not enabled is a no-op", []string{"issue_plan"}, "quack:implement", false},
		{"plan label does not fire implement", []string{"issue_implement"}, "quack:plan", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := stubGitHub(t, make(chan string, 4))
			defer srv.Close()
			ext, fh := newTestExtension(t, srv.URL, tt.triggers)

			rec := httptest.NewRecorder()
			ext.handleWebhook(rec, signedRequest("issues", issuesBody("labeled", tt.label, "alice", false)))
			if rec.Code != http.StatusAccepted && rec.Code != http.StatusOK {
				t.Fatalf("status = %d", rec.Code)
			}
			if !tt.wantRun {
				select {
				case <-fh.notify:
					t.Error("issues event should not have dispatched a run")
				case <-time.After(200 * time.Millisecond):
				}
				return
			}
			req := fh.waitForDispatch(t, 2*time.Second)
			for _, want := range []string{"Closes #7", `<issue number="7">`} {
				if !strings.Contains(req.Ask.Message, want) {
					t.Errorf("implement message missing %q: %q", want, req.Ask.Message)
				}
			}
			if req.Chat.LocalID != "github-acme-widgets-7" {
				t.Errorf("LocalID = %q, want the issue's session (plan continuity)", req.Chat.LocalID)
			}
		})
	}
}

// TestHandleWebhookRequestChangesEngagesOwnPR pins #656 test case 3 (closes
// #655): a request_changes review on a PR quack authored engages it to
// address the findings - authorship IS the flag, no label on the PR at all.
func TestHandleWebhookRequestChangesEngagesOwnPR(t *testing.T) {
	posted := make(chan string, 4)
	srv := stubFixGitHubFull(t, posted, nil, false, "", "quack[bot]") // no labels; PR authored by quack itself
	defer srv.Close()
	ext, fh := newTestExtension(t, srv.URL, []string{"ci_fix"})

	rec := httptest.NewRecorder()
	ext.handleWebhook(rec, signedRequest("pull_request_review", pullRequestReviewBody("changes_requested", "alice", 7)))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d; want 202", rec.Code)
	}
	req := fh.waitForDispatch(t, 2*time.Second)
	for _, want := range []string{"requested changes", `"actor":"alice"`} {
		if !strings.Contains(req.Ask.Message, want) {
			t.Errorf("engagement message missing %q: %q", want, req.Ask.Message)
		}
	}
}

// TestHandleWebhookRequestChangesIgnoresOtherPRs proves the label/mention
// triggers, not this path, still own a PR quack did NOT author - and an
// approving/commented review never engages regardless of authorship.
func TestHandleWebhookRequestChangesIgnoresOtherPRs(t *testing.T) {
	tests := []struct {
		name          string
		state         string
		prAuthorLogin string
	}{
		{"not quack's PR", "changes_requested", "someone-else"},
		{"quack's PR but an approval, not changes requested", "approved", "quack[bot]"},
		{"quack's PR but a plain comment review", "commented", "quack[bot]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			posted := make(chan string, 4)
			srv := stubFixGitHubFull(t, posted, nil, false, "", tt.prAuthorLogin)
			defer srv.Close()
			ext, fh := newTestExtension(t, srv.URL, []string{"ci_fix"})

			rec := httptest.NewRecorder()
			ext.handleWebhook(rec, signedRequest("pull_request_review", pullRequestReviewBody(tt.state, "alice", 7)))
			if rec.Code != http.StatusAccepted && rec.Code != http.StatusOK {
				t.Fatalf("status = %d", rec.Code)
			}
			// Authorship resolves async (an HTTP round trip); bound the wait and
			// fail immediately if it fires.
			select {
			case <-fh.notify:
				t.Error("must not engage")
			case <-time.After(150 * time.Millisecond):
			}
		})
	}
}

// reviewCommandBody is an issue_comment payload on a PR, with labels and author association.
func reviewCommandBody(comment, association string, labels ...string) []byte {
	quoted := make([]string, len(labels))
	for i, l := range labels {
		quoted[i] = fmt.Sprintf(`{"name":%q}`, l)
	}
	return []byte(fmt.Sprintf(`{
		"action":"created",
		"comment":{"id":999,"body":%q,"user":{"login":"alice"},"author_association":%q},
		"issue":{"number":7,"labels":[%s],"pull_request":{}},
		"repository":{"name":"widgets","owner":{"login":"acme"},"clone_url":"https://github.com/acme/widgets.git","default_branch":"main"},
		"installation":{"id":5}
	}`, comment, association, strings.Join(quoted, ",")))
}

func TestHandleWebhookReviewCommand(t *testing.T) {
	tests := []struct {
		name     string
		triggers []string
		body     []byte
		wantRun  bool
	}{
		{"bare /review with label and write access fires", []string{"label"}, reviewCommandBody("  /review \n", "COLLABORATOR", "quack-auto-review"), true},
		{"owner association fires", []string{"label"}, reviewCommandBody("/review", "OWNER", "quack-auto-review"), true},
		{"missing review label is a no-op", []string{"label"}, reviewCommandBody("/review", "OWNER", "other"), false},
		{"read-only author is a no-op", []string{"label"}, reviewCommandBody("/review", "CONTRIBUTOR", "quack-auto-review"), false},
		{"extra text is not the command", []string{"label"}, reviewCommandBody("/review please", "OWNER", "quack-auto-review"), false},
		{"label trigger disabled is a no-op", []string{"mention"}, reviewCommandBody("/review", "OWNER", "quack-auto-review"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := stubGitHub(t, make(chan string, 1))
			defer srv.Close()
			ext, fh := newTestExtension(t, srv.URL, tt.triggers)

			rec := httptest.NewRecorder()
			ext.handleWebhook(rec, signedRequest("issue_comment", tt.body))
			if rec.Code != http.StatusAccepted && rec.Code != http.StatusOK {
				t.Fatalf("status = %d", rec.Code)
			}

			if tt.wantRun {
				req := fh.waitForDispatch(t, 2*time.Second)
				if !strings.Contains(req.Ask.Message, "<deliverable>a review with inline comments and a verdict</deliverable>") {
					t.Errorf("ask = %q; want the auto-review deliverable", req.Ask.Message)
				}
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

// TestHandleWebhookReviewCommandRespectsAllowlist pins that /review still goes
// through the allowed_users gate like every human-invoked trigger.
func TestHandleWebhookReviewCommandRespectsAllowlist(t *testing.T) {
	srv := stubGitHub(t, make(chan string, 1))
	defer srv.Close()
	ext, fh := newTestExtension(t, srv.URL, []string{"label"})
	ext.allowedUsers = map[string]bool{"someone-else": true}

	rec := httptest.NewRecorder()
	ext.handleWebhook(rec, signedRequest("issue_comment", reviewCommandBody("/review", "OWNER", "quack-auto-review")))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rec.Code)
	}
	select {
	case <-fh.notify:
		t.Error("disallowed user should not dispatch")
	case <-time.After(200 * time.Millisecond):
	}
}

// TestHandleWebhookSynchronizeInvalidatesOnlyARunningPR pins both halves of
// the push signal: a PR with a run in flight gets its clone invalidated, and
// a PR with none must not - there is no clone to refresh, and no run to
// disturb.
func TestHandleWebhookSynchronizeInvalidatesOnlyARunningPR(t *testing.T) {
	srv := stubGitHub(t, make(chan string, 1))
	defer srv.Close()
	ext, fh := newTestExtension(t, srv.URL, nil)

	rec := httptest.NewRecorder()
	ext.handleWebhook(rec, signedRequest("pull_request", pullRequestBody("synchronize", "")))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got := fh.invalidateCalls(); len(got) != 0 {
		t.Fatalf("InvalidateSetup calls with no run in flight = %v, want none", got)
	}

	chatID := globalChatID("github-acme-widgets-7")
	ext.pending.Store(chatID, &pendingRun{sessionID: "github-acme-widgets-7", owner: "acme", repo: "widgets", number: 7, isPR: true})

	rec = httptest.NewRecorder()
	ext.handleWebhook(rec, signedRequest("pull_request", pullRequestBody("synchronize", "")))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got := fh.invalidateCalls(); len(got) != 1 || got[0] != chatID {
		t.Fatalf("InvalidateSetup calls = %v, want [%s]", got, chatID)
	}

	select {
	case <-fh.notify:
		t.Error("pull_request.synchronize should never dispatch a run")
	case <-time.After(100 * time.Millisecond):
	}
}
