package sleeper

import (
	"context"
	"fmt"
	"testing"
	"time"

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
		if g.Home == "" || g.Away == "" || g.Kickoff == "" || g.Roof == "" {
			t.Errorf("game %+v missing home/away/kickoff/roof", g)
		}
	}
	// The fixture's schedule status is a stale "pre_game"; locked must follow the kickoff instead.
	g := got.Games[0]
	want := scheduleGame{GameID: "202610201", Home: "ARI", Away: "SEA", Date: "2026-09-20", Status: "pre_game",
		Kickoff: "2026-09-20T20:25:00Z", Venue: "State Farm Stadium", City: "Glendale", Roof: "retractable_dome"}
	if g.Locked == nil || !*g.Locked {
		t.Errorf("games[0].locked = %v, want true", g.Locked)
	}
	g.Locked = nil
	if g != want {
		t.Errorf("games[0] = %+v, want %+v", g, want)
	}
	if got.Note != "" {
		t.Errorf("note = %q, want empty", got.Note)
	}
}

func TestGetScheduleWithoutScores(t *testing.T) {
	e := testExtension(t)
	got, err := e.getSchedule(context.Background(), scheduleArgs{Week: 1}) // no week-1 scores fixture: 404
	if err != nil {
		t.Fatalf("getSchedule: %v", err)
	}
	if got.Note == "" || len(got.Games) == 0 {
		t.Fatalf("want games plus a note, got %+v", got)
	}
	for _, g := range got.Games {
		if g.Kickoff != "" || g.Locked == nil || !*g.Locked {
			t.Errorf("game %+v: want no kickoff, locked from status complete", g)
		}
	}
}

func TestWithKickoff(t *testing.T) {
	now := time.Date(2026, 9, 27, 17, 0, 0, 0, time.UTC)
	score := func(start time.Time, status string) *sleepergen.GameScore {
		return &sleepergen.GameScore{StartTime: start.UnixMilli(), Metadata: sleepergen.GameScoreMetadata{Status: &status}}
	}
	yes, no := true, false
	tests := []struct {
		name       string
		status     string
		score      *sleepergen.GameScore
		wantKick   string
		wantTBD    bool
		wantLocked *bool
	}{
		{"kickoff now locks", "pre_game", score(now, "created"), "2026-09-27T17:00:00Z", false, &yes},
		{"future kickoff open", "pre_game", score(now.Add(time.Minute), "scheduled"), "2026-09-27T17:01:00Z", false, &no},
		{"stale future kickoff, complete", "complete", score(now.Add(time.Hour), "scheduled"), "2026-09-27T18:00:00Z", false, &yes},
		{"flex placeholder in the past", "pre_game", score(now.Add(-time.Hour), "flex-schedule"), "", true, nil},
		{"flex, in progress", "in_game", score(now.Add(time.Hour), "flex-schedule"), "", true, &yes},
		{"no start_time", "pre_game", &sleepergen.GameScore{}, "", false, nil},
		{"postponed keeps stale start_time", "postponed", score(now.Add(-time.Hour), "postponed"), "", false, nil},
		{"cancelled after it started", "complete", score(now.Add(-time.Hour), "cancelled"), "", false, &yes},
		{"canceled in schedule only", "canceled", score(now.Add(-time.Hour), "scheduled"), "", false, nil},
		{"no score, in progress", "in_game", nil, "", false, &yes},
		{"no score, pre_game", "pre_game", nil, "", false, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := withKickoff(scheduleGame{Status: tc.status}, tc.score, now)
			if got.Kickoff != tc.wantKick || got.KickoffTBD != tc.wantTBD || !sameBoolPtr(got.Locked, tc.wantLocked) {
				t.Errorf("got kickoff=%q tbd=%v locked=%v, want %q %v %v", got.Kickoff, got.KickoffTBD, fmtBoolPtr(got.Locked), tc.wantKick, tc.wantTBD, fmtBoolPtr(tc.wantLocked))
			}
			if got.Venue != "" || got.Roof != "" {
				t.Errorf("nil stadium produced venue %q roof %q", got.Venue, got.Roof)
			}
		})
	}
}

func sameBoolPtr(a, b *bool) bool { return (a == nil) == (b == nil) && (a == nil || *a == *b) }

func fmtBoolPtr(b *bool) string {
	if b == nil {
		return "<unknown>"
	}
	return fmt.Sprint(*b)
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
