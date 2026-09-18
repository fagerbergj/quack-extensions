// The daily snapshot ticker (sdk.Starter) and the on-disk format
// sleeper_trends diffs; fetches only, per sleeper.go's Start.
package sleeper

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/fagerbergj/quack-extensions/sleeper/sleepergen"
)

// snapshotTickInterval: once a day, per the extension's config contract.
const snapshotTickInterval = 24 * time.Hour

// playerSnapshot is the sliver of a player's state trends diffs day to day.
type playerSnapshot struct {
	InjuryStatus        string  `json:"injury_status,omitempty"`
	PracticeDescription string  `json:"practice_description,omitempty"`
	DepthChartOrder     int     `json:"depth_chart_order,omitempty"`
	OwnedPct            float32 `json:"owned_pct,omitempty"`
}

type snapshot struct {
	Date         string                    `json:"date"`
	Players      map[string]playerSnapshot `json:"players"`
	TrendingAdd  map[string]int            `json:"trending_add,omitempty"`
	TrendingDrop map[string]int            `json:"trending_drop,omitempty"`
}

func snapshotDir(dataDir, leagueID string) string {
	return filepath.Join(dataDir, "snapshots", leagueID)
}

func snapshotPath(dataDir, leagueID, date string) string {
	return filepath.Join(snapshotDir(dataDir, leagueID), date+".json")
}

// runSnapshotTicker fetches immediately, then every snapshotTickInterval.
// Errors are logged, never fatal - a missed day just leaves a gap in trends.
func (e *extension) runSnapshotTicker(ctx context.Context) {
	e.snapshotOnce(ctx)
	ticker := time.NewTicker(snapshotTickInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			e.snapshotOnce(ctx)
		}
	}
}

// snapshotOnce writes today's snapshot for the configured default league -
// "configured leagues" in the issue is one league until config grows a list.
func (e *extension) snapshotOnce(ctx context.Context) {
	leagueID := e.cfg.DefaultLeague
	if leagueID == "" {
		return
	}
	snap, err := e.buildSnapshot(ctx, leagueID)
	if err != nil {
		e.logWarn("sleeper: snapshot failed", "league_id", leagueID, "error", err)
		return
	}
	if err := writeSnapshot(e.host.DataDir, leagueID, snap); err != nil {
		e.logWarn("sleeper: snapshot write failed", "league_id", leagueID, "error", err)
	}
}

// logWarn no-ops when Host.Log is unset (a zero-value Host in a test).
func (e *extension) logWarn(msg string, args ...any) {
	if e.host.Log != nil {
		e.host.Log.Warn(msg, args...)
	}
}

func (e *extension) buildSnapshot(ctx context.Context, leagueID string) (snapshot, error) {
	rosters, err := e.client.Rosters(ctx, leagueID)
	if err != nil {
		return snapshot{}, fmt.Errorf("rosters: %w", err)
	}
	dump, err := e.client.PlayersDump(ctx)
	if err != nil {
		return snapshot{}, fmt.Errorf("players dump: %w", err)
	}
	season, state, err := e.season(ctx)
	if err != nil {
		return snapshot{}, err
	}
	research, err := e.client.Research(ctx, season, state.Week)
	if err != nil {
		return snapshot{}, fmt.Errorf("research: %w", err)
	}
	adds, err := e.client.TrendingPlayers(ctx, sleepergen.Add, 24, 25)
	if err != nil {
		return snapshot{}, fmt.Errorf("trending add: %w", err)
	}
	drops, err := e.client.TrendingPlayers(ctx, sleepergen.Drop, 24, 25)
	if err != nil {
		return snapshot{}, fmt.Errorf("trending drop: %w", err)
	}
	snap := snapshot{
		Date:        time.Now().UTC().Format(time.DateOnly),
		Players:     playerSnapshots(rostered(rosters), dump, research),
		TrendingAdd: trendingCounts(adds), TrendingDrop: trendingCounts(drops),
	}
	return snap, nil
}

func playerSnapshots(rosteredIDs map[string]bool, dump map[string]sleepergen.Player, research map[string]sleepergen.ResearchEntry) map[string]playerSnapshot {
	out := make(map[string]playerSnapshot, len(rosteredIDs))
	for pid := range rosteredIDs {
		p, ok := dump[pid]
		if !ok {
			continue
		}
		ps := playerSnapshot{InjuryStatus: strVal(p.InjuryStatus), PracticeDescription: strVal(p.PracticeDescription), DepthChartOrder: intVal(p.DepthChartOrder)}
		if r, ok := research[pid]; ok && r.Owned != nil {
			ps.OwnedPct = *r.Owned
		}
		out[pid] = ps
	}
	return out
}

func writeSnapshot(dataDir, leagueID string, snap snapshot) error {
	if err := os.MkdirAll(snapshotDir(dataDir, leagueID), 0o755); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}
	data, err := json.Marshal(snap)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	return os.WriteFile(snapshotPath(dataDir, leagueID, snap.Date), data, 0o644)
}

func readSnapshot(dataDir, leagueID, date string) (snapshot, error) {
	data, err := os.ReadFile(snapshotPath(dataDir, leagueID, date))
	if err != nil {
		return snapshot{}, err
	}
	var snap snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return snapshot{}, fmt.Errorf("unmarshal snapshot %s: %w", date, err)
	}
	return snap, nil
}

// snapshotDates lists a league's stored snapshot dates, oldest first.
func snapshotDates(dataDir, leagueID string) ([]string, error) {
	entries, err := os.ReadDir(snapshotDir(dataDir, leagueID))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var dates []string
	for _, e := range entries {
		name := e.Name()
		if filepath.Ext(name) == ".json" {
			dates = append(dates, name[:len(name)-len(".json")])
		}
	}
	sort.Strings(dates)
	return dates, nil
}
