package classifier

import (
	"context"
	"math"

	"github.com/romeo-folie/trade-engine/internal/config"
)

// RuleBased is a zero-token classifier. It evaluates RSI delta, EMA gap delta,
// price move %, news novelty, and HOLD cooldown deterministically.
// Implements the Classifier interface (Layer 1, PRD §7.2).
type RuleBased struct {
	cfg config.ClassifierConfig
}

// NewRuleBased creates a Layer-1 classifier with the supplied config.
func NewRuleBased(cfg config.ClassifierConfig) *RuleBased {
	return &RuleBased{cfg: cfg}
}

// Classify returns DecisionAnalyze if any threshold is exceeded, otherwise
// DecisionSkip with the first matching reason code.
//
// Rule precedence (any one above threshold → ANALYZE):
//  1. price_move_threshold_pct
//  2. rsi_delta_threshold
//  3. ema_gap_delta_pct
//  4. news_novelty (hash changed)
//  5. hold_cooldown_min elapsed since HOLD
func (r *RuleBased) Classify(_ context.Context, s TickState) (Result, error) {
	deltas := map[string]float64{
		"rsi_delta":         math.Abs(s.RSIDelta),
		"ema_gap_delta_pct": math.Abs(s.EMAGapDelta),
		"price_move_pct":    math.Abs(s.PriceMovesPct),
	}

	// HOLD cooldown takes priority: if we're still in cooldown → SKIP.
	// Cooldown is sourced from config, not TickState, to avoid silent misconfiguration.
	holdCooldown := float64(r.cfg.HoldCooldownMin)
	if s.LastDecision == "HOLD" && s.ElapsedMin < holdCooldown {
		deltas["hold_elapsed_min"] = s.ElapsedMin
		deltas["hold_cooldown_min"] = holdCooldown
		return Result{
			Decision:   DecisionSkip,
			ReasonCode: "HOLD_COOLDOWN",
			Deltas:     deltas,
		}, nil
	}

	// Any threshold breach → ANALYZE.
	if deltas["price_move_pct"] >= r.cfg.PriceMoveThresholdPct {
		return Result{Decision: DecisionAnalyze, ReasonCode: "PRICE_MOVE", Deltas: deltas}, nil
	}
	if deltas["rsi_delta"] >= r.cfg.RSIDeltaThreshold {
		return Result{Decision: DecisionAnalyze, ReasonCode: "RSI_DELTA", Deltas: deltas}, nil
	}
	if deltas["ema_gap_delta_pct"] >= r.cfg.EMAGapDeltaPct {
		return Result{Decision: DecisionAnalyze, ReasonCode: "EMA_GAP_DELTA", Deltas: deltas}, nil
	}
	if r.cfg.NewsNoveltyEnabled && s.NewsHash != s.LastNewsHash && s.NewsHash != "" {
		return Result{Decision: DecisionAnalyze, ReasonCode: "NEWS_NOVELTY", Deltas: deltas}, nil
	}

	return Result{Decision: DecisionSkip, ReasonCode: "NO_MATERIAL_CHANGE", Deltas: deltas}, nil
}
