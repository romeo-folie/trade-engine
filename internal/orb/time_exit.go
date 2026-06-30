package orb

import (
	"context"
	"log/slog"
	"math"
	"time"

	"github.com/romeo-folie/trade-engine/internal/events"
	"github.com/romeo-folie/trade-engine/internal/execution"
	"github.com/romeo-folie/trade-engine/internal/store"
)

// timeExitCheckInterval throttles the time-exit sweep. A filled ORB trade only
// crosses its max-hold boundary once, so minute resolution is ample and keeps the
// scheduler well within IBKR's request budget.
const timeExitCheckInterval = time.Minute

// flattenPollAttempts bounds how many times the flatten market order's status is
// polled before the sweep gives up for this tick. A forex/CFD market order fills
// near-instantly, so this is a safety bound, not the expected path. If the fill is
// never confirmed the DB row is deliberately left 'filled' (never closed on an
// unconfirmed flatten) and retried next sweep — see flattenAndRecordTimeExit.
const flattenPollAttempts = 6

// flattenPollInterval is the wait between flatten-order status polls.
const flattenPollInterval = 500 * time.Millisecond

// shouldTimeExit reports whether a filled trade has exceeded its max-hold limit.
// anchor is the entry fill time (callers fall back to row creation when fill time
// is unknown). maxHoldMinutes <= 0 disables time exits. The boundary is inclusive
// so a trade exactly at the limit is exited.
func shouldTimeExit(anchor time.Time, maxHoldMinutes int, now time.Time) bool {
	if maxHoldMinutes <= 0 {
		return false
	}
	return now.Sub(anchor) >= time.Duration(maxHoldMinutes)*time.Minute
}

// enforceTimeExits flattens filled ORB trades that have exceeded their per-instrument
// max-hold limit. It is the Phase-1 risk-containment backstop: the doc's worst losses
// came from trades that became multi-day holds (one GBP/USD buy ran six days to
// -9.62R), and a fixed time boundary caps that tail.
//
// It runs only in forex mode (the only path with per-instrument max-hold config) and
// only when a real executor + market data are present. For each over-age filled trade
// it cancels the dangling bracket legs, flattens the position at market, and records
// exit_type='time_exit'. Crucially it NEVER closes the DB row without first confirming
// the position was actually flattened at the broker — a DB-only close would repeat the
// PR #354 mistake of letting the live bracket fill later while the DB claims the trade
// is done. It runs before reconcileBarReplayExits so a genuinely-open trade is recorded
// as a time exit rather than pre-empted by a replayed TP/stop touch.
func (s *ORBScheduler) enforceTimeExits(ctx context.Context) {
	if s.forexCfg == nil || !s.forexCfg.Enabled {
		return
	}
	if s.db == nil || s.executor == nil {
		return
	}
	if time.Since(s.lastTimeExitCheckAt) < timeExitCheckInterval {
		return
	}
	s.lastTimeExitCheckAt = s.now()

	trades, err := store.GetOpenFilledORBTrades(ctx, s.db)
	if err != nil {
		slog.Warn("orb: time exit: load filled trades failed", "err", err)
		return
	}

	now := s.now()
	for i := range trades {
		t := trades[i]

		// A zero entry fill can't yield a meaningful PnL and signals a corrupt row;
		// skip it (matches the bar-replay backstop's guard).
		if t.FillPrice == 0 {
			continue
		}

		maxHold := s.forexCfg.EffectiveMaxHoldMinutes(t.Ticker)
		anchor := t.CreatedAt
		if t.FilledAt != nil {
			anchor = *t.FilledAt
		}
		if !shouldTimeExit(anchor, maxHold, now) {
			continue
		}

		s.flattenAndRecordTimeExit(ctx, t, maxHold)
	}
}

