package sleeper

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/fagerbergj/quack-extensions/sdk"
	"github.com/go-chi/chi/v5"
)

// fakeHost builds a minimal sdk.Host recording ReadArtifact/Dispatch calls,
// so a test can assert on the exact chat id and turn-append behavior
// without a real quack server.
type fakeHost struct {
	artifacts  map[string]map[string][]byte // chatID -> name -> data
	dispatched []sdk.DispatchRequest
}

func (h *fakeHost) readArtifact(chatID, user, name string) ([]byte, bool) {
	byName, ok := h.artifacts[chatID]
	if !ok {
		return nil, false
	}
	data, ok := byName[name]
	return data, ok
}

func (h *fakeHost) dispatch(_ context.Context, req sdk.DispatchRequest) error {
	h.dispatched = append(h.dispatched, req)
	return nil
}

func (h *fakeHost) sdkHost() sdk.Host {
	return sdk.Host{ReadArtifact: h.readArtifact, Dispatch: h.dispatch}
}

func newTestExtension(t *testing.T, host sdk.Host, cfg config) (*extension, *chi.Mux) {
	t.Helper()
	cfg.DefaultUser = firstNonEmpty(cfg.DefaultUser, testUser)
	e := &extension{host: host, cfg: cfg, client: newTestClient(t)}
	r := chi.NewRouter()
	e.RegisterRoutes(r, chi.NewRouter())
	return e, r
}

func TestHandleSeasons(t *testing.T) {
	_, r := newTestExtension(t, sdk.Host{}, config{})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/seasons?league_id="+testLeague, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	var resp seasonsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.CurrentSeason != "2026" {
		t.Errorf("current_season = %q, want 2026", resp.CurrentSeason)
	}
	found := map[string]seasonSummary{}
	for _, s := range resp.Seasons {
		found[s.LeagueID] = s
	}
	cur, ok := found[testLeague]
	if !ok {
		t.Fatalf("chain missing current league %s: %+v", testLeague, resp.Seasons)
	}
	if cur.Wins != 0 || cur.Losses != 1 {
		t.Errorf("current league record = %d-%d, want 0-1", cur.Wins, cur.Losses)
	}
	if _, ok := found[pastLeague]; !ok {
		t.Errorf("chain missing past league %s: %+v", pastLeague, resp.Seasons)
	}
	// Chronological, current last (the approved design; the underlying walk is newest-first).
	if len(resp.Seasons) < 2 {
		t.Fatalf("expected at least 2 seasons, got %+v", resp.Seasons)
	}
	if resp.Seasons[len(resp.Seasons)-1].LeagueID != testLeague {
		t.Errorf("last season = %+v, want the current league last", resp.Seasons[len(resp.Seasons)-1])
	}
	if resp.Seasons[0].LeagueID == testLeague {
		t.Errorf("first season is the current league; want oldest first: %+v", resp.Seasons)
	}
}

func TestHandleSeasonsUnknownLeague(t *testing.T) {
	_, r := newTestExtension(t, sdk.Host{}, config{})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/seasons?league_id=9999999999999999999", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

// TestHandleSeasonsUpstreamFailureIsBadGateway pins the 404-vs-502 split:
// a genuine upstream 5xx (unlike Sleeper's real 404 or null-body
// not-found) must not read as "unknown league".
func TestHandleSeasonsUpstreamFailureIsBadGateway(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	client, err := NewClient(srv.URL, nil)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	e := &extension{client: client, cfg: config{DefaultUser: testUser}}
	r := chi.NewRouter()
	e.RegisterRoutes(r, chi.NewRouter())

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/seasons?league_id="+testLeague, nil))
	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502, body = %s", rec.Code, rec.Body)
	}
}

func TestHandleSeasonsMissingLeagueID(t *testing.T) {
	_, r := newTestExtension(t, sdk.Host{}, config{})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/seasons", nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestHandleSeason(t *testing.T) {
	_, r := newTestExtension(t, sdk.Host{}, config{})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/season?league_id="+testLeague, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	var resp seasonResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.League.Name == "" {
		t.Error("league name is empty")
	}
	if len(resp.Standings) != 10 {
		t.Errorf("standings length = %d, want 10", len(resp.Standings))
	}
	if resp.Me == nil || resp.Me.Team != "Substation Supremacy" {
		t.Errorf("me = %+v, want team Substation Supremacy", resp.Me)
	}
	if resp.Opponent == nil {
		t.Error("opponent is nil, want the week-2 matchup opponent")
	} else if resp.Opponent.Team != "Pitts and Giggles" {
		t.Errorf("opponent team = %q, want Pitts and Giggles (roster 7, matchup_id 3)", resp.Opponent.Team)
	}
	if resp.ReserveSlots != 0 {
		t.Errorf("reserve_slots = %d, want 0 (this league's roster_positions has no IR slot)", resp.ReserveSlots)
	}
}

