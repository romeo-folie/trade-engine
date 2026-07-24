package engine

import (
	"context"
	"fmt"

	"github.com/romeo-folie/trade-engine/internal/events"
	"github.com/romeo-folie/trade-engine/internal/execution"
	"github.com/romeo-folie/trade-engine/internal/logger"
	"github.com/romeo-folie/trade-engine/internal/store"
)

// executeSellExit is what a stock-path SELL signal actually means: close the
// ticker's open LONG position. Before #436 the SELL branch of executeSignal
// placed a cash-sized market sell and inserted a brand-new short trade row,
// leaving the long rows open forever — phantom long+short pairs that clogged
// the position budget while the real position was flat (or worse, short).
// The ownership gate (#306) already guarantees SELL only fires with an open
// long, so genuine short-selling was never a supported path.
//
// Semantics: one market sell for the FULL held long quantity (all lots), the
// ticker's working exit legs / broker stops cancelled first (they hold the
// shares — Alpaca rejects a sell of held shares), then every long lot closed
// at the confirmed fill with per-lot P&L. The exit-fill reconciler heals
// unconfirmed fills to broker truth afterwards, exactly like the software
// stop/TP exit path.
func (e *Engine) executeSellExit(ctx context.Context, sigID string, sig store.Signal) {
	blockSignal := func(reason, failedCheck string) {
		if err := store.UpdateSignalStatus(ctx, sigID, "blocked", reason); err != nil {
			logger.Warn("update signal status", "id", sigID, "err", err)
		}
		logger.Warn("sell exit blocked", "ticker", sig.Ticker, "reason", reason)
		e.bus.Publish(events.SignalBlocked, events.SignalBlockedPayload{
			Ticker: sig.Ticker, Action: sig.Action, Confidence: sig.Confidence,
			BlockReason: reason, FailedCheck: failedCheck,
		})
	}

	if minConf := e.cfg.Risk.ExitConfMin; !sellExitConfidenceAllowed(sig.Confidence, minConf) {
		blockSignal(
			fmt.Sprintf("SELL confidence %.3f is below exit minimum %.3f; trailing stop remains in control", sig.Confidence, minConf),
			"exit_confidence",
		)
		return
	}

	// The lots this SELL exits. DB quantity is broker-truth maintained by
	// the fill polls and reconcilers; if it drifts, checkExitResidue flags
	// the difference after the exit settles.
	openTrades, err := store.GetOpenTrades(ctx)
	if err != nil {
		blockSignal("unable to verify open positions (DB error)", "position lookup")
		return
	}
	var longs []store.Trade
	var totalQty float64
	for _, t := range openTrades {
		if t.Ticker == sig.Ticker && t.Direction == "long" && t.Quantity > 0 {
			longs = append(longs, t)
			totalQty += t.Quantity
		}
	}
	if len(longs) == 0 || totalQty <= 0 {
		// The ownership gate should have blocked this earlier; re-checked
		// here because state can change between generation and execution.
		blockSignal(fmt.Sprintf("no open long position in %s — cannot sell", sig.Ticker), "position_ownership")
		return
	}

	// Working exit legs and broker-resident stops hold the long's shares;
	// they must be confirmed gone before the sell or the broker rejects it
	// (and a still-live leg would race the fill).
	if !e.cancelBracketLegsForTicker(ctx, sig.Ticker) {
		blockSignal("working exit legs could not be confirmed cancelled — sell would race a live leg", "bracket_leg_cancel")
		return
	}

	result, err := e.executor.PlaceOrder(ctx, execution.OrderRequest{
		Ticker:     sig.Ticker,
		Side:       execution.OrderSideSell,
		Type:       execution.OrderTypeMarket,
		Quantity:   totalQty,
		LimitPrice: sig.SignalPrice, // paper executor fills at this price
	})
	if err != nil {
		// Legs are already cancelled: the position is UNPROTECTED until the
		// software monitor resumes on the next tick (it will — no working
		// legs means app-managed monitoring applies).
		logger.Error("sell exit order failed — position temporarily unprotected until the monitor resumes",
			"ticker", sig.Ticker, "qty", totalQty, "err", err)
		blockSignal(fmt.Sprintf("sell order failed: %v", err), "order placement")
		return
	}

	if err := store.UpdateSignalStatus(ctx, sigID, "executed", ""); err != nil {
		logger.Warn("update signal status", "id", sigID, "err", err)
	}
	e.costTracker.RecordTrade()

	// Confirm the fill; fall back to the signal price when the broker
	// hasn't reported yet — the exit-fill reconciler heals the rows to
	// broker truth within its 10-minute sweep.
	exitPrice := result.FilledPrice
	confirmed := exitPrice > 0
	if !confirmed && result.BrokerOrderID != "" {
		if res, ok := e.pollExitFill(ctx, result.BrokerOrderID); ok && res.FilledPrice > 0 {
			result = res
			exitPrice = res.FilledPrice
			confirmed = true
		}
	}
	if exitPrice <= 0 {
		exitPrice = sig.SignalPrice
	}
	if exitPrice <= 0 {
		// Defensive: executeSignal validates SignalPrice before routing
		// here, so this is unreachable in the normal flow. If it ever
		// fires, leave the lots OPEN — the shares are sold at the broker,
		// which checkExitResidue and the broker-drift reconciler surface —
		// rather than closing rows at a fabricated zero price that no
		// reconciler heals in paper mode.
		logger.Error("sell exit: no usable exit price — lots left open for reconciliation",
			"ticker", sig.Ticker, "broker_order_id", result.BrokerOrderID)
		e.checkExitResidue(sig.Ticker)
		return
	}

	// Close every lot at the (single) fill, per-lot P&L. One exit order row
	// per lot shares the broker order id so the reconciler heals each.
	for _, t := range longs {
		pnl := (exitPrice - t.EntryPrice) * t.Quantity
		var pnlPct float64
		if t.EntryPrice > 0 {
			pnlPct = pnl / (t.EntryPrice * t.Quantity) * 100
		}
		if err := store.CloseTrade(ctx, t.ID, exitPrice, pnl, pnlPct); err != nil {
			logger.Warn("sell exit: close trade", "trade_id", t.ID, "err", err)
			continue
		}
		if pnl < 0 {
			e.portfolio.RecordLoss()
		}
		exitOrder := store.Order{
			TradeID:       t.ID,
			BrokerOrderID: result.BrokerOrderID,
			Ticker:        sig.Ticker,
			Side:          string(execution.OrderSideSell),
			OrderType:     string(execution.OrderTypeMarket),
			Quantity:      t.Quantity,
			Status:        execution.NormalizeOrderStatus(result.Status),
		}
		if confirmed {
			exitOrder.FilledPrice = exitPrice
			exitOrder.FilledQty = t.Quantity
		}
		if _, err := store.InsertOrder(ctx, exitOrder); err != nil {
			logger.Warn("sell exit: insert exit order", "trade_id", t.ID, "err", err)
		}
		logger.Info("trade closed by sell signal",
			"trade_id", t.ID,
			"ticker", sig.Ticker,
			"exit_price", exitPrice,
			"qty", t.Quantity,
			"pnl", fmt.Sprintf("%.2f", pnl),
			"fill_confirmed", confirmed,
		)
		e.bus.Publish(events.TradeClosed, events.TradeClosedPayload{
			TradeID: t.ID, Ticker: sig.Ticker, Direction: t.Direction,
			EntryPrice: t.EntryPrice, ExitPrice: exitPrice,
			PnL: pnl, PnLPct: pnlPct, Reason: "sell signal",
		})
	}
	e.checkExitResidue(sig.Ticker)
}

func sellExitConfidenceAllowed(confidence, minimum float64) bool {
	return minimum <= 0 || confidence >= minimum
}
