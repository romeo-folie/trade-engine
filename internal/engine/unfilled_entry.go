package engine

import (
	"context"
	"time"

	"github.com/romeo-folie/trade-engine/internal/logger"
	"github.com/romeo-folie/trade-engine/internal/store"
)

// cancelUnfilledEntry cleans up an entry whose broker order died or went
// permanently silent with ZERO fill: best-effort broker cancel, one final
// status check (a fill that raced in gets backfilled instead of cancelled),
// then the qty-0 trade row and its order row are marked cancelled so the
// position counter never sees a phantom slot (#437 — the after-hours
// zero-entry rows). Runs from the fill-poll goroutine, so it builds its own
// bounded contexts.
func (e *Engine) cancelUnfilledEntry(tradeID, brokerOrderID, ticker string) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// Best-effort: a terminal (expired/rejected) order errors here — fine.
	if err := e.executor.CancelOrder(ctx, brokerOrderID); err != nil {
		logger.Debug("unfilled entry: broker cancel (may already be terminal)",
			"broker_order_id", brokerOrderID, "err", err)
	}

	// Final check: if a fill raced in between the last poll and the cancel,
	// record it — cancelling the row would orphan real shares.
	if status, err := e.executor.GetOrderStatus(ctx, brokerOrderID); err == nil && status.FilledQty > 0 {
		logger.Info("unfilled entry: late fill detected — backfilling instead of cancelling",
			"trade_id", tradeID, "ticker", ticker,
			"filled_qty", status.FilledQty, "filled_price", status.FilledPrice)
		if err := store.UpdateTradeEntryPrice(ctx, tradeID, status.FilledPrice, status.FilledQty); err != nil {
			logger.Warn("unfilled entry: backfill trade", "trade_id", tradeID, "err", err)
		}
		if err := store.UpdateOrderFillPrice(ctx, brokerOrderID, status.FilledPrice, status.FilledQty); err != nil {
			logger.Warn("unfilled entry: backfill order", "broker_order_id", brokerOrderID, "err", err)
		}
		return
	}

	cancelled, err := store.CancelUnfilledTrade(ctx, tradeID)
	if err != nil {
		logger.Warn("unfilled entry: cancel trade row", "trade_id", tradeID, "err", err)
		return
	}
	if !cancelled {
		// The guarded UPDATE matched nothing: another path already resolved
		// this trade (a fill was recorded, or a reconciler closed it). That
		// path owns the terminal state — clobbering the order row to
		// "cancelled" here would overwrite e.g. a recorded fill.
		logger.Info("unfilled entry: trade already resolved elsewhere — leaving rows untouched",
			"trade_id", tradeID, "ticker", ticker)
		return
	}
	if err := store.MarkOrderStatus(ctx, brokerOrderID, "cancelled"); err != nil {
		logger.Warn("unfilled entry: mark order cancelled", "broker_order_id", brokerOrderID, "err", err)
	}
	logger.Warn("entry order died with zero fill — trade row cancelled (no position was opened)",
		"trade_id", tradeID, "ticker", ticker, "broker_order_id", brokerOrderID)
}
