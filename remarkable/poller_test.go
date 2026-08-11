package remarkable

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/fagerbergj/quack-extensions/sdk"
)

// fakeDispatchHost is the SDK-fake used across this package's poller
// tests: it captures dispatched requests and lets a test inject a
// per-call error, mirroring noop's fakeHost pattern (this module can't
// import quack, so there's no real orchestrator to dispatch into).
type fakeDispatchHost struct {
	mu    sync.Mutex
	calls []sdk.DispatchRequest
	fn    func(req sdk.DispatchRequest) error
}

func (f *fakeDispatchHost) dispatch(ctx context.Context, req sdk.DispatchRequest) error {
	f.mu.Lock()
	f.calls = append(f.calls, req)
	fn := f.fn
	f.mu.Unlock()
	if fn != nil {
		return fn(req)
	}
	return nil
}

func (f *fakeDispatchHost) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func newTestPoller(t *testing.T, fc *fakeRMCloud, fh *fakeDispatchHost) *poller {
	t.Helper()
	return newTestPollerWithDataDir(t, fc, fh, t.TempDir())
}

func newTestPollerWithDataDir(t *testing.T, fc *fakeRMCloud, fh *fakeDispatchHost, dataDir string) *poller {
	t.Helper()
	host := sdk.Host{
		Dispatch: fh.dispatch,
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		DataDir:  dataDir,
	}
	p := &poller{
		host:      host,
		client:    newRMClient(fc.Server.URL, fc.email, fc.password, nil),
		interval:  time.Hour,
		statePath: statePath(dataDir),
	}
	st, err := loadState(p.statePath)
	if err != nil {
		t.Fatalf("loadState: %v", err)
	}
	p.st = st
	return p
}

func TestPollOnceDispatchesNewDocument(t *testing.T) {
	fc := newFakeRMCloud("user@example.com", "pw")
	defer fc.Close()
	mod := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	fc.setDocs([]fixtureDoc{
		{ID: "doc-1", Name: "Meeting Notes", Folder: "Inbox", LastModified: mod, PDF: []byte("%PDF-x")},
	})

	fh := &fakeDispatchHost{}
	p := newTestPoller(t, fc, fh)
	p.pollOnce(context.Background())

	fh.mu.Lock()
	defer fh.mu.Unlock()
	if len(fh.calls) != 1 {
		t.Fatalf("got %d dispatch calls, want 1", len(fh.calls))
	}
	req := fh.calls[0]

	if req.Chat.LocalID != "doc-1" {
		t.Errorf("LocalID = %q, want doc-1", req.Chat.LocalID)
	}
	if req.Chat.Title != "Meeting Notes" {
		t.Errorf("Title = %q, want Meeting Notes", req.Chat.Title)
	}
	if req.Run.Workflow != workflowDocumentIngest {
		t.Errorf("Workflow = %q, want %q", req.Run.Workflow, workflowDocumentIngest)
	}
	if req.Chat.Origin == nil {
		t.Fatal("Chat.Origin is nil")
	}
	if req.Chat.Origin.Extension != extensionName || req.Chat.Origin.Kind != "document" || req.Chat.Origin.Label != "Meeting Notes" {
		t.Errorf("Origin = %+v", req.Chat.Origin)
	}
	folderLabels := req.Chat.Origin.Labels["folder"]
	if len(folderLabels) != 1 || folderLabels[0].Value != "Inbox" {
		t.Errorf("Labels[folder] = %+v, want one value Inbox", folderLabels)
	}
	if len(req.Ask.Attachments) != 1 {
		t.Fatalf("got %d attachments, want 1", len(req.Ask.Attachments))
	}
	att := req.Ask.Attachments[0]
	if att.Name != "doc-1.pdf" || att.MIME != "application/pdf" || string(att.Data) != "%PDF-x" {
		t.Errorf("Attachment = %+v, want Name=doc-1.pdf MIME=application/pdf Data=%%PDF-x", att)
	}
}

