package trailing

import (
	"math"
	"strings"

	"github.com/romeo-folie/trade-engine/internal/execution"
	"github.com/romeo-folie/trade-engine/internal/store"
)

// Short trailing stop (#599 PR 4b). The manager trails both sides of a stock
// position with one code path; every place that used to assume a long goes
// through the helpers below. For a short:
//
//   - the favourable extreme is the LOW since entry, stored in the existing
//     high_water_mark column (its meaning is "most favourable price"; the
//     trade's direction decides how it is read);
//   - the trail arms on a peak gain of (entry - low) / entry;
//   - the stop is low + atr_multiple x ATR and only ever moves DOWN;
//   - the stop is a BUY above price, and a crossing covers at market.
//
// Long behaviour is byte-identical to before: every helper's long branch is
// the original expression.

// isShortStockTrade mirrors isLongStockTrade for an open short.
func isShortStockTrade(t store.Trade) bool {
	if t.Status != "open" || t.Direction != "short" {
		return false
	}
	strat := strings.ToLower(t.Strategy)
	return !strings.Contains(strat, "orb") && !strings.Contains(strat, "forex")
}

// isManagedStockTrade reports whether the manager trails this trade.
func isManagedStockTrade(t store.Trade) bool {
	return isLongStockTrade(t) || isShortStockTrade(t)
}

// sameManagedSide reports whether fresh is still a managed trade on the same
// side as t. A reload that changed direction is never acted on.
func sameManagedSide(t, fresh store.Trade) bool {
	if t.Direction == "short" {
		return isShortStockTrade(fresh)
	}
	return isLongStockTrade(fresh)
}

// exitSideFor is the order side that closes t: SELL for a long, BUY for a
// short.
func exitSideFor(t store.Trade) execution.OrderSide {
	return execution.ExitSideForDirection(t.Direction)
}

// seedOrAdvanceExtreme returns the favourable extreme after observing price:
// the high for a long (seeded at max(entry, price)), the low for a short
// (seeded at min(entry, price)).
func seedOrAdvanceExtreme(t store.Trade, price float64) float64 {
	extreme := t.HighWaterMark
	if t.Direction == "short" {
		switch {
		case extreme <= 0:
			if t.EntryPrice > 0 {
				return math.Min(t.EntryPrice, price)
			}
			return price
		case price > 0 && price < extreme:
			return price
		}
		return extreme
	}
	if extreme <= 0 {
		return math.Max(t.EntryPrice, price)
	}
	if price > extreme {
		return price
	}
	return extreme
}

// trailArmedFor applies TrailArmed on the trade's side.
func trailArmedFor(t store.Trade, extreme, activateGainPct float64) bool {
	if t.Direction == "short" {
		return TrailArmedShort(t.EntryPrice, extreme, activateGainPct)
	}
	return TrailArmed(t.EntryPrice, extreme, activateGainPct)
}

// TrailArmedShort is TrailArmed for a short: the peak gain is the drop from
// entry to the low-water mark. It latches for the same reason (the low only
// ratchets down).
func TrailArmedShort(entryPrice, lowWaterMark, activateGainPct float64) bool {
	if activateGainPct <= 0 {
		return true
	}
	if entryPrice <= 0 || lowWaterMark <= 0 {
		return false
	}
	return (entryPrice-lowWaterMark)/entryPrice*100 >= activateGainPct
}

// computeNewStopFor applies the side's trailing formula.
func computeNewStopFor(t store.Trade, currentStop, extreme, atr, atrMultiple float64) float64 {
	if t.Direction == "short" {
		return ComputeNewShortStop(currentStop, extreme, atr, atrMultiple)
	}
	return ComputeNewStop(currentStop, extreme, atr, atrMultiple)
}

// ComputeNewShortStop is ComputeNewStop for a short:
// min(currentStop, lowWaterMark + atrMultiple x atr). The clamp guarantees the
// stop ratchets DOWN only, so a transient ATR spike can never loosen it. A
// missing current stop (<= 0) is replaced by the candidate.
func ComputeNewShortStop(currentStop, lowWaterMark, atr, atrMultiple float64) float64 {
	if atr <= 0 || lowWaterMark <= 0 || atrMultiple <= 0 {
		return currentStop
	}
	candidate := lowWaterMark + atrMultiple*atr
	if currentStop <= 0 || candidate < currentStop {
		return candidate
	}
	return currentStop
}

// stopCrossed reports whether the trail is already through the market: at
// or above price for a long, at or below price for a short.
func stopCrossed(t store.Trade, stop, price float64) bool {
	if t.Direction == "short" {
		return stop <= price
	}
	return stop >= price
}

// stopTightened reports whether newStop moved in the favourable direction
// from persisted: up for a long, down for a short.
func stopTightened(t store.Trade, newStop, persisted float64) bool {
	if t.Direction == "short" {
		return persisted <= 0 || newStop < persisted
	}
	return newStop > persisted
}

// tighterStop merges two candidate stops so a stale worker can never widen
// the durable stop: the higher for a long, the lower positive for a short.
func tighterStop(t store.Trade, a, b float64) float64 {
	if t.Direction == "short" {
		return minPositive(a, b)
	}
	return math.Max(a, b)
}

// moreFavourableExtreme merges two favourable extremes so a stale worker can
// never lose a printed extreme: the higher for a long, the lower positive for
// a short.
func moreFavourableExtreme(t store.Trade, a, b float64) float64 {
	if t.Direction == "short" {
		return minPositive(a, b)
	}
	return math.Max(a, b)
}

// minPositive is the smaller of two values, ignoring non-positive ones (0 is
// "unset" for both stop_loss and high_water_mark).
func minPositive(a, b float64) float64 {
	switch {
	case a <= 0:
		return b
	case b <= 0:
		return a
	}
	return math.Min(a, b)
}

// emaExitSignal reports whether the daily close is through the EMA against
// the position: below it for a long, above it for a short.
func emaExitSignal(t store.Trade, close, ema float64) bool {
	if t.Direction == "short" {
		return close > ema
	}
	return close < ema
}