// flattenAndRecordTimeExit cancels a filled trade's remaining bracket legs, flattens
// the position at market, and records the resulting exit as exit_type='time_exit'.
// It is intentionally conservative: if the broker reports the position already flat
// (the bracket filled but the exit was not yet recorded) it does nothing and leaves
// the broker/bar-replay reconcilers to record the real exit; if the flatten fill can
// not be confirmed it leaves the row 'filled' for the next sweep rather than guessing.
func (s *ORBScheduler) flattenAndRecordTimeExit(ctx context.Context, t store.ORBTrade, maxHold int) {
	secType := s.forexSecType(t.Ticker)

	// 1. Cancel the dangling TP/SL legs so they can't fill after we flatten. The leg
	//    order IDs were never captured (IBKR's bracket flow doesn't return them), so we
	//    find them by symbol via the working-orders lookup. Best-effort: a leg that is
	//    already gone is fine, and a cancel failure is logged but does not block the
	//    flatten (the market order still reduces the net position).
	s.cancelWorkingLegs(ctx, t.Ticker)

	// 2. Read the live position. Flatten only this trade's quantity, bounded by the
	//    broker's net position, so forex netting across trades doesn't over-flatten.
	positions, err := s.executor.GetPositions(ctx)
	if err != nil {
		slog.Warn("orb: time exit: get positions failed — will retry", "ticker", t.Ticker, "err", err)
		return
	}
	var netQty float64
	for _, p := range positions {
		if p.Ticker == t.Ticker {
			netQty = p.Quantity
			break
		}
	}

	flattenSide := execution.OrderSideSell
	available := netQty // long position available to sell
	if t.Side == "sell" {
		flattenSide = execution.OrderSideBuy
		available = -netQty // short position available to buy back (netQty is negative)
	}
	flattenQty := math.Min(float64(t.Qty), available)
	if flattenQty <= 0 {
		// Position already closed for this trade (bracket filled, just not yet recorded).
		// Don't fabricate a time exit — let the broker/bar-replay reconcilers record the
		// real exit. Leave the row 'filled'.
		slog.Info("orb: time exit: position already flat, deferring to reconcilers",
			"ticker", t.Ticker, "side", t.Side, "net_qty", netQty, "trade_qty", t.Qty)
		return
	}

	// 3. Flatten at market.
	slog.Info("orb: time exit: flattening filled trade past max hold",
		"ticker", t.Ticker, "side", t.Side, "flatten_side", flattenSide, "qty", flattenQty,
		"max_hold_min", maxHold, "entry_fill", t.FillPrice)
	res, err := s.executor.PlaceOrder(ctx, execution.OrderRequest{
		Ticker:   t.Ticker,
		Side:     flattenSide,
		Type:     execution.OrderTypeMarket,
		Quantity: flattenQty,
		SecType:  secType,
		TradeID:  t.ID,
	})
	if err != nil {
		slog.Warn("orb: time exit: flatten order failed — will retry", "ticker", t.Ticker, "err", err)
		return
	}

	// 4. Confirm the flatten fill price. Only close the DB row once the flatten is a
	//    confirmed fill — never on an unconfirmed market order.
	exitPrice, ok := s.awaitFlattenFill(ctx, res)
	if !ok {
		slog.Warn("orb: time exit: flatten fill not confirmed — leaving filled for next sweep",
			"ticker", t.Ticker, "order_id", res.BrokerOrderID)
		return
	}

	// 5. Record the exit. PnL is gross (matches the engine's exitPnL convention)
	//    and is computed on the quantity actually flattened, not the original trade
	//    size: flattenQty == t.Qty in the normal case, but is smaller when the net
	//    position was already partially reduced (a partial leg fill) — using t.Qty
	//    there would overstate PnL by the t.Qty/flattenQty ratio. flattenQty is
	//    bounded to t.Qty above, so this never inflates.
	pnl := exitPnL(t.Side, t.FillPrice, exitPrice, int(flattenQty))
	if dbErr := store.CloseORBTrade(ctx, s.db, t.ID, "time_exit", "broker", exitPrice, pnl); dbErr != nil {
		slog.Error("orb: time exit: close trade failed", "ticker", t.Ticker, "err", dbErr)
		return
	}
	slog.Info("orb: time exit: recorded", "ticker", t.Ticker, "side", t.Side,
		"exit_price", exitPrice, "entry_price", t.FillPrice, "pnl", pnl)
	pnlVal := pnl
	s.publish(events.ORBTradeUpdate, events.ORBTradeUpdatePayload{
		OrderID: t.OrderID, Ticker: t.Ticker, Side: t.Side,
		Status: "closed", FillPrice: t.FillPrice, PnL: &pnlVal,
	})
}

