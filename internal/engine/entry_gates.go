package engine

import (
	"fmt"
	"strings"
	"time"

	"github.com/romeo-folie/trade-engine/internal/analysis/prompts"
	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/data"
	"github.com/romeo-folie/trade-engine/internal/indicators"
	"github.com/romeo-folie/trade-engine/internal/store"
	"github.com/romeo-folie/trade-engine/internal/strategy/pullback"
)

// stockTimeframesForEntry returns a normalized copy and ensures the complete
// pullback gate has its required daily evidence without mutating the watchlist.
func stockTimeframesForEntry(timeframes []string, pullbackEnabled bool) []string {
	if len(timeframes) == 0 {
		return []string{string(data.Timeframe1Day)}
	}
	out := make([]string, len(timeframes))
	hasDaily := false
	for i, timeframe := range timeframes {
		out[i] = strings.ToLower(timeframe)
		hasDaily = hasDaily || out[i] == string(data.Timeframe1Day)
	}
	if pullbackEnabled && !hasDaily {
		out = append(out, string(data.Timeframe1Day))
	}
	return out
}

func primaryStockTimeframe(timeframes []string) data.Timeframe {
	if len(timeframes) == 0 {
		return data.Timeframe1Day
	}
	primary := data.Timeframe(timeframes[0])
	for _, timeframe := range timeframes {
		if timeframe == string(data.Timeframe1Day) {
			return data.Timeframe1Day
		}
	}
	return primary
}

// widestHistoryWindow returns the largest configured history window across the
// admissible lens set, falling back to the primary label's window.
//
// The prompt now offers every admissible lens as a candidate, so the candle
// budget must satisfy the most demanding one: a row of momentum,trend_following
// capped at momentum's 14 candles would hand the analyst too little history to
// honestly confirm or reject the trend read it is being asked to consider.
// Only lenses with a configured window participate; unknown labels and
// entry-gate tags contribute nothing.
func widestHistoryWindow(windows map[string]int, primary string, strategies []string) int {
	widest := 0
	for _, lens := range prompts.Lenses(append([]string{primary}, strategies...)) {
		if n, ok := windows[lens]; ok && n > widest {
			widest = n
		}
	}
	return widest
}

// effectiveGateStrategy returns the lens the entry-quality gates must judge
// against: the one the analyst actually reasoned under, not the watchlist's
// configured primary.
//
// The run-up gates below are keyed by strategy — the news tape-confirmation
// gate fires only for news_sentiment, the anti-chase gate only for momentum.
// Before the candidate-lens change the configured primary WAS the lens the
// analyst reasoned under (it was injected as the model's identity), so keying
// on it was safe. It no longer is: a row admitting
// [trend_following, news_sentiment] can now produce a BUY reasoned entirely
// under news_sentiment, and keying on the primary would wave through exactly
// the stale-catalyst chase these gates were built to block (PR #434).
//
// Falls back to the configured primary whenever the analyst reported no usable
// selection — empty (older model, or dropped in truncation recovery), "none"
// (a regime-mismatch HOLD), or an unrecognised label. Falling back preserves
// prior behaviour; trusting a bad value would silently disable the gate.
func effectiveGateStrategy(configured, selected string) string {
	if prompts.IsLens(selected) {
		return strings.ToLower(strings.TrimSpace(selected))
	}
	return configured
}

func hasEntryStrategy(strategies []string, want string) bool {
	for _, strategy := range strategies {
		if strings.EqualFold(strings.TrimSpace(strategy), want) {
			return true
		}
	}
	return false
}

func pullbackEntryEvidence(setup pullback.Result) string {
	return fmt.Sprintf(
		"Momentum %.1f/%.1f/%.1f%%; reclaimed $%.2f support; low-of-day stop $%.2f (%.2f%% risk)",
		setup.Return1M, setup.Return3M, setup.Return6M,
		setup.Support, setup.StopLoss, setup.StopWidthPct,
	)
}

type additiveEntryDecision struct {
	PullbackConfirmed bool
	EntrySetup        string
	Evidence          string
}

func resolveAdditiveEntry(setup pullback.Result) additiveEntryDecision {
	if !setup.Eligible {
		return additiveEntryDecision{EntrySetup: store.EntrySetupStandard}
	}
	return additiveEntryDecision{
		PullbackConfirmed: true,
		EntrySetup:        store.EntrySetupPullbackStrength,
		Evidence:          pullbackEntryEvidence(setup),
	}
}

const (
	entryTrendFastEMAPeriod = 9
	entryTrendSlowEMAPeriod = 21
)

// Entry-quality gates (see config.EntryGatesConfig). The May–Jul 2026
// execution study found the stock path's losses concentrated in three
// mechanical cohorts, each addressed by one gate here:
//
//   - news_sentiment BUYs where the tape never confirmed the headline
//     (price ~flat vs prior close) won 16% — the market's non-reaction is
//     information the analysts ignore.
//   - momentum BUYs chasing a >2% run-up won 14% vs 55% un-chased.
//   - the session's first analysis tick fired a correlated burst of
//     overnight-headline entries (~:46 past the open hour) that made up the
//     worst recurring cohort; deferring them one tick with a price-hold
//     check keeps the good ones and drops the fades.
//
// All logic here is pure so it can be unit-tested without an engine.

