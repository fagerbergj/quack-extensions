package sleeper

import (
	"context"
	"testing"

	"github.com/fagerbergj/quack-extensions/sleeper/sleepergen"
)

func TestGetDraft(t *testing.T) {
	e := testExtension(t)
	got, err := e.getDraft(context.Background(), draftArgs{LeagueID: testLeague})
	if err != nil {
		t.Fatalf("getDraft: %v", err)
	}
	if got.Status != "complete" {
		t.Errorf("status = %q, want complete", got.Status)
	}
	if len(got.Picks) != 140 {
		t.Errorf("picks = %d, want 140", len(got.Picks))
	}
	if len(got.Order) != 10 {
		t.Errorf("order = %d, want 10", len(got.Order))
	}
	if got.OnClock != nil {
		t.Errorf("on_clock = %+v, want nil for a completed draft", got.OnClock)
	}
	first := got.Picks[0]
	if first.PickNo != 1 || first.PlayerID != "9221" {
		t.Errorf("first pick = %+v, want pick 1 / player 9221 (Jahmyr Gibbs)", first)
	}
	// The ADP sentinel: a player with no real draft consensus reads 999/1000,
	// never a literal 999th-overall pick.
	var sawNoADP bool
	for _, p := range got.Picks {
		if p.NoADP && p.ADP < noADPSentinel {
			t.Errorf("pick %+v: no_adp=true but adp=%v < sentinel", p, p.ADP)
		}
		if p.NoADP {
			sawNoADP = true
		}
	}
	if !sawNoADP {
		t.Error("expected at least one drafted player with no ADP")
	}
}

func TestOnClockSnakeMath(t *testing.T) {
	names := map[int]string{1: "a", 2: "b", 3: "c"}
	slotToRoster := map[string]int{"1": 1, "2": 2, "3": 3}
	d := &sleepergen.Draft{
		Status: "drafting", Type: "snake",
		Settings:       map[string]int{"teams": 3, "rounds": 2},
		SlotToRosterId: &slotToRoster,
	}
	cases := []struct {
		picksMade  int
		wantSlot   int
		wantRoster int
	}{
		{0, 1, 1}, // round 1, pick 1: slot 1
		{2, 3, 3}, // round 1, pick 3: slot 3
		{3, 3, 3}, // round 2, pick 4: snake reverses, slot 3 goes first
		{5, 1, 1}, // round 2, pick 6: slot 1 goes last
	}
	for _, tc := range cases {
		got := onClock(d, tc.picksMade, names)
		if got == nil {
			t.Fatalf("picksMade=%d: onClock = nil, want a pick", tc.picksMade)
		}
		if got.Slot != tc.wantSlot || got.RosterID != tc.wantRoster {
			t.Errorf("picksMade=%d: got slot=%d roster=%d, want slot=%d roster=%d", tc.picksMade, got.Slot, got.RosterID, tc.wantSlot, tc.wantRoster)
		}
	}
	if got := onClock(d, 6, names); got != nil {
		t.Errorf("draft complete (6/6 picks): onClock = %+v, want nil", got)
	}
}

func TestOnClockNilWhenNotDrafting(t *testing.T) {
	slotToRoster := map[string]int{"1": 1}
	d := &sleepergen.Draft{Status: "complete", Type: "snake", SlotToRosterId: &slotToRoster}
	if got := onClock(d, 0, nil); got != nil {
		t.Errorf("onClock = %+v, want nil for a non-drafting status", got)
	}
}

func TestBestAvailableExcludesPickedAndCapsPerPosition(t *testing.T) {
	dump := playerDumpAt("rb1", "RB", "rb2", "RB", "rb3", "RB", "qb1", "QB")
	proj := map[string]sleepergen.StatMap{
		"rb1": {"pts_ppr": 20}, "rb2": {"pts_ppr": 15}, "rb3": {"pts_ppr": 10}, "qb1": {"pts_ppr": 25},
	}
	picks := []sleepergen.DraftPick{{PlayerId: "rb1"}}
	got := bestAvailable(dump, picks, proj)
	rbs := got["RB"]
	if len(rbs) != 2 {
		t.Fatalf("RB best available = %+v, want 2 (rb1 picked)", rbs)
	}
	if rbs[0].PlayerID != "rb2" || rbs[1].PlayerID != "rb3" {
		t.Errorf("RB order = %+v, want [rb2, rb3] by projection desc", rbs)
	}
	if len(got["QB"]) != 1 || got["QB"][0].PlayerID != "qb1" {
		t.Errorf("QB best available = %+v, want [qb1]", got["QB"])
	}
	if len(got["WR"]) != 0 {
		t.Errorf("WR best available = %+v, want none in the dump", got["WR"])
	}
}
