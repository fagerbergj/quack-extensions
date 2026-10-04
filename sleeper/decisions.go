package sleeper

import (
	"context"
	"encoding/json"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/fagerbergj/quack-extensions/sdk"
	"github.com/fagerbergj/quack-extensions/sleeper/sleepergen"
)

// decideTimeout bounds each Decide call and the Sleeper reads the state needs.
// Above the host's 20 s handler timeout so the host decides; Clef serves one request at a time, so asks queue.
var decideTimeout = 25 * time.Second

// recentWeeks is how many completed weeks of points each player row carries.
const recentWeeks = 3

// priorityLevels is the waiver_priority score scale, low to high; index 4 is the first claim.
var priorityLevels = []string{"fifth claim or later", "fourth claim", "third claim", "second claim", "first claim"}

var decisionPoints = []sdk.DecisionPoint{
	{Name: "lineup_change", Description: "whether one start/sit swap the lineup analyst recommends should be made",
		Questions: map[string]sdk.DecisionQuestion{"swap": {Type: "noul",
			Instructions: "Should the proposed player start in this lineup slot instead of the current one, to score more fantasy points this week?"}},
		Primary: "swap", Modes: []string{"observe"}},
	{Name: "waiver_pickup", Description: "whether adding one free agent is worth its drop and waiver priority or FAAB",
		Questions: map[string]sdk.DecisionQuestion{"pickup": {Type: "noul",
			Instructions: "Is adding this free agent worth the roster spot it costs (the named drop, if any) and the waiver priority or FAAB the claim uses?"}},
		Primary: "pickup", Modes: []string{"observe"}},
	{Name: "waiver_priority", Description: "where one recommended add ranks among this week's waiver claims",
		Questions: map[string]sdk.DecisionQuestion{"priority": {Type: "score",
			Instructions: "Where should this add rank among this week's waiver claims, given the other candidates?", Criteria: priorityLevels}},
		Primary: "priority", Modes: []string{"observe"}},
	{Name: "trade_accept", Description: "whether one trade offer should be made or accepted as written",
		Questions: map[string]sdk.DecisionQuestion{"accept": {Type: "noul",
			Instructions: "Should this trade happen exactly as written, judged from my side (I give the give list and get the get list)?"}},
		Primary: "accept", Modes: []string{"observe"}},
}

// DecisionPoints declares the weekly fantasy points, observe only: the host
// records the answers and they never change an artifact or a dispatch.
func (e *extension) DecisionPoints() []sdk.DecisionPoint { return decisionPoints }

// artPlayer is the player object every job artifact schema shares.
type artPlayer struct {
	ID   string   `json:"id"`
	Name string   `json:"name"`
	Pos  string   `json:"pos"`
	Team string   `json:"team"`
	Inj  string   `json:"inj"`
	Prac string   `json:"prac"`
	Proj *float64 `json:"proj"`
}

// playerRow is one player in a decision state, with this week's opponent and
// recent points joined when Sleeper has them.
type playerRow struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Pos      string    `json:"pos,omitempty"`
	Team     string    `json:"team,omitempty"`
	Opp      string    `json:"opp,omitempty"`
	Status   string    `json:"status,omitempty"`
	Practice string    `json:"practice,omitempty"`
	Proj     *float64  `json:"proj,omitempty"`
	Floor    *float64  `json:"floor,omitempty"`
	Ceiling  *float64  `json:"ceiling,omitempty"`
	Recent   []weekPts `json:"recent,omitempty"` // newest week first; a week with no stat line is left out
	Why      string    `json:"reasoning,omitempty"`
}

type weekPts struct {
	Week   int     `json:"week"`
	PtsPPR float32 `json:"pts_ppr"`
}

type lineupArtifact struct {
	Week     int `json:"week"`
	Starters []struct {
		Slot     string     `json:"slot"`
		Player   artPlayer  `json:"player"`
		Floor    *float64   `json:"floor"`
		Ceiling  *float64   `json:"ceiling"`
		Replaces *artPlayer `json:"replaces"`
		Why      string     `json:"why"`
	} `json:"starters"`
	Bench []struct {
		Player  artPlayer `json:"player"`
		Floor   *float64  `json:"floor"`
		Ceiling *float64  `json:"ceiling"`
		Why     string    `json:"why"`
	} `json:"bench"`
}

