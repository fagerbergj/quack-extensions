// Shared plumbing every sleeper_* tool needs: resolving user/league/week
// from args or config defaults, and naming players/teams from cached data.
package sleeper

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/fagerbergj/quack-extensions/sleeper/sleepergen"
)

// nowRFC3339 stamps a tool result's fetched_at - the time this tool call
// read the (possibly cached) upstream data, so a judge can tell staleness.
func nowRFC3339() string { return time.Now().UTC().Format(time.RFC3339) }

// resolveLeagueID falls back to the extension's configured default league.
func (e *extension) resolveLeagueID(arg string) (string, error) {
	if arg != "" {
		return arg, nil
	}
	if e.cfg.DefaultLeague != "" {
		return e.cfg.DefaultLeague, nil
	}
	return "", fmt.Errorf("league_id is required (no default_league configured)")
}

// resolveUserIdentifier falls back to the extension's configured default user.
func (e *extension) resolveUserIdentifier(arg string) (string, error) {
	if arg != "" {
		return arg, nil
	}
	if e.cfg.DefaultUser != "" {
		return e.cfg.DefaultUser, nil
	}
	return "", fmt.Errorf("user is required (no default_user configured)")
}

// season returns the configured season, or the live one from /state/nfl.
func (e *extension) season(ctx context.Context) (string, *sleepergen.NflState, error) {
	state, err := e.client.State(ctx)
	if err != nil {
		return "", nil, fmt.Errorf("sleeper: state: %w", err)
	}
	if e.cfg.Season > 0 {
		return fmt.Sprintf("%d", e.cfg.Season), state, nil
	}
	return state.Season, state, nil
}

// resolveWeek returns arg if positive, else the state's current week.
func resolveWeek(arg int, state *sleepergen.NflState) int {
	if arg > 0 {
		return arg
	}
	return state.Week
}

// rosterIDForUser finds the roster a user_id (or, failing that, a
// display/team name) owns in a league's roster list.
func rosterIDForUser(rosters []sleepergen.Roster, users []sleepergen.LeagueUser, userOrName string) (int, bool) {
	for _, r := range rosters {
		if r.OwnerId != nil && *r.OwnerId == userOrName {
			return r.RosterId, true
		}
	}
	needle := strings.ToLower(userOrName)
	for _, u := range users {
		if strings.ToLower(u.DisplayName) != needle && strings.ToLower(teamName(u)) != needle {
			continue
		}
		for _, r := range rosters {
			if r.OwnerId != nil && *r.OwnerId == u.UserId {
				return r.RosterId, true
			}
		}
	}
	return 0, false
}

// teamName prefers the league metadata team_name a user set, falling back
// to their Sleeper display name.
func teamName(u sleepergen.LeagueUser) string {
	if u.Metadata != nil {
		if n, ok := (*u.Metadata)["team_name"]; ok && n != "" {
			return n
		}
	}
	return u.DisplayName
}

// teamNames maps roster_id -> team name for a league, joining rosters (for
// owner_id) with league users (for team_name/display_name).
func teamNames(rosters []sleepergen.Roster, users []sleepergen.LeagueUser) map[int]string {
	byUser := make(map[string]string, len(users))
	for _, u := range users {
		byUser[u.UserId] = teamName(u)
	}
	out := make(map[int]string, len(rosters))
	for _, r := range rosters {
		name := fmt.Sprintf("roster %d", r.RosterId)
		if r.OwnerId != nil {
			if n, ok := byUser[*r.OwnerId]; ok {
				name = n
			}
		}
		out[r.RosterId] = name
	}
	return out
}

// playerName resolves a player_id to a display name via the dump.
func playerName(dump map[string]sleepergen.Player, playerID string) string {
	p, ok := dump[playerID]
	if !ok {
		return playerID
	}
	return playerNameOf(p)
}

// rosterFor finds a league's roster by roster_id.
func rosterFor(rosters []sleepergen.Roster, rosterID int) (sleepergen.Roster, bool) {
	for _, r := range rosters {
		if r.RosterId == rosterID {
			return r, true
		}
	}
	return sleepergen.Roster{}, false
}

// sortedByProjection ranks player ids by their pts_ppr projection,
// highest first - the one scoring figure every Sleeper projection carries.
func sortedByProjection(ids []string, proj map[string]sleepergen.StatMap) []string {
	out := append([]string(nil), ids...)
	sort.SliceStable(out, func(i, j int) bool {
		return proj[out[i]]["pts_ppr"] > proj[out[j]]["pts_ppr"]
	})
	return out
}