func TestPollOnceSkipsUnchangedDocument(t *testing.T) {
	fc := newFakeRMCloud("user@example.com", "pw")
	defer fc.Close()
	mod := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	fc.setDocs([]fixtureDoc{{ID: "doc-1", Name: "Notes", LastModified: mod, PDF: []byte("x")}})

	fh := &fakeDispatchHost{}
	p := newTestPoller(t, fc, fh)
	ctx := context.Background()

	p.pollOnce(ctx)
	p.pollOnce(ctx)
	p.pollOnce(ctx)

	if n := fh.callCount(); n != 1 {
		t.Fatalf("dispatch calls = %d after 3 polls of an unchanged doc, want 1", n)
	}
}

func TestPollOnceRedispatchesUpdatedDocumentSameLocalID(t *testing.T) {
	fc := newFakeRMCloud("user@example.com", "pw")
	defer fc.Close()
	mod1 := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	fc.setDocs([]fixtureDoc{{ID: "doc-1", Name: "Notes", LastModified: mod1, PDF: []byte("v1")}})

	fh := &fakeDispatchHost{}
	p := newTestPoller(t, fc, fh)
	ctx := context.Background()
	p.pollOnce(ctx)

	mod2 := mod1.Add(time.Hour)
	fc.setDocs([]fixtureDoc{{ID: "doc-1", Name: "Notes", LastModified: mod2, PDF: []byte("v2")}})
	p.pollOnce(ctx)

	fh.mu.Lock()
	defer fh.mu.Unlock()
	if len(fh.calls) != 2 {
		t.Fatalf("got %d dispatch calls, want 2", len(fh.calls))
	}
	for i, req := range fh.calls {
		if req.Chat.LocalID != "doc-1" {
			t.Errorf("call %d LocalID = %q, want doc-1 (same chat across versions)", i, req.Chat.LocalID)
		}
	}
	if string(fh.calls[1].Ask.Attachments[0].Data) != "v2" {
		t.Errorf("second dispatch attachment = %q, want v2 (the updated content)", fh.calls[1].Ask.Attachments[0].Data)
	}
}

func TestPollOnceRetriesFailedDispatchNextCycleOnly(t *testing.T) {
	fc := newFakeRMCloud("user@example.com", "pw")
	defer fc.Close()
	mod := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	fc.setDocs([]fixtureDoc{{ID: "doc-1", Name: "Notes", LastModified: mod, PDF: []byte("x")}})

	fh := &fakeDispatchHost{}
	var attempt int
	fh.fn = func(req sdk.DispatchRequest) error {
		attempt++
		if attempt == 1 {
			return errors.New("boom")
		}
		return nil
	}

	p := newTestPoller(t, fc, fh)
	ctx := context.Background()

	p.pollOnce(ctx) // fails
	if n := fh.callCount(); n != 1 {
		t.Fatalf("after first (failing) poll, calls = %d, want 1", n)
	}
	ds := p.st.Documents["doc-1"]
	if ds.LastOutcome != outcomeFailed || ds.InFlight {
		t.Fatalf("doc-1 state after failure = %+v, want LastOutcome=failed InFlight=false", ds)
	}

	p.pollOnce(ctx) // same cycle boundary: retries once, succeeds
	if n := fh.callCount(); n != 2 {
		t.Fatalf("after second poll, calls = %d, want 2 (exactly one retry)", n)
	}

	p.pollOnce(ctx) // doc unchanged and last outcome is not "failed" anymore: no further retry
	if n := fh.callCount(); n != 2 {
		t.Fatalf("after third poll, calls = %d, want still 2 (no retry once succeeded)", n)
	}
}

func TestStateSurvivesRestart(t *testing.T) {
	fc := newFakeRMCloud("user@example.com", "pw")
	defer fc.Close()
	mod := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	fc.setDocs([]fixtureDoc{{ID: "doc-1", Name: "Notes", LastModified: mod, PDF: []byte("x")}})

	dataDir := t.TempDir()
	ctx := context.Background()

	fh1 := &fakeDispatchHost{}
	p1 := newTestPollerWithDataDir(t, fc, fh1, dataDir)
	p1.pollOnce(ctx)
	p1.runEnded("doc-1", sdk.RunOutcome{Status: sdk.RunDone})

	// a fresh poller over the same DataDir, as after a process restart
	fh2 := &fakeDispatchHost{}
	p2 := newTestPollerWithDataDir(t, fc, fh2, dataDir)
	p2.pollOnce(ctx)

	if n := fh1.callCount(); n != 1 {
		t.Fatalf("first poller dispatched %d times, want 1", n)
	}
	if n := fh2.callCount(); n != 0 {
		t.Fatalf("second poller (after restart) dispatched %d times, want 0 - state should have loaded from disk", n)
	}
}