type waiverCandidate struct {
	Rank     int       `json:"rank"`
	Player   artPlayer `json:"player"`
	OwnedPct *float64  `json:"owned_pct"`
	Adds24h  *int      `json:"adds_24h"`
	Drop     string    `json:"drop"`
	Why      string    `json:"why"`
}

type waiversArtifact struct {
	Week        int               `json:"week"`
	WaiverType  string            `json:"waiver_type"`
	Candidates  []waiverCandidate `json:"candidates"`
	AlsoChecked []waiverCandidate `json:"also_checked"`
}

type tradeArtifact struct {
	Partner   string `json:"partner"`
	PartnerID string `json:"partner_id"`
	Offers    []struct {
		By      string      `json:"by"`
		Give    []artPlayer `json:"give"`
		Get     []artPlayer `json:"get"`
		Verdict string      `json:"verdict"`
		Delta   string      `json:"delta"`
		Why     string      `json:"why"`
	} `json:"offers"`
}

// decisionKeys are the ids a later scorer joins against actual points.
type decisionKeys struct {
	ChatID   string `json:"chat_id"`
	LeagueID string `json:"league_id"`
	Season   string `json:"season,omitempty"`
	Week     int    `json:"week,omitempty"`
}

type lineupState struct {
	decisionKeys
	Slot     string    `json:"slot"`
	Current  playerRow `json:"current"`
	Proposed playerRow `json:"proposed"`
}

type waiverState struct {
	decisionKeys
	WaiverType string      `json:"waiver_type,omitempty"`
	Add        playerRow   `json:"add"`
	OwnedPct   *float64    `json:"owned_pct,omitempty"`
	Adds24h    *int        `json:"adds_24h,omitempty"`
	Drop       string      `json:"drop,omitempty"`
	DropID     string      `json:"drop_id,omitempty"`
	Others     []playerRow `json:"other_candidates,omitempty"`
}

type tradeState struct {
	decisionKeys
	Partner    string      `json:"partner,omitempty"`
	PartnerID  string      `json:"partner_id"`
	OfferIndex int         `json:"offer_index"`
	By         string      `json:"offered_by,omitempty"`
	Give       []playerRow `json:"give"`
	Get        []playerRow `json:"get"`
	Delta      string      `json:"delta,omitempty"`
	Why        string      `json:"reasoning,omitempty"`
}

// observeRun asks a finished lineup, waivers or trade run's shadow questions
// off the run path, so a slow or failing handler never touches delivery.
func (e *extension) observeRun(chatID string, outcome sdk.RunOutcome) {
	if outcome.Status != sdk.RunDone || e.host.Decide == nil || e.host.ReadArtifact == nil {
		return
	}
	go e.decideRun(context.Background(), chatID)
}

// decideRun runs the calls one at a time: a burst per player can exhaust the handler's GPU batch.
func (e *extension) decideRun(ctx context.Context, chatID string) {
	keys, job, ok := parseJobChat(chatID)
	if !ok {
		return
	}
	data, found := e.host.ReadArtifact(chatID, e.cfg.DefaultUser, job)
	if !found {
		return
	}
	raw, ok := parseArtifactJSON(data)
	if !ok {
		return
	}
	for _, req := range e.decisionRequests(ctx, keys, job, raw) {
		req.ChatID = chatID
		e.ask(ctx, req)
	}
}

// parseJobChat reads ext:sleeper:<league>:<week>:<lineup|waivers> and
// ext:sleeper:<league>:trade:<partner>; any other chat asks nothing.
func parseJobChat(chatID string) (decisionKeys, string, bool) {
	parts := strings.Split(strings.TrimPrefix(chatID, "ext:sleeper:"), ":")
	if len(parts) != 3 || !strings.HasPrefix(chatID, "ext:sleeper:") {
		return decisionKeys{}, "", false
	}
	keys := decisionKeys{ChatID: chatID, LeagueID: parts[0]}
	if parts[1] == "trade" {
		return keys, "trade", true
	}
	week, err := strconv.Atoi(parts[1])
	if err != nil || (parts[2] != "lineup" && parts[2] != "waivers") {
		return decisionKeys{}, "", false
	}
	keys.Week = week
	return keys, parts[2], true
}

