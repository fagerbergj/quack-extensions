package usage

import "testing"

func TestStepForSpan(t *testing.T) {
	cases := []struct {
		name string
		span int64
		want int64
	}{
		{"1h", 3600, 30},
		{"24h", 86400, 900},
		{"7d", 604800, 7200},
		{"30d", 2592000, 21600},
		{"zero span floors to 15s", 0, 15},
		{"tiny span floors to 15s", 100, 15},
		{"exact nice-step boundary stays put", 900 * 150, 900},
		{"span past the largest nice step falls back to raw", 200_000_000, 200_000_000 / 150},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := stepForSpan(tc.span); got != tc.want {
				t.Errorf("stepForSpan(%d) = %d, want %d", tc.span, got, tc.want)
			}
		})
	}
}

// TestStepForSpanPointCountStaysReasonable pins the "~100-200 points"
// design intent: rounding to nice units can push a span outside that band,
// but never wildly so.
func TestStepForSpanPointCountStaysReasonable(t *testing.T) {
	for _, span := range []int64{3600, 86400, 604800, 2592000} {
		step := stepForSpan(span)
		points := span / step
		if points < 50 || points > 300 {
			t.Errorf("span %ds -> step %ds -> %d points, want roughly 100-200", span, step, points)
		}
	}
}

func TestStepForSpanNeverBelowFloor(t *testing.T) {
	for _, span := range []int64{-100, 0, 1, 14, 15, 16} {
		if got := stepForSpan(span); got < 15 {
			t.Errorf("stepForSpan(%d) = %d, want >= 15", span, got)
		}
	}
}
