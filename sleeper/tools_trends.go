package sleeper

import (
	"fmt"
	"time"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
)

// defaultTrendDays: how far back sleeper_trends looks when days is omitted.
const defaultTrendDays = 7

type trendsArgs struct {
	LeagueID  string   `json:"league_id,omitempty"`
	PlayerIDs []string `json:"player_ids,omitempty"`
	Days      int      `json:"days,omitempty"`
}

type playerTrend struct {
	PlayerID            string  `json:"player_id"`
	InjuryStatusChanged bool    `json:"injury_status_changed,omitempty"`
	InjuryStatus        string  `json:"injury_status,omitempty"`
	PracticeChanged     bool    `json:"practice_changed,omitempty"`
	PracticeDescription string  `json:"practice_description,omitempty"`
	DepthChartChanged   bool    `json:"depth_chart_changed,omitempty"`
	DepthChartOrder     int     `json:"depth_chart_order,omitempty"`
	TrendingAddDelta    int     `json:"trending_add_delta,omitempty"`
	OwnedPctDelta       float32 `json:"owned_pct_delta,omitempty"`
}

type trendsResult struct {
	LeagueID  string        `json:"league_id"`
	FromDate  string        `json:"from_date,omitempty"`
	ToDate    string        `json:"to_date,omitempty"`
	Trends    []playerTrend `json:"trends,omitempty"`
	Note      string        `json:"note,omitempty"`
	FetchedAt string        `json:"fetched_at"`
}

func (e *extension) trendsTool() tool.Tool {
	t, _ := functiontool.New[trendsArgs, trendsResult](
		functiontool.Config{
			Name: "sleeper_trends",
			Description: "Diff the extension's stored daily snapshots (injury/practice/depth-chart " +
				"changes, trending add/drop counts, ownership swings) over the last `days` (default 7). " +
				"Optionally filter to `player_ids`. Needs snapshots: daily configured.",
		},
		func(ctx adkagent.Context, a trendsArgs) (trendsResult, error) { return e.getTrends(a) },
	)
	return t
}

func (e *extension) getTrends(a trendsArgs) (trendsResult, error) {
	leagueID, err := e.resolveLeagueID(a.LeagueID)
	if err != nil {
		return trendsResult{}, err
	}
	days := a.Days
	if days <= 0 {
		days = defaultTrendDays
	}
	dates, err := snapshotDates(e.host.DataDir, leagueID)
	if err != nil {
		return trendsResult{}, fmt.Errorf("sleeper_trends: %w", err)
	}
	dates = withinDays(dates, days)
	if len(dates) < 2 {
		return trendsResult{LeagueID: leagueID, Note: "fewer than two snapshots in range; nothing to diff yet", FetchedAt: nowRFC3339()}, nil
	}
	oldSnap, err := readSnapshot(e.host.DataDir, leagueID, dates[0])
	if err != nil {
		return trendsResult{}, fmt.Errorf("sleeper_trends: %w", err)
	}
	newSnap, err := readSnapshot(e.host.DataDir, leagueID, dates[len(dates)-1])
	if err != nil {
		return trendsResult{}, fmt.Errorf("sleeper_trends: %w", err)
	}
	return trendsResult{
		LeagueID: leagueID, FromDate: oldSnap.Date, ToDate: newSnap.Date,
		Trends: diffSnapshots(oldSnap, newSnap, a.PlayerIDs), FetchedAt: nowRFC3339(),
	}, nil
}

// withinDays keeps only dates within the last `days`, oldest first.
func withinDays(dates []string, days int) []string {
	cutoff := time.Now().UTC().AddDate(0, 0, -days).Format(time.DateOnly)
	var out []string
	for _, d := range dates {
		if d >= cutoff {
			out = append(out, d)
		}
	}
	return out
}

// diffSnapshots compares two snapshots player by player; playerIDs, when
// non-empty, restricts the diff to those ids.
func diffSnapshots(oldSnap, newSnap snapshot, playerIDs []string) []playerTrend {
	want := toIDSet(playerIDs)
	var out []playerTrend
	for pid, cur := range newSnap.Players {
		if len(want) > 0 && !want[pid] {
			continue
		}
		prev := oldSnap.Players[pid]
		trend := playerTrend{PlayerID: pid}
		changed := false
		if prev.InjuryStatus != cur.InjuryStatus {
			trend.InjuryStatusChanged, trend.InjuryStatus, changed = true, cur.InjuryStatus, true
		}
		if prev.PracticeDescription != cur.PracticeDescription {
			trend.PracticeChanged, trend.PracticeDescription, changed = true, cur.PracticeDescription, true
		}
		if prev.DepthChartOrder != cur.DepthChartOrder {
			trend.DepthChartChanged, trend.DepthChartOrder, changed = true, cur.DepthChartOrder, true
		}
		if d := cur.OwnedPct - prev.OwnedPct; d != 0 {
			trend.OwnedPctDelta, changed = d, true
		}
		if d := newSnap.TrendingAdd[pid] - oldSnap.TrendingAdd[pid]; d != 0 {
			trend.TrendingAddDelta, changed = d, true
		}
		if changed {
			out = append(out, trend)
		}
	}
	return out
}

func toIDSet(ids []string) map[string]bool {
	out := make(map[string]bool, len(ids))
	for _, id := range ids {
		out[id] = true
	}
	return out
}
