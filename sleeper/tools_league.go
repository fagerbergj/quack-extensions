package sleeper

import (
	"context"
	"fmt"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
)

// waiverTypeName follows Sleeper's documented settings.waiver_type enum.
func waiverTypeName(v int) string {
	switch v {
	case 0:
		return "rolling"
	case 1:
		return "reverse_standings"
	case 2:
		return "faab"
	default:
		return "unknown"
	}
}

type userArgs struct {
	Username string `json:"username,omitempty"`
	UserID   string `json:"user_id,omitempty"`
}

type userLeagueSummary struct {
	LeagueID string `json:"league_id"`
	Name     string `json:"name"`
	Season   string `json:"season"`
	Status   string `json:"status"`
}

type userResult struct {
	UserID      string              `json:"user_id"`
	DisplayName string              `json:"display_name"`
	Leagues     []userLeagueSummary `json:"leagues"`
	FetchedAt   string              `json:"fetched_at"`
}

//nolint:dupl // functiontool wrapper boilerplate: each tool differs only in name/description/handler
func (e *extension) userTool() tool.Tool {
	t, _ := functiontool.New[userArgs, userResult](
		functiontool.Config{
			Name: "sleeper_user",
			Description: "Look up a Sleeper user (by username or user_id, falling back to the configured " +
				"default_user) and the leagues they're in this season. Returns {user_id, display_name, leagues}.",
		},
		func(ctx adkagent.Context, a userArgs) (userResult, error) { return e.getUser(ctx, a) },
	)
	return t
}

func (e *extension) getUser(ctx context.Context, a userArgs) (userResult, error) {
	id := a.UserID
	if id == "" {
		id = a.Username
	}
	id, err := e.resolveUserIdentifier(id)
	if err != nil {
		return userResult{}, err
	}
	u, err := e.client.User(ctx, id)
	if err != nil {
		return userResult{}, fmt.Errorf("sleeper_user: %w", err)
	}
	season, _, err := e.season(ctx)
	if err != nil {
		return userResult{}, err
	}
	leagues, err := e.client.UserLeagues(ctx, u.UserId, season)
	if err != nil {
		return userResult{}, fmt.Errorf("sleeper_user: leagues: %w", err)
	}
	out := make([]userLeagueSummary, len(leagues))
	for i, l := range leagues {
		out[i] = userLeagueSummary{LeagueID: l.LeagueId, Name: l.Name, Season: l.Season, Status: l.Status}
	}
	return userResult{UserID: u.UserId, DisplayName: strVal(u.DisplayName), Leagues: out, FetchedAt: nowRFC3339()}, nil
}

type leagueArgs struct {
	LeagueID string `json:"league_id,omitempty"`
}

type leagueResult struct {
	LeagueID         string             `json:"league_id"`
	Name             string             `json:"name"`
	Season           string             `json:"season"`
	Week             int                `json:"week"`
	RosterPositions  []string           `json:"roster_positions"`
	ReserveSlots     int                `json:"reserve_slots"`
	TaxiSlots        int                `json:"taxi_slots"`
	PlayoffTeams     int                `json:"playoff_teams"`
	PlayoffWeekStart int                `json:"playoff_week_start"`
	WaiverType       string             `json:"waiver_type"`
	WaiverBudget     int                `json:"waiver_budget"`
	ScoringSettings  map[string]float32 `json:"scoring_settings"`
	FetchedAt        string             `json:"fetched_at"`
}

//nolint:dupl // functiontool wrapper boilerplate: each tool differs only in name/description/handler
func (e *extension) leagueTool() tool.Tool {
	t, _ := functiontool.New[leagueArgs, leagueResult](
		functiontool.Config{
			Name: "sleeper_league",
			Description: "Get a league's settings: scoring, roster_positions, reserve/taxi slot counts, " +
				"playoff config, waiver type and budget, season, and the current week. Falls back to the " +
				"configured default_league when league_id is omitted.",
		},
		func(ctx adkagent.Context, a leagueArgs) (leagueResult, error) { return e.getLeague(ctx, a) },
	)
	return t
}

func (e *extension) getLeague(ctx context.Context, a leagueArgs) (leagueResult, error) {
	leagueID, err := e.resolveLeagueID(a.LeagueID)
	if err != nil {
		return leagueResult{}, err
	}
	l, err := e.client.League(ctx, leagueID)
	if err != nil {
		return leagueResult{}, fmt.Errorf("sleeper_league: %w", err)
	}
	state, err := e.client.State(ctx)
	if err != nil {
		return leagueResult{}, fmt.Errorf("sleeper_league: state: %w", err)
	}
	return leagueResult{
		LeagueID:         l.LeagueId,
		Name:             l.Name,
		Season:           l.Season,
		Week:             state.Week,
		RosterPositions:  l.RosterPositions,
		ReserveSlots:     l.Settings["reserve_slots"],
		TaxiSlots:        l.Settings["taxi_slots"],
		PlayoffTeams:     l.Settings["playoff_teams"],
		PlayoffWeekStart: l.Settings["playoff_week_start"],
		WaiverType:       waiverTypeName(l.Settings["waiver_type"]),
		WaiverBudget:     l.Settings["waiver_budget"],
		ScoringSettings:  l.ScoringSettings,
		FetchedAt:        nowRFC3339(),
	}, nil
}

type scheduleArgs struct {
	Week int `json:"week,omitempty"`
}

type scheduleGame struct {
	GameID string `json:"game_id"`
	Home   string `json:"home"`
	Away   string `json:"away"`
	Date   string `json:"date,omitempty"`
	Status string `json:"status,omitempty"`
}

type scheduleResult struct {
	Week      int            `json:"week"`
	Games     []scheduleGame `json:"games"`
	FetchedAt string         `json:"fetched_at"`
}

func (e *extension) scheduleTool() tool.Tool {
	t, _ := functiontool.New[scheduleArgs, scheduleResult](
		functiontool.Config{
			Name:        "sleeper_schedule",
			Description: "Get the NFL schedule for a week (default the current week): games, kickoff dates, and status.",
		},
		func(ctx adkagent.Context, a scheduleArgs) (scheduleResult, error) { return e.getSchedule(ctx, a) },
	)
	return t
}

func (e *extension) getSchedule(ctx context.Context, a scheduleArgs) (scheduleResult, error) {
	season, state, err := e.season(ctx)
	if err != nil {
		return scheduleResult{}, err
	}
	week := resolveWeek(a.Week, state)
	games, err := e.client.Schedule(ctx, season)
	if err != nil {
		return scheduleResult{}, fmt.Errorf("sleeper_schedule: %w", err)
	}
	var out []scheduleGame
	for _, g := range games {
		if g.Week != week {
			continue
		}
		out = append(out, scheduleGame{GameID: g.GameId, Home: g.Home, Away: g.Away, Date: strVal(g.Date), Status: strVal(g.Status)})
	}
	return scheduleResult{Week: week, Games: out, FetchedAt: nowRFC3339()}, nil
}
