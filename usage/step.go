package usage

// niceSteps are the step widths query_range is ever asked for, in seconds.
// Rounding up to one of these (rather than using the raw span/target-points
// value) keeps step boundaries human-legible ("15m", "1h") instead of
// arbitrary numbers like 577s - the JS side mirrors this same table for
// custom ranges the server never sees (see assets/app.js's stepForSpan).
var niceSteps = []int64{
	15, 30, 60, 300, 900, 1800, 3600, 7200, 21600, 43200, 86400, 172800, 604800,
}

// stepForSpan picks a query_range step for a span of spanSeconds, aiming for
// roughly 100-200 points: step = max(15s, span/150), rounded up to the
// nearest niceSteps entry. A span longer than the largest nice step falls
// back to the raw (unrounded) value rather than under-sampling forever.
func stepForSpan(spanSeconds int64) int64 {
	raw := spanSeconds / 150
	if raw < 15 {
		raw = 15
	}
	for _, s := range niceSteps {
		if s >= raw {
			return s
		}
	}
	return raw
}
