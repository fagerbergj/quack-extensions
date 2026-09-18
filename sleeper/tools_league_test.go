package sleeper

import (
	"context"
	"testing"

	"github.com/fagerbergj/quack-extensions/sleeper/sleepergen"
)

func TestGetUser(t *testing.T) {
	e := testExtension(t)
	got, err := e.getUser(context.Background(), userArgs{})
	if err != nil {
		t.Fatalf("getUser: %v", err)
	}
	if got.DisplayName != "jffagerberg" {
		t.Errorf("display_name = %q, want jffagerberg", got.DisplayName)
	}
	if len(got.Leagues) == 0 {
		t.Error("expected at least one league")
	}
	if got.FetchedAt == "" {
		t.Error("expected fetched_at to be set")
	}
}

func TestGetLeague(t *testing.T) {
	e := testExtension(t)
	got, err := e.getLeague(context.Background(), leagueArgs{})
	if err != nil {
		t.Fatalf("getLeague: %v", err)
	}
	if got.Name != "Roger is a Clown" {
		t.Errorf("name = %q, want %q", got.Name, "Roger is a Clown")
	}
	if got.Week != 2 {
		t.Errorf("week = %d, want 2", got.Week)
	}
	if got.WaiverType != "rolling" {
		t.Errorf("waiver_type = %q, want rolling", got.WaiverType)
	}
	if got.ReserveSlots != 1 || got.TaxiSlots != 0 {
		t.Errorf("reserve_slots/taxi_slots = %d/%d, want 1/0", got.ReserveSlots, got.TaxiSlots)
	}
	if got.PlayoffTeams != 6 || got.WaiverBudget != 100 {
		t.Errorf("playoff_teams/waiver_budget = %d/%d, want 6/100", got.PlayoffTeams, got.WaiverBudget)
	}
}

func TestGetSchedule(t *testing.T) {
	e := testExtension(t)
	got, err := e.getSchedule(context.Background(), scheduleArgs{})
	if err != nil {
		t.Fatalf("getSchedule: %v", err)
	}
	if got.Week != 2 {
		t.Errorf("week = %d, want 2 (current week)", got.Week)
	}
	if len(got.Games) == 0 {
		t.Error("expected week-2 games")
	}
	for _, g := range got.Games {
		if g.Home == "" || g.Away == "" {
			t.Errorf("game %+v missing home/away", g)
		}
	}
}

// TestResolveWeekRequiresExplicitWeekForPinnedSeason covers the fix: a
// config season that isn't the live NFL season has no borrowable "current week".
func TestResolveWeekRequiresExplicitWeekForPinnedSeason(t *testing.T) {
	state := &sleepergen.NflState{Season: "2026", Week: 2}
	if _, err := resolveWeek(0, "2025", state); err == nil {
		t.Fatal("expected an error: season 2025 is pinned but the live NFL season is 2026")
	}
	got, err := resolveWeek(1, "2025", state)
	if err != nil || got != 1 {
		t.Errorf("resolveWeek(1, ...) = %d, %v; want 1, nil (explicit week bypasses the mismatch)", got, err)
	}
	got, err = resolveWeek(0, "2026", state)
	if err != nil || got != 2 {
		t.Errorf("resolveWeek(0, matching season) = %d, %v; want 2, nil", got, err)
	}
}
