package sleeper

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/fagerbergj/quack-extensions/sdk"
	"github.com/fagerbergj/quack-extensions/sleeper/sleepergen"
	"github.com/go-chi/chi/v5"
)

//go:embed ui/static
var uiStaticFS embed.FS

//go:embed ui/fixtures
var uiFixturesFS embed.FS

var fixtureBytes = loadFixtures()

// fixtureJobNames enumerates every artifact name a fixture file exists for -
// the job ids plus the two non-job artifacts (season-notes, trade).
var fixtureJobNames = []string{"lineup", "waivers", "trade", "digest", "trends", "retro", "draft", "history", "season-notes"}

func loadFixtures() map[string]json.RawMessage {
	out := make(map[string]json.RawMessage, len(fixtureJobNames))
	for _, name := range fixtureJobNames {
		b, err := uiFixturesFS.ReadFile("ui/fixtures/" + name + ".json")
		if err == nil {
			out[name] = json.RawMessage(b)
		}
	}
	return out
}

// mountUI wires the extension's served page and JSON API onto the authed
// router; called from sleeper.go's RegisterRoutes.
func (e *extension) mountUI(authed chi.Router) {
	authed.Get("/api/seasons", e.handleSeasons)
	authed.Get("/api/season", e.handleSeason)
	authed.Get("/api/artifacts", e.handleArtifacts)
	authed.Post("/api/jobs", e.handleJobs)

	static, err := fs.Sub(uiStaticFS, "ui/static")
	if err != nil {
		// The embedded FS is compiled into the binary; a broken Sub here is a
		// build-time bug (a renamed/missing directory), never a runtime state.
		panic("sleeper: embedded UI assets missing: " + err.Error())
	}
	// chi's r.Mount("/"+name, combined) in quack's router does not strip the
	// prefix, so the file server must strip it itself or every asset 404s.
	fileServer := http.StripPrefix("/"+extensionName, http.FileServer(http.FS(static)))
	authed.Handle("/*", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Bare "/sleeper" (no trailing slash) would serve index.html at a path where
		// its relative asset/API refs resolve wrong; send it to the real page root.
		if r.URL.Path == "/"+extensionName {
			http.Redirect(w, r, r.URL.Path+"/", http.StatusMovedPermanently)
			return
		}
		fileServer.ServeHTTP(w, r)
	}))
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// writeLeagueErr honors client.go's typed ErrNotFound (a real 404 or
// Sleeper's HTTP-200-null-body) as 404; anything else is an upstream failure (502).
func writeLeagueErr(w http.ResponseWriter, err error) {
	if errors.Is(err, ErrNotFound) {
		writeErr(w, http.StatusNotFound, "unknown league")
		return
	}
	writeErr(w, http.StatusBadGateway, err.Error())
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// resolveUserID looks cfg.DefaultUser (a username or user_id) up once per
// request; ok is false when no default user is configured or Sleeper
// doesn't recognize it, and every "me"-scoped field degrades to absent.
func (e *extension) resolveUserID(ctx context.Context) (string, bool) {
	if e.cfg.DefaultUser == "" {
		return "", false
	}
	u, err := e.client.User(ctx, e.cfg.DefaultUser)
	if err != nil {
		return "", false
	}
	return u.UserId, true
}

func rosterPoints(r sleepergen.Roster, prefix string) float64 {
	return float64(r.Settings[prefix]) + float64(r.Settings[prefix+"_decimal"])/100
}

func intSetting(m map[string]int, key string, def int) int {
	if v, ok := m[key]; ok {
		return v
	}
	return def
}

func reserveSlots(positions []string) int {
	n := 0
	for _, p := range positions {
		if p == "IR" {
			n++
		}
	}
	return n
}

func scoringType(s map[string]float32) string {
	rec, ok := s["rec"]
	switch {
	case !ok:
		return ""
	case rec >= 1:
		return "PPR"
	case rec > 0:
		return "Half-PPR"
	default:
		return "Standard"
	}
}

func waiverTypeLabel(wt int) string {
	if wt == 2 {
		return "FAAB waivers"
	}
	return "Rolling waivers"
}

func buildFacts(league *sleepergen.League) []string {
	facts := []string{fmt.Sprintf("%d teams", league.TotalRosters)}
	if s := scoringType(league.ScoringSettings); s != "" {
		facts = append(facts, s)
	}
	facts = append(facts, waiverTypeLabel(league.Settings["waiver_type"]))
	if pt, ok := league.Settings["playoff_teams"]; ok && pt > 0 {
		facts = append(facts, fmt.Sprintf("%d playoff spots · week %d", pt, league.Settings["playoff_week_start"]))
	}
	return facts
}

func usersByOwnerID(users []sleepergen.LeagueUser) map[string]sleepergen.LeagueUser {
	out := make(map[string]sleepergen.LeagueUser, len(users))
	for _, u := range users {
		out[u.UserId] = u
	}
	return out
}

func rosterByOwner(rosters []sleepergen.Roster, userID string) *sleepergen.Roster {
	for i := range rosters {
		if rosters[i].OwnerId != nil && *rosters[i].OwnerId == userID {
			return &rosters[i]
		}
	}
	return nil
}

func rosterByID(rosters []sleepergen.Roster, id int) *sleepergen.Roster {
	for i := range rosters {
		if rosters[i].RosterId == id {
			return &rosters[i]
		}
	}
	return nil
}

type standingRow struct {
	ID     string  `json:"id"` // the owner's Sleeper user_id - stable, unlike team name
	Team   string  `json:"team"`
	Owner  string  `json:"owner"`
	Wins   int     `json:"wins"`
	Losses int     `json:"losses"`
	PF     float64 `json:"pf"`
	PA     float64 `json:"pa"`
	Mine   bool    `json:"mine"`
}

func standingRowFor(ro sleepergen.Roster, usersByOwner map[string]sleepergen.LeagueUser, myRoster *sleepergen.Roster) standingRow {
	var u sleepergen.LeagueUser
	var id string
	if ro.OwnerId != nil {
		u = usersByOwner[*ro.OwnerId]
		id = *ro.OwnerId
	}
	return standingRow{
		ID: id, Team: teamName(u), Owner: u.DisplayName,
		Wins: ro.Settings["wins"], Losses: ro.Settings["losses"],
		PF: rosterPoints(ro, "fpts"), PA: rosterPoints(ro, "fpts_against"),
		Mine: myRoster != nil && ro.RosterId == myRoster.RosterId,
	}
}

func buildStandings(rosters []sleepergen.Roster, usersByOwner map[string]sleepergen.LeagueUser, myRoster *sleepergen.Roster) []standingRow {
	out := make([]standingRow, 0, len(rosters))
	for _, ro := range rosters {
		out = append(out, standingRowFor(ro, usersByOwner, myRoster))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Wins != out[j].Wins {
			return out[i].Wins > out[j].Wins
		}
		return out[i].PF > out[j].PF
	})
	return out
}

