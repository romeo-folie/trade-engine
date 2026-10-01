package engine

import (
	"context"
	"errors"
	"time"

	"github.com/romeo-folie/trade-engine/internal/execution"
	"github.com/romeo-folie/trade-engine/internal/logger"
	"github.com/romeo-folie/trade-engine/internal/store"
)

// unfilledCancelRetryDelay paces the cancel-and-confirm attempts. Package
// variable so tests can shrink it.
var unfilledCancelRetryDelay = 5 * time.Second

type stockIntentRecoverer interface {
	Recover(context.Context) error
}

// cancelTimedOutStockIntent closes the order-only durable entry window after
// its two-minute fill poll expires. There may be no trade row yet, so terminal
// broker truth is written to the intent and recovery owns the signal/order
// projection. A late fill is recovered instead of being discarded.
func (e *Engine) cancelTimedOutStockIntent(intentKey, brokerOrderID string, recoverer stockIntentRecoverer) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	var terminal execution.OrderResult
	confirmed := false
	for attempt := 0; attempt < 3 && !confirmed; attempt++ {
		if attempt > 0 {
			time.Sleep(unfilledCancelRetryDelay)
		}
		if err := e.executor.CancelOrder(ctx, brokerOrderID); err != nil {
			logger.Debug("unfilled stock intent: broker cancel attempt",
				"intent_key", intentKey, "broker_order_id", brokerOrderID,
				"attempt", attempt+1, "err", err)
		}
		status, err := e.executor.GetOrderStatus(ctx, brokerOrderID)
		if err != nil {
			if !errors.Is(err, execution.ErrOrderNotFound) {
				continue
			}
			// A successful cancel followed by 404 means the order is gone. Make
			// that terminal fact durable so recovery can retire the reservation.
			terminal = execution.OrderResult{BrokerOrderID: brokerOrderID, Status: "cancelled"}
			confirmed = true
			break
		}
		terminal = status
		if execution.IsTerminalOrderStatus(status.Status) {
			confirmed = true
			continue
		}
		if status.FilledQty > 0 {
			// A cancel request is not terminal broker truth. Project the
			// cumulative fill now so it is owned and protected, then keep
			// cancelling until the remaining BUY quantity is confirmed dead.
			if err := e.projectTimedOutStockIntentSnapshot(ctx, intentKey, brokerOrderID, status, recoverer); err != nil {
				logger.Warn("unfilled stock intent: project interim cancellation fill",
					"intent_key", intentKey, "broker_order_id", brokerOrderID, "err", err)
			}
		}
	}
	if !confirmed {
		logger.Error("unfilled stock intent: could not confirm broker order terminal",
			"intent_key", intentKey, "broker_order_id", brokerOrderID)
		return
	}

	if err := e.projectTimedOutStockIntentSnapshot(ctx, intentKey, brokerOrderID, terminal, recoverer); err != nil {
		logger.Warn("unfilled stock intent: recover terminal entry",
			"intent_key", intentKey, "broker_order_id", brokerOrderID, "err", err)
	}
}

func (e *Engine) projectTimedOutStockIntentSnapshot(
	ctx context.Context,
	intentKey, brokerOrderID string,
	snapshot execution.OrderResult,
	recoverer stockIntentRecoverer,
) error {
	intent, err := store.GetStockOrderIntentByKey(ctx, intentKey)
	if err != nil || intent == nil {
		if err == nil {
			err = errors.New("durable stock intent is missing")
		}
		return err
	}
	status := execution.NormalizeOrderStatus(snapshot.Status)
	if err := store.UpdateStockOrderIntentBrokerWithFillTime(ctx, intent.ClientOrderID, brokerOrderID,
		status, snapshot.FilledPrice, snapshot.FilledQty, snapshot.RequestID, "", false, snapshot.FilledAt); err != nil {
		return err
	}
	if err := recoverer.Recover(ctx); err != nil {
		return err
	}
	if snapshot.FilledQty > 0 {
		e.emitRecoveredEntryEffects(ctx, intentKey)
	}
	return nil
}

// cancelUnfilledEntry cleans up an entry whose broker order died or went
// permanently silent with ZERO fill, so the qty-0 trade row never occupies a
// position slot (#437 — the after-hours zero-entry rows).
//
// The ledger is only touched after the broker order is CONFIRMED terminal:
// cancel, then verify — up to three rounds. A transiently-unreachable broker
// does not mean the order died (Codex P1 on #437): a still-working order can
// fill later, and a cancelled trade row with real shares behind it recreates
// the phantom-position class of bug in the opposite direction. When
// terminality cannot be confirmed, the row is left open and logged at ERROR
// — visible and heal-able beats silently wrong. A fill discovered along the
// way is backfilled instead of cancelled.
//
// Runs from the fill-poll goroutine, so it builds its own bounded contexts.
func (e *Engine) cancelUnfilledEntry(tradeID, brokerOrderID, ticker string) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	confirmed := false
	for attempt := 0; attempt < 3 && !confirmed; attempt++ {
		if attempt > 0 {
			time.Sleep(unfilledCancelRetryDelay)
		}
		// Best-effort: an already-terminal (expired/rejected) order errors
		// here — the status check below is what decides anything.
		if err := e.executor.CancelOrder(ctx, brokerOrderID); err != nil {
			logger.Debug("unfilled entry: broker cancel attempt",
				"broker_order_id", brokerOrderID, "attempt", attempt+1, "err", err)
		}
		status, err := e.executor.GetOrderStatus(ctx, brokerOrderID)
		if err != nil {
			if errors.Is(err, execution.ErrOrderNotFound) {
				confirmed = true // purged at the broker — terminal and gone
				break
			}
			continue // transient: retry the round
		}
		if status.FilledQty > 0 {
			// A fill raced in — record it; cancelling would orphan shares.
			logger.Info("unfilled entry: late fill detected — backfilling instead of cancelling",
				"trade_id", tradeID, "ticker", ticker,
				"filled_qty", status.FilledQty, "filled_price", status.FilledPrice)
			if err := store.ApplyEntryFill(
				ctx, tradeID, brokerOrderID, status.FilledPrice, status.FilledQty,
				execution.NormalizeOrderStatus(status.Status),
			); err != nil {
				logger.Warn("unfilled entry: atomically backfill late fill",
					"trade_id", tradeID, "broker_order_id", brokerOrderID, "err", err)
			}
			return
		}
		if execution.IsTerminalOrderStatus(status.Status) {
			confirmed = true
		}
	}
	if !confirmed {
		logger.Error("unfilled entry: could not confirm the broker order terminal — trade row left open for reconciler attention",
			"trade_id", tradeID, "ticker", ticker, "broker_order_id", brokerOrderID)
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
