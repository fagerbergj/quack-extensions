package sleeper

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/fagerbergj/quack-extensions/sleeper/sleepergen"
)

// Per-endpoint TTLs (quack-extensions issue #93): state moves fastest,
// rosters/league settings change rarely, players/projections/stats are
// bulk fetches worth holding onto longer.
const (
	ttlState       = 5 * time.Minute
	ttlLeague      = 10 * time.Minute
	ttlMatchups    = 2 * time.Minute
	ttlPlayersDump = 24 * time.Hour
	ttlStatMap     = time.Hour
	ttlChain       = 24 * time.Hour

	// maxChainSeasons bounds Chain against a malformed or cyclic
	// previous_league_id chain - no real league runs this deep.
	maxChainSeasons = 10
)

// Client is a thin, in-memory-cached wrapper over the generated Sleeper
// client. Tools (a later slice) are the only intended caller.
type Client struct {
	gen *sleepergen.ClientWithResponses

	mu    sync.Mutex
	cache map[string]cacheEntry

	namesMu sync.RWMutex
	names   map[string][]string // lowercased "first last"/last name/DEF team code -> player_ids
}

type cacheEntry struct {
	value   any
	expires time.Time
}

// NewClient builds a Client against baseURL (https://api.sleeper.app in
// production, a qa-mock server in tests). httpClient may be nil to use the
// generated client's default.
func NewClient(baseURL string, httpClient *http.Client) (*Client, error) {
	var opts []sleepergen.ClientOption
	if httpClient != nil {
		opts = append(opts, sleepergen.WithHTTPClient(httpClient))
	}
	gen, err := sleepergen.NewClientWithResponses(baseURL, opts...)
	if err != nil {
		return nil, fmt.Errorf("sleeper: new client: %w", err)
	}
	return &Client{gen: gen, cache: map[string]cacheEntry{}}, nil
}

// cached fetches, or replays a cached, value under key - shared by every
// method below so each endpoint's caching stays a one-line call.
func cached[T any](c *Client, key string, ttl time.Duration, fetch func() (T, error)) (T, error) {
	c.mu.Lock()
	if e, ok := c.cache[key]; ok && time.Now().Before(e.expires) {
		c.mu.Unlock()
		return e.value.(T), nil
	}
	c.mu.Unlock()

	v, err := fetch()
	if err != nil {
		var zero T
		return zero, err
	}
	c.mu.Lock()
	c.cache[key] = cacheEntry{value: v, expires: time.Now().Add(ttl)}
	c.mu.Unlock()
	return v, nil
}

func (c *Client) State(ctx context.Context) (*sleepergen.NflState, error) {
	return cached(c, "state", ttlState, func() (*sleepergen.NflState, error) {
		resp, err := c.gen.GetNflStateWithResponse(ctx)
		if err != nil {
			return nil, fmt.Errorf("sleeper: get state: %w", err)
		}
		return okJSON(resp.JSON200, resp.HTTPResponse, resp.Body)
	})
}

func (c *Client) League(ctx context.Context, leagueID string) (*sleepergen.League, error) {
	return cached(c, "league:"+leagueID, ttlLeague, func() (*sleepergen.League, error) {
		resp, err := c.gen.GetLeagueWithResponse(ctx, leagueID)
		if err != nil {
			return nil, fmt.Errorf("sleeper: get league %s: %w", leagueID, err)
		}
		return okJSON(resp.JSON200, resp.HTTPResponse, resp.Body)
	})
}

// cachedList is cached specialized for the common shape below: a generated
// call returning a *slice-or-map JSON200 plus the raw response for error
// reporting. Collapses what would otherwise be five near-identical methods.
func cachedList[T any](c *Client, key string, ttl time.Duration, fetch func() (*T, *http.Response, []byte, error)) (T, error) {
	return cached(c, key, ttl, func() (T, error) {
		var zero T
		json, resp, body, err := fetch() //nolint:bodyclose // resp is ClientWithResponses' post-parse HTTPResponse; the body is already read and closed
		if err != nil {
			return zero, err
		}
		out, err := okJSON(json, resp, body)
		if err != nil {
			return zero, err
		}
		return *out, nil
	})
}

//nolint:dupl // each generated ...WithResponse call differs only by type and method name; not worth a reflection-based dispatcher for 4 read-only calls
func (c *Client) Rosters(ctx context.Context, leagueID string) ([]sleepergen.Roster, error) {
	return cachedList(c, "rosters:"+leagueID, ttlLeague, func() (*[]sleepergen.Roster, *http.Response, []byte, error) {
		resp, err := c.gen.GetLeagueRostersWithResponse(ctx, leagueID)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("sleeper: get rosters %s: %w", leagueID, err)
		}
		return resp.JSON200, resp.HTTPResponse, resp.Body, nil
	})
}

//nolint:dupl // see Rosters
func (c *Client) LeagueUsers(ctx context.Context, leagueID string) ([]sleepergen.LeagueUser, error) {
	return cachedList(c, "league_users:"+leagueID, ttlLeague, func() (*[]sleepergen.LeagueUser, *http.Response, []byte, error) {
		resp, err := c.gen.GetLeagueUsersWithResponse(ctx, leagueID)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("sleeper: get league users %s: %w", leagueID, err)
		}
		return resp.JSON200, resp.HTTPResponse, resp.Body, nil
	})
}

