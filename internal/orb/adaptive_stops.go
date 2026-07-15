package orb

import (
	"context"
	"log/slog"
	"math"
	"time"

	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/data"
	"github.com/romeo-folie/trade-engine/internal/execution"
	"github.com/romeo-folie/trade-engine/internal/store"
)

// adaptiveStopInterval throttles the adaptive-stop sweep. A 30s tick is plenty to ratchet
// a stop and keeps the scheduler within IBKR's request budget.
const adaptiveStopInterval = time.Minute

// enforceAdaptiveStops is the LIVE counterpart of the backtest `adaptive` variant's
// break-even and trailing logic (Phase 4): once per minute it walks the filled forex
// trades whose instrument opts into break_even_r / trail_atr_mult, recomputes the desired
// stop from the favourable high-water mark, and — when that's a genuine tightening —
// pushes the new stop to the broker's live SL leg and records it.
//
// Partial take-profit is NOT done here (it needs scale-out accounting); only stop movement.
// Runs for any executor that can both list working orders and replace a stop; a no-op
// otherwise. Stops only ever ratchet toward price, never widen.
func (s *ORBScheduler) enforceAdaptiveStops(ctx context.Context) {
	if (!s.isForexMode() && !s.isFuturesMode()) || s.db == nil || s.marketData == nil {
		return
	}
	if _, ok := s.executor.(execution.StopOrderReplacer); !ok {
		return
	}
	if _, ok := s.executor.(execution.WorkingOrdersLister); !ok {
		return
	}
	if time.Since(s.lastAdaptiveStopCheckAt) < adaptiveStopInterval {
		return
	}
	s.lastAdaptiveStopCheckAt = s.now()

	trades, err := store.GetOpenFilledORBTrades(ctx, s.db)
	if err != nil {
		slog.Warn("orb: adaptive stop: load filled trades failed", "err", err)
		return
	}
	rr := s.effectiveForexCfg().RiskReward
	if rr <= 0 {
		rr = 2.0
	}
	now := s.now()
	for i := range trades {
		t := trades[i]
		if !s.ownsInstrument(t.Ticker) || t.FillPrice == 0 {
			continue
		}
		inst := s.effectiveInstrument(t.Ticker)
		if inst == nil || (inst.BreakEvenR <= 0 && inst.TrailATRMult <= 0) {
			continue
		}
		// Planned risk R from the immutable entry/target geometry (the stored stop_loss may
		// already have been moved): TP sits at entry ± mult×R, so R = |TP − entry| / mult.
		R := math.Abs(t.TakeProfit-t.EntryStop) / scaleOutTargetR(t.ScaleOutRole, *inst, rr)
		if R <= 0 {
			continue
		}

		from := t.CreatedAt
		if t.FilledAt != nil {
			from = *t.FilledAt
		}
		candles, err := s.marketData.FetchCandles(ctx, t.Ticker, data.Timeframe1Min, from, now)
		if err != nil || len(candles) == 0 {
			continue
		}
		hwm := highWaterMark(t.Side, t.FillPrice, candles)

		var atr float64
		if inst.TrailATRMult > 0 {
			atr = s.instrumentATR(ctx, t.Ticker)
		}
		newStop, tighter := adaptiveStop(t.Side, t.FillPrice, t.StopLoss, R, hwm, atr, *inst)
		if !tighter {
			continue
		}
		if s.moveStopLeg(ctx, t, newStop) {
			s.persistMovedStop(ctx, t, newStop)
		}
	}
}

// persistMovedStop records a stop the broker has already accepted. The broker leg is the
// source of truth and is moved FIRST; this then aligns the DB so the next sweep matches the
// (now-moved) working leg and ratchets from it, and so the bar-replay backstop checks the
// correct stop level. A persist failure is dangerous — it freezes adaptive stops for the
// trade (subsequent sweeps look for the leg at the stale price and miss) and feeds the
// bar-replay a phantom stop level — so retry a few times before escalating to a CRITICAL,
// recovery-actionable log. Inverting the order (DB first) is not safer: it would show a
// tightened stop the broker never accepted, a false sense of protection.
func (s *ORBScheduler) persistMovedStop(ctx context.Context, t store.ORBTrade, newStop float64) {
	movedAt := s.now()
	var err error
	for range 3 {
		if err = store.UpdateORBTradeStopLoss(ctx, s.db, t.ID, newStop, movedAt); err == nil {
			return
		}
	}
	slog.Error("orb: adaptive stop: CRITICAL — broker stop moved but DB persist failed; "+
		"orb_trades.stop_loss is now STALE and adaptive stops are frozen for this trade. "+
		"Recover manually: UPDATE orb_trades SET stop_loss=<live_broker_stop> WHERE id=<trade_id> AND status='filled'",
		"trade_id", t.ID, "ticker", t.Ticker,
		"stale_db_stop", t.StopLoss, "live_broker_stop", newStop, "err", err)
}