type opponentInfo struct {
	Team   string `json:"team"`
	Owner  string `json:"owner"`
	Wins   int    `json:"wins"`
	Losses int    `json:"losses"`
}

func matchupIDFor(matchups []sleepergen.Matchup, rosterID int) *int {
	for _, m := range matchups {
		if m.RosterId == rosterID {
			return m.MatchupId
		}
	}
	return nil
}

func opponentRosterID(matchups []sleepergen.Matchup, myRosterID, matchupID int) (int, bool) {
	for _, m := range matchups {
		if m.RosterId != myRosterID && m.MatchupId != nil && *m.MatchupId == matchupID {
			return m.RosterId, true
		}
	}
	return 0, false
}

// findOpponent is nil whenever there's no "me" perspective, this league
// isn't the live NFL season, or the current week has no matchup pairing yet
// (e.g. before week 1 locks).
func (e *extension) findOpponent(ctx context.Context, c *Client, league *sleepergen.League, rosters []sleepergen.Roster, usersByOwner map[string]sleepergen.LeagueUser, myRoster *sleepergen.Roster) *opponentInfo {
	if myRoster == nil {
		return nil
	}
	st, err := c.State(ctx)
	if err != nil || st.Season != league.Season {
		return nil
	}
	matchups, err := c.Matchups(ctx, league.LeagueId, st.Week)
	if err != nil {
		return nil
	}
	myMatchupID := matchupIDFor(matchups, myRoster.RosterId)
	if myMatchupID == nil {
		return nil
	}
	oppRosterID, ok := opponentRosterID(matchups, myRoster.RosterId, *myMatchupID)
	if !ok {
		return nil
	}
	oppRoster := rosterByID(rosters, oppRosterID)
	if oppRoster == nil || oppRoster.OwnerId == nil {
		return nil
	}
	u := usersByOwner[*oppRoster.OwnerId]
	return &opponentInfo{Team: teamName(u), Owner: u.DisplayName, Wins: oppRoster.Settings["wins"], Losses: oppRoster.Settings["losses"]}
}

