package sleeper

import (
	"context"
	"fmt"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"

	"github.com/fagerbergj/quack-extensions/sleeper/sleepergen"
)

type matchupArgs struct {
	LeagueID string `json:"league_id,omitempty"`
	Week     int    `json:"week,omitempty"`
	User     string `json:"user,omitempty"`
	RosterID int    `json:"roster_id,omitempty"`
}

type lineupSlot struct {
	Slot       string  `json:"slot"`
	PlayerID   string  `json:"player_id"`
	Name       string  `json:"name"`
	Points     float32 `json:"points"`
	Projection float32 `json:"projection"`
}

type side struct {
	RosterID int          `json:"roster_id"`
	Team     string       `json:"team"`
	Record   string       `json:"record"`
	Points   float32      `json:"points"`
	Starters []lineupSlot `json:"starters"`
}

type matchupResult struct {
	Week      int    `json:"week"`
	MatchupID int    `json:"matchup_id"`
	Me        side   `json:"me"`
	Opponent  side   `json:"opponent"`
	FetchedAt string `json:"fetched_at"`
}

//nolint:dupl // functiontool wrapper boilerplate: each tool differs only in name/description/handler
func (e *extension) matchupTool() tool.Tool {
	t, _ := functiontool.New[matchupArgs, matchupResult](
		functiontool.Config{
			Name: "sleeper_matchup",
			Description: "Get one week's matchup (default the current week): both lineups with points " +
				"and projections, and the opponent's record. `user`/`roster_id` picks my side, falling " +
				"back to default_user.",
		},
		func(ctx adkagent.Context, a matchupArgs) (matchupResult, error) { return e.getMatchup(ctx, a) },
	)
	return t
}

func (e *extension) getMatchup(ctx context.Context, a matchupArgs) (matchupResult, error) {
	leagueID, err := e.resolveLeagueID(a.LeagueID)
	if err != nil {
		return matchupResult{}, err
	}
	league, rosters, users, err := e.leagueRostersUsers(ctx, leagueID)
	if err != nil {
		return matchupResult{}, err
	}
	rosterID := a.RosterID
	if rosterID == 0 {
		uid, err := e.resolveUserIdentifier(a.User)
		if err != nil {
			return matchupResult{}, err
		}
		id, ok := rosterIDForUser(rosters, users, uid)
		if !ok {
			return matchupResult{}, fmt.Errorf("sleeper_matchup: no roster found for %q", uid)
		}
		rosterID = id
	}
	season, state, err := e.season(ctx)
	if err != nil {
		return matchupResult{}, err
	}
	week, err := resolveWeek(a.Week, season, state)
	if err != nil {
		return matchupResult{}, fmt.Errorf("sleeper_matchup: %w", err)
	}
	matchups, err := e.client.Matchups(ctx, leagueID, week)
	if err != nil {
		return matchupResult{}, fmt.Errorf("sleeper_matchup: %w", err)
	}
	mine, ok := findMatchup(matchups, rosterID)
	if !ok {
		return matchupResult{}, fmt.Errorf("sleeper_matchup: roster %d has no matchup in week %d", rosterID, week)
	}
	opp, hasOpp := opponent(matchups, mine)
	proj, err := e.client.WeekProjections(ctx, season, week)
	if err != nil {
		return matchupResult{}, fmt.Errorf("sleeper_matchup: projections: %w", err)
	}
	dump, err := e.client.PlayersDump(ctx)
	if err != nil {
		return matchupResult{}, fmt.Errorf("sleeper_matchup: %w", err)
	}
	names := teamNames(rosters, users)
	result := matchupResult{
		Week: week, FetchedAt: nowRFC3339(),
		Me: buildSide(league, mine, rosters, names, dump, proj),
	}
	if mine.MatchupId != nil {
		result.MatchupID = *mine.MatchupId
	}
	if hasOpp {
		result.Opponent = buildSide(league, opp, rosters, names, dump, proj)
	}
	return result, nil
}

func findMatchup(matchups []sleepergen.Matchup, rosterID int) (sleepergen.Matchup, bool) {
	for _, m := range matchups {
		if m.RosterId == rosterID {
			return m, true
		}
	}
	return sleepergen.Matchup{}, false
}

func opponent(matchups []sleepergen.Matchup, mine sleepergen.Matchup) (sleepergen.Matchup, bool) {
	if mine.MatchupId == nil {
		return sleepergen.Matchup{}, false
	}
	for _, m := range matchups {
		if m.RosterId != mine.RosterId && m.MatchupId != nil && *m.MatchupId == *mine.MatchupId {
			return m, true
		}
	}
	return sleepergen.Matchup{}, false
}

func buildSide(league *sleepergen.League, m sleepergen.Matchup, rosters []sleepergen.Roster, names map[int]string, dump map[string]sleepergen.Player, proj map[string]sleepergen.StatMap) side {
	slots := nonBenchSlots(league.RosterPositions)
	starters := make([]lineupSlot, 0, len(m.Starters))
	for i, pid := range m.Starters {
		slot := "?"
		if i < len(slots) {
			slot = slots[i]
		}
		starters = append(starters, lineupSlot{
			Slot: slot, PlayerID: pid, Name: playerName(dump, pid),
			Points: m.PlayersPoints[pid], Projection: proj[pid]["pts_ppr"],
		})
	}
	record := ""
	if r, ok := rosterFor(rosters, m.RosterId); ok {
		record = recordString(r.Settings["wins"], r.Settings["losses"], r.Settings["ties"])
	}
	return side{RosterID: m.RosterId, Team: names[m.RosterId], Record: record, Points: m.Points, Starters: starters}
}
