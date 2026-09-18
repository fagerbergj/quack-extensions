package sleeper

import (
	"context"
	"testing"
)

func TestGetMatchup(t *testing.T) {
	e := testExtension(t)
	got, err := e.getMatchup(context.Background(), matchupArgs{Week: 2})
	if err != nil {
		t.Fatalf("getMatchup: %v", err)
	}
	if got.Week != 2 {
		t.Errorf("week = %d, want 2", got.Week)
	}
	if got.Me.RosterID == 0 {
		t.Error("expected my roster to resolve")
	}
	if len(got.Me.Starters) != 9 {
		t.Errorf("my starters = %d, want 9", len(got.Me.Starters))
	}
	if got.Opponent.RosterID == 0 {
		t.Error("expected an opponent to resolve from matchup_id pairing")
	}
	if got.MatchupID == 0 {
		t.Error("expected a matchup_id")
	}
}

func TestGetMatchupExplicitRoster(t *testing.T) {
	e := testExtension(t)
	got, err := e.getMatchup(context.Background(), matchupArgs{Week: 2, RosterID: 2})
	if err != nil {
		t.Fatalf("getMatchup: %v", err)
	}
	if got.Me.RosterID != 2 {
		t.Errorf("roster_id = %d, want 2", got.Me.RosterID)
	}
}
