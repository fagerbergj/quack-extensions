package usage

// cacheRateFor mirrors assets/app.js's cacheRateFor. ok is false when
// cached+input is 0 - the zero-traffic exclusion, so an idle model/agent
// never renders as a fake "0%" row.
func cacheRateFor(cached, input float64) (rate float64, ok bool) {
	volume := cached + input
	if volume <= 0 {
		return 0, false
	}
	return (cached / volume) * 100, true
}
