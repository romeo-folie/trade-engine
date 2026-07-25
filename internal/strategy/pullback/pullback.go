// Package pullback implements the deterministic "buy weakness in strength"
// setup from docs/exit-policy-study.md. It is deliberately independent of the
// LLM pipeline so live/paper execution and cached backtests evaluate exactly
// the same market evidence.
package pullback

import (
	"fmt"
	"math"

	"github.com/romeo-folie/trade-engine/internal/data"
	"github.com/romeo-folie/trade-engine/internal/indicators"
)

const (
	fastEMAPeriod        = 9
	slowEMAPeriod        = 21
	oneMonthSessions     = 21
	threeMonthSessions   = 63
	sixMonthSessions     = 126
	defaultConsolidation = 40
)

// Config controls the few thresholds that the research specifies. Zero values
// are resolved to the documented defaults by Evaluate.
type Config struct {
	MinMomentumReturnPct  float64
	ConsolidationSessions int
	SupportTolerancePct   float64
	MaxStopWidthPct       float64
}

// Result exposes the complete setup state for logging, replay attribution, and
// stop placement. Eligible is true only when every required component passes.
type Result struct {
	Eligible    bool
	Reason      string
	FailedCheck string

	Return1M     float64
	Return3M     float64
	Return6M     float64
	EMA9         float64
	EMA21        float64
	AVWAPHigh    float64
	AVWAPLow     float64
	Support      float64
	StopLoss     float64
	StopWidthPct float64
}

// Evaluate treats the final candle as the signal session and all preceding
// candles as completed daily history. This matches both the live partial daily
// bar and a daily replay candle without look-ahead.
func Evaluate(candles []data.Candle, signalPrice float64, cfg Config) Result {
	cfg = withDefaults(cfg)
	need := max(sixMonthSessions, cfg.ConsolidationSessions+slowEMAPeriod) + 1
	if len(candles) < need {
		return blocked("history", "pullback strength: insufficient daily history (%d candles, need %d)", len(candles), need)
	}
	session := candles[len(candles)-1]
	history := candles[:len(candles)-1]
	if signalPrice <= 0 {
		signalPrice = session.Close
	}
	if signalPrice <= 0 || session.Open <= 0 || session.Low <= 0 {
		return blocked("history", "pullback strength: invalid signal-session prices")
	}

	r := Result{
		Return1M: horizonReturn(signalPrice, history, oneMonthSessions),
		Return3M: horizonReturn(signalPrice, history, threeMonthSessions),
		Return6M: horizonReturn(signalPrice, history, sixMonthSessions),
	}
	if maxFloat(r.Return1M, r.Return3M, r.Return6M) < cfg.MinMomentumReturnPct {
		r.Reason = fmt.Sprintf(
			"pullback strength: momentum below %.1f%% on all horizons (1m %.1f%%, 3m %.1f%%, 6m %.1f%%)",
			cfg.MinMomentumReturnPct, r.Return1M, r.Return3M, r.Return6M,
		)
		r.FailedCheck = "momentum"
		return r
	}

	r.EMA9 = indicators.ComputeEMA(history, fastEMAPeriod)
	r.EMA21 = indicators.ComputeEMA(history, slowEMAPeriod)
	slopeLookback := 5
	prior := history[:len(history)-slopeLookback]
	priorEMA9 := indicators.ComputeEMA(prior, fastEMAPeriod)
	priorEMA21 := indicators.ComputeEMA(prior, slowEMAPeriod)
	if r.EMA9 <= priorEMA9 || r.EMA21 <= priorEMA21 {
		r.Reason = fmt.Sprintf(
			"pullback strength: daily EMA9/EMA21 are not both rising (%.2f/%.2f)",
			r.EMA9, r.EMA21,
		)
		r.FailedCheck = "ema_trend"
		return r
	}

	consolidation := history[len(history)-cfg.ConsolidationSessions:]
	half := len(consolidation) / 2
	if minLow(consolidation[half:]) <= minLow(consolidation[:half]) {
		r.Reason = "pullback strength: consolidation has not formed a higher low"
		r.FailedCheck = "higher_low"
		return r
	}
	quarter := max(5, len(consolidation)/4)
	if averageRangePct(consolidation[len(consolidation)-quarter:]) >= averageRangePct(consolidation[:quarter]) {
		r.Reason = "pullback strength: consolidation range is not narrowing"
		r.FailedCheck = "narrowing_range"
		return r
	}

	highAnchor, lowAnchor := swingAnchors(consolidation)
	base := len(history) - len(consolidation)
	withSession := appendCopy(history, session)
	r.AVWAPHigh = anchoredVWAP(withSession, base+highAnchor)
	r.AVWAPLow = anchoredVWAP(withSession, base+lowAnchor)
	if r.AVWAPHigh <= 0 || r.AVWAPLow <= 0 {
		r.Reason = "pullback strength: anchored VWAP unavailable (volume missing)"
		r.FailedCheck = "avwap"
		return r
	}

	tolerance := cfg.SupportTolerancePct / 100
	horizontal := minLow(consolidation[len(consolidation)-quarter:])
	avwap := closer(r.EMA9, r.AVWAPHigh, r.AVWAPLow)
	stackedWithEMA := relativeDistance(avwap, r.EMA9) <= tolerance
	stackedWithHorizontal := relativeDistance(avwap, horizontal) <= tolerance
	if !stackedWithEMA && !stackedWithHorizontal {
		r.Reason = fmt.Sprintf(
			"pullback strength: AVWAP %.2f is not stacked with EMA9 %.2f or horizontal support %.2f",
			avwap, r.EMA9, horizontal,
		)
		r.FailedCheck = "stacked_support"
		return r
	}
	companion := r.EMA9
	if !stackedWithEMA {
		companion = horizontal
	}
	r.Support = math.Max(avwap, companion)
	if session.Low > r.Support*(1+tolerance) {
		r.Reason = fmt.Sprintf(
			"pullback strength: session low %.2f did not flush into stacked support %.2f",
			session.Low, r.Support,
		)
		r.FailedCheck = "flush"
		return r
	}
	if signalPrice < r.Support || signalPrice <= session.Open {
		r.Reason = fmt.Sprintf(
			"pullback strength: no positive reclaim (price %.2f, open %.2f, support %.2f)",
			signalPrice, session.Open, r.Support,
		)
		r.FailedCheck = "reclaim"
		return r
	}

	r.StopLoss = session.Low
	r.StopWidthPct = (signalPrice - r.StopLoss) / signalPrice * 100
	if r.StopWidthPct <= 0 || r.StopWidthPct > cfg.MaxStopWidthPct {
		r.Reason = fmt.Sprintf(
			"pullback strength: low-of-day stop is %.2f%% wide (max %.2f%%)",
			r.StopWidthPct, cfg.MaxStopWidthPct,
		)
		r.FailedCheck = "stop_width"
		return r
	}
	r.Eligible = true
	r.Reason = fmt.Sprintf(
		"pullback strength confirmed: momentum %.1f/%.1f/%.1f%%, support %.2f reclaimed, low-of-day stop %.2f (%.2f%%)",
		r.Return1M, r.Return3M, r.Return6M, r.Support, r.StopLoss, r.StopWidthPct,
	)
	return r
}

