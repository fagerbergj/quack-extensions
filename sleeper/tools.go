// Shared plumbing every sleeper_* tool needs: resolving user/league/week
// from args or config defaults, and naming players/teams from cached data.
package sleeper

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
	_ "time/tzdata" // the `tz` argument must resolve even in an image without /usr/share/zoneinfo

	"github.com/fagerbergj/quack-extensions/sleeper/sleepergen"
)

// localLayout is the *_local display format, e.g. "Sun Sep 27 3:05 PM CDT".
const localLayout = "Mon Jan 2 3:04 PM MST"

// clock is one tool call's single "now" plus the zone its *_local fields render in.
type clock struct {
	now time.Time
	loc *time.Location
}

// newClock resolves the optional `tz` tool argument (an IANA zone name),
// falling back to the process's local zone.
func newClock(tz string) (clock, error) {
	loc := time.Local
	if tz = strings.TrimSpace(tz); tz != "" {
		l, err := time.LoadLocation(tz)
		if err != nil {
			return clock{}, fmt.Errorf("tz %q is not a valid IANA time zone name (e.g. America/Chicago): %w", tz, err)
		}
		loc = l
	}
	return clock{now: time.Now(), loc: loc}, nil
}

func (c clock) local(t time.Time) string { return t.In(c.loc).Format(localLayout) }

// callContext heads every week-scoped result: which week it covers,
// relative to the live NFL week, and when (UTC and the user's zone) it was read.
type callContext struct {
	Week           int    `json:"week"`
	CurrentWeek    int    `json:"current_week,omitempty"`
	WeekNote       string `json:"week_note,omitempty"`
	ScheduleNote   string `json:"schedule_note,omitempty"`
	FetchedAt      string `json:"fetched_at"`
	FetchedAtLocal string `json:"fetched_at_local"`
}

func newCallContext(c clock, week int, season string, state *sleepergen.NflState) callContext {
	cc := callContext{Week: week, FetchedAt: c.now.UTC().Format(time.RFC3339), FetchedAtLocal: c.local(c.now)}
	switch {
	case season != state.Season:
		cc.WeekNote = fmt.Sprintf("season %s week %d, not the live NFL season (%s)", season, week, state.Season)
	case week != state.Week:
		cc.CurrentWeek = state.Week
		cc.WeekNote = fmt.Sprintf("week %d is NOT the current NFL week (%d)", week, state.Week)
	default:
		cc.CurrentWeek = state.Week
	}
	return cc
}

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

// resolveWeek returns arg if positive, else the live current week - but
// only when season is that live season; a pinned past/future season has no
// borrowable "current week", so it must be given explicitly.
func resolveWeek(arg int, season string, state *sleepergen.NflState) (int, error) {
	if arg > 0 {
		return arg, nil
	}
	return weekForSeason(season, state)
}

// weekForSeason is resolveWeek's no-arg-given core, reused by tools with no
// week argument of their own (roster byes, free agents/player projections).
func weekForSeason(season string, state *sleepergen.NflState) (int, error) {
	if season != state.Season {
		return 0, fmt.Errorf("season %s is not the live NFL season (%s); week must be given explicitly", season, state.Season)
	}
	return state.Week, nil
}

// resolveRosterID returns rosterID if set, else the roster owned by user
// (falling back to default_user).
func (e *extension) resolveRosterID(rosterID int, user string, rosters []sleepergen.Roster, users []sleepergen.LeagueUser) (int, error) {
	if rosterID != 0 {
		return rosterID, nil
	}
	uid, err := e.resolveUserIdentifier(user)
	if err != nil {
		return 0, err
	}
	id, ok := rosterIDForUser(rosters, users, uid)
	if !ok {
		return 0, fmt.Errorf("no roster found for %q", uid)
	}
	return id, nil
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
		if n, ok := (*u.Metadata)["team_name"]; ok {
			if trimmed := strings.TrimSpace(n); trimmed != "" {
				return trimmed
			}
		}
	}
	return ownerName(u)
}

// ownerName trims a Sleeper display name - some carry trailing spaces
// ("Brown Tuddies Likely "), and every render surface must agree.
func ownerName(u sleepergen.LeagueUser) string {
	return strings.TrimSpace(u.DisplayName)
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

// playerPosition resolves a player_id's position via the dump - "DEF" for a
// team-defense id (e.g. "CAR"), same as sleeper_free_agents.
func playerPosition(dump map[string]sleepergen.Player, playerID string) string {
	return strVal(dump[playerID].Position)
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