func (e *extension) decisionRequests(ctx context.Context, keys decisionKeys, job string, raw json.RawMessage) []sdk.DecideRequest {
	switch job {
	case "lineup":
		var a lineupArtifact
		if json.Unmarshal(raw, &a) != nil {
			return nil
		}
		return lineupRequests(e.joiner(ctx, &keys, a.Week), keys, a)
	case "waivers":
		var a waiversArtifact
		if json.Unmarshal(raw, &a) != nil {
			return nil
		}
		return waiverRequests(e.joiner(ctx, &keys, a.Week), keys, a, e.resolveDrop)
	default:
		var a tradeArtifact
		if json.Unmarshal(raw, &a) != nil {
			return nil
		}
		return tradeRequests(e.joiner(ctx, &keys, 0), keys, a)
	}
}

// lineupRequests asks one lineup_change per starter row that replaces the
// current starter; the row's verdict and which side is the starter stay out.
func lineupRequests(join rowJoiner, keys decisionKeys, a lineupArtifact) []sdk.DecideRequest {
	bench := map[string]int{}
	for i, b := range a.Bench {
		bench[b.Player.ID] = i
	}
	var out []sdk.DecideRequest
	for _, s := range a.Starters {
		if s.Replaces == nil || s.Replaces.ID == "" {
			continue
		}
		cur := join(*s.Replaces)
		if i, ok := bench[s.Replaces.ID]; ok {
			cur.Floor, cur.Ceiling, cur.Why = a.Bench[i].Floor, a.Bench[i].Ceiling, a.Bench[i].Why
		}
		prop := join(s.Player)
		prop.Floor, prop.Ceiling, prop.Why = s.Floor, s.Ceiling, s.Why
		out = append(out, sdk.DecideRequest{Point: "lineup_change", Baseline: "true",
			State: lineupState{decisionKeys: keys, Slot: s.Slot, Current: cur, Proposed: prop}})
	}
	return out
}

// waiverRequests asks waiver_pickup for every recommended add (true) and
// every add the scout checked and passed on (false), and waiver_priority per
// ranked add once two or more are ranked; ranks never reach the state.
func waiverRequests(join rowJoiner, keys decisionKeys, a waiversArtifact, resolveDrop func(string) string) []sdk.DecideRequest {
	var out []sdk.DecideRequest
	var ranked []waiverCandidate
	for _, c := range a.Candidates {
		out = append(out, sdk.DecideRequest{Point: "waiver_pickup", Baseline: "true", State: waiverStateFor(join, keys, a, c, resolveDrop)})
		if c.Rank > 0 {
			ranked = append(ranked, c)
		}
	}
	for _, c := range a.AlsoChecked {
		out = append(out, sdk.DecideRequest{Point: "waiver_pickup", Baseline: "false", State: waiverStateFor(join, keys, a, c, resolveDrop)})
	}
	if len(ranked) < 2 {
		return out
	}
	sort.Slice(ranked, func(i, j int) bool { return ranked[i].Player.ID < ranked[j].Player.ID })
	for i, c := range ranked {
		st := waiverStateFor(join, keys, a, c, resolveDrop)
		for j, o := range ranked {
			if j != i {
				st.Others = append(st.Others, join(o.Player))
			}
		}
		level := len(priorityLevels) - min(c.Rank, len(priorityLevels))
		out = append(out, sdk.DecideRequest{Point: "waiver_priority", Baseline: strconv.Itoa(level), State: st})
	}
	return out
}

func waiverStateFor(join rowJoiner, keys decisionKeys, a waiversArtifact, c waiverCandidate, resolveDrop func(string) string) waiverState {
	add := join(c.Player)
	add.Why = c.Why
	st := waiverState{decisionKeys: keys, WaiverType: a.WaiverType, Add: add, OwnedPct: c.OwnedPct, Adds24h: c.Adds24h, Drop: c.Drop}
	if c.Drop != "" {
		st.DropID = resolveDrop(c.Drop)
	}
	return st
}

