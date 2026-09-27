package sleeper

import (
	"context"
	"fmt"
	"time"

	"github.com/fagerbergj/quack-extensions/sleeper/sleepergen"

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
	Week int    `json:"week,omitempty"`
	TZ   string `json:"tz,omitempty"`
}

type scheduleGame struct {
	GameID       string `json:"game_id"`
	Home         string `json:"home"`
	Away         string `json:"away"`
	Date         string `json:"date,omitempty"`
	Kickoff      string `json:"kickoff,omitempty"`
	KickoffLocal string `json:"kickoff_local,omitempty"`
	KickoffTBD   bool   `json:"kickoff_tbd,omitempty"`
	Locked       *bool  `json:"locked,omitempty"`
	Status       string `json:"status,omitempty"`
	Venue        string `json:"venue,omitempty"`
	City         string `json:"city,omitempty"`
	Roof         string `json:"roof,omitempty"`
}

type scheduleResult struct {
	callContext
	Games []scheduleGame `json:"games"`
}

// tzArgDoc and lockDoc are shared by every tool whose result carries games.
const (
	tzArgDoc = "Pass `tz` = the user's IANA time zone as printed by the current_date tool (e.g. " +
		"America/Chicago); kickoff_local and fetched_at_local are rendered in it and are what to show " +
		"the user - never convert the UTC fields yourself. Without `tz` they use the server's zone. " +
		"week/current_week/week_note say which NFL week the data covers; the default is the current week."
	playerGameDoc = "Each player already carries their own NFL game for this week, joined in code by " +
		"team: game {game_id, away, home, opponent, is_home, kickoff, kickoff_local, kickoff_tbd, locked, " +
		"status, venue, city, roof}, or game=null with bye=true / no_game_reason. Use it as-is: never look " +
		"up or reuse another game's kickoff or opponent for a player. " + lockDoc
	lockDoc = "locked is true once kickoff has passed as of fetched_at or the game is in_game/complete; " +
		"absent = unknown, treat as possibly locked. Only give a kickoff time for games with locked=false. " +
		"kickoff_tbd marks a flex game whose time is not set yet. Canceled/postponed games carry no kickoff; " +
		"ignore their lock state."
)

func (e *extension) scheduleTool() tool.Tool {
	t, _ := functiontool.New[scheduleArgs, scheduleResult](
		functiontool.Config{
			Name: "sleeper_schedule",
			Description: "Get the NFL schedule for a week (default the current week). Per game: status " +
				"(pre_game, in_game, complete, canceled, postponed), date (US Eastern calendar date), " +
				"kickoff (RFC3339 UTC), kickoff_local, locked, venue, city, roof " +
				"(outdoor/dome/retractable_dome). " + lockDoc + " " + tzArgDoc + " For a rostered " +
				"player's game use sleeper_roster/sleeper_matchup/sleeper_player, which already join it.",
		},
		func(ctx adkagent.Context, a scheduleArgs) (scheduleResult, error) { return e.getSchedule(ctx, a) },
	)
	return t
}

func (e *extension) getSchedule(ctx context.Context, a scheduleArgs) (scheduleResult, error) {
	c, err := newClock(a.TZ)
	if err != nil {
		return scheduleResult{}, fmt.Errorf("sleeper_schedule: %w", err)
	}
	season, state, err := e.season(ctx)
	if err != nil {
		return scheduleResult{}, err
	}
	week, err := resolveWeek(a.Week, season, state)
	if err != nil {
		return scheduleResult{}, fmt.Errorf("sleeper_schedule: %w", err)
	}
	sl, err := e.weekSlate(ctx, season, week, c)
	if err != nil {
		return scheduleResult{}, fmt.Errorf("sleeper_schedule: %w", err)
	}
	res := scheduleResult{callContext: newCallContext(c, week, season, state), Games: sl.games}
	res.ScheduleNote = sl.note
	return res, nil
}

// slate is one week's games with kickoff/lock joined, indexed by team code
// so per-player lookups never guess which game a player is in.
type slate struct {
	games  []scheduleGame
	byTeam map[string]scheduleGame
	note   string
}

