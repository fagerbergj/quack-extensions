// sleeper_history: per season, a draft report card, lineup efficiency,
// weekly hindsight, and the bracket outcome, computed from recorded data.
package sleeper

import (
	"context"
	"fmt"
	"sort"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"

	"github.com/fagerbergj/quack-extensions/sleeper/sleepergen"
)

// closeLossFraction: a loss decided within 10% of the two teams' combined
// score reads as "close" - roughly one full player's output either way.
const closeLossFraction = 0.10

// reachStealThreshold: a positional finish 5+ spots off the drafted-as
// rank is worth calling a reach or a steal; smaller gaps are just variance.
const reachStealThreshold = 5

// maxHistoryWeeks bounds the per-week hindsight loop; a real season never
// runs past week 18, and a missing fixture/week just gets skipped.
const maxHistoryWeeks = 18

type historyArgs struct {
	LeagueID    string `json:"league_id,omitempty"`
	User        string `json:"user,omitempty"`
	SeasonsBack int    `json:"seasons_back,omitempty"`
}

type draftReportCardEntry struct {
	PlayerID      string `json:"player_id"`
	Name          string `json:"name"`
	Position      string `json:"position"`
	Round         int    `json:"round"`
	PickNo        int    `json:"pick_no"`
	DraftedAsRank int    `json:"drafted_as_rank"`
	FinishRank    int    `json:"finish_rank"`
	Verdict       string `json:"verdict"` // "reach" | "steal" | "as_expected"
}

type weekHindsight struct {
	Week         int     `json:"week"`
	Result       string  `json:"result"` // "W" | "L" | "T"
	Started      float32 `json:"started"`
	BestPossible float32 `json:"best_possible"`
	PointsLeft   float32 `json:"points_left"`
	CloseLoss    bool    `json:"close_loss"`
}

type seasonHistory struct {
	Season              string                 `json:"season"`
	LeagueID            string                 `json:"league_id"`
	Wins                int                    `json:"wins"`
	Losses              int                    `json:"losses"`
	Ties                int                    `json:"ties"`
	LineupEfficiencyPct float64                `json:"lineup_efficiency_pct"`
	CloseLosses         int                    `json:"close_losses"`
	Champion            string                 `json:"champion,omitempty"`
	Draft               []draftReportCardEntry `json:"draft,omitempty"`
	Weeks               []weekHindsight        `json:"weeks,omitempty"`
}

type historyResult struct {
	Seasons          []seasonHistory `json:"seasons"`
	TotalCloseLosses int             `json:"total_close_losses"`
	FetchedAt        string          `json:"fetched_at"`
}

func (e *extension) historyTool() tool.Tool {
	t, _ := functiontool.New[historyArgs, historyResult](
		functiontool.Config{
			Name: "sleeper_history",
			Description: "Walk a league's chain (previous_league_id) and report, per season: a draft " +
				"report card (drafted-as rank vs positional finish), lineup efficiency (started vs best " +
				"possible), close losses, and the champion. `seasons_back` caps how many seasons back " +
				"from league_id to include (default all available).",
		},
		func(ctx adkagent.Context, a historyArgs) (historyResult, error) { return e.getHistory(ctx, a) },
	)
	return t
}

func (e *extension) getHistory(ctx context.Context, a historyArgs) (historyResult, error) {
	leagueID, err := e.resolveLeagueID(a.LeagueID)
	if err != nil {
		return historyResult{}, err
	}
	uid, err := e.resolveUserIdentifier(a.User)
	if err != nil {
		return historyResult{}, err
	}
	chain, err := e.walkChain(ctx, leagueID, a.SeasonsBack)
	if err != nil {
		return historyResult{}, fmt.Errorf("sleeper_history: %w", err)
	}
	out := historyResult{FetchedAt: nowRFC3339()}
	for _, league := range chain {
		sh, err := e.seasonHistoryFor(ctx, league, uid)
		if err != nil {
			return historyResult{}, fmt.Errorf("sleeper_history: season %s: %w", league.Season, err)
		}
		out.Seasons = append(out.Seasons, sh)
		out.TotalCloseLosses += sh.CloseLosses
	}
	return out, nil
}

