package engine

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/romeo-folie/trade-engine/internal/events"
	"github.com/romeo-folie/trade-engine/internal/execution"
	"github.com/romeo-folie/trade-engine/internal/logger"
	"github.com/romeo-folie/trade-engine/internal/store"
)

// Broker-resident bracket exits (#435). The execution study measured the
// stock path's hourly candle-close stop checks costing ~1.5%/trade: 49 of 53
// stop breaches happened intraday where a broker-resident stop would have
// filled at the level, and 11 trades touched their take-profit intrabar yet
// were recorded as losses. With risk.bracket_exits_enabled, BUY entries are
// submitted as Alpaca bracket orders (GTC, whole shares) whose TP/SL legs
// live at the broker; monitorPositions then only reconciles leg fills into
// the trades table instead of running its own price-compare exits.
//
// Scope note: OCO-linked bracket CHILDREN (parent_order_id set, migration 044)
// are always broker-managed. A standalone trailing stop is broker-managed only
// when the trade has no software take-profit; otherwise the software monitor
// must retain ownership of that TP (Codex P2 on #435). The no-TP case covers
// uncapped ATR/EMA trades and prevents the monitor from double-selling after
// their broker-resident hard stop fills.
//
// Degradation is graceful in both directions: a bracket entry whose broker
// returns no child legs (e.g. the no-API-key PaperExecutor) simply stays
// app-managed, and a bracket trade whose legs disappear (operator cancel at
// the broker) reverts to app-managed monitoring on the next sweep.

// legState pairs a leg row with its live broker status for close bookkeeping.
type legState struct {
	leg store.Order
	res execution.OrderResult
	ok  bool // res is populated (status call succeeded)
}

// bracketChildren returns only the OCO-linked bracket legs from a working
// exit-leg set.
func bracketChildren(legs []store.Order) []store.Order {
	out := legs[:0:0]
	for _, l := range legs {
		if l.ParentOrderID != "" {
			out = append(out, l)
		}
	}
	return out
}

// brokerManagedExitLegs returns the broker orders that own all exits for this
// trade. Bracket children always qualify. A standalone stop qualifies only
// when no software take-profit remains; in that case there is no app-managed
// winning exit for monitorPositions to preserve.
func brokerManagedExitLegs(t store.Trade, legs []store.Order) []store.Order {
	if children := bracketChildren(legs); len(children) > 0 {
		return children
	}
	if t.TakeProfit > 0 {
		return nil
	}
	out := legs[:0:0]
	for _, leg := range legs {
		if leg.ParentOrderID == "" && leg.IsStopLoss {
			out = append(out, leg)
		}
	}
	return out
}

