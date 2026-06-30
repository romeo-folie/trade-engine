package feedback

import "github.com/romeo-folie/trade-engine/internal/config"

// orbAutoThrottleMultiplier maps a window's realised expectancy to an ORB risk multiplier in
// [floor, cap]. avgPnlPct is the average per-trade PnL in PERCENT (the feedback avg_pnl_pct
// metric); a profitable instrument (positive expectancy) sizes up, a losing one sizes down.
// Below minTrades there is too little evidence to act, so it returns the neutral 1.0 — which
// also leaves sizing unchanged. Pure; all parameters default to sane values when unset.
func orbAutoThrottleMultiplier(avgPnlPct float64, tradeCount int, cfg config.ORBAutoThrottleConfig) float64 {
	if tradeCount < cfg.MinTradesOrDefault() {
		return 1.0
	}
	// Default sensitivity only when explicitly invalid (negative). sensitivity:0 is a valid
	// "flat — no performance scaling" choice (always yields a neutral 1.0) and must be honoured.
	sens := cfg.Sensitivity
	if sens < 0 {
		sens = 1.0
	}
	floor := cfg.Floor
	if floor <= 0 {
		floor = 0.5
	}
	cap := cfg.Cap
	if cap <= 0 {
		cap = 1.5
	}

	m := 1.0 + sens*avgPnlPct
	if m < floor {
		m = floor
	}
	if m > cap {
		m = cap
	}
	return m
}