// walkChain follows previous_league_id up to seasonsBack; unlike
// Client.Chain, a broken hop stops the walk instead of erroring it.
func (e *extension) walkChain(ctx context.Context, leagueID string, seasonsBack int) ([]sleepergen.League, error) {
	limit := maxChainSeasons
	if seasonsBack > 0 && seasonsBack < limit {
		limit = seasonsBack
	}
	var out []sleepergen.League
	id := leagueID
	for i := 0; i < limit && id != ""; i++ {
		league, err := e.client.League(ctx, id)
		if err != nil {
			break
		}
		out = append(out, *league)
		if league.PreviousLeagueId == nil {
			break
		}
		id = *league.PreviousLeagueId
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no league found for %s", leagueID)
	}
	return out, nil
}

func (e *extension) seasonHistoryFor(ctx context.Context, league sleepergen.League, uid string) (seasonHistory, error) {
	rosters, err := e.client.Rosters(ctx, league.LeagueId)
	if err != nil {
		return seasonHistory{}, fmt.Errorf("rosters: %w", err)
	}
	// rosterIDForUser resolves by owner_id without user names, so a missing
	// fixture here degrades to "roster N" display names, not a hard failure.
	users, _ := e.client.LeagueUsers(ctx, league.LeagueId)
	myRosterID, ok := rosterIDForUser(rosters, users, uid)
	if !ok {
		return seasonHistory{}, fmt.Errorf("no roster found for %q", uid)
	}
	myRoster, _ := rosterFor(rosters, myRosterID)
	dump, err := e.client.PlayersDump(ctx)
	if err != nil {
		return seasonHistory{}, err
	}
	weeks, closeLosses := e.weeklyHindsight(ctx, league, myRosterID, dump)
	sh := seasonHistory{
		Season: league.Season, LeagueID: league.LeagueId,
		Wins: myRoster.Settings["wins"], Losses: myRoster.Settings["losses"], Ties: myRoster.Settings["ties"],
		LineupEfficiencyPct: lineupEfficiency(myRoster.Settings),
		CloseLosses:         closeLosses,
		Weeks:               weeks,
	}
	sh.Champion, err = e.champion(ctx, league.LeagueId, rosters, users)
	if err != nil {
		return seasonHistory{}, err
	}
	sh.Draft, err = e.draftReportCard(ctx, league, myRosterID)
	if err != nil {
		return seasonHistory{}, err
	}
	return sh, nil
}

func lineupEfficiency(settings map[string]int) float64 {
	ppts := pointsField(settings, "ppts", "ppts_decimal")
	if ppts == 0 {
		return 0
	}
	fpts := pointsField(settings, "fpts", "fpts_decimal")
	return roundTo1(fpts / ppts * 100)
}

func roundTo1(v float64) float64 {
	return float64(int(v*10+0.5)) / 10
}

func (e *extension) champion(ctx context.Context, leagueID string, rosters []sleepergen.Roster, users []sleepergen.LeagueUser) (string, error) {
	bracket, err := e.client.WinnersBracket(ctx, leagueID)
	if err != nil {
		return "", fmt.Errorf("winners bracket: %w", err)
	}
	names := teamNames(rosters, users)
	for _, m := range bracket {
		if m.P != nil && *m.P == 1 && m.W != nil {
			return names[*m.W], nil
		}
	}
	return "", nil
}

// weeklyHindsight walks every week with matchup data; a missing week
// (a fixture gap, or the season hasn't reached it yet) is skipped, not an error.
func (e *extension) weeklyHindsight(ctx context.Context, league sleepergen.League, myRosterID int, dump map[string]sleepergen.Player) ([]weekHindsight, int) {
	var weeks []weekHindsight
	closeLosses := 0
	for week := 1; week <= maxHistoryWeeks; week++ {
		matchups, err := e.client.Matchups(ctx, league.LeagueId, week)
		if err != nil || len(matchups) == 0 {
			continue
		}
		mine, ok := findMatchup(matchups, myRosterID)
		if !ok {
			continue
		}
		opp, hasOpp := opponent(matchups, mine)
		w := hindsightFor(week, league.RosterPositions, mine, dump)
		if hasOpp {
			w.Result, w.CloseLoss = weekResult(mine.Points, opp.Points)
			if w.CloseLoss {
				closeLosses++
			}
		}
		weeks = append(weeks, w)
	}
	return weeks, closeLosses
}

