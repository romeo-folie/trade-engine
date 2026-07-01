package sweep

import (
	"math"

	"github.com/romeo-folie/trade-engine/internal/backtest/orbreplay"
)

// MinTradesFloor is the sample-size floor below which a parameter set is not trustworthy: a
// strategy with a handful of trades can post a great expectancy by luck. Candidates under the
// floor are demoted beneath every above-floor candidate — visible, and ordered among
// themselves, but they can never top the run.
const MinTradesFloor = 100

// subFloorDemotion is subtracted from any below-floor candidate's score. It is far larger than
// the realistic spread of above-floor scores (expectancy ± nudges, order 1), so the trade floor
// is a HARD gate: no thin high-expectancy fluke can outrank an above-floor set, matching the
// plan's "≥100 trades" acceptance requirement.
const subFloorDemotion = 1000.0

// RobustnessScore ranks a parameter set by how DEPENDABLE its edge looks — deliberately not net
// profit, which rewards over-trading and lucky months. Primary term is expectancy per trade
// (mean R); profit factor and tail loss are secondary nudges; a set below the trade floor is
// hard-demoted beneath every above-floor set. Higher is better. This is the v1 heuristic — the
// refine/validate stages re-rank survivors on out-of-sample splits and cross-period consistency,
// which single-period coarse scoring cannot see.
func RobustnessScore(m orbreplay.Metrics, minTrades int) float64 {
	if m.Trades == 0 {
		return math.Inf(-1)
	}
	score := m.ExpectancyR // mean gross R per trade — the core edge signal

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

	// Hard trade-count floor. Below it, demote beneath every above-floor candidate while keeping
	// sub-floor sets ordered among themselves (fewer trades → deeper demotion, so within the band
	// more trades and better metrics still rank higher).
	if m.Trades < minTrades {
		deficit := float64(minTrades-m.Trades) / float64(minTrades) // (0, 1]
		score -= subFloorDemotion + deficit
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