// reconcileBracketTrade books broker-side exit fills into the trades table and
// reports whether the trade's exits are broker-resident this tick. A true
// result means monitorPositions must not run its own price-compare exit: the
// position is protected by working legs, was closed here, or has unknown broker
// state. A false result means app-managed monitoring applies. Standalone stops
// on trades with a software TP deliberately do not count.
func (e *Engine) reconcileBracketTrade(ctx context.Context, t store.Trade) bool {
	all, err := store.OpenExitLegsForTrade(ctx, t.ID)
	if err != nil {
		// Unknown state: legs may be live at the broker. A software exit on
		// top of a live leg double-sells, so skip this tick and retry.
		logger.Warn("bracket exits: leg lookup failed — skipping software exit this tick",
			"trade_id", t.ID, "ticker", t.Ticker, "err", err)
		return true
	}
	legs := brokerManagedExitLegs(t, all)
	if len(legs) == 0 {
		return false
	}

	states := make([]legState, 0, len(legs))
	working := 0
	var filledIdx = -1
	for _, leg := range legs {
		res, err := e.executor.GetOrderStatus(ctx, leg.BrokerOrderID)
		if err != nil {
			if errors.Is(err, execution.ErrOrderNotFound) {
				// Purged from the broker's lookup — terminal and gone.
				if uErr := store.UpdateOrderFill(ctx, leg.ID, 0, 0, "cancelled"); uErr != nil {
					logger.Warn("bracket exits: mark purged leg", "order_id", leg.ID, "err", uErr)
				}
				states = append(states, legState{leg: leg})
				continue
			}
			// Transient broker error: assume the leg is still working.
			logger.Warn("bracket exits: leg status check failed",
				"trade_id", t.ID, "broker_order_id", leg.BrokerOrderID, "err", err)
			states = append(states, legState{leg: leg})
			working++
			continue
		}
		states = append(states, legState{leg: leg, res: res, ok: true})
		switch execution.NormalizeOrderStatus(res.Status) {
		case "filled":
			filledIdx = len(states) - 1
		case "cancelled", "rejected":
			// Persist any partial fill the leg accumulated before its OCO
			// sibling terminated it — those shares were genuinely sold and
			// belong in the exit accounting below.
			if uErr := store.UpdateOrderFill(ctx, leg.ID, res.FilledPrice, res.FilledQty, "cancelled"); uErr != nil {
				logger.Warn("bracket exits: mark cancelled leg", "order_id", leg.ID, "err", uErr)
			}
		default: // pending | partial — still working at the broker
			working++
		}
	}

	if filledIdx >= 0 {
		e.closeBracketTrade(ctx, t, states, filledIdx)
		return true
	}
	if working == 0 {
		// All legs are terminal without a FULL fill while the trade is
		// still open (operator cancelled at the broker, or rejection). Any
		// partial fills already SOLD shares — account for them before
		// handing back to app-managed monitoring, or the software exit
		// would later market-sell the stale full quantity (over-sell or
		// broker-rejected order) and compute pnl on shares no longer held.
		var partialQty, partialProceeds float64
		for _, s := range states {
			if s.ok && s.res.FilledQty > 0 {
				partialQty += s.res.FilledQty
				partialProceeds += s.res.FilledQty * fillPriceOf(s)
			}
		}
		if partialQty > 0 && partialQty >= t.Quantity-1e-9 {
			// The partials add up to the whole position — that IS the exit.
			e.bookBracketClose(ctx, t, partialProceeds/partialQty, partialQty,
				"bracket legs partially filled to completion")
			return true
		}
		if partialQty > 0 {
			remaining := t.Quantity - partialQty
			if uErr := store.UpdateTradeEntryPrice(ctx, t.ID, t.EntryPrice, remaining); uErr != nil {
				logger.Warn("bracket exits: reduce tracked quantity after partial leg exit failed — retrying next sweep",
					"trade_id", t.ID, "err", uErr)
				return true // don't hand an over-sized quantity to the software exit
			}
			// Known limitation shared with the wider partial-fill lineage
			// (#369): the partial slice's realized pnl is carried on the
			// leg order rows, not the trade row — the trade's eventual
			// close books pnl over the remaining shares only.
			logger.Warn("bracket exits: legs died after a partial exit — tracked quantity reduced",
				"trade_id", t.ID, "ticker", t.Ticker,
				"sold_qty", partialQty, "remaining_qty", remaining)
		}
		// Position (or its remainder) is now UNPROTECTED — hand it back to
		// app-managed monitoring, which resumes on this very sweep.
		logger.Warn("bracket exits: all legs gone with trade still open — reverting to app-managed exits",
			"trade_id", t.ID, "ticker", t.Ticker)
		return false
	}
	return true
}

// fillPriceOf resolves a leg's fill price, falling back to the leg's own
// level when the broker reported a fill without a price yet (the 10-min
// exit-fill reconciler heals rows to broker truth afterwards).
func fillPriceOf(s legState) float64 {
	if s.ok && s.res.FilledPrice > 0 {
		return s.res.FilledPrice
	}
	if s.leg.IsTakeProfit {
		return s.leg.LimitPrice
	}
	return s.leg.StopPrice
}