func playerLabel(id string, players map[string]sleepergen.Player) string {
	p, ok := players[id]
	if !ok {
		return id
	}
	if name := strVal(p.FullName); name != "" {
		return name
	}
	if full := strings.TrimSpace(strVal(p.FirstName) + " " + strVal(p.LastName)); full != "" {
		return full
	}
	if strVal(p.Position) == "DEF" {
		return strVal(p.Team) + " DEF"
	}
	return id
}

func playerNames(m *map[string]int, players map[string]sleepergen.Player) []string {
	if m == nil {
		return nil
	}
	out := make([]string, 0, len(*m))
	for id := range *m {
		out = append(out, playerLabel(id, players))
	}
	sort.Strings(out)
	return out
}

func teamsFor(ids []int, ownerName map[int]string) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if n, ok := ownerName[id]; ok {
			out = append(out, n)
		}
	}
	return out
}

func moveLabel(txType string, adds, drops []string) string {
	switch {
	case txType == "trade":
		return "Trade"
	case len(adds) > 0:
		return "Add " + adds[0]
	case len(drops) > 0:
		return "Drop " + drops[0]
	default:
		return "Move"
	}
}

type moveRow struct {
	Type  string   `json:"type"`
	Week  int      `json:"week"`
	Label string   `json:"label"`
	Adds  []string `json:"adds"`
	Drops []string `json:"drops"`
	By    []string `json:"by"`
}

func moveRowFor(tx sleepergen.Transaction, week int, players map[string]sleepergen.Player, ownerName map[int]string) moveRow {
	adds := playerNames(tx.Adds, players)
	drops := playerNames(tx.Drops, players)
	return moveRow{
		Type: tx.Type, Week: week, Label: moveLabel(tx.Type, adds, drops),
		Adds: adds, Drops: drops, By: teamsFor(tx.RosterIds, ownerName),
	}
}

// buildMoves is best-effort: any upstream failure (transactions, players
// dump) yields an empty list rather than failing the whole season response.
func (e *extension) buildMoves(ctx context.Context, c *Client, leagueID string, week int, rosters []sleepergen.Roster, users []sleepergen.LeagueUser) []moveRow {
	if week < 1 {
		week = 1
	}
	txs, err := c.Transactions(ctx, leagueID, week)
	if err != nil {
		return nil
	}
	players, _ := c.PlayersDump(ctx)
	ownerName := teamNames(rosters, users)
	moves := make([]moveRow, 0, len(txs))
	for _, tx := range txs {
		moves = append(moves, moveRowFor(tx, week, players, ownerName))
	}
	return moves
}

type leagueFacts struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Season  string `json:"season"`
	WeekNow int    `json:"week_now"`
	Status  string `json:"status"`
}

type meInfo struct {
	Team      string `json:"team"`
	Owner     string `json:"owner"`
	Wins      int    `json:"wins"`
	Losses    int    `json:"losses"`
	WaiverPos int    `json:"waiver_pos"`
}

