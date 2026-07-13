package engine

import (
	"context"
	"errors"
	"fmt"

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
// Degradation is graceful in both directions: a bracket entry whose broker
// returns no child legs (e.g. the no-API-key PaperExecutor) simply stays
// app-managed, and a bracket trade whose legs disappear (operator cancel at
// the broker) reverts to app-managed monitoring on the next sweep.

// reconcileBracketTrade books broker-side leg fills into the trades table
// and reports whether the trade's exits are broker-resident this tick.
// true  → monitorPositions must NOT run its own price-compare exit (the
//         position is either still protected by working legs, was just
//         closed here, or its state is unknown — never risk a double-sell).
// false → the trade has no working legs; app-managed monitoring applies.
func (e *Engine) reconcileBracketTrade(ctx context.Context, t store.Trade) bool {
	legs, err := store.OpenExitLegsForTrade(ctx, t.ID)
	if err != nil {
		// Unknown state: legs may be live at the broker. A software exit on
		// top of a live leg double-sells, so skip this tick and retry.
		logger.Warn("bracket exits: leg lookup failed — skipping software exit this tick",
			"trade_id", t.ID, "ticker", t.Ticker, "err", err)
		return true
	}
	if len(legs) == 0 {
		return false
	}

	working := 0
	for _, leg := range legs {
		res, err := e.executor.GetOrderStatus(ctx, leg.BrokerOrderID)
		if err != nil {
			if errors.Is(err, execution.ErrOrderNotFound) {
				// Purged from the broker's lookup — terminal and gone.
				if uErr := store.UpdateOrderFill(ctx, leg.ID, 0, 0, "cancelled"); uErr != nil {
					logger.Warn("bracket exits: mark purged leg", "order_id", leg.ID, "err", uErr)
				}
				continue
			}
			// Transient broker error: assume the leg is still working.
			logger.Warn("bracket exits: leg status check failed",
				"trade_id", t.ID, "broker_order_id", leg.BrokerOrderID, "err", err)
			working++
			continue
		}
		switch execution.NormalizeOrderStatus(res.Status) {
		case "filled":
			e.closeBracketTrade(ctx, t, leg, res, legs)
			return true
		case "cancelled", "rejected":
			if uErr := store.UpdateOrderFill(ctx, leg.ID, res.FilledPrice, res.FilledQty, "cancelled"); uErr != nil {
				logger.Warn("bracket exits: mark cancelled leg", "order_id", leg.ID, "err", uErr)
			}
		default: // pending | partial — still working at the broker
			working++
		}
	}
	if working == 0 {
		// Both legs are terminal without a fill while the trade is still
		// open (operator cancelled at the broker, or rejection). The
		// position is now UNPROTECTED — hand it back to app-managed
		// monitoring, which resumes on this very sweep.
		logger.Warn("bracket exits: all legs gone with trade still open — reverting to app-managed exits",
			"trade_id", t.ID, "ticker", t.Ticker)
		return false
	}
	return true
}

// closeBracketTrade books a filled exit leg: closes the trade at the leg's
// real fill, persists the fill on the leg row, and records the sibling legs
// as cancelled (the broker's OCO does the actual cancelling).
func (e *Engine) closeBracketTrade(ctx context.Context, t store.Trade, filled store.Order, res execution.OrderResult, legs []store.Order) {
	exitPrice := res.FilledPrice
	if exitPrice <= 0 {
		// Extremely rare (filled with no price yet): fall back to the leg's
		// own level; the 10-min exit-fill reconciler heals it to broker truth.
		exitPrice = filled.StopPrice
		if filled.IsTakeProfit {
			exitPrice = filled.LimitPrice
		}
	}
	exitReason := "stop-loss hit"
	if filled.IsTakeProfit {
		exitReason = "take-profit hit"
	}

	pnl := (exitPrice - t.EntryPrice) * t.Quantity
	if t.Direction == "short" { // defensive: brackets are only placed on BUYs
		pnl = -pnl
	}
	var pnlPct float64
	if t.EntryPrice > 0 {
		pnlPct = pnl / (t.EntryPrice * t.Quantity) * 100
	}

	if err := store.CloseTrade(ctx, t.ID, exitPrice, pnl, pnlPct); err != nil {
		logger.Warn("bracket exits: close trade", "trade_id", t.ID, "err", err)
		return // leg row stays pending → retried next sweep
	}
	filledQty := res.FilledQty
	if filledQty == 0 {
		filledQty = t.Quantity
	}
	if err := store.UpdateOrderFill(ctx, filled.ID, exitPrice, filledQty, "filled"); err != nil {
		logger.Warn("bracket exits: persist leg fill", "order_id", filled.ID, "err", err)
	}
	for _, sib := range legs {
		if sib.ID == filled.ID {
			continue
		}
		if err := store.UpdateOrderFill(ctx, sib.ID, 0, 0, "cancelled"); err != nil {
			logger.Warn("bracket exits: mark OCO sibling cancelled", "order_id", sib.ID, "err", err)
		}
	}

	logger.Info("trade closed by broker bracket leg",
		"trade_id", t.ID,
		"ticker", t.Ticker,
		"reason", exitReason,
		"exit_price", exitPrice,
		"pnl", fmt.Sprintf("%.2f", pnl),
		"pnl_pct", fmt.Sprintf("%.2f%%", pnlPct),
	)
	e.bus.Publish(events.TradeClosed, events.TradeClosedPayload{
		TradeID: t.ID, Ticker: t.Ticker, Direction: t.Direction,
		EntryPrice: t.EntryPrice, ExitPrice: exitPrice,
		PnL: pnl, PnLPct: pnlPct, Reason: exitReason,
	})
	e.checkExitResidue(t.Ticker)
}

// cancelBracketLegsForTicker cancels every working exit leg across the
// ticker's open trades. Called before a SELL order: the legs hold the long's
// shares at the broker, so any other sell is rejected for insufficient free
// quantity until they are gone. Leg rows are marked cancelled only after the
// broker confirms the cancel — an optimistic mark on a failed cancel would
// make the monitor treat the trade as app-managed while a live leg still
// works the position (double-sell).
//
// NOTE: this serves the existing SELL flow, which does NOT close the long
// trade rows (it books a new short row — pre-existing bug, tracked for a
// dedicated fix). Cancelling the legs here keeps that flow from being
// broker-rejected and ensures no orphaned exit legs outlive the position.
func (e *Engine) cancelBracketLegsForTicker(ctx context.Context, ticker string) {
	openTrades, err := store.GetOpenTrades(ctx)
	if err != nil {
		logger.Warn("bracket exits: open-trades lookup for leg cancel failed", "ticker", ticker, "err", err)
		return
	}
	for _, t := range openTrades {
		if t.Ticker != ticker {
			continue
		}
		legs, err := store.OpenExitLegsForTrade(ctx, t.ID)
		if err != nil {
			logger.Warn("bracket exits: leg lookup for cancel failed", "trade_id", t.ID, "err", err)
			continue
		}
		for _, leg := range legs {
			if err := e.executor.CancelOrder(ctx, leg.BrokerOrderID); err != nil {
				logger.Warn("bracket exits: cancel leg before sell failed",
					"trade_id", t.ID, "broker_order_id", leg.BrokerOrderID, "err", err)
				continue
			}
			if err := store.UpdateOrderFill(ctx, leg.ID, 0, 0, "cancelled"); err != nil {
				logger.Warn("bracket exits: mark cancelled leg", "order_id", leg.ID, "err", err)
			}
		}
	}
}