func TestHandleSeasonBadInput(t *testing.T) {
	_, r := newTestExtension(t, sdk.Host{}, config{})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/season", nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestHandleArtifactsReadsExactChatID(t *testing.T) {
	wantChatID := "ext:sleeper:" + testLeague + ":2:lineup"
	host := &fakeHost{artifacts: map[string]map[string][]byte{
		wantChatID: {"lineup": []byte(`{"week":2}`)},
	}}
	_, r := newTestExtension(t, host.sdkHost(), config{})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/artifacts?league_id="+testLeague+"&stop=2", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	var resp artifactsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	lineup, ok := resp.Jobs["lineup"]
	if !ok || !lineup.Found {
		t.Fatalf("jobs[lineup] = %+v, want found", resp.Jobs["lineup"])
	}
	if lineup.Example {
		t.Error("a real artifact must not be marked Example")
	}
	if string(lineup.Data) != `{"week":2}` {
		t.Errorf("data = %s, want the stored bytes verbatim", lineup.Data)
	}
	for _, job := range []string{"waivers", "digest", "trends", "retro"} {
		if resp.Jobs[job].Found {
			t.Errorf("jobs[%s] should not be found (no artifact stored)", job)
		}
	}
}

func TestHandleArtifactsDraftAndReviewStops(t *testing.T) {
	host := &fakeHost{artifacts: map[string]map[string][]byte{
		"ext:sleeper:" + testLeague + ":draft:draft":    {"draft": []byte(`{"season":"2026"}`)},
		"ext:sleeper:" + testLeague + ":review:history": {"history": []byte(`{"season":"2025"}`)},
	}}
	_, r := newTestExtension(t, host.sdkHost(), config{})

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/artifacts?league_id="+testLeague+"&stop=draft", nil))
	var draftResp artifactsResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &draftResp)
	if !draftResp.Jobs["draft"].Found {
		t.Errorf("draft stop should read the draft artifact: %+v", draftResp.Jobs)
	}
	if len(draftResp.Talks) != 0 {
		t.Errorf("draft stop must not include trade talks, got %d", len(draftResp.Talks))
	}

	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/artifacts?league_id="+testLeague+"&stop=review", nil))
	var reviewResp artifactsResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &reviewResp)
	if !reviewResp.Jobs["history"].Found {
		t.Errorf("review stop should read the history artifact: %+v", reviewResp.Jobs)
	}
}

func TestHandleArtifactsBadStop(t *testing.T) {
	_, r := newTestExtension(t, sdk.Host{}, config{})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/artifacts?league_id="+testLeague+"&stop=nope", nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestHandleArtifactsFixtureFallback(t *testing.T) {
	_, r := newTestExtension(t, sdk.Host{}, config{Fixture: true})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/artifacts?league_id="+testLeague+"&stop=2", nil))
	var resp artifactsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	lineup := resp.Jobs["lineup"]
	if !lineup.Found || !lineup.Example {
		t.Errorf("lineup = %+v, want found+example from the fixture", lineup)
	}
	if len(lineup.Data) == 0 {
		t.Error("fixture data is empty")
	}
	if resp.SeasonNotes == nil || !resp.SeasonNotes.Found || !resp.SeasonNotes.Example {
		t.Errorf("season_notes = %+v, want a fixture example", resp.SeasonNotes)
	}
	if len(resp.Talks) != 1 || !resp.Talks[0].Example {
		t.Errorf("talks = %+v, want one example talk", resp.Talks)
	}
}