type seasonResponse struct {
	League       leagueFacts   `json:"league"`
	Facts        []string      `json:"facts"`
	Me           *meInfo       `json:"me"`
	Opponent     *opponentInfo `json:"opponent"`
	Standings    []standingRow `json:"standings"`
	RecentMoves  []moveRow     `json:"recent_moves"`
	ReserveSlots int           `json:"reserve_slots"`
	PlayoffLine  int           `json:"playoff_line"`
}

// handleSeason serves GET /sleeper/api/season?league_id= - one season's live
// league/me/opponent/standings/moves, never from job artifacts (see handleArtifacts).
func (e *extension) handleSeason(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	leagueID := firstNonEmpty(r.URL.Query().Get("league_id"), e.cfg.DefaultLeague)
	if leagueID == "" {
		writeErr(w, http.StatusBadRequest, "league_id is required")
		return
	}
	c := e.client
	league, err := c.League(ctx, leagueID)
	if err != nil {
		writeLeagueErr(w, err)
		return
	}
	rosters, err := c.Rosters(ctx, leagueID)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	users, err := c.LeagueUsers(ctx, leagueID)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	usersByOwner := usersByOwnerID(users)
	weekNow := 0
	if st, err := c.State(ctx); err == nil {
		weekNow = st.Week
	}

	myRoster := e.findMyRoster(ctx, rosters)
	resp := seasonResponse{
		League:       leagueFacts{ID: league.LeagueId, Name: league.Name, Season: league.Season, WeekNow: weekNow, Status: league.Status},
		Facts:        buildFacts(league),
		Standings:    buildStandings(rosters, usersByOwner, myRoster),
		ReserveSlots: reserveSlots(league.RosterPositions),
		PlayoffLine:  intSetting(league.Settings, "playoff_teams", 6),
		RecentMoves:  e.buildMoves(ctx, c, leagueID, weekNow, rosters, users),
	}
	if myRoster != nil {
		u := usersByOwner[*myRoster.OwnerId]
		resp.Me = &meInfo{Team: teamName(u), Owner: u.DisplayName, Wins: myRoster.Settings["wins"], Losses: myRoster.Settings["losses"], WaiverPos: myRoster.Settings["waiver_position"]}
		resp.Opponent = e.findOpponent(ctx, c, league, rosters, usersByOwner, myRoster)
	}
	writeJSON(w, resp)
}

func (e *extension) findMyRoster(ctx context.Context, rosters []sleepergen.Roster) *sleepergen.Roster {
	userID, ok := e.resolveUserID(ctx)
	if !ok {
		return nil
	}
	return rosterByOwner(rosters, userID)
}

type seasonSummary struct {
	Season   string `json:"season"`
	LeagueID string `json:"league_id"`
	Wins     int    `json:"wins"`
	Losses   int    `json:"losses"`
	Status   string `json:"status"`
}

type seasonsResponse struct {
	CurrentSeason string          `json:"current_season"`
	Seasons       []seasonSummary `json:"seasons"`
}

func (e *extension) seasonSummaryFor(ctx context.Context, c *Client, lg sleepergen.League, userID string, haveUser bool) seasonSummary {
	out := seasonSummary{Season: lg.Season, LeagueID: lg.LeagueId, Status: lg.Status}
	if !haveUser {
		return out
	}
	rosters, err := c.Rosters(ctx, lg.LeagueId)
	if err != nil {
		return out
	}
	if ro := rosterByOwner(rosters, userID); ro != nil {
		out.Wins, out.Losses = ro.Settings["wins"], ro.Settings["losses"]
	}
	return out
}

// reverseLeagues returns the chain oldest-first (Client.Chain walks
// newest-first via previous_league_id); the approved design wants the
// seasons row chronological with the current season last.
func reverseLeagues(chain []sleepergen.League) []sleepergen.League {
	out := make([]sleepergen.League, len(chain))
	for i, lg := range chain {
		out[len(chain)-1-i] = lg
	}
	return out
}

