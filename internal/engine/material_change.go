package engine

import (
	"crypto/sha256"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/romeo-folie/trade-engine/internal/config"
)

// SkipReason describes why analysis was skipped.
type SkipReason string

const (
	SkipReasonNone         SkipReason = ""
	SkipReasonHoldCooldown SkipReason = "HOLD_COOLDOWN"
	SkipReasonNoChange     SkipReason = "NO_MATERIAL_CHANGE"
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

// NewsDigestHash returns a SHA-256 hex hash of the sorted, NUL-delimited
// headlines. Sorting makes the hash order-independent so identical headline
// sets always produce the same digest regardless of DB query row order.
func NewsDigestHash(headlines []string) string {
	sorted := make([]string, len(headlines))
	copy(sorted, headlines)
	sort.Strings(sorted)

	h := sha256.New()
	for _, hl := range sorted {
		_, _ = h.Write([]byte(hl))
		_, _ = h.Write([]byte{0}) // NUL delimiter prevents ["ab","c"] == ["a","bc"]
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

// applyScannerOverrides returns a copy of the AnalysisConfig with
// scanner-only material-change relaxations applied. Manual or
// config-sourced tickers (source != "scanner") are returned unchanged.
//
// Two relaxations:
//
//  1. HOLD cooldown ÷ 4 (PR #309): scanner-added tickers shouldn't sit on a
//     2-hour cooldown after one HOLD when they're meant to be actively
//     watched. Floored at 10 minutes so we don't thrash the LLM on noise.
//  2. RSI-delta threshold loosened to 2.0 (#317), but only when the
//     scanner's conviction in the ticker is ≥ 0.75. High-conviction
//     scanner finds re-trigger LLM analysis on smaller indicator moves
//     than the default 3.0 floor would allow.
func applyScannerOverrides(cfg config.AnalysisConfig, source string, scannerConviction float64) config.AnalysisConfig {
	if source != "scanner" {
		return cfg
	}

	if cfg.HoldCooldownMin > 0 {
		reduced := cfg.HoldCooldownMin / 4
		if reduced < 10 {
			reduced = 10
		}
		if reduced > cfg.HoldCooldownMin {
			reduced = cfg.HoldCooldownMin
		}
		cfg.HoldCooldownMin = reduced
	}

	const scannerRSIDeltaFloor = 2.0
	const scannerConvictionFloor = 0.75
	if scannerConviction >= scannerConvictionFloor && cfg.RSIDeltaThreshold > scannerRSIDeltaFloor {
		cfg.RSIDeltaThreshold = scannerRSIDeltaFloor
	}
	return cfg
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
