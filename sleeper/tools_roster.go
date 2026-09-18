package sleeper

import (
	"context"
	"fmt"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"

	"github.com/fagerbergj/quack-extensions/sleeper/sleepergen"
)

// pointsField reassembles a Sleeper whole+decimal points pair (e.g.
// settings["fpts"], settings["fpts_decimal"]) into one float.
func pointsField(settings map[string]int, whole, decimal string) float64 {
	return float64(settings[whole]) + float64(settings[decimal])/100
}

type playerRef struct {
	PlayerID string `json:"player_id"`
	Name     string `json:"name"`
}

type slotPlayer struct {
	Slot     string `json:"slot"`
	PlayerID string `json:"player_id"`
	Name     string `json:"name"`
	Bye      bool   `json:"bye,omitempty"`
}

// weekTeams returns the NFL team codes with a game in week, from a
// schedule already fetched for the season - a team missing from this set has a bye.
func weekTeams(games []sleepergen.Game, week int) map[string]bool {
	out := map[string]bool{}
	for _, g := range games {
		if g.Week != week {
			continue
		}
		out[g.Home] = true
		out[g.Away] = true
	}
	return out
}

// playerTeam returns a rostered player's NFL team code, falling back to the
// player_id itself for a DEF unit (whose id already is the team code).
func playerTeam(dump map[string]sleepergen.Player, playerID string) string {
	if p, ok := dump[playerID]; ok && p.Team != nil && *p.Team != "" {
		return *p.Team
	}
	return playerID
}

type rosterArgs struct {
	LeagueID string `json:"league_id,omitempty"`
	User     string `json:"user,omitempty"`
	RosterID int    `json:"roster_id,omitempty"`
}

type rosterResult struct {
	RosterID  int          `json:"roster_id"`
	Team      string       `json:"team"`
	Starters  []slotPlayer `json:"starters"`
	Bench     []playerRef  `json:"bench"`
	Reserve   []playerRef  `json:"reserve,omitempty"`
	Taxi      []playerRef  `json:"taxi,omitempty"`
	Wins      int          `json:"wins"`
	Losses    int          `json:"losses"`
	Ties      int          `json:"ties"`
	Fpts      float64      `json:"fpts"`
	WaiverPos int          `json:"waiver_position"`
	FaabUsed  int          `json:"faab_used"`
	FaabLeft  int          `json:"faab_left"`
	FetchedAt string       `json:"fetched_at"`
}

//nolint:dupl // functiontool wrapper boilerplate: each tool differs only in name/description/handler
func (e *extension) rosterTool() tool.Tool {
	t, _ := functiontool.New[rosterArgs, rosterResult](
		functiontool.Config{
			Name: "sleeper_roster",
			Description: "Get a roster: starters by slot, bench, reserve/taxi, record, fpts, waiver " +
				"position, FAAB used/left, and which starters are on a bye this week. `league_id` falls " +
				"back to default_league; `user` or `roster_id` picks the team, falling back to default_user.",
		},
		func(ctx adkagent.Context, a rosterArgs) (rosterResult, error) { return e.getRoster(ctx, a) },
	)
	return t
}

func (e *extension) getRoster(ctx context.Context, a rosterArgs) (rosterResult, error) {
	leagueID, err := e.resolveLeagueID(a.LeagueID)
	if err != nil {
		return rosterResult{}, err
	}
	league, rosters, users, err := e.leagueRostersUsers(ctx, leagueID)
	if err != nil {
		return rosterResult{}, err
	}
	rosterID := a.RosterID
	if rosterID == 0 {
		uid, err := e.resolveUserIdentifier(a.User)
		if err != nil {
			return rosterResult{}, err
		}
		id, ok := rosterIDForUser(rosters, users, uid)
		if !ok {
			return rosterResult{}, fmt.Errorf("sleeper_roster: no roster found for %q", uid)
		}
		rosterID = id
	}
	r, ok := rosterFor(rosters, rosterID)
	if !ok {
		return rosterResult{}, fmt.Errorf("sleeper_roster: roster %d not found", rosterID)
	}
	dump, err := e.client.PlayersDump(ctx)
	if err != nil {
		return rosterResult{}, fmt.Errorf("sleeper_roster: %w", err)
	}
	season, state, err := e.season(ctx)
	if err != nil {
		return rosterResult{}, err
	}
	games, err := e.client.Schedule(ctx, season)
	if err != nil {
		return rosterResult{}, fmt.Errorf("sleeper_roster: schedule: %w", err)
	}
	playing := weekTeams(games, state.Week)
	return buildRosterResult(league, r, dump, playing, teamNames(rosters, users)), nil
}

// leagueRostersUsers is the three-call combo almost every roster/standings
// tool needs; collapsing it here keeps each tool's handler small.
func (e *extension) leagueRostersUsers(ctx context.Context, leagueID string) (*sleepergen.League, []sleepergen.Roster, []sleepergen.LeagueUser, error) {
	league, err := e.client.League(ctx, leagueID)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("league: %w", err)
	}
	rosters, err := e.client.Rosters(ctx, leagueID)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("rosters: %w", err)
	}
	users, err := e.client.LeagueUsers(ctx, leagueID)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("league users: %w", err)
	}
	return league, rosters, users, nil
}

func buildRosterResult(league *sleepergen.League, r sleepergen.Roster, dump map[string]sleepergen.Player, playing map[string]bool, names map[int]string) rosterResult {
	starterSlots := nonBenchSlots(league.RosterPositions)
	starters := make([]slotPlayer, 0, len(r.Starters))
	onField := map[string]bool{}
	for i, pid := range r.Starters {
		slot := "?"
		if i < len(starterSlots) {
			slot = starterSlots[i]
		}
		onField[pid] = true
		starters = append(starters, slotPlayer{Slot: slot, PlayerID: pid, Name: playerName(dump, pid), Bye: !playing[playerTeam(dump, pid)]})
	}
	reserveSet, taxiSet := toSet(r.Reserve), toSet(r.Taxi)
	var bench, reserve, taxi []playerRef
	for _, pid := range r.Players {
		if onField[pid] {
			continue
		}
		ref := playerRef{PlayerID: pid, Name: playerName(dump, pid)}
		switch {
		case reserveSet[pid]:
			reserve = append(reserve, ref)
		case taxiSet[pid]:
			taxi = append(taxi, ref)
		default:
			bench = append(bench, ref)
		}
	}
	return rosterResult{
		RosterID: r.RosterId, Team: names[r.RosterId],
		Starters: starters, Bench: bench, Reserve: reserve, Taxi: taxi,
		Wins: r.Settings["wins"], Losses: r.Settings["losses"], Ties: r.Settings["ties"],
		Fpts:      pointsField(r.Settings, "fpts", "fpts_decimal"),
		WaiverPos: r.Settings["waiver_position"],
		FaabUsed:  r.Settings["waiver_budget_used"],
		FaabLeft:  league.Settings["waiver_budget"] - r.Settings["waiver_budget_used"],
		FetchedAt: nowRFC3339(),
	}
}

// nonBenchSlots strips BN/IR/TAXI entries, leaving the starting-lineup
// slot names in the order Roster.Starters is indexed against.
func nonBenchSlots(positions []string) []string {
	var out []string
	for _, p := range positions {
		if p == "BN" || p == "IR" || p == "TAXI" {
			continue
		}
		out = append(out, p)
	}
	return out
}

func toSet(ids *[]string) map[string]bool {
	out := map[string]bool{}
	if ids == nil {
		return out
	}
	for _, id := range *ids {
		out[id] = true
	}
	return out
}
