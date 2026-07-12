// Package study replays recorded stock-path trades against historical 1-minute
// bars to price the execution layer: what market entries cost vs the signal
// price, what patient limit entries would have filled and returned, and what a
// broker-resident bracket stop would have saved vs the hourly candle-close
// monitor. Everything here is an offline audit over already-recorded trades —
// no orders are placed and no engine state is touched.
//
// All simulation is conservative under bar-granularity ambiguity: when a stop
// and target are both touchable within one bar, the stop wins; fills through a
// gap happen at the bar open, never at the trigger price.
package study

import (
	"time"

	"github.com/romeo-folie/trade-engine/internal/data"
)

// dirSign maps a trade direction to the sign of a favorable price move.
func dirSign(direction string) float64 {
	if direction == "short" {
		return -1
	}
	return 1
}

// pctReturn is the signed percent return of exit vs entry for the direction.
func pctReturn(direction string, entry, exit float64) float64 {
	if entry == 0 {
		return 0
	}
	return dirSign(direction) * (exit - entry) / entry * 100
}

// firstBarAtOrAfter returns the index of the first bar whose timestamp is not
// before t, or -1 when no such bar exists.
func firstBarAtOrAfter(bars []data.Candle, t time.Time) int {
	for i, b := range bars {
		if !b.Timestamp.Before(t) {
			return i
		}
	}
	return -1
}

// EntryFill is the result of simulating an entry tactic.
type EntryFill struct {
	Idx   int     // bar index of the fill
	Price float64 // fill price
	OK    bool    // false = never filled inside the TTL window
}

// SimMarketEntry fills at the open of the first bar at or after the signal.
// This models what a live market order pays (paper mode fills at signal_price
// instead, hiding this cost entirely).
func SimMarketEntry(bars []data.Candle, signalAt time.Time) EntryFill {
	i := firstBarAtOrAfter(bars, signalAt)
	if i < 0 {
		return EntryFill{OK: false}
	}
	return EntryFill{Idx: i, Price: bars[i].Open, OK: true}
}

// SimLimitEntry walks bars from the signal until ttlEnd looking for a limit
// fill. A long limit fills when a bar's low touches the limit; the fill price
// is min(bar open, limit) — a gap through the limit fills at the better open.
// Shorts mirror. Bars at or after ttlEnd never fill.
func SimLimitEntry(bars []data.Candle, signalAt, ttlEnd time.Time, direction string, limit float64) EntryFill {
	if limit <= 0 {
		return EntryFill{OK: false}
	}
	start := firstBarAtOrAfter(bars, signalAt)
	if start < 0 {
		return EntryFill{OK: false}
	}
	long := dirSign(direction) > 0
	for i := start; i < len(bars); i++ {
		b := bars[i]
		if !b.Timestamp.Before(ttlEnd) {
			break
		}
		if long && b.Low <= limit {
			return EntryFill{Idx: i, Price: minF(b.Open, limit), OK: true}
		}
		if !long && b.High >= limit {
			return EntryFill{Idx: i, Price: maxF(b.Open, limit), OK: true}
		}
	}
	return EntryFill{OK: false}
}

// ExitSim is the result of replaying a bracket (stop + target) over bars.
type ExitSim struct {
	Idx      int     // bar index of the exit
	Price    float64 // simulated fill
	Reason   string  // "stop" | "target" | "unresolved"
	Resolved bool    // false = neither leg touched before bars ran out
}

// SimBracketExit walks bars from startIdx and fires the first intrabar touch
// of the stop or target, the way broker-resident bracket legs behave. Within a
// single ambiguous bar the stop is evaluated first (conservative). A gap
// through the stop fills at the bar open (stop orders fill at market); a gap
// through the target fills at the open only when the open is better than the
// limit (limit orders never fill worse than their price).
// A zero stop or target disables that leg. When neither leg resolves, the
// last bar's close is returned as a mark-to-market with Resolved=false.
func SimBracketExit(bars []data.Candle, startIdx int, direction string, stop, target float64) ExitSim {
	long := dirSign(direction) > 0
	for i := startIdx; i < len(bars); i++ {
		b := bars[i]
		if stop > 0 {
			if long && b.Low <= stop {
				return ExitSim{Idx: i, Price: minF(b.Open, stop), Reason: "stop", Resolved: true}
			}
			if !long && b.High >= stop {
				return ExitSim{Idx: i, Price: maxF(b.Open, stop), Reason: "stop", Resolved: true}
			}
		}
		if target > 0 {
			if long && b.High >= target {
				return ExitSim{Idx: i, Price: maxF(b.Open, target), Reason: "target", Resolved: true}
			}
			if !long && b.Low <= target {
				return ExitSim{Idx: i, Price: minF(b.Open, target), Reason: "target", Resolved: true}
			}
		}
	}
	if len(bars) == 0 || startIdx >= len(bars) {
		return ExitSim{Reason: "unresolved"}
	}
	last := len(bars) - 1
	return ExitSim{Idx: last, Price: bars[last].Close, Reason: "unresolved"}
}

