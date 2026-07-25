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

	claimed := make([]store.Trade, 0, len(longs))
	releaseClaims := func() {
		for _, t := range claimed {
			if err := store.ReleaseTradeExitClaim(ctx, t.ID); err != nil {
				logger.Warn("sell exit: release trade claim", "trade_id", t.ID, "err", err)
			}
		}
	}
	for _, t := range longs {
		ok, err := store.ClaimTradeExit(ctx, t.ID)
		if err != nil || !ok {
			releaseClaims()
			blockSignal("another exit worker already owns this position", "exit_ownership")
			return
		}
		claimed = append(claimed, t)
	}

	// Working exit legs and broker-resident stops hold the long's shares;
	// they must be confirmed gone before the sell or the broker rejects it
	// (and a still-live leg would race the fill).
	remainingByTrade, remainingQty, clear, releaseSafe :=
		e.cancelClaimedExitLegs(ctx, claimed, sig.Ticker)
	if !clear {
		if releaseSafe {
			releaseClaims()
		}
		blockSignal("working exit legs could not be confirmed cancelled — sell would race a live leg", "bracket_leg_cancel")
		return
	}
	totalQty = remainingQty

	result, err := e.executor.PlaceOrder(ctx, execution.OrderRequest{
		Ticker:     sig.Ticker,
		Side:       execution.OrderSideSell,
		Type:       execution.OrderTypeMarket,
		Quantity:   totalQty,
		LimitPrice: sig.SignalPrice, // paper executor fills at this price
	})
	if err != nil {
		// Broker POST/response failures are ambiguous: the order may have
		// been accepted before the transport failed. Keep every claim so
		// orderless-exit recovery can inspect working orders/positions.
		logger.Error("sell exit placement ambiguous — claims retained for broker recovery",
			"ticker", sig.Ticker, "qty", totalQty, "err", err)
		blockSignal(fmt.Sprintf("sell order failed: %v", err), "order placement")
		return
	}

	if err := store.UpdateSignalStatus(ctx, sigID, "executed", ""); err != nil {
		logger.Warn("update signal status", "id", sigID, "err", err)
	}
	e.costTracker.RecordTrade()

	if result.FilledPrice == 0 && result.BrokerOrderID != "" {
		if res, ok := e.pollExitFill(ctx, result.BrokerOrderID); ok {
			result = res
		}
	}

	status := execution.NormalizeOrderStatus(result.Status)
	fullFill := status == "filled" && result.FilledPrice > 0 &&
		result.FilledQty >= totalQty-1e-9
	persistFailed := false
	for _, t := range longs {
		lotQuantity := remainingByTrade[t.ID]
		if lotQuantity <= 0 {
			continue
		}
		exitOrder := store.Order{
			TradeID:       t.ID,
			BrokerOrderID: result.BrokerOrderID,
			Ticker:        sig.Ticker,
			Side:          string(execution.OrderSideSell),
			OrderType:     string(execution.OrderTypeMarket),
			Quantity:      lotQuantity,
			Status:        status,
		}
		if fullFill {
			exitOrder.FilledPrice = result.FilledPrice
			exitOrder.FilledQty = lotQuantity
		}
		if _, err := store.InsertOrderWithRetry(ctx, exitOrder); err != nil {
			persistFailed = true
			logger.Error("sell exit: record accepted aggregate order — lot remains exiting",
				"trade_id", t.ID, "broker_order_id", result.BrokerOrderID, "err", err)
		}
	}
	if persistFailed {
		return
	}
	if (status == "cancelled" || status == "rejected") && result.FilledQty == 0 {
		releaseClaims()
		return
	}
	if !fullFill {
		return
	}

	// A broker-confirmed aggregate fill closes every lot at the same execution
	// price. Pending and partial responses remain durably exiting.
	for _, t := range longs {
		lotQuantity := remainingByTrade[t.ID]
		if lotQuantity <= 0 {
			continue
		}
		pnl, pnlPct := tradePnL("long", t.EntryPrice, lotQuantity, result.FilledPrice)
		if err := store.CloseTrade(ctx, t.ID, result.FilledPrice, pnl, pnlPct); err != nil {
			logger.Warn("sell exit: close confirmed trade", "trade_id", t.ID, "err", err)
			continue
		}
		if pnl < 0 {
			e.portfolio.RecordLoss()
		}
		logger.Info("trade closed by sell signal",
			"trade_id", t.ID,
			"ticker", sig.Ticker,
			"exit_price", result.FilledPrice,
			"qty", lotQuantity,
			"pnl", fmt.Sprintf("%.2f", pnl),
			"fill_confirmed", true,
		)
		e.bus.Publish(events.TradeClosed, events.TradeClosedPayload{
			TradeID: t.ID, Ticker: sig.Ticker, Direction: t.Direction,
			EntryPrice: t.EntryPrice, ExitPrice: result.FilledPrice,
			PnL: pnl, PnLPct: pnlPct, Reason: "sell signal",
		})
	}
	e.checkExitResidue(sig.Ticker)
}

func sellExitConfidenceAllowed(confidence, minimum float64) bool {
	return minimum <= 0 || confidence >= minimum
}
