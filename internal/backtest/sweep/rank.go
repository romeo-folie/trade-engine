package sweep

import (
	"math"

	"github.com/romeo-folie/trade-engine/internal/backtest/orbreplay"
)

// MinTradesFloor is the sample-size floor below which a parameter set is not trustworthy: a
// strategy with a handful of trades can post a great expectancy by luck. Candidates under the
// floor are pushed down rather than dropped, so they remain visible but never win.
const MinTradesFloor = 100

// RobustnessScore ranks a parameter set by how DEPENDABLE its edge looks — deliberately not net
// profit, which rewards over-trading and lucky months. Primary term is expectancy per trade
// (mean R); a sample-size penalty dominates below the trade floor; profit factor and tail loss
// are secondary nudges. Higher is better. This is the v1 heuristic — the refine/validate stages
// re-rank survivors on out-of-sample splits and cross-period consistency, which single-period
// coarse scoring cannot see.
func RobustnessScore(m orbreplay.Metrics, minTrades int) float64 {
	if m.Trades == 0 {
		return math.Inf(-1)
	}
	score := m.ExpectancyR // mean gross R per trade — the core edge signal

	// Sample-size gate: below the floor, subtract up to 1.0 R-equivalent proportionally, so a
	// 5-trade fluke cannot outrank a 200-trade positive expectancy.
	if m.Trades < minTrades {
		score -= float64(minTrades-m.Trades) / float64(minTrades)
	}

	// Profit factor nudge: reward >1 (net winners exceed net losers) on a log scale so a huge PF
	// on thin data doesn't dominate. +Inf PF (no losers) is treated as a modest fixed bonus.
	switch {
	case math.IsInf(m.ProfitFactor, 1):
		score += 0.10
	case m.ProfitFactor > 0:
		score += 0.05 * math.Log(m.ProfitFactor)
	}

	// Tail penalty: a worst-trade loss beyond the planned 1R of risk indicates slippage/gaps
	// eating the stop — penalise proportionally to how far past 1R it ran.
	if m.MaxLossR < -1.0 {
		score += 0.05 * (m.MaxLossR + 1.0) // MaxLossR<-1 → negative term
	}
	return score
}

// PassesAcceptanceGates reports whether a result clears the hard promotion gates from the plan:
// profit factor > 1.15, expectancy > +0.05R, and at least the trade floor. Used to flag which
// coarse candidates are worth refining/validating (consistency + out-of-sample checks are applied
// by the validate stage, not here).
func PassesAcceptanceGates(m orbreplay.Metrics, minTrades int) bool {
	return m.Trades >= minTrades &&
		m.ExpectancyR > 0.05 &&
		(math.IsInf(m.ProfitFactor, 1) || m.ProfitFactor > 1.15)
}
