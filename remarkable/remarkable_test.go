package remarkable

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/fagerbergj/quack-extensions/sdk"
	"github.com/go-chi/chi/v5"
)

func testHost(t *testing.T, fh *fakeDispatchHost) sdk.Host {
	t.Helper()
	return sdk.Host{
		Dispatch: fh.dispatch,
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		DataDir:  t.TempDir(),
	}
}

func TestFactoryRequiresBaseURL(t *testing.T) {
	fh := &fakeDispatchHost{}
	_, err := factory(testHost(t, fh), []byte("email: a@b.com\npassword: pw\n"))
	if err == nil {
		t.Fatal("expected an error for missing base_url, got nil")
	}
}

func TestFactoryRequiresCredentials(t *testing.T) {
	fh := &fakeDispatchHost{}
	_, err := factory(testHost(t, fh), []byte("base_url: http://example.com\n"))
	if err == nil {
		t.Fatal("expected an error for missing email/password, got nil")
	}
}

func TestFactoryRequiresDataDir(t *testing.T) {
	fh := &fakeDispatchHost{}
	host := testHost(t, fh)
	host.DataDir = ""
	_, err := factory(host, []byte("base_url: http://example.com\nemail: a@b.com\npassword: pw\n"))
	if err == nil {
		t.Fatal("expected an error for empty Host.DataDir, got nil")
	}
}

func TestFactoryRejectsBadPollInterval(t *testing.T) {
	fh := &fakeDispatchHost{}
	_, err := factory(testHost(t, fh), []byte("base_url: http://example.com\nemail: a@b.com\npassword: pw\npoll_interval: not-a-duration\n"))
	if err == nil {
		t.Fatal("expected an error for invalid poll_interval, got nil")
	}
}

func TestFactoryDefaultsPollInterval(t *testing.T) {
	fh := &fakeDispatchHost{}
	extVal, err := factory(testHost(t, fh), []byte("base_url: http://example.com\nemail: a@b.com\npassword: pw\n"))
	if err != nil {
		t.Fatalf("factory: %v", err)
	}
	ext := extVal.(*extension)
	if ext.poller.interval != defaultPollInterval {
		t.Errorf("interval = %v, want default %v", ext.poller.interval, defaultPollInterval)
	}
}

func TestFactoryParsesConfig(t *testing.T) {
	fh := &fakeDispatchHost{}
	raw := []byte("base_url: http://example.com\nemail: a@b.com\npassword: pw\npoll_interval: 5m\nfolder_filter: Inbox\n")
	extVal, err := factory(testHost(t, fh), raw)
	if err != nil {
		t.Fatalf("factory: %v", err)
	}
	ext := extVal.(*extension)
	if ext.poller.interval != 5*time.Minute {
		t.Errorf("interval = %v, want 5m", ext.poller.interval)
	}
	if ext.poller.folderFilter != "Inbox" {
		t.Errorf("folderFilter = %q, want Inbox", ext.poller.folderFilter)
	}
	if ext.poller.client.baseURL != "http://example.com" {
		t.Errorf("baseURL = %q, want http://example.com", ext.poller.client.baseURL)
	}
}

func TestRegisteredInInit(t *testing.T) {
	factories := sdk.Registered()
	if _, ok := factories[extensionName]; !ok {
		t.Fatalf("sdk.Registered() missing %q - init() should have registered it", extensionName)
	}
}

func TestExtensionImplementsInterfaces(t *testing.T) {
	fh := &fakeDispatchHost{}
	extVal, err := factory(testHost(t, fh), []byte("base_url: http://example.com\nemail: a@b.com\npassword: pw\n"))
	if err != nil {
		t.Fatalf("factory: %v", err)
	}
	ext := extVal.(*extension)

	if ext.Tools() != nil {
		t.Errorf("Tools() = %v, want nil (inbound-only extension)", ext.Tools())
	}

	if ui := ext.UI(); ui.Title == "" || ui.Href == "" {
		t.Errorf("UI() = %+v, want populated Title and Href", ui)
	}

	// RunEnded must reach the poller's own bookkeeping, not be a no-op stub.
	// st is normally populated by Start(); seed it directly here.
	st, err := loadState(ext.poller.statePath)
	if err != nil {
		t.Fatalf("loadState: %v", err)
	}
	ext.poller.st = st
	ext.poller.st.Documents["doc-1"] = docState{ID: "doc-1", InFlight: true}
	ext.RunEnded("doc-1", sdk.RunOutcome{Status: sdk.RunDone})
	if ext.poller.st.Documents["doc-1"].InFlight {
		t.Error("RunEnded via the extension did not clear InFlight on the poller's state")
	}
}

func TestStatusRouteReportsDocuments(t *testing.T) {
	fc := newFakeRMCloud("user@example.com", "pw")
	defer fc.Close()
	mod := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	fc.setDocs([]fixtureDoc{{ID: "doc-1", Name: "Notes", Folder: "Inbox", LastModified: mod, PDF: []byte("x")}})

	fh := &fakeDispatchHost{}
	p := newTestPoller(t, fc, fh)
	p.pollOnce(context.Background())

	ext := &extension{host: p.host, poller: p}
	r := chi.NewRouter()
	ext.RegisterRoutes(r, chi.NewRouter())

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/status", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}

	var got statusResponse
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Extension != extensionName {
		t.Errorf("Extension = %q, want %q", got.Extension, extensionName)
	}
	if len(got.Documents) != 1 || got.Documents[0].ID != "doc-1" {
		t.Fatalf("Documents = %+v, want one entry doc-1", got.Documents)
	}
	if got.Documents[0].Folder != "Inbox" {
		t.Errorf("Folder = %q, want Inbox", got.Documents[0].Folder)
	}
}

func TestStatusRouteBeforeAnyPollDoesNotPanic(t *testing.T) {
	fh := &fakeDispatchHost{}
	extVal, err := factory(testHost(t, fh), []byte("base_url: http://example.com\nemail: a@b.com\npassword: pw\n"))
	if err != nil {
		t.Fatalf("factory: %v", err)
	}
	ext := extVal.(*extension)
	r := chi.NewRouter()
	ext.RegisterRoutes(r, chi.NewRouter())

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/status", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
}
