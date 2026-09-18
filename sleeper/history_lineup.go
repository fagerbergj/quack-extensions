// bestLineupPoints: fill strict-position slots with top scorers, then FLEX
// with the best remaining RB/WR/TE - greedy is optimal with one shared slot.
package sleeper

import (
	"sort"

	"github.com/fagerbergj/quack-extensions/sleeper/sleepergen"
)

// flexPositions: what a FLEX slot may hold, per the UI design doc.
var flexPositions = map[string]bool{"RB": true, "WR": true, "TE": true}

func bestLineupPoints(rosterPositions []string, playerIDs []string, points map[string]float32, dump map[string]sleepergen.Player) float32 {
	counts, flexSlots := slotCounts(rosterPositions)
	byPos := groupByPosition(playerIDs, dump)
	var total float32
	usedFlexPool := map[string]bool{}
	for pos, n := range counts {
		if flexPositions[pos] {
			continue // filled after the flex-eligible minimums below
		}
		total += takeTop(byPos[pos], points, n)
	}
	var flexPool []string
	for pos := range flexPositions {
		group := byPos[pos]
		sort.SliceStable(group, func(i, j int) bool { return points[group[i]] > points[group[j]] })
		n := counts[pos]
		if n > len(group) {
			n = len(group)
		}
		for _, id := range group[:n] {
			total += points[id]
			usedFlexPool[id] = true
		}
		flexPool = append(flexPool, group[n:]...)
	}
	sort.SliceStable(flexPool, func(i, j int) bool { return points[flexPool[i]] > points[flexPool[j]] })
	if flexSlots > len(flexPool) {
		flexSlots = len(flexPool)
	}
	for _, id := range flexPool[:flexSlots] {
		total += points[id]
	}
	return total
}

// slotCounts tallies roster_positions into per-position minimums plus a
// separate FLEX count, ignoring bench/reserve/taxi entries.
func slotCounts(rosterPositions []string) (counts map[string]int, flex int) {
	counts = map[string]int{}
	for _, p := range rosterPositions {
		switch p {
		case "BN", "IR", "TAXI":
		case "FLEX":
			flex++
		default:
			counts[p]++
		}
	}
	return counts, flex
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

func takeTop(ids []string, points map[string]float32, n int) float32 {
	sort.SliceStable(ids, func(i, j int) bool { return points[ids[i]] > points[ids[j]] })
	if n > len(ids) {
		n = len(ids)
	}
	var total float32
	for _, id := range ids[:n] {
		total += points[id]
	}
	return total
}
