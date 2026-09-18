package sleeper

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/fagerbergj/quack-extensions/sdk"
)

func TestExtensionSatisfiesRunObserver(t *testing.T) {
	var _ sdk.RunObserver = (*extension)(nil)
}

func TestOriginLabel(t *testing.T) {
	cases := []struct{ stop, job, partner, want string }{
		{"3", "lineup", "", "Lineup · week 3"},
		{"3", "waivers", "", "Waivers · week 3"},
		{"3", "digest", "", "Digest · week 3"},
		{"draft", "draft", "", "Draft"},
		{"review", "history", "", "Season review"},
		{"3", "trade", "Rice Cooker", "Trade · Rice Cooker"},
	}
	for _, c := range cases {
		if got := originLabel(c.stop, c.job, c.partner); got != c.want {
			t.Errorf("originLabel(%q,%q,%q) = %q, want %q", c.stop, c.job, c.partner, got, c.want)
		}
	}
}

// TestHandleArtifactsReportsRunning pins the fix for the lost Running
// badge: Dispatch marks a chat running, RunEnded clears it.
func TestHandleArtifactsReportsRunning(t *testing.T) {
	host := &fakeHost{artifacts: map[string]map[string][]byte{}}
	e, r := newTestExtension(t, host.sdkHost(), config{})
	body := `{"league_id":"` + testLeague + `","stop":"2","job":"lineup"}`
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/jobs", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("dispatch status = %d, body = %s", rec.Code, rec.Body)
	}

	wantChatID := "ext:sleeper:" + testLeague + ":2:lineup"
	rec2 := httptest.NewRecorder()
	r.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/api/artifacts?league_id="+testLeague+"&stop=2", nil))
	var resp artifactsResponse
	if err := json.Unmarshal(rec2.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.Jobs["lineup"].Running {
		t.Fatal("jobs[lineup].Running = false right after dispatch, want true")
	}

	e.RunEnded(wantChatID, sdk.RunOutcome{Status: sdk.RunDone})

	rec3 := httptest.NewRecorder()
	r.ServeHTTP(rec3, httptest.NewRequest(http.MethodGet, "/api/artifacts?league_id="+testLeague+"&stop=2", nil))
	var resp3 artifactsResponse
	if err := json.Unmarshal(rec3.Body.Bytes(), &resp3); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp3.Jobs["lineup"].Running {
		t.Error("jobs[lineup].Running = true after RunEnded, want false")
	}
}

func TestRunEndedUnknownChatIsNoop(t *testing.T) {
	e := &extension{}
	e.RunEnded("ext:sleeper:nope", sdk.RunOutcome{Status: sdk.RunFailed})
	if e.isRunning("ext:sleeper:nope") {
		t.Error("an unknown chat should never end up marked running")
	}
}

func TestJobRunnableMatchesJobWorkflows(t *testing.T) {
	for _, job := range []string{"lineup", "waivers", "trends"} {
		if !jobRunnable(job) {
			t.Errorf("jobRunnable(%q) = false, want true (in jobWorkflows)", job)
		}
	}
	for _, job := range []string{"digest", "retro", "draft", "history", "trade"} {
		if jobRunnable(job) {
			t.Errorf("jobRunnable(%q) = true, want false (no jobWorkflows entry)", job)
		}
	}
}
