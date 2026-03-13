// Package depth implements the adaptive ensemble depth scorer described in
// the cost-reduction PRD §7.3.
//
// The scorer weights four signals into a [0,1] impact score that determines
// how many analysts run for a given tick:
//
//	Depth 1 (score < low_threshold):  cheapest analyst only, no arbiter.
//	Depth 2 (low ≤ score < high):     two analysts, arbiter on disagreement.
//	Depth 3 (score ≥ high_threshold): all analysts, standard arbiter rules.
package depth

import (
	"math"

	"github.com/romeo-folie/trade-engine/internal/config"
)

// TickContext carries the per-ticker signals needed for impact scoring.
type TickContext struct {
	// ATRPct is the ATR as a percentage of price (volatility signal).
	ATRPct float64

	// NewsChanged is true when the news digest hash differs from the prior tick.
	NewsChanged bool

	// RiskProximityPct is how close the current position is to a stop-loss or
	// max-drawdown limit, expressed as 0 (far) → 1 (at limit).
	RiskProximityPct float64

	// ADX is the raw Average Directional Index value (0–100). Normalised to
	// [0,1] before weighting: normalised = min(ADX, 50) / 50.
	ADX float64
}

// ImpactScore holds the scoring result.
type ImpactScore struct {
	// Score is the weighted composite in [0, 1].
	Score float64

	// Depth is the analyst tier: 1, 2, or 3.
	Depth int

	// Inputs records the per-signal normalised contributions (for logging).
	Inputs map[string]float64
}

// maxATRPct is the ATR% value that maps to a normalised volatility of 1.0.
// Typical daily ATR for equities is 1–3 %; 5 % is elevated; 10 % is extreme.
const maxATRPct = 5.0

// Score computes the impact score for a tick and returns the recommended depth.
func Score(tc TickContext, cfg config.AdaptiveDepthConfig) ImpactScore {
	w := cfg.Weights

	// Normalise each signal to [0, 1].
	volatility := math.Min(tc.ATRPct/maxATRPct, 1.0)
	newsNovelty := 0.0
	if tc.NewsChanged {
		newsNovelty = 1.0
	}
	riskProximity := math.Min(math.Max(tc.RiskProximityPct, 0), 1.0)
	trendStrength := math.Min(tc.ADX/50.0, 1.0)

	score := w.Volatility*volatility +
		w.NewsNovelty*newsNovelty +
		w.RiskProximity*riskProximity +
		w.TrendStrength*trendStrength

	inputs := map[string]float64{
		"volatility":     volatility,
		"news_novelty":   newsNovelty,
		"risk_proximity": riskProximity,
		"trend_strength": trendStrength,
	}

	depth := depthFromScore(score, cfg.LowThreshold, cfg.HighThreshold)
	return ImpactScore{Score: score, Depth: depth, Inputs: inputs}
}

func depthFromScore(score, low, high float64) int {
	if score < low {
		return 1
	}
	if score < high {
		return 2
	}
	return 3
}