// closeBracketTrade books the exit when a bracket leg fills. The exit price
// and quantity are the fill-weighted aggregate across ALL legs that sold
// shares — a take-profit that partially filled before the ratcheted stop
// took out the remainder contributes its real proceeds instead of being
// discarded as a zero-fill cancel (Codex P2 on #435).
func (e *Engine) closeBracketTrade(ctx context.Context, t store.Trade, states []legState, filledIdx int) {
	filled := states[filledIdx]

	var soldQty, proceeds float64
	for i, s := range states {
		qty := 0.0
		switch {
		case i == filledIdx:
			qty = s.res.FilledQty
			if qty == 0 {
				qty = t.Quantity
			}
		case s.ok && s.res.FilledQty > 0:
			qty = s.res.FilledQty // partial fill before the OCO cancel
		}
		if qty > 0 {
			soldQty += qty
			proceeds += qty * fillPriceOf(s)
		}
	}
	if soldQty <= 0 { // defensive; filled leg always contributes above
		soldQty = t.Quantity
		proceeds = soldQty * fillPriceOf(filled)
	}
	exitPrice := proceeds / soldQty

	exitReason := "stop-loss hit"
	if filled.leg.IsTakeProfit {
		exitReason = "take-profit hit"
	}

	if !e.bookBracketClose(ctx, t, exitPrice, soldQty, exitReason) {
		return // leg row stays pending → retried next sweep
	}
	fq := filled.res.FilledQty
	if fq == 0 {
		fq = t.Quantity
	}
	if err := store.UpdateOrderFill(ctx, filled.leg.ID, fillPriceOf(filled), fq, "filled"); err != nil {
		logger.Warn("bracket exits: persist leg fill", "order_id", filled.leg.ID, "err", err)
	}
	for i, s := range states {
		if i == filledIdx {
			continue
		}
		// Persist the sibling's real partial fill (if any) with its cancel.
		var sq, sp float64
		if s.ok {
			sq, sp = s.res.FilledQty, s.res.FilledPrice
		}
		if err := store.UpdateOrderFill(ctx, s.leg.ID, sp, sq, "cancelled"); err != nil {
			logger.Warn("bracket exits: mark OCO sibling cancelled", "order_id", s.leg.ID, "err", err)
		}
	}
}

// bookBracketClose writes the trade close, records the loss cooldown, and
// emits the same observability as the software exit path. Returns false when
// the close could not be persisted (caller retries next sweep).
func (e *Engine) bookBracketClose(ctx context.Context, t store.Trade, exitPrice, soldQty float64, exitReason string) bool {
	pnl := (exitPrice - t.EntryPrice) * soldQty
	if t.Direction == "short" { // defensive: brackets are only placed on BUYs
		pnl = -pnl
	}
	var pnlPct float64
	if t.EntryPrice > 0 && soldQty > 0 {
		pnlPct = pnl / (t.EntryPrice * soldQty) * 100
	}

	if err := store.CloseTrade(ctx, t.ID, exitPrice, pnl, pnlPct); err != nil {
		logger.Warn("bracket exits: close trade", "trade_id", t.ID, "err", err)
		return false
	}
	if pnl < 0 {
		// Same cooldown bookkeeping as the software exit path — without it,
		// risk.cooldown_after_loss never fires on bracket stop-outs.
		e.portfolio.RecordLoss()
	}

	logger.Info("trade closed by broker bracket leg",
		"trade_id", t.ID,
		"ticker", t.Ticker,
		"reason", exitReason,
		"exit_price", exitPrice,
		"sold_qty", soldQty,
		"pnl", fmt.Sprintf("%.2f", pnl),
		"pnl_pct", fmt.Sprintf("%.2f%%", pnlPct),
	)
	e.bus.Publish(events.TradeClosed, events.TradeClosedPayload{
		TradeID: t.ID, Ticker: t.Ticker, Direction: t.Direction,
		EntryPrice: t.EntryPrice, ExitPrice: exitPrice,
		PnL: pnl, PnLPct: pnlPct, Reason: exitReason,
	})
	e.checkExitResidue(t.Ticker)
	return true
}

