package engine

import (
	"crypto/sha256"
	"fmt"
	"math"
	"time"

	"github.com/romeo-folie/trade-engine/internal/config"
)

// SkipReason describes why analysis was skipped.
type SkipReason string

const (
	SkipReasonNone          SkipReason = ""
	SkipReasonHoldCooldown  SkipReason = "HOLD_COOLDOWN"
	SkipReasonNoChange      SkipReason = "NO_MATERIAL_CHANGE"
)

// TickerState holds the last-seen indicator snapshot for a single ticker.
// Stored in-memory in the engine. On restart, the first tick always analyses
// (no prior state → conservative default).
type TickerState struct {
	LastRSI        float64
	LastEMAGapPct  float64 // (price - EMA20) / EMA20 * 100
	LastPrice      float64
	LastNewsHash   string
	LastDecision   string // BUY | SELL | HOLD | ""
	LastAnalysedAt time.Time
}

// MaterialChangeInput carries current-tick values for the gate check.
type MaterialChangeInput struct {
	RSI      float64
	EMA20    float64
	Price    float64 // latest close
	NewsHash string  // SHA-256 digest of current news headlines
	Now      time.Time
}

// NewsDigestHash returns a SHA-256 hex hash of the concatenated news headlines.
// Headlines are separated by a NUL byte to avoid collision ambiguity.
func NewsDigestHash(headlines []string) string {
	h := sha256.New()
	for _, hl := range headlines {
		_, _ = h.Write([]byte(hl))
		_, _ = h.Write([]byte{0})
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

// emaGapPct computes (price - ema) / ema * 100.
// Returns 0 if ema is zero to avoid divide-by-zero.
func emaGapPct(price, ema float64) float64 {
	if ema == 0 {
		return 0
	}
	return (price - ema) / ema * 100
}

// CheckMaterialChange determines whether a full LLM analysis should run for
// this ticker on this tick. Returns (shouldSkip bool, reason SkipReason).
//
// Decision matrix:
//  1. If MaterialChangeOnly is disabled → never skip (always analyze).
//  2. If last decision was HOLD and HoldCooldownMin has not elapsed → HOLD_COOLDOWN skip.
//  3. If no prior state exists → analyze (conservative first-run default).
//  4. If all deltas are below their thresholds AND news hash unchanged → NO_MATERIAL_CHANGE skip.
//  5. Otherwise → analyze.
//
// Callers should log the skip reason and the delta values for observability.
func CheckMaterialChange(prev *TickerState, in MaterialChangeInput, cfg config.AnalysisConfig) (skip bool, reason SkipReason, deltas map[string]float64) {
	deltas = map[string]float64{}

	if !cfg.MaterialChangeOnly {
		return false, SkipReasonNone, deltas
	}

	// Rule 1: HOLD cooldown.
	if prev != nil && prev.LastDecision == "HOLD" && !prev.LastAnalysedAt.IsZero() {
		cooldown := time.Duration(cfg.HoldCooldownMin) * time.Minute
		elapsed := in.Now.Sub(prev.LastAnalysedAt)
		if elapsed < cooldown {
			deltas["hold_elapsed_min"] = elapsed.Minutes()
			deltas["hold_cooldown_min"] = float64(cfg.HoldCooldownMin)
			return true, SkipReasonHoldCooldown, deltas
		}
	}

	// Rule 2: No prior state → analyze.
	if prev == nil {
		return false, SkipReasonNone, deltas
	}

	currentGap := emaGapPct(in.Price, in.EMA20)

	rsiDelta := math.Abs(in.RSI - prev.LastRSI)
	emaGapDelta := math.Abs(currentGap - prev.LastEMAGapPct)
	priceDelta := 0.0
	if prev.LastPrice > 0 {
		priceDelta = math.Abs(in.Price-prev.LastPrice) / prev.LastPrice * 100
	}
	newsChanged := cfg.NewsNoveltyEnabled && in.NewsHash != prev.LastNewsHash

	deltas["rsi_delta"] = rsiDelta
	deltas["ema_gap_delta_pct"] = emaGapDelta
	deltas["price_move_pct"] = priceDelta

	// Rule 3: Any threshold exceeded → analyze.
	if rsiDelta >= cfg.RSIDeltaThreshold {
		return false, SkipReasonNone, deltas
	}
	if emaGapDelta >= cfg.EMAGapDeltaPct {
		return false, SkipReasonNone, deltas
	}
	if priceDelta >= cfg.PriceMoveThresholdPct {
		return false, SkipReasonNone, deltas
	}
	if newsChanged {
		return false, SkipReasonNone, deltas
	}

	// Rule 4: Nothing changed materially → skip.
	return true, SkipReasonNoChange, deltas
}