// cancelWorkingLegs cancels a ticker's still-working orders before a time-exit
// flatten, removing the bracket's live TP/SL legs (whose IDs were never captured)
// so they can't fire against the freshly-flattened position. No-op when the executor
// doesn't expose a working-orders lookup (e.g. PaperExecutor, whose bracket is
// flattened to a single entry with no live legs).
//
// GetWorkingOrders returns EVERY working order on the account, so it is filtered to
// the target ticker AND made to skip the current session's known entry orders: a
// prior-session trade time-exiting must never cancel today's freshly-placed entry
// stops on the same symbol. The dangling legs we DO want gone were never tracked, so
// they aren't in the protected set.
func (s *ORBScheduler) cancelWorkingLegs(ctx context.Context, ticker string) {
	lister, ok := s.executor.(execution.WorkingOrdersLister)
	if !ok {
		return
	}
	orders, err := lister.GetWorkingOrders(ctx)
	if err != nil {
		slog.Warn("orb: time exit: list working orders failed", "ticker", ticker, "err", err)
		return
	}
	protected := s.protectedOrderIDs()
	for _, o := range orders {
		if o.Ticker != ticker {
			continue
		}
		if _, skip := protected[o.OrderID]; skip {
			continue
		}
		if err := s.executor.CancelOrder(ctx, o.OrderID); err != nil {
			slog.Warn("orb: time exit: cancel working leg failed",
				"ticker", ticker, "order_id", o.OrderID, "err", err)
		}
	}
}

// protectedOrderIDs returns the current session's known entry order IDs. These must
// never be cancelled by a time-exit flatten of a different (prior-session) trade —
// they are the live entry stops of an in-progress session, not stale bracket legs.
func (s *ORBScheduler) protectedOrderIDs() map[string]struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]struct{})
	for _, ids := range s.orderIDs {
		for _, id := range ids {
			if id != "" {
				out[id] = struct{}{}
			}
		}
	}
	return out
}

// tickersWithOpenFilledTrades returns the set of tickers that currently have an
// ORB trade in status='filled' (entry filled, exit not yet recorded) across any
// session. tickExecuting consults it to avoid stacking a new position onto a ticker
// that still carries an unexited fill. Returns an empty set when the DB is
// unavailable so a transient DB error never blocks all trading.
func (s *ORBScheduler) tickersWithOpenFilledTrades(ctx context.Context) map[string]struct{} {
	out := make(map[string]struct{})
	if s.db == nil {
		return out
	}
	trades, err := store.GetOpenFilledORBTrades(ctx, s.db)
	if err != nil {
		slog.Warn("orb: could not check for open filled trades before placing brackets", "err", err)
		return out
	}
	for _, t := range trades {
		out[t.Ticker] = struct{}{}
	}
	return out
}

// awaitFlattenFill resolves a flatten market order's fill price. It returns the
// immediate fill if PlaceOrder already reported one, otherwise it polls the order
// status a bounded number of times. Returns ok=false if no fill is confirmed within
// the bound, so the caller can avoid recording an exit on an unconfirmed flatten.
func (s *ORBScheduler) awaitFlattenFill(ctx context.Context, res execution.OrderResult) (float64, bool) {
	if res.Status == "filled" && res.FilledPrice > 0 {
		return res.FilledPrice, true
	}
	if res.BrokerOrderID == "" {
		return 0, false
	}
	for range flattenPollAttempts {
		st, err := s.executor.GetOrderStatus(ctx, res.BrokerOrderID)
		if err == nil && st.Status == "filled" && st.FilledPrice > 0 {
			return st.FilledPrice, true
		}
		select {
		case <-ctx.Done():
			return 0, false
		case <-time.After(flattenPollInterval):
		}
	}
	return 0, false
}

// forexSecType returns the configured IBKR security type for a forex/CFD symbol
// ("CASH" for spot forex, "CFD" for metals). Defaults to empty (executor treats as
// CASH) when the symbol isn't configured.
func (s *ORBScheduler) forexSecType(ticker string) string {
	if s.forexCfg == nil {
		return ""
	}
	for _, inst := range s.forexCfg.Instruments {
		if inst.Symbol == ticker {
			return inst.SecType
		}
	}
	return ""
}
