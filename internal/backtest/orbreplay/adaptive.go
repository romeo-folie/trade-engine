package orbreplay

import (
	"time"

	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/data"
	"github.com/romeo-folie/trade-engine/internal/orb"
)

// resolveAdaptiveExit replays the Phase 4 adaptive exit management over the candles after
// entry and returns the final exit type, the TOTAL gross PnL (partial + runner), the exit
// time, and the TOTAL commission (entry fill + every exit fill, each charged at that
// fill's ACTUAL quantity). Commission is computed here rather than reconstructed from a
// fill count in resolveTrade because with a per-contract CommissionPerUnit the partial and
// runner fills trade different quantities — count × perFill(fullQty) would overbill.
//
// Per candle, in order: enforce the max-hold deadline; update the favourable high-water
// mark; move the stop to break-even once break_even_r is reached; trail the stop at
// hwm ∓ trail_atr_mult×ATR once trail_after_r is reached (ratchet only); take the partial
// once partial_take_profit_r is reached (switching the runner to runner_take_profit_r);
// then check the (possibly moved) stop before the target — the conservative same-bar
// straddle tie-break, matching resolveExitFromCandles. R is the planned risk
// |entry_stop − stop_loss|; "favourable R" is measured from the realised entry fill.
func (e *Engine) resolveAdaptiveExit(maxHoldMin int, p orb.BracketParams, inst config.Instrument, entryFill, half, atr float64, rest []data.Candle, entryTime time.Time) (exitType string, gross float64, exitTime time.Time, commission float64) {
	long := p.Side == "buy"
	pv := pointValue(inst)
	cost := e.costFor(inst.Symbol)
	commission = cost.perFillCommission(p.Qty) // entry fill
	R := absf(p.EntryStop - p.StopLoss)
	if R <= 0 {
		R = 0.0001
	}
	stop := p.StopLoss
	target := p.TakeProfit
	remaining := p.Qty
	var partialGross float64
	partialDone := false
	peak := entryFill // most-favourable price reached (high-water mark)

	hasDeadline := maxHoldMin > 0
	deadline := entryTime.Add(time.Duration(maxHoldMin) * time.Minute)

	favR := func(price float64) float64 {
		if long {
			return (price - entryFill) / R
		}
		return (entryFill - price) / R
	}
	// finish exits the REMAINING position at exitMid and returns the combined result,
	// charging the final exit fill at the remaining quantity.
	finish := func(et string, exitMid float64, ts time.Time) (string, float64, time.Time, float64) {
		return et, partialGross + pnl(p.Side, entryFill, exitWithSpread(p.Side, exitMid, half), remaining, pv), ts,
			commission + cost.perFillCommission(remaining)
	}

	if len(rest) == 0 {
		return "end_of_data", 0, entryTime, commission + cost.perFillCommission(remaining)
	}

	for _, c := range rest {
		if hasDeadline && !c.Timestamp.Before(deadline) {
			return finish("time_exit", c.Open, c.Timestamp) // flatten at the deadline bar's open
		}

		// Exit checks use the stop/target as set by PRIOR bars (start-of-bar state), and are
		// ordered conservatively: an adverse stop before a favourable partial before the
		// target. This avoids the optimistic intra-bar assumption that a favourable move
		// (partial, or this bar's trail-up) preceded the adverse one on a straddle bar.
		if (long && c.Low <= stop) || (!long && c.High >= stop) {
			return finish("stop", stop, c.Timestamp)
		}
		if !partialDone && inst.PartialTakeProfitR > 0 && inst.PartialFraction > 0 {
			level := entryFill + inst.PartialTakeProfitR*R
			hit := c.High >= level
			if !long {
				level = entryFill - inst.PartialTakeProfitR*R
				hit = c.Low <= level
			}
			if hit {
				f := inst.PartialFraction
				if f > 1 {
					f = 1
				}
				pq := int(float64(p.Qty) * f)
				if inst.MinLotSize > 0 { // a partial must be a tradeable lot multiple
					pq = (pq / inst.MinLotSize) * inst.MinLotSize
				}
				switch {
				case pq >= remaining:
					return finish("tp", level, c.Timestamp) // fraction closes the whole position
				case pq > 0:
					partialGross += pnl(p.Side, entryFill, exitWithSpread(p.Side, level, half), pq, pv)
					commission += cost.perFillCommission(pq) // partial exit fill at its own qty
					remaining -= pq
					partialDone = true
					if inst.RunnerTakeProfitR > 0 {
						if long {
							target = entryFill + inst.RunnerTakeProfitR*R
						} else {
							target = entryFill - inst.RunnerTakeProfitR*R
						}
					}
				}
			}
		}
		if (long && c.High >= target) || (!long && c.Low <= target) {
			return finish("tp", target, c.Timestamp)
		}

		// Still open — ratchet the stop for the NEXT bar from this bar's high-water mark.
		// Done after the exit checks so a favourable excursion on this bar can't tighten the
		// stop in time to be credited against this same bar's adverse move.
		if long {
			if c.High > peak {
				peak = c.High
			}
		} else if c.Low < peak {
			peak = c.Low
		}
		favPeak := favR(peak)
		if inst.BreakEvenR > 0 && favPeak >= inst.BreakEvenR {
			if long && stop < entryFill {
				stop = entryFill
			} else if !long && stop > entryFill {
				stop = entryFill
			}
		}
		if inst.TrailATRMult > 0 && atr > 0 && favPeak >= inst.TrailAfterR {
			if long {
				if ns := peak - inst.TrailATRMult*atr; ns > stop {
					stop = ns
				}
			} else if ns := peak + inst.TrailATRMult*atr; ns < stop {
				stop = ns
			}
		}
	}

	last := rest[len(rest)-1]
	return finish("end_of_data", last.Close, last.Timestamp)
}