// BreachClass classifies how the price reached an exit trigger.
const (
	BreachIntraday = "intraday" // breached during the session — faster polling or broker stops fix this
	BreachGap      = "gap"      // opened beyond the trigger after a session break — only sizing/flatten policy helps
)

// ClassifyBreach reports whether the breach bar opened already beyond the
// trigger after a session break (overnight/weekend gap) or crossed it
// intraday. sessionBreak is the gap to the previous bar that counts as a new
// session; 1-minute RTH bars are contiguous, so anything well above a minute
// (lunch-halt safe: use >= 2h) indicates an overnight boundary.
func ClassifyBreach(bars []data.Candle, breachIdx int, direction string, trigger float64) string {
	b := bars[breachIdx]
	long := dirSign(direction) > 0
	openBeyond := (long && b.Open <= trigger) || (!long && b.Open >= trigger)
	if !openBeyond {
		return BreachIntraday
	}
	if breachIdx == 0 {
		return BreachGap
	}
	if b.Timestamp.Sub(bars[breachIdx-1].Timestamp) >= 2*time.Hour {
		return BreachGap
	}
	return BreachIntraday
}

// Excursions returns the maximum adverse and favorable excursion (both as
// positive percentages of entry) between startIdx and endIdx inclusive.
func Excursions(bars []data.Candle, startIdx, endIdx int, direction string, entry float64) (maePct, mfePct float64) {
	if entry == 0 || startIdx > endIdx || startIdx < 0 || endIdx >= len(bars) {
		return 0, 0
	}
	long := dirSign(direction) > 0
	worst, best := entry, entry
	for i := startIdx; i <= endIdx; i++ {
		if long {
			worst = minF(worst, bars[i].Low)
			best = maxF(best, bars[i].High)
		} else {
			worst = maxF(worst, bars[i].High)
			best = minF(best, bars[i].Low)
		}
	}
	maePct = dirSign(direction) * (entry - worst) / entry * 100
	mfePct = dirSign(direction) * (best - entry) / entry * 100
	return maePct, mfePct
}

// ATR14 computes a 14-period average true range from daily candles (simple
// average, matching indicators.ComputeATR's period semantics). Returns 0 when
// fewer than 15 bars are supplied.
func ATR14(daily []data.Candle) float64 {
	const period = 14
	if len(daily) < period+1 {
		return 0
	}
	sum := 0.0
	for i := len(daily) - period; i < len(daily); i++ {
		hl := daily[i].High - daily[i].Low
		hc := absF(daily[i].High - daily[i-1].Close)
		lc := absF(daily[i].Low - daily[i-1].Close)
		sum += maxF(hl, maxF(hc, lc))
	}
	return sum / period
}

// SimTrailingExit replays a ratcheting ATR trailing stop: after each bar the
// high-water mark advances and the stop ratchets to hwm − mult×atr (long),
// never below the original stop. The exit check for a bar uses the stop
// derived from PRIOR bars only — the current bar cannot both raise the
// high-water mark and stop the trade out at its own raised level (no intrabar
// lookahead). The fixed target still applies, evaluated after the stop within
// a bar (conservative).
func SimTrailingExit(bars []data.Candle, startIdx int, direction string, entry, origStop, target, atr, mult float64) ExitSim {
	if atr <= 0 || mult <= 0 {
		return SimBracketExit(bars, startIdx, direction, origStop, target)
	}
	long := dirSign(direction) > 0
	hwm := entry
	stop := origStop
	for i := startIdx; i < len(bars); i++ {
		b := bars[i]
		if stop > 0 {
			if long && b.Low <= stop {
				return ExitSim{Idx: i, Price: minF(b.Open, stop), Reason: "stop", Resolved: true}
			}
			if !long && b.High >= stop {
				return ExitSim{Idx: i, Price: maxF(b.Open, stop), Reason: "stop", Resolved: true}
			}
		}
		if target > 0 {
			if long && b.High >= target {
				return ExitSim{Idx: i, Price: maxF(b.Open, target), Reason: "target", Resolved: true}
			}
			if !long && b.Low <= target {
				return ExitSim{Idx: i, Price: minF(b.Open, target), Reason: "target", Resolved: true}
			}
		}
		if long {
			hwm = maxF(hwm, b.High)
			stop = maxF(stop, hwm-mult*atr)
		} else {
			hwm = minF(hwm, b.Low)
			trail := hwm + mult*atr
			if stop <= 0 || trail < stop {
				stop = trail
			}
		}
	}
	if len(bars) == 0 || startIdx >= len(bars) {
		return ExitSim{Reason: "unresolved"}
	}
	last := len(bars) - 1
	return ExitSim{Idx: last, Price: bars[last].Close, Reason: "unresolved"}
}

func minF(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}

func maxF(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

func absF(a float64) float64 {
	if a < 0 {
		return -a
	}
	return a
}
