package engine

import (
	"fmt"
	"time"

	"github.com/romeo-folie/trade-engine/internal/config"
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
