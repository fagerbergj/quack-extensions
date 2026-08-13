package usage

// sparseMaxNonZero is the line-vs-columns threshold: below this many
// NON-ZERO points, an additive chart draws one column per step that had
// traffic instead of a polyline through the zeros between them. At a few
// runs a day the line's slopes imply traffic that never happened.
//
// This file MIRRORS assets/app.js's SPARSE_MAX_NONZERO / countNonZeroPoints
// / shouldRenderBars - same threshold, same comparisons. Nothing on the
// server calls it (the decision is the page's, per chart, per refresh);
// it exists so the rule is executable and table-tested somewhere, the same
// arrangement stepForSpan has with the page's step heuristic. The string
// pins in assets_test.go tie the JS copy to this one.
const sparseMaxNonZero = 8

func countNonZeroPoints(values []float64) int {
	n := 0
	for _, v := range values {
		if v > 0 {
			n++
		}
	}
	return n
}

// shouldRenderBars decides columns-vs-line for a WHOLE chart - never a mix,
// since two mark types in one plot read as two different measurements.
// Non-additive charts (latency percentiles, a cache-rate percentage) are
// excluded: stacking those would be a lie. The busiest series decides, so
// one dense series keeps the whole chart on lines.
func shouldRenderBars(series [][]float64, additive bool) bool {
	if !additive {
		return false
	}
	most := 0
	for _, points := range series {
		if n := countNonZeroPoints(points); n > most {
			most = n
		}
	}
	return most > 0 && most < sparseMaxNonZero
}
