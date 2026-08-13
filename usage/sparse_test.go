package usage

import "testing"

// points builds a length-n series whose first nonZero samples are positive
// and the rest zero - the shape a per-step increase() query returns when a
// handful of runs land in an otherwise idle window.
func points(n, nonZero int) []float64 {
	out := make([]float64, n)
	for i := 0; i < nonZero && i < n; i++ {
		out[i] = 1500
	}
	return out
}

func TestShouldRenderBars(t *testing.T) {
	cases := []struct {
		name     string
		series   [][]float64
		additive bool
		want     bool
	}{
		{"sparse additive burst", [][]float64{points(96, 3)}, true, true},
		{"one below the threshold", [][]float64{points(96, 7)}, true, true},
		{"exactly at the threshold stays a line", [][]float64{points(96, 8)}, true, false},
		{"dense additive stays a line", [][]float64{points(96, 20)}, true, false},
		{"sparse but not additive stays a line", [][]float64{points(96, 3)}, false, false},
		{"the busiest series decides", [][]float64{points(96, 3), points(96, 40)}, true, false},
		{"every series sparse", [][]float64{points(96, 2), points(96, 5)}, true, true},
		{"all zero is not sparse, it's empty", [][]float64{points(96, 0)}, true, false},
		{"no series at all", nil, true, false},
		{"a single point still counts", [][]float64{points(1, 1)}, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldRenderBars(tc.series, tc.additive); got != tc.want {
				t.Errorf("shouldRenderBars(%v, additive=%v) = %v, want %v", tc.series, tc.additive, got, tc.want)
			}
		})
	}
}

// A zero sample is an absent burst, not a small one - the whole point of
// the columns switch is that those steps draw nothing.
func TestCountNonZeroPoints(t *testing.T) {
	cases := []struct {
		name   string
		values []float64
		want   int
	}{
		{"empty", nil, 0},
		{"all zero", []float64{0, 0, 0}, 0},
		{"mixed", []float64{0, 3, 0, 0.0001, 0}, 2},
		{"a negative is not traffic", []float64{-5, 0, 2}, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := countNonZeroPoints(tc.values); got != tc.want {
				t.Errorf("countNonZeroPoints(%v) = %d, want %d", tc.values, got, tc.want)
			}
		})
	}
}