// tradeRequests asks trade_accept per offer; only "send" means the trade
// should happen as written, so decline and counter are both false.
func tradeRequests(join rowJoiner, keys decisionKeys, a tradeArtifact) []sdk.DecideRequest {
	var out []sdk.DecideRequest
	for i, o := range a.Offers {
		if o.Verdict != "send" && o.Verdict != "decline" && o.Verdict != "counter" {
			continue
		}
		st := tradeState{decisionKeys: keys, Partner: a.Partner, PartnerID: a.PartnerID, OfferIndex: i, By: o.By, Delta: o.Delta, Why: o.Why}
		for _, p := range o.Give {
			st.Give = append(st.Give, join(p))
		}
		for _, p := range o.Get {
			st.Get = append(st.Get, join(p))
		}
		out = append(out, sdk.DecideRequest{Point: "trade_accept", Baseline: strconv.FormatBool(o.Verdict == "send"), State: st})
	}
	return out
}

// resolveDrop maps the scout's drop text ("Rico Dowdle (Q)") to a player id
// only when the name index has exactly one match; else the scorer joins on the name.
func (e *extension) resolveDrop(drop string) string {
	name, _, _ := strings.Cut(drop, " (")
	if ids := e.client.ResolvePlayer(strings.TrimSpace(name)); len(ids) == 1 {
		return ids[0]
	}
	return ""
}

type rowJoiner func(artPlayer) playerRow

// joiner fills keys.Season (and keys.Week when 0: the artifact's week, else the live week) and returns
// a row builder joining opponent and recent points best-effort. A chat's stop wins over the artifact's
// week, which an agent run before Sleeper's week flips writes as the live week.
func (e *extension) joiner(ctx context.Context, keys *decisionKeys, week int) rowJoiner {
	ctx, cancel := context.WithTimeout(ctx, decideTimeout)
	defer cancel()
	base := func(p artPlayer) playerRow {
		return playerRow{ID: p.ID, Name: p.Name, Pos: p.Pos, Team: p.Team, Status: p.Inj, Practice: p.Prac, Proj: p.Proj}
	}
	season, state, err := e.season(ctx)
	if err != nil {
		return base
	}
	keys.Season = season
	keys.Week = firstNonZero(keys.Week, week, state.Week)
	var sl slate
	if state.SeasonType == "regular" && season == state.Season && keys.Week == state.Week {
		sl, _ = e.weekSlate(ctx, season, keys.Week, e.newClock(""))
	}
	recent := map[int]map[string]sleepergen.StatMap{}
	for w := keys.Week - 1; w >= 1 && w >= keys.Week-recentWeeks; w-- {
		if stats, err := e.client.WeekStats(ctx, season, w); err == nil {
			recent[w] = stats
		}
	}
	return func(p artPlayer) playerRow {
		r := base(p)
		pos := p.Pos
		dump := map[string]sleepergen.Player{p.ID: {Team: &p.Team, Position: &pos}}
		if g := sl.gameFor(dump, p.ID).Game; g != nil {
			r.Opp = g.NFLOpponent
		}
		for w := keys.Week - 1; w >= keys.Week-recentWeeks; w-- {
			if s, ok := recent[w][p.ID]; ok {
				r.Recent = append(r.Recent, weekPts{Week: w, PtsPPR: s["pts_ppr"]})
			}
		}
		return r
	}
}

func firstNonZero(vs ...int) int {
	for _, v := range vs {
		if v != 0 {
			return v
		}
	}
	return 0
}

func (e *extension) ask(ctx context.Context, req sdk.DecideRequest) {
	ctx, cancel := context.WithTimeout(ctx, decideTimeout)
	defer cancel()
	if _, err := e.host.Decide(ctx, req); err != nil {
		slog.Debug("sleeper: no decision", "component", "sleeper", "point", req.Point, "err", err)
	}
}
