package usage

import "testing"

func TestCacheRateFor(t *testing.T) {
	cases := []struct {
		name     string
		cached   float64
		input    float64
		wantRate float64
		wantOK   bool
	}{
		{"maintainer's example", 8_300_000, 2_300_000, 78.30188679245283, true},
		{"all cached", 10, 0, 100, true},
		{"all input", 0, 10, 0, true},
		{"zero traffic excluded", 0, 0, 0, false},
		{"negative volume treated as no traffic", -5, -5, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rate, ok := cacheRateFor(tc.cached, tc.input)
			if ok != tc.wantOK {
				t.Fatalf("cacheRateFor(%v, %v) ok = %v, want %v", tc.cached, tc.input, ok, tc.wantOK)
			}
			if ok && rate != tc.wantRate {
				t.Errorf("cacheRateFor(%v, %v) rate = %v, want %v", tc.cached, tc.input, rate, tc.wantRate)
			}
		})
	}
}