// cancelBracketLegsForTicker cancels every working exit leg across the
// ticker's open trades and reports whether ALL of them are confirmed gone.
// Called before a SELL order: the legs hold the long's shares at the broker,
// so any other sell is rejected for insufficient free quantity until they
// are cancelled. Callers must NOT submit the sell when this returns false —
// a still-live leg would race the sell (Codex P2 on #435); the signal is
// blocked and the next analysis cycle retries with reconciled state.
//
// Leg rows are marked cancelled only after the broker confirms the cancel —
// an optimistic mark on a failed cancel would make the monitor treat the
// trade as app-managed while a live leg still works the position.
//
// Called by executeSellExit (#436) before its market sell: the SELL closes
// every open long lot, so no exit leg may outlive the position or hold its
// shares when the sell hits the broker.
func (e *Engine) cancelBracketLegsForTicker(ctx context.Context, ticker string) bool {
	openTrades, err := store.GetOpenTrades(ctx)
	if err != nil {
		logger.Warn("bracket exits: open-trades lookup for leg cancel failed", "ticker", ticker, "err", err)
		return false
	}
	return e.cancelExitLegsForTrades(ctx, openTrades, ticker)
}

func (e *Engine) cancelExitLegsForTrades(ctx context.Context, trades []store.Trade, ticker string) bool {
	allClear := true
	for _, t := range trades {
		if t.Ticker != ticker {
			continue
		}
		legs, err := store.OpenExitLegsForTrade(ctx, t.ID)
		if err != nil {
			logger.Warn("bracket exits: leg lookup for cancel failed", "trade_id", t.ID, "err", err)
			allClear = false
			continue
		}
		for _, leg := range legs {
			if err := e.executor.CancelOrder(ctx, leg.BrokerOrderID); err != nil {
				logger.Warn("bracket exits: cancel leg before sell failed",
					"trade_id", t.ID, "broker_order_id", leg.BrokerOrderID, "err", err)
				allClear = false
				continue
			}
			if err := store.UpdateOrderFill(ctx, leg.ID, 0, 0, "cancelled"); err != nil {
				logger.Warn("bracket exits: mark cancelled leg", "order_id", leg.ID, "err", err)
			}
		}
	}
	return allClear
}

// cancelClaimedExitLegs confirms every cancellation and returns the actual
// remaining aggregate quantity after any cancellation-race partial fills.
func (e *Engine) cancelClaimedExitLegs(
	ctx context.Context,
	trades []store.Trade,
	ticker string,
) (float64, bool, bool) {
	var totalRemaining float64
	for _, t := range trades {
		if t.Ticker != ticker {
			continue
		}
		remaining := t.Quantity
		legs, err := store.OpenExitLegsForTrade(ctx, t.ID)
		if err != nil {
			return 0, false, true
		}
		for _, leg := range legs {
			cancelErr := e.executor.CancelOrder(ctx, leg.BrokerOrderID)
			res, statusErr := e.executor.GetOrderStatus(ctx, leg.BrokerOrderID)
			if statusErr != nil {
				if errors.Is(statusErr, execution.ErrOrderNotFound) {
					continue
				}
				logger.Warn("analyst exit: cannot confirm leg cancellation",
					"broker_order_id", leg.BrokerOrderID,
					"cancel_err", cancelErr, "status_err", statusErr)
				return 0, false, true
			}
			switch execution.NormalizeOrderStatus(res.Status) {
			case "cancelled", "rejected":
				if res.FilledQty > 0 {
					if err := store.UpdateOrderFill(
						ctx, leg.ID, res.FilledPrice, res.FilledQty,
						execution.NormalizeOrderStatus(res.Status),
					); err != nil {
						return 0, false, false
					}
					remaining = math.Max(0, remaining-res.FilledQty)
				}
			case "filled":
				// The exit reconciler owns this claimed trade now. Releasing
				// it would permit a duplicate analyst/software sell.
				return 0, false, false
			default:
				return 0, false, true
			}
		}
		if remaining < t.Quantity {
			if err := store.UpdateExitingTradeQuantity(ctx, t.ID, remaining); err != nil {
				return 0, false, false
			}
		}
		totalRemaining += remaining
	}
	return totalRemaining, totalRemaining > 0, true
}