// scaleOutTargetR returns the R-multiple at which a leg's take-profit sits — used to recover
// the planned risk R from the immutable |TP − entry| distance. A split-bracket partial
// targets partial_take_profit_r and the runner runner_take_profit_r (each falling back to
// the strategy RR when unset); an ordinary trade targets RR. Dividing by a flat RR instead
// would mis-scale R for a partial/runner row and fire break-even/trailing at the wrong price.
func scaleOutTargetR(role string, inst config.Instrument, rr float64) float64 {
	switch role {
	case "partial":
		if inst.PartialTakeProfitR > 0 {
			return inst.PartialTakeProfitR
		}
	case "runner":
		if inst.RunnerTakeProfitR > 0 {
			return inst.RunnerTakeProfitR
		}
	}
	return rr
}

// adaptiveStop computes the desired (tightened) stop for a filled trade given the
// favourable high-water mark and ATR, and reports whether it improves on the current stop
// (so the caller only pushes a genuine ratchet). Break-even moves the stop to the entry
// fill once break_even_r is reached; trailing moves it to hwm ∓ trail_atr_mult×ATR once
// trail_after_r is reached. Pure — unit-tested.
func adaptiveStop(side string, entryFill, currentStop, R, hwm, atr float64, inst config.Instrument) (float64, bool) {
	if R <= 0 {
		return currentStop, false
	}
	long := side == "buy"
	favR := (hwm - entryFill) / R
	if !long {
		favR = (entryFill - hwm) / R
	}
	stop := currentStop
	if inst.BreakEvenR > 0 && favR >= inst.BreakEvenR {
		if long && entryFill > stop {
			stop = entryFill
		} else if !long && entryFill < stop {
			stop = entryFill
		}
	}
	if inst.TrailATRMult > 0 && atr > 0 && favR >= inst.TrailAfterR {
		if long {
			if c := hwm - inst.TrailATRMult*atr; c > stop {
				stop = c
			}
		} else if c := hwm + inst.TrailATRMult*atr; c < stop {
			stop = c
		}
	}
	tighter := (long && stop > currentStop) || (!long && stop < currentStop)
	return stop, tighter
}

// highWaterMark returns the most-favourable price reached since entry: the peak high for a
// long, the trough low for a short (never worse than the entry fill).
func highWaterMark(side string, entryFill float64, candles []data.Candle) float64 {
	hwm := entryFill
	for _, c := range candles {
		if side == "buy" {
			if c.High > hwm {
				hwm = c.High
			}
		} else if c.Low < hwm {
			hwm = c.Low
		}
	}
	return hwm
}

// moveStopLeg is the live bridge: it finds the trade's working stop-loss leg by symbol,
// side and price (the leg id was never captured — the #359 gap) and replaces its stop.
// The SL leg is the working order on the OPPOSITE side to the position whose stop price
// matches the trade's current stop_loss (the TP leg is a limit, with no stop price, so it
// is excluded). Returns whether the stop was moved at the broker.
func (s *ORBScheduler) moveStopLeg(ctx context.Context, t store.ORBTrade, newStop float64) bool {
	lister, ok := s.executor.(execution.WorkingOrdersLister)
	if !ok {
		return false
	}
	replacer, ok := s.executor.(execution.StopOrderReplacer)
	if !ok {
		return false
	}
	orders, err := lister.GetWorkingOrders(ctx)
	if err != nil {
		slog.Warn("orb: adaptive stop: list working orders failed", "ticker", t.Ticker, "err", err)
		return false
	}
	slSide := "sell" // a long's protective stop is a sell-stop
	if t.Side == "sell" {
		slSide = "buy"
	}
	// A dual-sided ORB places the buy's protective stop AND the opposing sell entry as
	// sell-stops at the same price (r.Low − pip): structurally indistinguishable in the
	// working-order list. Never modify a current-session entry order — replacing the sell
	// entry's trigger would arm a hair-trigger new position. Mirrors cancelBracketLegs.
	protected := s.protectedOrderIDs()
	for _, o := range orders {
		if o.Ticker != t.Ticker || o.Side != slSide || o.StopPrice <= 0 {
			continue
		}
		if _, skip := protected[o.OrderID]; skip {
			continue
		}
		if t.StopLoss <= 0 || math.Abs(o.StopPrice-t.StopLoss)/t.StopLoss >= legPriceTol {
			continue
		}
		if _, err := replacer.ReplaceStopOrder(ctx, o.OrderID, newStop); err != nil {
			slog.Warn("orb: adaptive stop: replace failed", "ticker", t.Ticker, "order_id", o.OrderID, "err", err)
			return false
		}
		slog.Info("orb: adaptive stop: moved live SL", "ticker", t.Ticker, "side", t.Side,
			"old_stop", t.StopLoss, "new_stop", newStop)
		return true
	}
	slog.Warn("orb: adaptive stop: SL leg not found among working orders — cannot move", "ticker", t.Ticker, "stop", t.StopLoss)
	return false
}