func withDefaults(cfg Config) Config {
	if cfg.MinMomentumReturnPct <= 0 {
		cfg.MinMomentumReturnPct = 30
	}
	if cfg.ConsolidationSessions < 20 {
		cfg.ConsolidationSessions = defaultConsolidation
	}
	if cfg.SupportTolerancePct <= 0 {
		cfg.SupportTolerancePct = 1.5
	}
	if cfg.MaxStopWidthPct <= 0 {
		cfg.MaxStopWidthPct = 4
	}
	return cfg
}

func blocked(check, format string, args ...any) Result {
	return Result{Reason: fmt.Sprintf(format, args...), FailedCheck: check}
}

func horizonReturn(price float64, history []data.Candle, sessions int) float64 {
	if len(history) < sessions {
		return math.Inf(-1)
	}
	base := history[len(history)-sessions].Close
	if base <= 0 {
		return math.Inf(-1)
	}
	return (price/base - 1) * 100
}

func minLow(candles []data.Candle) float64 {
	v := math.Inf(1)
	for _, c := range candles {
		if c.Low > 0 && c.Low < v {
			v = c.Low
		}
	}
	return v
}

func averageRangePct(candles []data.Candle) float64 {
	var sum float64
	var n int
	for _, c := range candles {
		if c.Close > 0 && c.High >= c.Low {
			sum += (c.High - c.Low) / c.Close
			n++
		}
	}
	if n == 0 {
		return math.Inf(1)
	}
	return sum / float64(n)
}

func swingAnchors(candles []data.Candle) (highIndex, lowIndex int) {
	high, low := math.Inf(-1), math.Inf(1)
	for i, c := range candles {
		if c.High > high {
			high, highIndex = c.High, i
		}
		if c.Low > 0 && c.Low < low {
			low, lowIndex = c.Low, i
		}
	}
	return highIndex, lowIndex
}

func anchoredVWAP(candles []data.Candle, anchor int) float64 {
	if anchor < 0 || anchor >= len(candles) {
		return 0
	}
	var pv, volume float64
	for _, c := range candles[anchor:] {
		if c.Volume <= 0 {
			continue
		}
		typical := (c.High + c.Low + c.Close) / 3
		pv += typical * float64(c.Volume)
		volume += float64(c.Volume)
	}
	if volume == 0 {
		return 0
	}
	return pv / volume
}

func appendCopy(candles []data.Candle, candle data.Candle) []data.Candle {
	out := make([]data.Candle, len(candles), len(candles)+1)
	copy(out, candles)
	return append(out, candle)
}

func closer(target, a, b float64) float64 {
	if math.Abs(target-a) <= math.Abs(target-b) {
		return a
	}
	return b
}

func relativeDistance(a, b float64) float64 {
	if a <= 0 || b <= 0 {
		return math.Inf(1)
	}
	return math.Abs(a-b) / math.Max(a, b)
}

func maxFloat(values ...float64) float64 {
	v := math.Inf(-1)
	for _, x := range values {
		if x > v {
			v = x
		}
	}
	return v
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