func (c *Client) Matchups(ctx context.Context, leagueID string, week int) ([]sleepergen.Matchup, error) {
	key := fmt.Sprintf("matchups:%s:%d", leagueID, week)
	return cachedList(c, key, ttlMatchups, func() (*[]sleepergen.Matchup, *http.Response, []byte, error) {
		resp, err := c.gen.GetLeagueMatchupsWithResponse(ctx, leagueID, week)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("sleeper: get matchups %s week %d: %w", leagueID, week, err)
		}
		return resp.JSON200, resp.HTTPResponse, resp.Body, nil
	})
}

// WeekProjections/WeekStats share Sleeper's stat-map shape and hour TTL.
//
//nolint:dupl // see Rosters
func (c *Client) WeekProjections(ctx context.Context, season string, week int) (map[string]sleepergen.StatMap, error) {
	key := fmt.Sprintf("projections:%s:%d", season, week)
	return cachedList(c, key, ttlStatMap, func() (*map[string]sleepergen.StatMap, *http.Response, []byte, error) {
		resp, err := c.gen.GetWeekProjectionsWithResponse(ctx, season, week)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("sleeper: get projections %s week %d: %w", season, week, err)
		}
		return resp.JSON200, resp.HTTPResponse, resp.Body, nil
	})
}

//nolint:dupl // see Rosters
func (c *Client) WeekStats(ctx context.Context, season string, week int) (map[string]sleepergen.StatMap, error) {
	key := fmt.Sprintf("stats:%s:%d", season, week)
	return cachedList(c, key, ttlStatMap, func() (*map[string]sleepergen.StatMap, *http.Response, []byte, error) {
		resp, err := c.gen.GetWeekStatsWithResponse(ctx, season, week)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("sleeper: get stats %s week %d: %w", season, week, err)
		}
		return resp.JSON200, resp.HTTPResponse, resp.Body, nil
	})
}

// PlayersDump fetches (or replays) the full player map and refreshes the
// name index used by ResolvePlayer - both share the 24h TTL since the
// index is only ever as fresh as the dump it was built from.
func (c *Client) PlayersDump(ctx context.Context) (map[string]sleepergen.Player, error) {
	out, err := cachedList(c, "players", ttlPlayersDump, func() (*map[string]sleepergen.Player, *http.Response, []byte, error) {
		resp, err := c.gen.GetAllPlayersWithResponse(ctx)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("sleeper: get players: %w", err)
		}
		return resp.JSON200, resp.HTTPResponse, resp.Body, nil
	})
	if err != nil {
		return nil, err
	}
	c.namesMu.Lock()
	if c.names == nil {
		c.names = buildNameIndex(out)
	}
	c.namesMu.Unlock()
	return out, nil
}

// ResolvePlayer looks a free-text name up in the index PlayersDump last
// built - call PlayersDump first in the same request path. Matching is
// exact on lowercased "first last", last name, or a DEF's team code.
func (c *Client) ResolvePlayer(name string) []string {
	c.namesMu.RLock()
	defer c.namesMu.RUnlock()
	return c.names[strings.ToLower(strings.TrimSpace(name))]
}

// Chain walks previous_league_id from leagueID back through past seasons,
// newest first, bounded to maxChainSeasons so a bad or cyclic pointer can't
// loop forever. Cached 24h per starting league.
func (c *Client) Chain(ctx context.Context, leagueID string) ([]sleepergen.League, error) {
	return cached(c, "chain:"+leagueID, ttlChain, func() ([]sleepergen.League, error) {
		var out []sleepergen.League
		id := leagueID
		for i := 0; i < maxChainSeasons && id != ""; i++ {
			league, err := c.League(ctx, id)
			if err != nil {
				// The starting league must resolve; an older season
				// vanishing (deleted, access revoked) just ends the chain.
				if i == 0 {
					return nil, err
				}
				break
			}
			out = append(out, *league)
			if league.PreviousLeagueId == nil {
				break
			}
			id = *league.PreviousLeagueId
		}
		return out, nil
	})
}

// okJSON turns a generated response's nil-on-non-200 JSON200 field into an
// error carrying the real status and body, so a caching layer never masks
// an upstream failure as a cache miss.
func okJSON[T any](json *T, resp *http.Response, body []byte) (*T, error) {
	if json == nil {
		status := "unknown"
		if resp != nil {
			status = resp.Status
		}
		return nil, fmt.Errorf("sleeper: unexpected response %s: %s", status, string(body))
	}
	return json, nil
}

// buildNameIndex indexes a players dump by lowercased "first last", last
// name alone, and (for DEF units, whose player_id already equals a team
// code) that code - the three shapes a tool's free-text name arg may use.
func buildNameIndex(players map[string]sleepergen.Player) map[string][]string {
	idx := map[string][]string{}
	add := func(key, id string) {
		key = strings.ToLower(strings.TrimSpace(key))
		if key == "" {
			return
		}
		idx[key] = append(idx[key], id)
	}
	for id, p := range players {
		first, last := strVal(p.FirstName), strVal(p.LastName)
		if first != "" && last != "" {
			add(first+" "+last, id)
		}
		if last != "" {
			add(last, id)
		}
		if strVal(p.Position) == "DEF" {
			add(strVal(p.Team), id)
		}
	}
	return idx
}

func strVal(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