func weekResult(mine, opp float32) (result string, close bool) {
	switch {
	case mine > opp:
		result = "W"
	case mine < opp:
		result = "L"
	default:
		result = "T"
	}
	total := mine + opp
	margin := mine - opp
	if margin < 0 {
		margin = -margin
	}
	close = result == "L" && total > 0 && float64(margin) <= closeLossFraction*float64(total)
	return result, close
}

func hindsightFor(week int, rosterPositions []string, m sleepergen.Matchup, dump map[string]sleepergen.Player) weekHindsight {
	best := bestLineupPoints(rosterPositions, m.Players, m.PlayersPoints, dump)
	return weekHindsight{
		Week: week, Started: m.Points, BestPossible: best, PointsLeft: best - m.Points,
	}
}

// draftReportCard grades the owner's own picks: drafted-as-rank vs
// finish-rank, both ranked within position among that season's picks.
func (e *extension) draftReportCard(ctx context.Context, league sleepergen.League, myRosterID int) ([]draftReportCardEntry, error) {
	if league.DraftId == nil {
		return nil, nil // no recorded draft for this season
	}
	picks, err := e.client.DraftPicks(ctx, *league.DraftId)
	if err != nil {
		return nil, fmt.Errorf("draft picks: %w", err)
	}
	// A season still in progress has no season-total stats yet - positional
	// finish isn't knowable until it ends, so the report card waits too.
	stats, err := e.client.SeasonStats(ctx, league.Season)
	if err != nil {
		return nil, nil
	}
	draftedRank := positionalDraftRank(picks)
	finishRank := positionalFinishRank(picks, stats)
	var out []draftReportCardEntry
	for _, p := range picks {
		if p.RosterId != myRosterID {
			continue
		}
		dr, fr := draftedRank[p.PlayerId], finishRank[p.PlayerId]
		out = append(out, draftReportCardEntry{
			PlayerID: p.PlayerId, Name: pickPlayerName(p), Position: pickPosition(p),
			Round: p.Round, PickNo: p.PickNo, DraftedAsRank: dr, FinishRank: fr,
			Verdict: reachOrSteal(dr, fr),
		})
	}
	return out, nil
}

func reachOrSteal(draftedAsRank, finishRank int) string {
	if draftedAsRank == 0 || finishRank == 0 {
		return "unknown"
	}
	diff := finishRank - draftedAsRank
	switch {
	case diff >= reachStealThreshold:
		return "reach"
	case diff <= -reachStealThreshold:
		return "steal"
	default:
		return "as_expected"
	}
}

// positionalDraftRank ranks every pick within its position by pick_no.
func positionalDraftRank(picks []sleepergen.DraftPick) map[string]int {
	byPos := map[string][]sleepergen.DraftPick{}
	for _, p := range picks {
		pos := pickPosition(p)
		byPos[pos] = append(byPos[pos], p)
	}
	out := map[string]int{}
	for _, group := range byPos {
		sort.Slice(group, func(i, j int) bool { return group[i].PickNo < group[j].PickNo })
		for i, p := range group {
			out[p.PlayerId] = i + 1
		}
	}
	return out
}

// positionalFinishRank ranks every drafted player within its position by
// season pts_ppr, highest first.
func positionalFinishRank(picks []sleepergen.DraftPick, stats map[string]sleepergen.StatMap) map[string]int {
	byPos := map[string][]sleepergen.DraftPick{}
	for _, p := range picks {
		pos := pickPosition(p)
		byPos[pos] = append(byPos[pos], p)
	}
	out := map[string]int{}
	for _, group := range byPos {
		sort.SliceStable(group, func(i, j int) bool {
			return stats[group[i].PlayerId]["pts_ppr"] > stats[group[j].PlayerId]["pts_ppr"]
		})
		for i, p := range group {
			out[p.PlayerId] = i + 1
		}
	}
	return out
}
