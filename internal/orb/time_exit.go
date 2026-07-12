package orb

import (
	"context"
	"fmt"
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

// orphanLegCheckInterval throttles the orphan-leg reconciler, and orphanLegLookback
// bounds how far back it considers a time-exited trade's legs eligible for cleanup.
const (
	orphanLegCheckInterval = time.Minute
	orphanLegLookback      = 24 * time.Hour
)

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

	s.flattenDueTimeExits(ctx, trades, s.now())
}

// flattenDueTimeExits is the post-load orchestration of enforceTimeExits, split out so the
// group-atomic flatten/cancel logic is unit-testable with injected trades.
func (s *ORBScheduler) flattenDueTimeExits(ctx context.Context, trades []store.ORBTrade, now time.Time) {
	// Group ALL still-open forex legs by their logical trade — not just the ones already past
	// max-hold. A split group's legs share a fill instant and the same max-hold, but their
	// fills can be stamped a few seconds apart, so one leg can become due a sweep before its
	// sibling. The runner and partial also share one stop PRICE: cancelling that stop while a
	// sibling leg is still open would leave it naked. So we assemble the whole group, and once
	// ANY leg is due we flatten EVERY open leg (a sibling a few seconds short of its own
	// deadline goes with it) and cancel the legs only after the whole group is confirmed flat.
	groups := make(map[string][]store.ORBTrade)
	var order []string
	due := make(map[string]bool)
	for i := range trades {
		t := trades[i]

		// Only act on this scheduler's own forex/CFD instruments. The orb_trades
		// table is shared: in orb-newyork mode an equities scheduler writes equity
		// trades to the same table, and the forex scheduler must not hand those to
		// its IBKR forex flatten/cancel path (wrong conid, invalid-pair calls).
		if !s.isForexInstrument(t.Ticker) {
			continue
		}

		// A zero entry fill can't yield a meaningful PnL and signals a corrupt row;
		// skip it (matches the bar-replay backstop's guard).
		if t.FillPrice == 0 {
			continue
		}

		key := t.ID
		if t.ScaleOutParentID != nil && *t.ScaleOutParentID != "" {
			key = *t.ScaleOutParentID
		}
		if _, seen := groups[key]; !seen {
			order = append(order, key)
		}
		groups[key] = append(groups[key], t)

		maxHold := s.forexCfg.EffectiveMaxHoldMinutes(t.Ticker)
		anchor := t.CreatedAt
		if t.FilledAt != nil {
			anchor = *t.FilledAt
		}
		if shouldTimeExit(anchor, maxHold, now) {
			due[key] = true
		}
	}

	for _, key := range order {
		if !due[key] {
			continue // no leg of this logical trade has reached max-hold yet
		}
		legs := groups[key]
		if len(legs) == 1 {
			// Ordinary trade, or a split leg whose sibling already closed (its shared stop was
			// OCO-cancelled with it, so nothing else holds that price): flatten and cancel its
			// legs inline, as before.
			s.flattenAndRecordTimeExit(ctx, legs[0], s.forexCfg.EffectiveMaxHoldMinutes(legs[0].Ticker), false)
			continue
		}
		// Split-bracket group with >1 open leg: flatten every leg first (deferring leg
		// cancellation), then cancel the group's legs only once EVERY leg is confirmed flat.
		// If any flatten failed, leave all legs intact and retry next sweep — never strip a
		// shared stop while a leg might still hold a position.
		allFlat := true
		for _, t := range legs {
			if !s.flattenAndRecordTimeExit(ctx, t, s.forexCfg.EffectiveMaxHoldMinutes(t.Ticker), true) {
				allFlat = false
			}
		}
		if allFlat {
			for _, t := range legs {
				s.cancelBracketLegs(ctx, t)
			}
		} else {
			slog.Warn("orb: time exit: split group not fully flattened — deferring leg cancel to next sweep",
				"ticker", legs[0].Ticker, "legs", len(legs))
		}
	}
}