// evaluateRunupGates returns a non-empty block reason when a configured
// run-up gate rejects the signal. Gates apply to BUY only. prevClose <= 0
// means the prior daily close is unknown: gates fail OPEN (these are alpha
// filters, not safety guards) and the caller logs the miss.
func evaluateRunupGates(strategy, action string, signalPrice, prevClose float64, cfg config.EntryGatesConfig) string {
	if action != "BUY" || signalPrice <= 0 || prevClose <= 0 {
		return ""
	}
	runupPct := (signalPrice - prevClose) / prevClose * 100

	if strategy == "news_sentiment" && cfg.NewsMinRunupPct > 0 && runupPct < cfg.NewsMinRunupPct {
		return fmt.Sprintf("news tape confirmation: price %+.2f%% vs prior close, needs >= %.2f%% — market has not confirmed the headline", runupPct, cfg.NewsMinRunupPct)
	}
	if strategy == "momentum" && cfg.MomentumMaxRunupPct > 0 && runupPct > cfg.MomentumMaxRunupPct {
		return fmt.Sprintf("momentum anti-chase: price %+.2f%% vs prior close, max %.2f%% — entry would chase the move", runupPct, cfg.MomentumMaxRunupPct)
	}
	return ""
}

// evaluateTrendAlignmentGate blocks a BUY only when the evidence agrees on a
// daily downtrend: live price is below both EMA9 and EMA21, and both EMAs are
// below their previous completed-session values. The current partial daily bar
// is excluded before 16:00 New York time so intraday weakness cannot fabricate
// a completed-session trend reversal.
//
// evaluated is false when the gate is disabled, out of scope, or lacks enough
// daily data. These are alpha filters rather than safety guards, so callers
// fail open and log the missing evidence.
func evaluateTrendAlignmentGate(
	action string,
	signalPrice float64,
	timeframe data.Timeframe,
	candles []data.Candle,
	now time.Time,
	cfg config.EntryGatesConfig,
) (reason string, evaluated bool) {
	if !cfg.TrendAlignmentEnabled || action != "BUY" {
		return "", false
	}
	if timeframe != data.Timeframe1Day || signalPrice <= 0 {
		return "", false
	}
	completed := completedEntryDailyCandles(candles, now)
	if len(completed) < entryTrendSlowEMAPeriod+1 {
		return "", false
	}

	ema9 := indicators.ComputeEMA(completed, entryTrendFastEMAPeriod)
	ema21 := indicators.ComputeEMA(completed, entryTrendSlowEMAPeriod)
	previous := completed[:len(completed)-1]
	prevEMA9 := indicators.ComputeEMA(previous, entryTrendFastEMAPeriod)
	prevEMA21 := indicators.ComputeEMA(previous, entryTrendSlowEMAPeriod)
	if ema9 <= 0 || ema21 <= 0 || prevEMA9 <= 0 || prevEMA21 <= 0 {
		return "", false
	}

	belowBoth := signalPrice < ema9 && signalPrice < ema21
	bothDeclining := ema9 < prevEMA9 && ema21 < prevEMA21
	if !belowBoth || !bothDeclining {
		return "", true
	}
	return fmt.Sprintf(
		"trend alignment: price %.2f below declining daily EMA9 %.2f and EMA21 %.2f — buy weakness in strength only",
		signalPrice, ema9, ema21,
	), true
}

// completedEntryDailyCandles excludes the current US session's partial daily
// candle. Zero timestamps are retained for deterministic fixtures and data
// providers that do not timestamp history.
func completedEntryDailyCandles(candles []data.Candle, now time.Time) []data.Candle {
	if len(candles) == 0 {
		return candles
	}
	last := candles[len(candles)-1]
	if last.Timestamp.IsZero() {
		return candles
	}
	nowET := now.In(nyLoc)
	barET := last.Timestamp.In(nyLoc)
	sameSessionDate := nowET.Year() == barET.Year() &&
		nowET.YearDay() == barET.YearDay()
	if sameSessionDate && nowET.Hour() < 16 {
		return candles[:len(candles)-1]
	}
	return candles
}

// deferredPriceHeld reports whether a deferred BUY signal's thesis price
// held: the current price is no more than tolPct below the signal price.
// A zero/negative tolerance uses the 1.0% default documented in config.
func deferredPriceHeld(signalPrice, currentPrice, tolPct float64) bool {
	if signalPrice <= 0 || currentPrice <= 0 {
		return false
	}
	if tolPct <= 0 {
		tolPct = 1.0
	}
	return currentPrice >= signalPrice*(1-tolPct/100)
}

// sameMarketDay reports whether two instants fall on the same calendar day
// in the given market timezone. Used to detect the session's first tick.
func sameMarketDay(a, b time.Time, loc *time.Location) bool {
	al, bl := a.In(loc), b.In(loc)
	return al.Year() == bl.Year() && al.YearDay() == bl.YearDay()
}
