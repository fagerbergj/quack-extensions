package sleeper

import (
	"context"
	"fmt"
	"math"
	"sort"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"

	"github.com/fagerbergj/quack-extensions/sleeper/sleepergen"
)

// maxGameLogWeeks caps sleeper_player's game_log - a floor/ceiling read
// only needs recent usage, not a full season back-catalog.
const maxGameLogWeeks = 5

type playerArgs struct {
	PlayerID string `json:"player_id,omitempty"`
	Name     string `json:"name,omitempty"`
}

type playerResult struct {
	PlayerID            string             `json:"player_id"`
	Name                string             `json:"name"`
	Position            string             `json:"position,omitempty"`
	Team                string             `json:"team,omitempty"`
	DepthChartOrder     int                `json:"depth_chart_order,omitempty"`
	InjuryStatus        string             `json:"injury_status,omitempty"`
	PracticeDescription string             `json:"practice_description,omitempty"`
	NewsUpdated         int                `json:"news_updated,omitempty"`
	SeasonStats         map[string]float32 `json:"season_stats,omitempty"`
	WeekProjection      float32            `json:"week_projection"`
	GameLog             []gameLogWeek      `json:"game_log,omitempty"`
	FetchedAt           string             `json:"fetched_at"`
}

// gameLogWeek is one of this season's completed weeks for a player -
// floor/ceiling reads snap share and volume trend from this, not a season average.
type gameLogWeek struct {
	Week    int     `json:"week"`
	Opp     string  `json:"opp,omitempty"`
	SnapPct *int    `json:"snap_pct,omitempty"`
	Targets float32 `json:"targets"`
	Carries float32 `json:"carries"`
	PtsPPR  float32 `json:"pts_ppr"`
}

//nolint:dupl // functiontool wrapper boilerplate: each tool differs only in name/description/handler
func (e *extension) playerTool() tool.Tool {
	t, _ := functiontool.New[playerArgs, playerResult](
		functiontool.Config{
			Name: "sleeper_player",
			Description: "Get one player's bio, team, depth chart order, injury/practice status, news " +
				"timestamp, season stats, and this week's projection. Look up by `player_id` or `name`.",
		},
		func(ctx adkagent.Context, a playerArgs) (playerResult, error) { return e.getPlayer(ctx, a) },
	)
	return t
}

func (e *extension) getPlayer(ctx context.Context, a playerArgs) (playerResult, error) {
	p, err := e.resolvePlayer(ctx, a.PlayerID, a.Name)
	if err != nil {
		return playerResult{}, fmt.Errorf("sleeper_player: %w", err)
	}
	season, state, err := e.season(ctx)
	if err != nil {
		return playerResult{}, err
	}
	week, err := weekForSeason(season, state)
	if err != nil {
		return playerResult{}, fmt.Errorf("sleeper_player: %w", err)
	}
	proj, err := e.client.WeekProjections(ctx, season, week)
	if err != nil {
		return playerResult{}, fmt.Errorf("sleeper_player: projections: %w", err)
	}
	var stats map[string]float32
	if entry, err := e.client.PlayerSeasonStats(ctx, p.PlayerId, season); err == nil && entry.Stats != nil {
		stats = *entry.Stats
	}
	var gameLog []gameLogWeek
	if entries, err := e.client.PlayerGameLog(ctx, p.PlayerId, season); err == nil {
		gameLog = buildGameLog(entries, week)
	}
	return playerResult{
		PlayerID: p.PlayerId, Name: playerNameOf(p), Position: strVal(p.Position), Team: strVal(p.Team),
		DepthChartOrder: intVal(p.DepthChartOrder), InjuryStatus: strVal(p.InjuryStatus),
		PracticeDescription: strVal(p.PracticeDescription), NewsUpdated: intVal(p.NewsUpdated),
		SeasonStats: stats, WeekProjection: proj[p.PlayerId]["pts_ppr"], GameLog: gameLog, FetchedAt: nowRFC3339(),
	}, nil
}

// buildGameLog keeps weeks strictly before currentWeek (the in-progress
// week's stats aren't final), newest first, capped at maxGameLogWeeks.
func buildGameLog(entries []sleepergen.PlayerStatEntry, currentWeek int) []gameLogWeek {
	var out []gameLogWeek
	for _, e := range entries {
		if e.Week == nil || *e.Week >= currentWeek {
			continue
		}
		out = append(out, gameLogWeekFrom(*e.Week, e))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Week > out[j].Week })
	if len(out) > maxGameLogWeeks {
		out = out[:maxGameLogWeeks]
	}
	return out
}

func gameLogWeekFrom(week int, e sleepergen.PlayerStatEntry) gameLogWeek {
	var stats map[string]float32
	if e.Stats != nil {
		stats = *e.Stats
	}
	return gameLogWeek{
		Week: week, Opp: strVal(e.Opponent), SnapPct: snapPct(stats),
		Targets: stats["rec_tgt"], Carries: stats["rush_att"], PtsPPR: stats["pts_ppr"],
	}
}

// snapPct is off_snp/tm_off_snp as a whole percent, nil when tm_off_snp is
// missing or 0 - a share of zero team snaps isn't a real percentage.
func snapPct(stats map[string]float32) *int {
	tm := stats["tm_off_snp"]
	if tm <= 0 {
		return nil
	}
	pct := int(math.Round(float64(stats["off_snp"]/tm) * 100))
	return &pct
}

// resolvePlayer looks a player up by id (the cheaper single-player
// endpoint) or by free-text name (via the dump's name index).
func (e *extension) resolvePlayer(ctx context.Context, playerID, name string) (sleepergen.Player, error) {
	if playerID != "" {
		p, err := e.client.Player(ctx, playerID)
		if err != nil {
			return sleepergen.Player{}, err
		}
		return *p, nil
	}
	if name == "" {
		return sleepergen.Player{}, fmt.Errorf("player_id or name is required")
	}
	dump, err := e.client.PlayersDump(ctx)
	if err != nil {
		return sleepergen.Player{}, err
	}
	ids := e.client.ResolvePlayer(name)
	if len(ids) == 0 {
		return sleepergen.Player{}, fmt.Errorf("no player found matching %q", name)
	}
	return dump[ids[0]], nil
}

func playerNameOf(p sleepergen.Player) string {
	if p.FullName != nil && *p.FullName != "" {
		return *p.FullName
	}
	first, last := strVal(p.FirstName), strVal(p.LastName)
	if first != "" || last != "" {
		return first + " " + last
	}
	if p.Position != nil && *p.Position == "DEF" {
		return strVal(p.Team)
	}
	return p.PlayerId
}

func intVal(v *int) int {
	if v == nil {
		return 0
	}
	return *v
}