// flattenAndRecordTimeExit flattens a filled trade's position at market and records
// the resulting exit as exit_type='time_exit'.
//
// Ordering is deliberate and safety-critical: the protective bracket legs are left in
// place until a flatten is CONFIRMED, and only then cancelled. A previous version
// cancelled the legs first — but if GetPositions/PlaceOrder/fill-confirmation then
// failed (an IBKR/auth hiccup), the position would be left naked with no stop until the
// next sweep, the opposite of risk containment. So: read position → flatten → confirm
// fill → cancel the now-redundant legs → close the row. It is also conservative: if the
// position is already flat (bracket filled, exit not yet recorded) it does nothing and
// defers to the reconcilers; if the flatten can't be confirmed it leaves the row
// 'filled' with legs intact for the next sweep rather than guessing.
// deferCancel leaves the bracket legs in place after a confirmed flatten (the caller cancels
// them once an entire split-bracket group is flat — see enforceTimeExits). Returns whether
// this trade's position is now confirmed FLAT (flattened here or already closed), which the
// group caller uses to decide it is safe to cancel the shared stop.
func (s *ORBScheduler) flattenAndRecordTimeExit(ctx context.Context, t store.ORBTrade, maxHold int, deferCancel bool) bool {
	secType := s.forexSecType(t.Ticker)

	// 1. Read the live position FIRST, while the bracket still protects it. Flatten
	//    only this trade's quantity, bounded by the broker's net position, so forex
	//    netting across trades doesn't over-flatten.
	positions, err := s.executor.GetPositions(ctx)
	if err != nil {
		slog.Warn("orb: time exit: get positions failed — will retry (legs intact)", "ticker", t.Ticker, "err", err)
		return false
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
		// real exit. Leave the row 'filled' and the (already-resolved) legs untouched.
		slog.Info("orb: time exit: position already flat, deferring to reconcilers",
			"ticker", t.Ticker, "side", t.Side, "net_qty", netQty, "trade_qty", t.Qty)
		return true // position is flat (bracket already filled) — safe for the group to cancel legs
	}

	// 2. Flatten at market while the bracket legs still protect the position.
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
		slog.Warn("orb: time exit: flatten order failed — will retry (legs intact)", "ticker", t.Ticker, "err", err)
		return false
	}

	// 3. Confirm the flatten fill. Only once the position is confirmed flat do we
	//    remove protection / close the row — never on an unconfirmed market order.
	exitPrice, ok := s.awaitFlattenFill(ctx, res)
	if !ok {
		slog.Warn("orb: time exit: flatten fill not confirmed — leaving filled, legs intact for next sweep",
			"ticker", t.Ticker, "order_id", res.BrokerOrderID)
		return false
	}

	// 4. Cancel the now-redundant bracket legs so a leftover leg can't fire against
	//    the flat position and open a new one. Position is already closed, so this is
	//    best-effort cleanup, not protection-critical. For a split-bracket group this is
	//    DEFERRED to the caller: the runner and partial share one stop price, so cancelling
	//    here would pull the shared stop while the sibling leg may still be open. The caller
	//    cancels only once every leg of the group is confirmed flat.
	if !deferCancel {
		s.cancelBracketLegs(ctx, t)
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
		return true // position IS flat (flatten confirmed); the record retries next sweep
	}
	slog.Info("orb: time exit: recorded", "ticker", t.Ticker, "side", t.Side,
		"exit_price", exitPrice, "entry_price", t.FillPrice, "pnl", pnl)
	pnlVal := pnl
	s.publish(events.ORBTradeUpdate, events.ORBTradeUpdatePayload{
		OrderID: t.OrderID, Ticker: t.Ticker, Side: t.Side,
		Status: "closed", FillPrice: t.FillPrice, PnL: &pnlVal,
	})
	return true
}

// legPriceTol is the relative tolerance for matching a working order's price to a
// trade's recorded TP/SL leg. The legs are placed at exactly those prices, so this
// only absorbs broker-side rounding while staying tight enough to exclude unrelated
// orders (5 bps ≈ 0.6 pip on EUR/USD).
const legPriceTol = 0.0005

// cancelBracketLegs cancels a filled trade's redundant TP/SL bracket legs after its
// position has been flattened, so a leftover leg can't fire against the flat position
// and open a new one. No-op when the executor doesn't expose a working-orders lookup
// (e.g. PaperExecutor, whose bracket is flattened to a single entry with no live legs).
//
// GetWorkingOrders is account-wide, so cancellation is narrowed three ways to avoid
// touching unrelated orders: same ticker, NOT one of the current session's known entry
// orders (protectedOrderIDs), and a price that matches this trade's recorded TP or SL
// (legPriceMatches — a positive ownership signal). A no-price order cannot be matched
// and is left alone here; the orphan-leg reconciler surfaces it to the operator rather
// than cancelling something we can't prove we own.
func (s *ORBScheduler) cancelBracketLegs(ctx context.Context, t store.ORBTrade) {
	lister, ok := s.executor.(execution.WorkingOrdersLister)
	if !ok {
		return
	}
	orders, err := lister.GetWorkingOrders(ctx)
	if err != nil {
		slog.Warn("orb: time exit: list working orders failed", "ticker", t.Ticker, "err", err)
		return
	}
	protected := s.protectedOrderIDs()
	for _, o := range orders {
		if o.Ticker != t.Ticker {
			continue
		}
		if _, skip := protected[o.OrderID]; skip {
			continue
		}
		if !legPriceMatches(o, t) {
			continue
		}
		if err := s.executor.CancelOrder(ctx, o.OrderID); err != nil {
			slog.Warn("orb: time exit: cancel bracket leg failed",
				"ticker", t.Ticker, "order_id", o.OrderID, "err", err)
		}
	}
}

