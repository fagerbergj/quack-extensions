package sleeper

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/fagerbergj/quack-extensions/sleeper/sleepergen"
)

// fixtureCase pairs a recorded request with the generated type its 200
// response must decode into. url mirrors what client.go actually requests,
// so a fixture drifting from the real shape fails here, not in prod.
type fixtureCase struct {
	name string
	url  string
	dst  func() any
}

// fixtureKey must match cmd/qa-mock's key function exactly - both compute
// the fixture filename from the same GET request shape.
func fixtureKey(rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	key := "GET_" + u.Path
	if u.RawQuery != "" {
		key += "_" + u.RawQuery
	}
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:8]) + ".json", nil
}

const (
	testLeague = "1356407594683482112"
	testUser   = "860211057418018816"
	testDraft  = "1356407594700242944"
	pastLeague = "1257102049204514816"
	pastDraft  = "1257102049204514817"
)

func fixtureCases() []fixtureCase {
	return []fixtureCase{
		{"user", "/v1/user/" + testUser, func() any { return &sleepergen.User{} }},
		{"user leagues", "/v1/user/" + testUser + "/leagues/nfl/2026", func() any { return &[]sleepergen.League{} }},
		{"league", "/v1/league/" + testLeague, func() any { return &sleepergen.League{} }},
		{"rosters", "/v1/league/" + testLeague + "/rosters", func() any { return &[]sleepergen.Roster{} }},
		{"league users", "/v1/league/" + testLeague + "/users", func() any { return &[]sleepergen.LeagueUser{} }},
		{"matchups w2", "/v1/league/" + testLeague + "/matchups/2", func() any { return &[]sleepergen.Matchup{} }},
		{"matchups w1", "/v1/league/" + testLeague + "/matchups/1", func() any { return &[]sleepergen.Matchup{} }},
		{"transactions w2", "/v1/league/" + testLeague + "/transactions/2", func() any { return &[]sleepergen.Transaction{} }},
		{"transactions w1", "/v1/league/" + testLeague + "/transactions/1", func() any { return &[]sleepergen.Transaction{} }},
		{"winners bracket", "/v1/league/" + testLeague + "/winners_bracket", func() any { return &[]sleepergen.BracketMatch{} }},
		{"losers bracket", "/v1/league/" + testLeague + "/losers_bracket", func() any { return &[]sleepergen.BracketMatch{} }},
		{"traded picks", "/v1/league/" + testLeague + "/traded_picks", func() any { return &[]sleepergen.TradedPick{} }},
		{"league drafts", "/v1/league/" + testLeague + "/drafts", func() any { return &[]sleepergen.Draft{} }},
		{"draft", "/v1/draft/" + testDraft, func() any { return &sleepergen.Draft{} }},
		{"draft picks", "/v1/draft/" + testDraft + "/picks", func() any { return &[]sleepergen.DraftPick{} }},
		{"draft traded picks", "/v1/draft/" + testDraft + "/traded_picks", func() any { return &[]sleepergen.TradedPick{} }},
		{"state", "/v1/state/nfl", func() any { return &sleepergen.NflState{} }},
		{"trending add", "/v1/players/nfl/trending/add?lookback_hours=24&limit=25", func() any { return &[]sleepergen.TrendingPlayer{} }},
		{"trending drop", "/v1/players/nfl/trending/drop?lookback_hours=24&limit=25", func() any { return &[]sleepergen.TrendingPlayer{} }},
		{"one player", "/v1/players/nfl/6797", func() any { return &sleepergen.Player{} }},
		{"players dump", "/v1/players/nfl", func() any { return &map[string]sleepergen.Player{} }},
		{"projections w2", "/v1/projections/nfl/regular/2026/2", func() any { return &map[string]sleepergen.StatMap{} }},
		{"stats w2", "/v1/stats/nfl/regular/2026/2", func() any { return &map[string]sleepergen.StatMap{} }},
		{"depth chart", "/players/nfl/TB/depth_chart", func() any { return &map[string][]string{} }},
		{"schedule", "/schedule/nfl/regular/2026", func() any { return &[]sleepergen.Game{} }},
		{"research", "/players/nfl/research/regular/2026/2", func() any { return &map[string]sleepergen.ResearchEntry{} }},
		{"player season stats", "/stats/nfl/player/6797?season_type=regular&season=2026", func() any { return &sleepergen.PlayerStatEntry{} }},
		{"past league", "/v1/league/" + pastLeague, func() any { return &sleepergen.League{} }},
		{"past rosters", "/v1/league/" + pastLeague + "/rosters", func() any { return &[]sleepergen.Roster{} }},
		{"past winners bracket", "/v1/league/" + pastLeague + "/winners_bracket", func() any { return &[]sleepergen.BracketMatch{} }},
		{"past matchups w1", "/v1/league/" + pastLeague + "/matchups/1", func() any { return &[]sleepergen.Matchup{} }},
		{"past matchups w14", "/v1/league/" + pastLeague + "/matchups/14", func() any { return &[]sleepergen.Matchup{} }},
		{"past transactions w1", "/v1/league/" + pastLeague + "/transactions/1", func() any { return &[]sleepergen.Transaction{} }},
		{"past draft", "/v1/draft/" + pastDraft, func() any { return &sleepergen.Draft{} }},
		{"past draft picks", "/v1/draft/" + pastDraft + "/picks", func() any { return &[]sleepergen.DraftPick{} }},
		{"past draft traded picks", "/v1/draft/" + pastDraft + "/traded_picks", func() any { return &[]sleepergen.TradedPick{} }},
		{"season stats 2025", "/v1/stats/nfl/regular/2025", func() any { return &map[string]sleepergen.StatMap{} }},
	}
}

// TestFixturesMatchGeneratedTypes decodes every recorded fixture with
// unknown fields disallowed - a field Sleeper renamed or dropped fails here
// instead of surfacing as a silent zero value in prod.
func TestFixturesMatchGeneratedTypes(t *testing.T) {
	for _, tc := range fixtureCases() {
		t.Run(tc.name, func(t *testing.T) {
			key, err := fixtureKey(tc.url)
			if err != nil {
				t.Fatalf("fixtureKey: %v", err)
			}
			path := filepath.Join("testdata", "get", key)
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read fixture %s (for %s): %v", path, tc.url, err)
			}
			dec := json.NewDecoder(bytes.NewReader(data))
			dec.DisallowUnknownFields()
			if err := dec.Decode(tc.dst()); err != nil {
				t.Fatalf("decode %s into generated type: %v", path, err)
			}
		})
	}
}

// TestAllFixturesCovered catches an orphaned recording no case exercises -
// otherwise a stale fixture rots un-decoded and drift goes unnoticed.
func TestAllFixturesCovered(t *testing.T) {
	want := map[string]bool{}
	for _, tc := range fixtureCases() {
		key, err := fixtureKey(tc.url)
		if err != nil {
			t.Fatalf("fixtureKey: %v", err)
		}
		want[key] = true
	}
	entries, err := os.ReadDir(filepath.Join("testdata", "get"))
	if err != nil {
		t.Fatalf("read testdata/get: %v", err)
	}
	for _, e := range entries {
		if !want[e.Name()] {
			t.Errorf("fixture %s has no contract-test case covering it", e.Name())
		}
	}
}