// TestHandleArtifactsRealTradeTalkDiscovery exercises readTradeTalks'
// actual discovery path (candidate chat ids built from real league
// members), not just the fixture fallback TestHandleArtifactsFixtureFallback covers.
func TestHandleArtifactsRealTradeTalkDiscovery(t *testing.T) {
	const riceCookerOwnerID = "740613226189987840" // pirates5 / "Rice Cooker" in testLeague's fixture users
	chatID := "ext:sleeper:" + testLeague + ":trade:" + riceCookerOwnerID
	host := &fakeHost{artifacts: map[string]map[string][]byte{
		chatID: {"trade": []byte(`{"partner":"Rice Cooker","partner_id":"` + riceCookerOwnerID + `","status":"open","offers":[]}`)},
	}}
	_, r := newTestExtension(t, host.sdkHost(), config{})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/artifacts?league_id="+testLeague+"&stop=2", nil))
	var resp artifactsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Talks) != 1 {
		t.Fatalf("talks = %+v, want exactly the one stored talk", resp.Talks)
	}
	got := resp.Talks[0]
	if got.PartnerID != riceCookerOwnerID || got.Partner != "Rice Cooker" || !got.Found || got.Example {
		t.Errorf("talk = %+v, want partner_id=%q partner=Rice Cooker found=true example=false", got, riceCookerOwnerID)
	}
}

// TestHandleArtifactsFixtureDoesNotShadowRealArtifact pins readArtifact's
// real-first order: with Fixture on AND a real chat, the real one must win.
func TestHandleArtifactsFixtureDoesNotShadowRealArtifact(t *testing.T) {
	chatID := "ext:sleeper:" + testLeague + ":2:lineup"
	host := &fakeHost{artifacts: map[string]map[string][]byte{
		chatID: {"lineup": []byte(`{"week":2,"team":"real"}`)},
	}}
	_, r := newTestExtension(t, host.sdkHost(), config{Fixture: true})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/artifacts?league_id="+testLeague+"&stop=2", nil))
	var resp artifactsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	lineup := resp.Jobs["lineup"]
	if !lineup.Found || lineup.Example {
		t.Errorf("lineup = %+v, want Found && !Example (real artifact must win over the fixture)", lineup)
	}
	if string(lineup.Data) != `{"week":2,"team":"real"}` {
		t.Errorf("data = %s, want the real stored bytes, not the fixture", lineup.Data)
	}
}

func TestHandleArtifactsNoFixtureLeavesEmpty(t *testing.T) {
	_, r := newTestExtension(t, sdk.Host{}, config{Fixture: false})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/artifacts?league_id="+testLeague+"&stop=2", nil))
	var resp artifactsResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	for job, env := range resp.Jobs {
		if env.Found {
			t.Errorf("jobs[%s] found with no host and fixture off: %+v", job, env)
		}
	}
	if len(resp.Talks) != 0 {
		t.Errorf("talks = %+v, want none with no host and fixture off", resp.Talks)
	}
}

func TestHandleJobsDispatchesChatIDAndAppendsTurn(t *testing.T) {
	host := &fakeHost{artifacts: map[string]map[string][]byte{}}
	_, r := newTestExtension(t, host.sdkHost(), config{})
	body := strings.NewReader(`{"league_id":"` + testLeague + `","stop":"2","job":"lineup"}`)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/jobs", body))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	var resp jobResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	wantChatID := "ext:sleeper:" + testLeague + ":2:lineup"
	if resp.ChatID != wantChatID {
		t.Errorf("chat_id = %q, want %q", resp.ChatID, wantChatID)
	}
	if resp.ChatURL != "/chat/"+wantChatID {
		t.Errorf("chat_url = %q, want /chat/%s", resp.ChatURL, wantChatID)
	}

	// A second POST for the same job/stop must target the same LocalID, so
	// the host appends a turn instead of starting a second chat.
	rec2 := httptest.NewRecorder()
	r.ServeHTTP(rec2, httptest.NewRequest(http.MethodPost, "/api/jobs", strings.NewReader(`{"league_id":"`+testLeague+`","stop":"2","job":"lineup"}`)))
	if len(host.dispatched) != 2 {
		t.Fatalf("dispatched %d requests, want 2", len(host.dispatched))
	}
	if host.dispatched[0].Chat.LocalID != host.dispatched[1].Chat.LocalID {
		t.Errorf("repeat dispatch used a different LocalID: %q vs %q", host.dispatched[0].Chat.LocalID, host.dispatched[1].Chat.LocalID)
	}
}