// handleSeasons serves GET /sleeper/api/seasons?league_id= - the season
// chain, oldest first, each with this league's own record for the default user.
func (e *extension) handleSeasons(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	leagueID := firstNonEmpty(r.URL.Query().Get("league_id"), e.cfg.DefaultLeague)
	if leagueID == "" {
		writeErr(w, http.StatusBadRequest, "league_id is required")
		return
	}
	c := e.client
	// A direct League() fetch first: Chain(stopOnUnreachable: true) below
	// swallows every failure into an empty result, which would otherwise
	// make a genuinely unknown league indistinguishable from an upstream 5xx.
	if _, err := c.League(ctx, leagueID); err != nil {
		writeLeagueErr(w, err)
		return
	}
	// stopOnUnreachable: a UI wants whatever history is reachable, not
	// all-or-nothing (the history job, Chain's other caller, wants the opposite).
	chain, err := c.Chain(ctx, leagueID, 0, true)
	if err != nil || len(chain) == 0 {
		writeErr(w, http.StatusNotFound, "unknown league")
		return
	}
	resp := seasonsResponse{CurrentSeason: chain[0].Season}
	userID, haveUser := e.resolveUserID(ctx)
	for _, lg := range reverseLeagues(chain) {
		resp.Seasons = append(resp.Seasons, e.seasonSummaryFor(ctx, c, lg, userID, haveUser))
	}
	writeJSON(w, resp)
}