func (e *extension) weekSlate(ctx context.Context, season string, week int, c clock) (slate, error) {
	games, err := e.client.Schedule(ctx, season)
	if err != nil {
		return slate{}, err
	}
	scores, err := e.client.WeekScores(ctx, season, week)
	if err != nil {
		e.logWarn("sleeper: scores fetch failed", "season", season, "week", week, "error", err)
	}
	byID := make(map[string]*sleepergen.GameScore, len(scores))
	for i := range scores {
		byID[scores[i].GameId] = &scores[i]
	}
	sl := slate{byTeam: map[string]scheduleGame{}}
	for _, g := range games {
		if g.Week != week {
			continue
		}
		sg := withKickoff(scheduleGame{GameID: g.GameId, Home: g.Home, Away: g.Away, Date: strVal(g.Date), Status: strVal(g.Status)}, byID[g.GameId], c)
		sl.games = append(sl.games, sg)
		for _, team := range []string{sg.Home, sg.Away} {
			if prev, ok := sl.byTeam[team]; !ok || voidGame(prev.Status) {
				sl.byTeam[team] = sg
			}
		}
	}
	if len(scores) == 0 && len(sl.games) > 0 {
		sl.note = "kickoff times unavailable from Sleeper this call; locked reflects game status only"
	}
	return sl, nil
}

func voidGame(status string) bool { return status == "canceled" || status == "postponed" }

// gameInfo is a player's own game for the slate's week, or why there is none.
type gameInfo struct {
	Game         *playerGame `json:"game"`
	Bye          bool        `json:"bye,omitempty"`
	NoGameReason string      `json:"no_game_reason,omitempty"`
}

type playerGame struct {
	scheduleGame
	Opponent string `json:"opponent"`
	IsHome   bool   `json:"is_home"`
}

func (sl slate) gameFor(dump map[string]sleepergen.Player, playerID string) gameInfo {
	if playerID == "" || playerID == "0" {
		return gameInfo{NoGameReason: "empty lineup slot"}
	}
	team := nflTeam(dump, playerID)
	switch {
	case team == "":
		return gameInfo{NoGameReason: "no NFL team (free agent or unknown player)"}
	case len(sl.games) == 0:
		return gameInfo{NoGameReason: "no NFL games scheduled this week"}
	}
	g, ok := sl.byTeam[team]
	if !ok {
		return gameInfo{Bye: true, NoGameReason: team + " has no game this week (bye)"}
	}
	pg := playerGame{scheduleGame: g, IsHome: g.Home == team, Opponent: g.Home}
	if pg.IsHome {
		pg.Opponent = g.Away
	}
	return gameInfo{Game: &pg}
}

// nflTeam is a player's NFL team code; a DEF unit's player_id is its team code.
func nflTeam(dump map[string]sleepergen.Player, playerID string) string {
	p, ok := dump[playerID]
	if !ok {
		return ""
	}
	if team := strVal(p.Team); team != "" {
		return team
	}
	if strVal(p.Position) == "DEF" {
		return playerID
	}
	return ""
}

// withKickoff joins on game_id, never on teams, so a missing score leaves
// kickoff empty rather than borrowing another game's time.
func withKickoff(g scheduleGame, s *sleepergen.GameScore, c clock) scheduleGame {
	statusLocked := g.Status == "in_game" || g.Status == "complete"
	if statusLocked {
		g.Locked = &statusLocked
	}
	if s == nil {
		return g
	}
	if sd := s.Metadata.StadiumDetails; sd != nil {
		g.Venue, g.City, g.Roof = strVal(sd.Name), strVal(sd.City), strVal(sd.Type)
	}
	switch strVal(s.Metadata.Status) {
	case "flex-schedule":
		g.KickoffTBD = true
		return g
	case "postponed", "cancelled": // postponed games keep their original, now meaningless, start_time
		return g
	}
	if voidGame(g.Status) || s.StartTime <= 0 {
		return g
	}
	k := time.UnixMilli(s.StartTime).UTC()
	g.Kickoff, g.KickoffLocal = k.Format(time.RFC3339), c.local(k)
	// The schedule's status (2m cache) can be newer than a 6h-cached start_time.
	locked := statusLocked || !k.After(c.now)
	g.Locked = &locked
	return g
}
