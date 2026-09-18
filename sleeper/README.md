# sleeper

Sleeper fantasy-football extension for quack. This slice (issue #93) ships only the OpenAPI
spec, the generated client, recorded fixtures, and a cached wrapper - no tools, skills, or UI
yet (see `~/workspace/wt/sleeper-requirements.md` for the full extension design).

## The spec is the source of truth

Sleeper publishes no OpenAPI or Swagger document. `openapi.yaml` is owned by this module,
written from endpoints probed live on 2026-09-18 and response shapes derived from real
recorded data - not from Sleeper's prose docs. Paths marked `x-undocumented: true` were found
by probing (not in Sleeper's own docs) and may disappear without notice; treat a 404 from one
of those as expected, not a bug.

The generated client lives in `sleepergen/` (`go generate ./sleeper/...`, config
`sleepergen/genconfig.yaml`, pinned to `oapi-codegen/v2@v2.7.0` - the same version and pattern
quack uses for its Langfuse client in `internal/langfuse`). Client + models only, no server:
this module only ever calls out to `api.sleeper.app`.

Contract tests (`contract_test.go`) decode every fixture in `testdata/get/` into its generated
type with unknown fields disallowed - an extra or renamed field fails the test instead of a
404/400 in prod. Missing-field drift needs a second check: `DisallowUnknownFields` doesn't
catch a field that just disappears (it decodes to a zero value, silently), so `openapi.yaml`
marks the fields tools depend on `required`, and the same test also asserts every fixture
instance carries those keys, read straight from `openapi.yaml` (not a hardcoded copy) so the
spec stays the source of truth. `TestMutationDroppedRequiredFieldFails` proves the check
actually fires. `TestAllFixturesCovered` fails if a fixture has no case exercising it, so a
stale recording can't rot unnoticed.

## Fixtures

Recorded from the live API for two leagues in owner jffagerberg's (user_id
860211057418018816) chain:

- **Current season**: league `1356407594683482112` (2026, week 2) - league settings, rosters,
  users, matchups for weeks 1-2, transactions for weeks 1-2, both playoff brackets, traded
  picks, the league's drafts, one draft with its picks, NFL state, trending add/drop, one
  player, the players dump, week-2 projections and stats, one team's depth chart, the season
  schedule, player research, and one player's season stats.
- **Past season**: league `1257102049204514816` (2025, complete) - league settings, rosters,
  winners bracket, matchups for weeks 1 and 14, transactions for weeks 1 and 5 (week 5 has two
  real trades - the only trade shape verified against live data; this league runs with
  `settings.pick_trading: 0`, so `Transaction.draft_picks` and both `traded_picks` endpoints
  are always `[]` and `TradedPick`'s field shape is unverified in both leagues), and its draft
  (`1257102049204514817`, all 140 picks) with its traded picks - recorded for the
  league-history job's `Chain` walk (`previous_league_id`), which the current league's chain
  reaches in one hop (a second hop exists live but isn't fixtured; `Chain` errors rather than
  silently truncating when it hits that gap - see `client.go`).

**Trimmed, not full dumps** (large upstream payloads, kept small deliberately):

- Players dump (`/v1/players/nfl`, ~14 MB live): trimmed to the union of every player_id
  referenced by the other fixtures - both leagues' rosters/starters, matchups, draft picks,
  transactions (adds/drops), trending add/drop, and the depth-chart sample (312 entries,
  including every DEF unit those fixtures cite). `TestPlayersDumpCoversReferencedIDs`
  (`client_test.go`) re-derives that reference set from the fixtures at test time and fails if
  the dump falls behind it again.
- Week-2 projections: trimmed to the current league's 143 rostered player_ids plus the top 30
  unrostered free agents by `pts_ppr` projection (173 entries). Week-2 stats: the live
  endpoint itself only carried 141 players total this early in the week (most hadn't accrued
  a stat line yet); intersected with the same keep-set, 11 remain.
- 2025 season stats: trimmed to the 140 players drafted in the past league.

Every other fixture is the live response as-is (all under 60 KB).

## Re-recording

```bash
cd sleeper
go run ./cmd/qa-mock --fixtures testdata/get --addr :8092 --record &
curl http://localhost:8092/v1/league/1356407594683482112   # any path from openapi.yaml
kill %1
```

`--record` proxies a fixture miss to the real `https://api.sleeper.app` and saves the
response under `testdata/get/<hash>.json`, keyed the same way `github/cmd/qa-mock` keys GitHub
fixtures: `sha256("GET_" + path [+ "_" + query])[:8 bytes hex] + ".json"`. Re-recording the
players dump, week-2 projections/stats, or the 2025 season stats will pull the full untrimmed
payload back down - re-run the trim script in this PR's history (or re-derive: filter to
rostered/drafted player_ids plus top free agents by `pts_ppr`) before committing.

`cmd/qa-mock` with no `--record` flag (there is no `serve` subcommand - just the one binary and
flags above) replays only what's already recorded and 404s on a miss, so CI and
`client_test.go` never touch the real API.

## Client

`client.go` wraps the generated client with an in-memory cache (per-endpoint TTLs from the
issue: state 5m, league/rosters/users 10m, matchups 2m, players dump 24h, projections/stats
1h) and a player name index (lowercased "first last", last name, and DEF team codes ->
player_ids), built from whatever `PlayersDump` last fetched. `Chain` walks a league's
`previous_league_id` back through past seasons (newest first, bounded to 10 seasons, cached
24h) for the league-history job.

`NewClient(baseURL, httpClient)` takes an explicit base URL so tests (and later, tools) point
at `cmd/qa-mock` instead of the real API. Sleeper answers an unknown id with HTTP 200 and a
literal `null` body rather than a 404; `okJSON` treats that the same as a real not-found error
instead of returning a zero-valued struct with no error.