func TestHandleJobsTradeRequiresPartner(t *testing.T) {
	host := &fakeHost{artifacts: map[string]map[string][]byte{}}
	_, r := newTestExtension(t, host.sdkHost(), config{})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/jobs", strings.NewReader(`{"league_id":"`+testLeague+`","stop":"2","job":"trade"}`)))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}

	rec2 := httptest.NewRecorder()
	body := `{"league_id":"` + testLeague + `","stop":"2","job":"trade","args":{"partner":"Rice Cooker"}}`
	r.ServeHTTP(rec2, httptest.NewRequest(http.MethodPost, "/api/jobs", strings.NewReader(body)))
	if rec2.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec2.Code, rec2.Body)
	}
	var resp jobResponse
	_ = json.Unmarshal(rec2.Body.Bytes(), &resp)
	want := "ext:sleeper:" + testLeague + ":trade:Rice Cooker"
	if resp.ChatID != want {
		t.Errorf("chat_id = %q, want %q", resp.ChatID, want)
	}
}

// TestHandleJobsTradeKeysOnUserIDNotName pins the dispatch side of the
// rename-safety contract (TestHandleArtifactsRealTradeTalkDiscovery pins
// the read side): the chat id keys on the user_id in args.partner, and
// args.partner_name (display-only) still reaches the chat title.
func TestHandleJobsTradeKeysOnUserIDNotName(t *testing.T) {
	const riceCookerOwnerID = "740613226189987840"
	host := &fakeHost{artifacts: map[string]map[string][]byte{}}
	_, r := newTestExtension(t, host.sdkHost(), config{})
	body := `{"league_id":"` + testLeague + `","stop":"2","job":"trade","args":{"partner":"` + riceCookerOwnerID + `","partner_name":"Rice Cooker"}}`
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/jobs", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	var resp jobResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	wantChatID := "ext:sleeper:" + testLeague + ":trade:" + riceCookerOwnerID
	if resp.ChatID != wantChatID {
		t.Errorf("chat_id = %q, want %q (keyed on the user_id, not the display name)", resp.ChatID, wantChatID)
	}
	if len(host.dispatched) != 1 {
		t.Fatalf("dispatched %d requests, want 1", len(host.dispatched))
	}
	wantTitle := "Sleeper trade talk with Rice Cooker"
	if host.dispatched[0].Chat.Title != wantTitle {
		t.Errorf("title = %q, want %q", host.dispatched[0].Chat.Title, wantTitle)
	}
}

func TestHandleJobsArgsReachTheMessage(t *testing.T) {
	host := &fakeHost{artifacts: map[string]map[string][]byte{}}
	_, r := newTestExtension(t, host.sdkHost(), config{})
	body := `{"league_id":"` + testLeague + `","stop":"2","job":"trade","args":{"partner":"Rice Cooker","give":"7594,8142","get":"9997"}}`
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/jobs", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	if len(host.dispatched) != 1 {
		t.Fatalf("dispatched %d requests, want 1", len(host.dispatched))
	}
	msg := host.dispatched[0].Ask.Message
	for _, want := range []string{"partner: Rice Cooker", "give: 7594,8142", "get: 9997"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q missing %q", msg, want)
		}
	}
}

func TestHandleJobsTrendsAlsoDispatchesSeasonNotes(t *testing.T) {
	host := &fakeHost{artifacts: map[string]map[string][]byte{}}
	_, r := newTestExtension(t, host.sdkHost(), config{})
	body := `{"league_id":"` + testLeague + `","stop":"2","job":"trends"}`
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/jobs", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	if len(host.dispatched) != 2 {
		t.Fatalf("dispatched %d requests, want 2 (trends + season-notes): %+v", len(host.dispatched), host.dispatched)
	}
	wantLocalID := testLeague + ":season-notes"
	if host.dispatched[1].Chat.LocalID != wantLocalID {
		t.Errorf("second dispatch LocalID = %q, want %q", host.dispatched[1].Chat.LocalID, wantLocalID)
	}
}

func TestHandleJobsBadJobForStop(t *testing.T) {
	host := &fakeHost{artifacts: map[string]map[string][]byte{}}
	_, r := newTestExtension(t, host.sdkHost(), config{})
	rec := httptest.NewRecorder()
	body := `{"league_id":"` + testLeague + `","stop":"draft","job":"lineup"}`
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/jobs", strings.NewReader(body)))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestHandleJobsBadInput(t *testing.T) {
	host := &fakeHost{artifacts: map[string]map[string][]byte{}}
	_, r := newTestExtension(t, host.sdkHost(), config{})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/jobs", strings.NewReader(`not json`)))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestIndexPageServes(t *testing.T) {
	_, r := newTestExtension(t, sdk.Host{}, config{})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "<title>Sleeper") {
		t.Errorf("index page missing expected title, got: %s", rec.Body.String()[:min(200, rec.Body.Len())])
	}
}
