package sleeper

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// mockServer replays testdata/get fixtures the same way cmd/qa-mock does,
// so client_test.go never touches the real Sleeper API.
func mockServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key, err := fixtureKey(r.URL.RequestURI())
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		b, err := os.ReadFile(filepath.Join("testdata", "get", key))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(b)
	}))
}

func newTestClient(t *testing.T) *Client {
	t.Helper()
	srv := mockServer(t)
	t.Cleanup(srv.Close)
	c, err := NewClient(srv.URL, nil)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return c
}

func TestClientState(t *testing.T) {
	c := newTestClient(t)
	st, err := c.State(context.Background())
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if st.Week == nil || *st.Week != 2 {
		t.Errorf("week = %v, want 2", st.Week)
	}
}

func TestClientLeagueAndRosters(t *testing.T) {
	c := newTestClient(t)
	ctx := context.Background()
	league, err := c.League(ctx, testLeague)
	if err != nil {
		t.Fatalf("League: %v", err)
	}
	if league.Name == nil || *league.Name == "" {
		t.Errorf("league name empty")
	}
	rosters, err := c.Rosters(ctx, testLeague)
	if err != nil {
		t.Fatalf("Rosters: %v", err)
	}
	if len(rosters) == 0 {
		t.Errorf("expected rosters, got none")
	}
}

func TestClientCachesRepeatedCalls(t *testing.T) {
	c := newTestClient(t)
	ctx := context.Background()
	if _, err := c.State(ctx); err != nil {
		t.Fatalf("State: %v", err)
	}
	c.mu.Lock()
	entry, ok := c.cache["state"]
	c.mu.Unlock()
	if !ok {
		t.Fatalf("expected state cached")
	}
	if _, err := c.State(ctx); err != nil {
		t.Fatalf("State (cached): %v", err)
	}
	c.mu.Lock()
	entry2 := c.cache["state"]
	c.mu.Unlock()
	if !entry.expires.Equal(entry2.expires) {
		t.Errorf("expected cache hit to reuse the same entry, expiry changed")
	}
}

func TestResolvePlayerAndChain(t *testing.T) {
	c := newTestClient(t)
	ctx := context.Background()
	if _, err := c.PlayersDump(ctx); err != nil {
		t.Fatalf("PlayersDump: %v", err)
	}
	if ids := c.ResolvePlayer("Buccaneers"); len(ids) == 0 || ids[0] != "TB" {
		t.Errorf("ResolvePlayer(Buccaneers) = %v, want [TB]", ids)
	}
	if ids := c.ResolvePlayer("tb"); len(ids) == 0 || ids[0] != "TB" {
		t.Errorf("ResolvePlayer(tb) = %v, want [TB]", ids)
	}

	chain, err := c.Chain(ctx, testLeague)
	if err != nil {
		t.Fatalf("Chain: %v", err)
	}
	if len(chain) != 2 {
		t.Fatalf("Chain length = %d, want 2 (current + one past season)", len(chain))
	}
	if chain[0].LeagueId == nil || *chain[0].LeagueId != testLeague {
		t.Errorf("chain[0] = %v, want current league %s", chain[0].LeagueId, testLeague)
	}
	if chain[1].LeagueId == nil || *chain[1].LeagueId != pastLeague {
		t.Errorf("chain[1] = %v, want past league %s", chain[1].LeagueId, pastLeague)
	}
}
