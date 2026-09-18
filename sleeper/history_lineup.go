// bestLineupPoints fills strict-position slots with top scorers, then
// solves the leftover flex slots as a max-weight bipartite assignment.
package sleeper

import (
	"sort"

	"github.com/fagerbergj/quack-extensions/sleeper/sleepergen"
)

// flexKindOrder: every FLEX kind slotCounts recognizes.
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
	// Seed every candidate as flex-eligible leftover first - a position
	// with no strict slot (e.g. TE in a QB/RB/WR/FLEX league) must still
	// reach the flex solver, not just whatever counts happens to name.
	leftover := map[string][]string{}
	for pos, ids := range byPos {
		leftover[pos] = sortedByPoints(ids, points)
	}
	var total float32
	for pos, n := range counts {
		group := leftover[pos]
		if n > len(group) {
			n = len(group)
		}
		for _, id := range group[:n] {
			total += points[id]
		}
		leftover[pos] = group[n:]
	}
	return total + fillFlexSlots(flexCounts, leftover, points)
}

// flexCandidate is one leftover player eligible for at least one flex slot.
type flexCandidate struct {
	id  string
	pos string
}

// fillFlexSlots solves every flex slot as one joint assignment, not
// kind-by-kind, so a player eligible for two crossing kinds isn't wasted.
func fillFlexSlots(flexCounts map[string]int, leftover map[string][]string, points map[string]float32) float32 {
	var slotKinds []string
	for _, kind := range flexKindOrder {
		for i := 0; i < flexCounts[kind]; i++ {
			slotKinds = append(slotKinds, kind)
		}
	}
	if len(slotKinds) == 0 {
		return 0
	}
	posSet := map[string]bool{}
	for _, kind := range slotKinds {
		for _, pos := range flexKindEligible[kind] {
			posSet[pos] = true
		}
	}
	var candidates []flexCandidate
	for pos := range posSet {
		for _, id := range leftover[pos] {
			candidates = append(candidates, flexCandidate{id: id, pos: pos})
		}
	}
	return maxWeightAssignment(slotKinds, candidates, points)
}

// maxWeightAssignment bitmask-DPs the best assignment of distinct
// candidates to slotKinds (a slot may go unfilled); exact, not greedy.
func maxWeightAssignment(slotKinds []string, candidates []flexCandidate, points map[string]float32) float32 {
	memo := map[uint64]float32{}
	var solve func(slotIdx int, used uint32) float32
	solve = func(slotIdx int, used uint32) float32 {
		if slotIdx == len(slotKinds) {
			return 0
		}
		key := uint64(slotIdx)<<32 | uint64(used)
		if v, ok := memo[key]; ok {
			return v
		}
		best := solve(slotIdx+1, used) // leave this slot unfilled
		for i, c := range candidates {
			bit := uint32(1) << uint(i)
			if used&bit != 0 || !flexEligible(slotKinds[slotIdx], c.pos) {
				continue
			}
			if v := points[c.id] + solve(slotIdx+1, used|bit); v > best {
				best = v
			}
		}
		memo[key] = best
		return best
	}
	return solve(0, 0)
}

func flexEligible(kind, pos string) bool {
	for _, p := range flexKindEligible[kind] {
		if p == pos {
			return true
		}
	}
	return false
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