// legPriceMatches reports whether a working order can be PROVEN to be one of a
// trade's recorded bracket legs, by matching the order's limit/stop price to the
// trade's take-profit or stop-loss within legPriceTol. A no-price order returns false:
// we never cancel an order we can't prove we own (it could be a manual order or another
// strategy's). An unprovable stale leg is instead surfaced to the operator by the
// orphan-leg reconciler rather than silently cancelled or silently left live.
func legPriceMatches(o execution.WorkingOrder, t store.ORBTrade) bool {
	if !orderHasPrice(o) {
		return false
	}
	return orderMatchesPrices(o, []float64{t.TakeProfit, t.StopLoss})
}

// orderHasPrice reports whether the gateway exposed any usable price for the order.
func orderHasPrice(o execution.WorkingOrder) bool {
	return o.Price > 0 || o.StopPrice > 0
}

// orderMatchesPrices reports whether either of the order's prices is within
// legPriceTol of any positive target price.
func orderMatchesPrices(o execution.WorkingOrder, targets []float64) bool {
	for _, p := range []float64{o.Price, o.StopPrice} {
		if p <= 0 {
			continue
		}
		for _, target := range targets {
			if target > 0 && math.Abs(p-target)/target < legPriceTol {
				return true
			}
		}
	}
	return false
}

// isForexInstrument reports whether a ticker is one of this scheduler's configured
// forex/CFD instruments. Used to scope time exits to trades this scheduler owns,
// since the orb_trades table is shared with the equities scheduler in combined modes.
func (s *ORBScheduler) isForexInstrument(ticker string) bool {
	if s.forexCfg == nil {
		return false
	}
	for _, inst := range s.forexCfg.Instruments {
		if inst.Symbol == ticker {
			return true
		}
	}
	return false
}

// protectedOrderIDs returns the current session's known entry order IDs. These must
// never be cancelled by a time-exit flatten of a different (prior-session) trade —
// they are the live entry stops of an in-progress session, not stale bracket legs.
func (s *ORBScheduler) protectedOrderIDs() map[string]struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]struct{})
	add := func(m map[string][2]string) {
		for _, ids := range m {
			for _, id := range ids {
				if id != "" {
					out[id] = struct{}{}
				}
			}
		}
	}
	add(s.orderIDs)         // runner / primary entries
	add(s.scaleOutEntryIDs) // split-bracket partial entries (empty unless partial-TP configured)
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
	// Options-ORB trades are excluded from GetOpenFilledORBTrades (they have their
	// own reader + exit path), but the anti-stacking entry guard MUST still see them
	// — otherwise a new option entry could stack on an underlying whose prior option
	// is still open (exit pending/unconfirmed or a missed EOD flatten). t.Ticker is
	// the underlying for options, which is what the guard keys on.
	if optTrades, oerr := store.GetOpenFilledOptionTrades(ctx, s.db); oerr == nil {
		for _, t := range optTrades {
			out[t.Ticker] = struct{}{}
		}
	} else {
		slog.Warn("orb: could not check open filled option trades before placing brackets", "err", oerr)
	}
	return out
}

