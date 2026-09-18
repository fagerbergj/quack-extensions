// bestLineupPoints: fill strict-position slots with top scorers, then each
// FLEX kind narrowest-to-widest from the best remaining eligible players.
package sleeper

import (
	"sort"

	"github.com/fagerbergj/quack-extensions/sleeper/sleepergen"
)

// flexKindOrder: narrowest eligibility first - each kind's set nests in
// the next, so an exchange argument makes narrowest-first-widest-last optimal.
var flexKindOrder = []string{"WRRB_FLEX", "REC_FLEX", "FLEX", "SUPER_FLEX"}

var flexKindEligible = map[string][]string{
	"WRRB_FLEX":  {"RB", "WR"},
	"REC_FLEX":   {"WR", "TE"},
	"FLEX":       {"RB", "WR", "TE"},
	"SUPER_FLEX": {"QB", "RB", "WR", "TE"},
}

func bestLineupPoints(rosterPositions []string, playerIDs []string, points map[string]float32, dump map[string]sleepergen.Player) float32 {
	counts, flexCounts := slotCounts(rosterPositions)
	byPos := groupByPosition(playerIDs, dump)
	leftover := map[string][]string{}
	var total float32
	for pos, n := range counts {
		group := sortedByPoints(byPos[pos], points)
		if n > len(group) {
			n = len(group)
		}
		for _, id := range group[:n] {
			total += points[id]
		}
		leftover[pos] = group[n:]
	}
	for _, kind := range flexKindOrder {
		n := flexCounts[kind]
		if n == 0 {
			continue
		}
		total += fillFlexKind(kind, n, leftover, points)
	}
	return total
}

// fillFlexKind takes the top n players across kind's eligible positions'
// current leftovers, and removes them so a wider flex kind can't reuse them.
func fillFlexKind(kind string, n int, leftover map[string][]string, points map[string]float32) float32 {
	var pool []string
	for _, pos := range flexKindEligible[kind] {
		pool = append(pool, leftover[pos]...)
	}
	pool = sortedByPoints(pool, points)
	if n > len(pool) {
		n = len(pool)
	}
	taken := map[string]bool{}
	var total float32
	for _, id := range pool[:n] {
		total += points[id]
		taken[id] = true
	}
	for _, pos := range flexKindEligible[kind] {
		leftover[pos] = removeIDs(leftover[pos], taken)
	}
	return total
}

func removeIDs(ids []string, remove map[string]bool) []string {
	out := ids[:0]
	for _, id := range ids {
		if !remove[id] {
			out = append(out, id)
		}
	}
	return out
}

func sortedByPoints(ids []string, points map[string]float32) []string {
	out := append([]string(nil), ids...)
	sort.SliceStable(out, func(i, j int) bool { return points[out[i]] > points[out[j]] })
	return out
}

// slotCounts tallies roster_positions into per-position minimums plus a
// count per FLEX kind, ignoring bench/reserve/taxi entries.
func slotCounts(rosterPositions []string) (counts map[string]int, flexCounts map[string]int) {
	counts, flexCounts = map[string]int{}, map[string]int{}
	for _, p := range rosterPositions {
		switch {
		case p == "BN" || p == "IR" || p == "TAXI":
		case flexKindEligible[p] != nil:
			flexCounts[p]++
		default:
			counts[p]++
		}
	}
	return counts, flexCounts
}

func groupByPosition(playerIDs []string, dump map[string]sleepergen.Player) map[string][]string {
	out := map[string][]string{}
	for _, id := range playerIDs {
		pos := ""
		if p, ok := dump[id]; ok && p.Position != nil {
			pos = *p.Position
		}
		out[pos] = append(out[pos], id)
	}
	return out
}
