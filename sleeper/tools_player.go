package sleeper

import (
	"context"
	"fmt"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"

	"github.com/fagerbergj/quack-extensions/sleeper/sleepergen"
)

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
	FetchedAt           string             `json:"fetched_at"`
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
	return playerResult{
		PlayerID: p.PlayerId, Name: playerNameOf(p), Position: strVal(p.Position), Team: strVal(p.Team),
		DepthChartOrder: intVal(p.DepthChartOrder), InjuryStatus: strVal(p.InjuryStatus),
		PracticeDescription: strVal(p.PracticeDescription), NewsUpdated: intVal(p.NewsUpdated),
		SeasonStats: stats, WeekProjection: proj[p.PlayerId]["pts_ppr"], FetchedAt: nowRFC3339(),
	}, nil
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