func TestRunEndedIgnoresUnknownChatID(t *testing.T) {
	fc := newFakeRMCloud("user@example.com", "pw")
	defer fc.Close()
	fh := &fakeDispatchHost{}
	p := newTestPoller(t, fc, fh)

	// must not panic or create a phantom entry
	p.runEnded("never-dispatched", sdk.RunOutcome{Status: sdk.RunDone})
	if _, ok := p.st.Documents["never-dispatched"]; ok {
		t.Fatal("runEnded created a state entry for an unknown chat id")
	}
}

func TestPollOnceRespectsFolderFilter(t *testing.T) {
	fc := newFakeRMCloud("user@example.com", "pw")
	defer fc.Close()
	mod := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	fc.setDocs([]fixtureDoc{
		{ID: "doc-in", Name: "In", Folder: "Inbox", LastModified: mod, PDF: []byte("x")},
		{ID: "doc-in-sub", Name: "InSub", Folder: "Inbox/Scans", LastModified: mod, PDF: []byte("x")},
		{ID: "doc-out", Name: "Out", Folder: "Archive", LastModified: mod, PDF: []byte("x")},
		{ID: "doc-root", Name: "Root", LastModified: mod, PDF: []byte("x")},
	})

	fh := &fakeDispatchHost{}
	p := newTestPoller(t, fc, fh)
	p.folderFilter = "Inbox"
	p.pollOnce(context.Background())

	fh.mu.Lock()
	defer fh.mu.Unlock()
	if len(fh.calls) != 2 {
		t.Fatalf("got %d dispatch calls, want 2 (Inbox + Inbox/Scans)", len(fh.calls))
	}
	seen := map[string]bool{}
	for _, req := range fh.calls {
		seen[req.Chat.LocalID] = true
	}
	if !seen["doc-in"] || !seen["doc-in-sub"] {
		t.Errorf("dispatched = %v, want doc-in and doc-in-sub", seen)
	}
}

func TestPollerStopIsIdempotentEvenWithoutStart(t *testing.T) {
	fc := newFakeRMCloud("user@example.com", "pw")
	defer fc.Close()
	fh := &fakeDispatchHost{}
	p := newTestPoller(t, fc, fh)

	if err := p.Stop(context.Background()); err != nil {
		t.Fatalf("Stop before Start: %v", err)
	}
	if err := p.Stop(context.Background()); err != nil {
		t.Fatalf("second Stop: %v", err)
	}
}

func TestPollerStartFailsLoudOnUnreachableCloud(t *testing.T) {
	fh := &fakeDispatchHost{}
	host := sdk.Host{
		Dispatch: fh.dispatch,
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		DataDir:  t.TempDir(),
	}
	p := &poller{
		host:      host,
		client:    newRMClient("http://127.0.0.1:1", "u", "p", nil), // nothing listens here
		interval:  time.Hour,
		statePath: statePath(host.DataDir),
	}

	err := p.Start(context.Background())
	if err == nil {
		t.Fatal("Start against an unreachable rmfakecloud returned nil, want an error")
	}
	_ = p.Stop(context.Background())
}

func TestPollerStartAndStopLifecycle(t *testing.T) {
	fc := newFakeRMCloud("user@example.com", "pw")
	defer fc.Close()
	fc.setDocs([]fixtureDoc{{ID: "doc-1", Name: "Notes", LastModified: time.Now().UTC(), PDF: []byte("x")}})

	fh := &fakeDispatchHost{}
	host := sdk.Host{
		Dispatch: fh.dispatch,
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		DataDir:  t.TempDir(),
	}
	p := &poller{
		host:      host,
		client:    newRMClient(fc.Server.URL, fc.email, fc.password, nil),
		interval:  10 * time.Millisecond,
		statePath: statePath(host.DataDir),
	}

	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for fh.callCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if fh.callCount() == 0 {
		t.Fatal("poll loop never dispatched the seeded document")
	}

	stopCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := p.Stop(stopCtx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}
