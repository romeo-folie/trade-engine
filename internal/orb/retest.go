package orb

import (
	"math"

	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/data"
)

// Retest entry modes (Phase 6). A retest waits for the breakout to pull back to the range level
// and bounce, filtering first-breakout traps in exchange for fewer, higher-quality entries. Both
// modes use the same bounce entry; they differ only in stop placement, so the backtest can sweep
// which is better per instrument (see config.Instrument.BreakoutConfirmation).
const (
	ConfirmRetestSwing = "retest_swing" // stop just beyond the retest swing (tighter → better R:R)
	ConfirmRetestRange = "retest_range" // stop at the opposite side of the opening range (wider)
)

// IsRetestMode reports whether a confirmation mode is a retest entry.
func IsRetestMode(mode string) bool {
	return mode == ConfirmRetestSwing || mode == ConfirmRetestRange
}

// RetestResult is the outcome of a retest entry search.
type RetestResult struct {
	Side       string  // "buy" | "sell"
	EntryIdx   int     // candle index at which the bounce entry triggers
	EntryPrice float64 // entry = retest bar's extreme in the breakout direction + buffer
	SwingLow   float64 // the retest bar's low (buy) — for a swing stop
	SwingHigh  float64 // the retest bar's high (sell) — for a swing stop
	OK         bool
}

// DetectRetest walks 1-minute candles and finds the first retest entry. The state machine:
//
//  1. Breakout — a bar CLOSES beyond its trigger (buyTrigger = range high + buffer, sellTrigger =
//     range low − buffer). The first side to close beyond commits; a bar that straddles both is
//     broken by its own direction (bullish → buy). A non-positive trigger disables that side.
//  2. Pullback — price returns toward the level: a bar whose extreme comes within
//     tolerance×rangeWidth of the trigger. The DEEPEST such bar (lowest low for a buy) is the
//     retest bar. If any bar CLOSES back inside the range (buy: close < rangeHigh), the breakout
//     failed → abandon.
//  3. Bounce (entry) — a later bar ticks beyond the retest bar's extreme (buy: high > retest
//     high). Enter at that extreme + buffer; the retest bar's opposite extreme is the swing stop.
//
// The entry must occur within timeoutBars of the breakout. This is the single source of retest
// truth to be shared by the live scheduler and the backtest (live wiring is a follow-up).
func DetectRetest(candles []data.Candle, buyTrigger, sellTrigger, rangeHigh, rangeLow, buffer, tolerance float64, timeoutBars int) RetestResult {
	rangeWidth := rangeHigh - rangeLow
	if rangeWidth <= 0 {
		return RetestResult{}
	}
	tol := tolerance * rangeWidth

	// Find the committing breakout.
	side := ""
	breakoutIdx := -1
	for i, c := range candles {
		buyBreak := buyTrigger > 0 && c.Close >= buyTrigger
		sellBreak := sellTrigger > 0 && c.Close <= sellTrigger
		switch {
		case buyBreak && sellBreak:
			if c.Close >= c.Open {
				side = "buy"
			} else {
				side = "sell"
			}
		case buyBreak:
			side = "buy"
		case sellBreak:
			side = "sell"
		}
		if side != "" {
			breakoutIdx = i
			break
		}
	}
	if side == "" {
		return RetestResult{}
	}

	// Track the pullback and wait for the bounce.
	haveRetest := false
	var retestHigh, retestLow float64
	for j := breakoutIdx + 1; j < len(candles) && j-breakoutIdx <= timeoutBars; j++ {
		c := candles[j]
		if side == "buy" {
			if c.Close < rangeHigh {
				return RetestResult{} // closed back inside the range → failed breakout
			}
			if haveRetest && c.High > retestHigh {
				return RetestResult{Side: "buy", EntryIdx: j, EntryPrice: retestHigh + buffer, SwingLow: retestLow, OK: true}
			}
			if c.Low <= buyTrigger+tol && (!haveRetest || c.Low < retestLow) {
				haveRetest, retestHigh, retestLow = true, c.High, c.Low // deepest pullback so far
			}
		} else { // sell
			if c.Close > rangeLow {
				return RetestResult{} // closed back inside the range → failed breakout
			}
			if haveRetest && c.Low < retestLow {
				return RetestResult{Side: "sell", EntryIdx: j, EntryPrice: retestLow - buffer, SwingHigh: retestHigh, OK: true}
			}
			if c.High >= sellTrigger-tol && (!haveRetest || c.High > retestHigh) {
				haveRetest, retestHigh, retestLow = true, c.High, c.Low
			}
		}
	}
	return RetestResult{} // no bounce within the timeout, or ran out of candles
}

// ComputeRetestBracket builds the bracket for a retest entry: the given entry and stop, an RR
// target measured from that (tighter) risk, and a risk-sized qty via the same sizer as the range
// bracket. stopLevel is the swing (± buffer) or the range-opposite level, chosen by the caller.
func ComputeRetestBracket(ticker, side string, entry, stopLevel, portfolioValue float64, cfg config.ForexORBConfig, inst config.Instrument) BracketParams {
	risk := math.Abs(entry - stopLevel)
	var takeProfit float64
	if side == "buy" {
		takeProfit = entry + risk*cfg.RiskReward
	} else {
		takeProfit = entry - risk*cfg.RiskReward
	}
	return BracketParams{
		Ticker:     ticker,
		Side:       side,
		EntryStop:  entry,
		StopLoss:   stopLevel,
		TakeProfit: takeProfit,
		Qty:        sizeForexQty(entry, stopLevel, portfolioValue, cfg, inst),
		SecType:    inst.SecType,
	}
}
