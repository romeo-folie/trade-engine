// Package cache implements the analysis result cache described in the
// cost-reduction PRD §7.5.
//
// The fingerprint builder produces a canonical SHA-256 digest that identifies
// a unique (market state × model profile) combination. Bucketing ensures that
// small indicator fluctuations reuse the same cache entry rather than busting
// it on every tick.
package cache

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math"
	"sort"
)

// FingerprintInput carries all dimensions that contribute to the cache key.
type FingerprintInput struct {
	// Identity dimensions — exact match required.
	Ticker          string
	AssetClass      string // "stock" | "prediction"
	Timeframe       string
	StrategyName    string
	StrategyVersion string // bumped when prompt template changes
	ModelProfile    string // "budget" | "balanced" | "pro"

	// Analyst set — "provider/model" strings.
	// Build() sorts this slice internally, so order is irrelevant at the call site.
	// Equivalent analyst sets always produce the same fingerprint.
	Analysts []string
	// Arbiter model — "provider/model".
	Arbiter string

	// Bucketed indicator state — small noise must not bust the cache.
	RSI         float64 // bucketed in steps of 5
	EMAGapPct   float64 // (price-EMA20)/EMA20 * 100, bucketed in steps of 0.5
	MACDSign    int     // +1 | 0 | -1
	MACDBucket  int     // magnitude bucket (0–4)
	ADXBucket   int     // 0–3: weak/moderate/strong/extreme
	ATRPctBucket int    // 0–4

	// Price regime — return over lookback in one of 5 buckets.
	PriceRegimeBucket int // -2 | -1 | 0 | +1 | +2

	// News digest hash (already stable — computed by NewsDigestHash).
	NewsHash string

	// Risk context.
	OpenPositionsBucket int // 0–4
	ExposurePctBucket   int // 0–4
}

// Build constructs the canonical fingerprint for the given input.
// It applies bucketing before serialising so that small indicator noise
// does not produce different fingerprints.
func Build(in FingerprintInput) string {
	key := struct {
		Ticker          string   `json:"ticker"`
		AssetClass      string   `json:"asset_class"`
		Timeframe       string   `json:"timeframe"`
		StrategyName    string   `json:"strategy_name"`
		StrategyVersion string   `json:"strategy_version"`
		ModelProfile    string   `json:"model_profile"`
		Analysts        []string `json:"analysts"`
		Arbiter         string   `json:"arbiter"`
		RSIBucket       int      `json:"rsi_bucket"`
		EMAGapBucket    int      `json:"ema_gap_bucket"`
		MACDSign        int      `json:"macd_sign"`
		MACDBucket      int      `json:"macd_bucket"`
		ADXBucket       int      `json:"adx_bucket"`
		ATRPctBucket    int      `json:"atr_pct_bucket"`
		PriceRegime     int      `json:"price_regime_bucket"`
		NewsHash        string   `json:"news_hash"`
		OpenPosBucket   int      `json:"open_positions_bucket"`
		ExposureBucket  int      `json:"exposure_pct_bucket"`
	}{
		Ticker:          in.Ticker,
		AssetClass:      in.AssetClass,
		Timeframe:       in.Timeframe,
		StrategyName:    in.StrategyName,
		StrategyVersion: in.StrategyVersion,
		ModelProfile:    in.ModelProfile,
		Analysts:        sortedCopy(in.Analysts),
		Arbiter:         in.Arbiter,
		RSIBucket:       bucketRSI(in.RSI),
		EMAGapBucket:    bucketEMAGap(in.EMAGapPct),
		MACDSign:        in.MACDSign,
		MACDBucket:      in.MACDBucket,
		ADXBucket:       in.ADXBucket,
		ATRPctBucket:    in.ATRPctBucket,
		PriceRegime:     in.PriceRegimeBucket,
		NewsHash:        in.NewsHash,
		OpenPosBucket:   in.OpenPositionsBucket,
		ExposureBucket:  in.ExposurePctBucket,
	}

	// Canonical JSON — map keys are in struct field order (stable).
	data, _ := json.Marshal(key)
	sum := sha256.Sum256(data)
	return fmt.Sprintf("%x", sum)
}

// bucketRSI rounds RSI to the nearest multiple of 5 (bucket size = 5).
// RSI 0–100 → 21 distinct buckets.
func bucketRSI(rsi float64) int {
	return int(math.Round(rsi/5.0)) * 5
}

// bucketEMAGap rounds the EMA gap percentage to the nearest 0.5-step bucket index.
// Uses math.Round which rounds half away from zero (not banker's rounding). Examples:
//   - 1.1%  → Round(2.2) = 2  (1.0% bucket)
//   - 1.4%  → Round(2.8) = 3  (1.5% bucket)
//   - 2.25% → Round(4.5) = 5  (2.5% bucket)
//   - 2.75% → Round(5.5) = 6  (3.0% bucket)
func bucketEMAGap(pct float64) int {
	return int(math.Round(pct / 0.5)) // 0.5 % steps
}

// sortedCopy returns a sorted copy of a string slice, leaving the original
// unmodified. Used to normalise analyst lists before hashing.
func sortedCopy(ss []string) []string {
	if len(ss) == 0 {
		return ss
	}
	cp := make([]string, len(ss))
	copy(cp, ss)
	sort.Strings(cp)
	return cp
}