type artifactEnvelope struct {
	Found   bool            `json:"found"`
	Example bool            `json:"example"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// readArtifact tries the real chat first; behind cfg.Fixture, a miss falls
// back to the reference example JSON, marked Example so the UI can badge it.
func (e *extension) readArtifact(chatID, name string) artifactEnvelope {
	if e.host.ReadArtifact != nil {
		if data, ok := e.host.ReadArtifact(chatID, e.cfg.DefaultUser, name); ok {
			return artifactEnvelope{Found: true, Data: json.RawMessage(data)}
		}
	}
	if e.cfg.Fixture {
		if fx, ok := fixtureBytes[name]; ok {
			return artifactEnvelope{Found: true, Example: true, Data: fx}
		}
	}
	return artifactEnvelope{Found: false}
}

type tradeTalkEnvelope struct {
	Partner   string `json:"partner"`    // display team name
	PartnerID string `json:"partner_id"` // owner's Sleeper user_id - what the chat id and dispatch actually key on
	artifactEnvelope
}

// teamRef is one other league member: ID is the owner's Sleeper user_id -
// stable across a team rename, unlike the display name a chat id used to embed.
type teamRef struct {
	ID   string
	Name string
}

func (e *extension) otherTeams(ctx context.Context, leagueID string) []teamRef {
	users, err := e.client.LeagueUsers(ctx, leagueID)
	if err != nil {
		return nil
	}
	myID, haveMe := e.resolveUserID(ctx)
	out := make([]teamRef, 0, len(users))
	for _, u := range users {
		if haveMe && u.UserId == myID {
			continue
		}
		out = append(out, teamRef{ID: u.UserId, Name: teamName(u)})
	}
	return out
}

// readTradeTalks tries every other team as a candidate trade partner (the
// SDK has no chat-listing call, so this is the only way to discover which
// per-partner chats exist) and keeps the ones that actually have a chat.
func (e *extension) readTradeTalks(ctx context.Context, leagueID string) []tradeTalkEnvelope {
	var out []tradeTalkEnvelope
	for _, t := range e.otherTeams(ctx, leagueID) {
		chatID := fmt.Sprintf("ext:sleeper:%s:trade:%s", leagueID, t.ID)
		if env := e.readArtifact(chatID, "trade"); env.Found && !env.Example {
			out = append(out, tradeTalkEnvelope{Partner: t.Name, PartnerID: t.ID, artifactEnvelope: env})
		}
	}
	if len(out) == 0 && e.cfg.Fixture {
		if fx, ok := fixtureBytes["trade"]; ok {
			name, id := fixturePartner(fx)
			out = append(out, tradeTalkEnvelope{Partner: name, PartnerID: id, artifactEnvelope: artifactEnvelope{Found: true, Example: true, Data: fx}})
		}
	}
	return out
}

// fixturePartner reads the fixture's own partner_id (a real user_id from
// the recorded league) rather than standing in the display name for it -
// keeping fixture-mode talks discoverable the same way real ones are.
func fixturePartner(fx json.RawMessage) (name, id string) {
	var v struct {
		Partner   string `json:"partner"`
		PartnerID string `json:"partner_id"`
	}
	_ = json.Unmarshal(fx, &v)
	return v.Partner, v.PartnerID
}

// jobsForStop is the closed vocabulary of artifact-backed jobs each stop
// type reads/dispatches; talksAllowed marks the one stop type (a week) that
// also has per-partner trade talks.
func jobsForStop(stop string) (jobs []string, talksAllowed bool, err error) {
	switch stop {
	case "draft":
		return []string{"draft"}, false, nil
	case "review":
		return []string{"history"}, false, nil
	default:
		wk, convErr := strconv.Atoi(stop)
		if convErr != nil || wk < 1 || wk > 18 {
			return nil, false, fmt.Errorf("stop must be \"draft\", \"review\", or a week number 1-18, got %q", stop)
		}
		return []string{"lineup", "waivers", "digest", "trends", "retro"}, true, nil
	}
}

type artifactsResponse struct {
	Stop        string                      `json:"stop"`
	Jobs        map[string]artifactEnvelope `json:"jobs"`
	Talks       []tradeTalkEnvelope         `json:"talks,omitempty"`
	SeasonNotes *artifactEnvelope           `json:"season_notes"`
}

// handleArtifacts serves GET /sleeper/api/artifacts?league_id=&stop= - every
// job artifact for that stop, season notes, and (week stops only) trade
// talks, each read via Host.ReadArtifact by its chat id.
func (e *extension) handleArtifacts(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	leagueID := firstNonEmpty(r.URL.Query().Get("league_id"), e.cfg.DefaultLeague)
	stop := r.URL.Query().Get("stop")
	if leagueID == "" || stop == "" {
		writeErr(w, http.StatusBadRequest, "league_id and stop are required")
		return
	}
	jobs, talksAllowed, err := jobsForStop(stop)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	out := artifactsResponse{Stop: stop, Jobs: make(map[string]artifactEnvelope, len(jobs))}
	for _, job := range jobs {
		chatID := fmt.Sprintf("ext:sleeper:%s:%s:%s", leagueID, stop, job)
		out.Jobs[job] = e.readArtifact(chatID, job)
	}
	notes := e.readArtifact(fmt.Sprintf("ext:sleeper:%s:season-notes", leagueID), "season-notes")
	out.SeasonNotes = &notes
	if talksAllowed {
		out.Talks = e.readTradeTalks(ctx, leagueID)
	}
	writeJSON(w, out)
}

// jobWorkflows maps a job id to its bound workflow-catalog shape name (quack
// config, PR #1501). A job with no agent yet is absent, so it keeps the
// planner path (Workflow == "") until one exists.
var jobWorkflows = map[string]string{
	"lineup":  "sleeper-lineup",
	"waivers": "sleeper-waivers",
	"trends":  "sleeper-trends",
}

type jobRequest struct {
	LeagueID string            `json:"league_id"`
	Stop     string            `json:"stop"`
	Job      string            `json:"job"`
	Args     map[string]string `json:"args"`
}

type jobResponse struct {
	ChatID  string `json:"chat_id"`
	ChatURL string `json:"chat_url"`
}

func jobIsValid(job string, jobs []string, talksAllowed bool) bool {
	if job == "trade" {
		return talksAllowed
	}
	for _, j := range jobs {
		if j == job {
			return true
		}
	}
	return false
}

func localIDFor(req jobRequest) (localID, title string, err error) {
	if req.Job == "trade" {
		partner := req.Args["partner"]
		if partner == "" {
			return "", "", fmt.Errorf("args.partner is required for the trade job")
		}
		// partner is the stable id the chat id keys on; partner_name (when the
		// UI has it) is display-only, for a readable chat title.
		name := firstNonEmpty(req.Args["partner_name"], partner)
		return fmt.Sprintf("%s:trade:%s", req.LeagueID, partner), fmt.Sprintf("Sleeper trade talk with %s", name), nil
	}
	return fmt.Sprintf("%s:%s:%s", req.LeagueID, req.Stop, req.Job), fmt.Sprintf("Sleeper %s, %s", req.Job, req.Stop), nil
}

// formatArgs appends every args key/value to the dispatched message (sorted
// for a deterministic message) - e.g. the trade job's partner/give/get, so
// the analyst sees the counterparty and offer, not just a chat id.
func formatArgs(args map[string]string) string {
	keys := make([]string, 0, len(args))
	for k := range args {
		if args[k] != "" {
			keys = append(keys, k)
		}
	}
	if len(keys) == 0 {
		return ""
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%s: %s", k, args[k])
	}
	return " " + strings.Join(parts, "; ") + "."
}

// dispatchSeasonNotes: only trends writes notes, into their own chat id
// separate from the per-week trends chat - a trends run dispatches twice.
func (e *extension) dispatchSeasonNotes(ctx context.Context, leagueID string) {
	localID := leagueID + ":season-notes"
	err := e.host.Dispatch(ctx, sdk.DispatchRequest{
		Chat: sdk.ChatRef{LocalID: localID, User: e.cfg.DefaultUser, Title: "Sleeper season notes"},
		Ask:  sdk.Ask{Message: fmt.Sprintf("Update the running season notes for league %s from this week's trends findings.", leagueID)},
		// Fixed shape: bound to skip the planner LLM call; quack's workflow catalog owns it.
		Run: sdk.RunConfig{ReadOnly: true, Workflow: "sleeper-season-notes"},
	})
	if err != nil && e.host.Log != nil {
		e.host.Log.Error("sleeper: season-notes dispatch failed", "league_id", leagueID, "err", err)
	}
}

// handleJobs serves POST /sleeper/api/jobs {league_id, stop, job, args} -
// dispatches (or appends a turn to) the chat convention handleArtifacts
// reads back from, and returns its chat link.
func (e *extension) handleJobs(w http.ResponseWriter, r *http.Request) {
	var req jobRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad JSON body")
		return
	}
	req.LeagueID = firstNonEmpty(req.LeagueID, e.cfg.DefaultLeague)
	if req.LeagueID == "" || req.Stop == "" || req.Job == "" {
		writeErr(w, http.StatusBadRequest, "league_id, stop, and job are required")
		return
	}
	jobs, talksAllowed, err := jobsForStop(req.Stop)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if !jobIsValid(req.Job, jobs, talksAllowed) {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("job %q is not valid for stop %q", req.Job, req.Stop))
		return
	}
	localID, title, err := localIDFor(req)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if e.host.Dispatch == nil {
		writeErr(w, http.StatusServiceUnavailable, "dispatch is not available")
		return
	}
	ctx := r.Context()
	message := fmt.Sprintf("Run the %s job for league %s, stop %s.%s", req.Job, req.LeagueID, req.Stop, formatArgs(req.Args))
	err = e.host.Dispatch(ctx, sdk.DispatchRequest{
		Chat: sdk.ChatRef{LocalID: localID, User: e.cfg.DefaultUser, Title: title},
		Ask:  sdk.Ask{Message: message},
		Run:  sdk.RunConfig{ReadOnly: true, Workflow: jobWorkflows[req.Job]},
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if req.Job == "trends" {
		e.dispatchSeasonNotes(ctx, req.LeagueID)
	}
	chatID := "ext:sleeper:" + localID
	writeJSON(w, jobResponse{ChatID: chatID, ChatURL: "/chat/" + chatID})
}