// reconcileOrphanedLegs cancels stale bracket legs left live after a time exit, with
// cross-tick retry. The time-exit flatten cancels the legs inline, but that cancel is
// best-effort: if it failed (an IBKR/auth hiccup), a TP/SL leg can remain live against
// a now-flat position and later open an unintended one. Because the trade row is closed
// by then, the time-exit sweep won't revisit it — so cleanup is decoupled from the trade
// row and driven off the working-order list here, retrying every tick until the leg is
// gone (the doc's "no child TP/SL orders remain live after a time exit" goal).
//
// Safety: it only cancels an order it can PROVE is a stale ORB leg — on a configured
// forex ticker, not a protected current-session entry, on a ticker with NO open filled
// trade (so no live trade could legitimately own a bracket leg), and at a price matching
// a recently time-exited trade's TP/SL. A forex order on such an idle ticker that exposes
// no price can't be proven either way, so it is surfaced to the operator (once) rather
// than cancelled — never cancelling something we can't prove we own.
func (s *ORBScheduler) reconcileOrphanedLegs(ctx context.Context) {
	if s.forexCfg == nil || !s.forexCfg.Enabled || s.db == nil {
		return
	}
	lister, ok := s.executor.(execution.WorkingOrdersLister)
	if !ok {
		return
	}
	if time.Since(s.lastOrphanLegCheckAt) < orphanLegCheckInterval {
		return
	}
	s.lastOrphanLegCheckAt = s.now()

	orders, err := lister.GetWorkingOrders(ctx)
	if err != nil {
		slog.Warn("orb: orphan leg: list working orders failed", "err", err)
		return
	}
	if len(orders) == 0 {
		return
	}

	filled, err := store.GetOpenFilledORBTrades(ctx, s.db)
	if err != nil {
		slog.Warn("orb: orphan leg: load filled trades failed", "err", err)
		return
	}
	openTickers := make(map[string]struct{}, len(filled))
	for _, t := range filled {
		openTickers[t.Ticker] = struct{}{}
	}

	recent, err := store.GetRecentORBTrades(ctx, s.db, 500)
	if err != nil {
		slog.Warn("orb: orphan leg: load recent trades failed", "err", err)
		return
	}
	cutoff := s.now().Add(-orphanLegLookback)
	timeExitPrices := make(map[string][]float64)
	for _, t := range recent {
		if t.Status != "closed" || t.ExitType != "time_exit" || !s.isForexInstrument(t.Ticker) {
			continue
		}
		if t.ClosedAt == nil || t.ClosedAt.Before(cutoff) {
			continue
		}
		if t.TakeProfit > 0 {
			timeExitPrices[t.Ticker] = append(timeExitPrices[t.Ticker], t.TakeProfit)
		}
		if t.StopLoss > 0 {
			timeExitPrices[t.Ticker] = append(timeExitPrices[t.Ticker], t.StopLoss)
		}
	}

	cancel, ambiguous := s.classifyOrphanLegs(orders, openTickers, timeExitPrices, s.protectedOrderIDs())
	for _, id := range cancel {
		if err := s.executor.CancelOrder(ctx, id); err != nil {
			slog.Warn("orb: orphan leg: cancel failed — will retry next sweep", "order_id", id, "err", err)
		} else {
			slog.Info("orb: orphan leg: cancelled stale time-exit bracket leg", "order_id", id)
		}
	}
	for _, o := range ambiguous {
		if _, alerted := s.alertedOrphanLegs[o.OrderID]; alerted {
			continue
		}
		s.alertedOrphanLegs[o.OrderID] = struct{}{}
		slog.Warn("orb: orphan leg: working order on idle forex ticker exposes no price — cannot prove ownership; left live",
			"ticker", o.Ticker, "order_id", o.OrderID)
		s.notify(ctx, fmt.Sprintf(
			"ORB: working order %s on %s could not be matched to a bracket leg (no price exposed) and was left live. "+
				"Please verify it is not an orphaned stop.", o.OrderID, o.Ticker))
	}
}

// classifyOrphanLegs partitions account-wide working orders into the stale ORB bracket
// legs that can be safely cancelled and the ambiguous orders that must be surfaced to an
// operator instead. See reconcileOrphanedLegs for the ownership rules. Pure (no I/O) so
// the decision logic is unit-tested directly.
func (s *ORBScheduler) classifyOrphanLegs(
	orders []execution.WorkingOrder,
	openTickers map[string]struct{},
	timeExitPrices map[string][]float64,
	protected map[string]struct{},
) (cancel []string, ambiguous []execution.WorkingOrder) {
	for _, o := range orders {
		if !s.isForexInstrument(o.Ticker) {
			continue
		}
		if _, p := protected[o.OrderID]; p {
			continue
		}
		if _, open := openTickers[o.Ticker]; open {
			continue // a live filled trade may legitimately own a bracket leg here
		}
		// Idle ticker: no open filled trade, so no legitimate live bracket leg exists.
		switch {
		case orderMatchesPrices(o, timeExitPrices[o.Ticker]):
			cancel = append(cancel, o.OrderID) // proven stale time-exit leg
		case !orderHasPrice(o):
			ambiguous = append(ambiguous, o) // can't prove ownership → alert, don't cancel
		}
	}
	return cancel, ambiguous
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
