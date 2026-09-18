package sleeper

import (
	"context"
	"testing"
)

// TestGetHistoryKnownValues pins derived numbers independently checked
// against the raw fixtures: Barkley drafted RB3 finished RB14, 88.5% 2025 efficiency, 1 close loss (<10pt margin).
func TestGetHistoryKnownValues(t *testing.T) {
	e := testExtension(t)
	got, err := e.getHistory(context.Background(), historyArgs{})
	if err != nil {
		t.Fatalf("getHistory: %v", err)
	}
	if len(got.Seasons) != 2 {
		t.Fatalf("seasons = %d, want 2 (2026, 2025 - Chain stops at the recorded hop)", len(got.Seasons))
	}
	var season2025 *seasonHistory
	for i := range got.Seasons {
		if got.Seasons[i].Season == "2025" {
			season2025 = &got.Seasons[i]
		}
	}
	if season2025 == nil {
		t.Fatal("expected a 2025 season in the chain")
	}
	if season2025.LineupEfficiencyPct != 88.5 {
		t.Errorf("2025 lineup_efficiency_pct = %v, want 88.5", season2025.LineupEfficiencyPct)
	}
	var barkley *draftReportCardEntry
	for i := range season2025.Draft {
		if season2025.Draft[i].PlayerID == "4866" {
			barkley = &season2025.Draft[i]
		}
	}
	if barkley == nil {
		t.Fatal("expected Saquon Barkley (4866) in the 2025 draft report card")
	}
	if barkley.DraftedAsRank != 3 {
		t.Errorf("Barkley drafted_as_rank = %d, want 3 (RB3)", barkley.DraftedAsRank)
	}
	if barkley.FinishRank != 14 {
		t.Errorf("Barkley finish_rank = %d, want 14 (RB14)", barkley.FinishRank)
	}
	if barkley.Verdict != "reach" {
		t.Errorf("Barkley verdict = %q, want reach (RB3 finishing RB14)", barkley.Verdict)
	}
	// 2025 wk14 (margin 1.46) is close; 2026 wk1 (margin 15.34) is not, and
	// 2026 wk2 is in progress and excluded entirely.
	if got.TotalCloseLosses != 1 {
		t.Errorf("total_close_losses = %d, want 1", got.TotalCloseLosses)
	}
	for _, sh := range got.Seasons {
		for _, w := range sh.Weeks {
			if sh.Season == "2026" && w.Week >= 2 {
				t.Errorf("season 2026 week %+v: in-progress weeks must be excluded from hindsight", w)
			}
		}
	}
}

func TestGetHistorySeasonsBackCapsChain(t *testing.T) {
	e := testExtension(t)
	got, err := e.getHistory(context.Background(), historyArgs{SeasonsBack: 1})
	if err != nil {
		t.Fatalf("getHistory: %v", err)
	}
	if len(got.Seasons) != 1 {
		t.Fatalf("seasons = %d, want 1", len(got.Seasons))
	}
	if got.Seasons[0].Season != "2026" {
		t.Errorf("season = %q, want 2026 (the current/most recent)", got.Seasons[0].Season)
	}
}

func TestWeekResultCloseLossFixedMargin(t *testing.T) {
	for _, tc := range []struct {
		mine, opp float32
		wantClose bool
	}{
		{134.48, 135.94, true},  // 2025 wk14: 1.46 margin, close
		{100.66, 116.00, false}, // 2026 wk1: 15.34 margin, not close
		{112.14, 108.86, false}, // a win is never a "close loss"
	} {
		result, close := weekResult(tc.mine, tc.opp)
		if close != tc.wantClose {
			t.Errorf("weekResult(%v, %v) close = %v, want %v (result %s)", tc.mine, tc.opp, close, tc.wantClose, result)
		}
	}
}

func TestBestLineupPointsFlexPicksHighestRemaining(t *testing.T) {
	dump := playerDumpAt("rb1", "RB", "rb2", "RB", "rb3", "RB", "wr1", "WR", "wr2", "WR", "wr3", "WR", "te1", "TE")
	points := map[string]float32{
		"rb1": 20, "rb2": 15, "rb3": 12,
		"wr1": 18, "wr2": 10, "wr3": 8,
		"te1": 5,
	}
	ids := []string{"rb1", "rb2", "rb3", "wr1", "wr2", "wr3", "te1"}
	positions := []string{"RB", "RB", "WR", "WR", "TE", "FLEX", "BN"}
	got := bestLineupPoints(positions, ids, points, dump)
	// RB1+RB2 (35) + WR1+WR2 (28) + TE1 (5) + FLEX best remaining (RB3=12 > WR3=8) = 80
	want := float32(80)
	if got != want {
		t.Errorf("bestLineupPoints = %v, want %v", got, want)
	}
}
