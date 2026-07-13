package engine

import (
	"context"
	"errors"
	"math"

	"github.com/romeo-folie/trade-engine/internal/execution"
	"github.com/romeo-folie/trade-engine/internal/logger"
	"github.com/romeo-folie/trade-engine/internal/store"
)

// DefaultExitReconcileBatch caps how many pending exit orders a single sweep
// polls at the broker, bounding API load per run. The sweep runs repeatedly,
// so a backlog larger than the cap drains across consecutive runs.
const DefaultExitReconcileBatch = 100

// exitPriceEpsilon is the threshold below which a broker fill and the
// recorded exit price are considered the same (NUMERIC round-trip noise).
const exitPriceEpsilon = 1e-6

// exitReconcileExecutor is the minimum Executor surface used by the exit-fill
// reconciler. Kept inline so tests can fake it without a full broker.
type exitReconcileExecutor interface {
	GetOrderStatus(ctx context.Context, brokerOrderID string) (execution.OrderResult, error)
}

// exitReconcileStore is the minimum store surface used by the exit-fill
// reconciler. Production wires the package-level store funcs through
// exitStoreAdapter; tests supply an in-memory implementation.
type exitReconcileStore interface {
	GetPendingExitOrders(ctx context.Context, limit int) ([]store.PendingExitOrder, error)
	// UpdateOrderFill is scoped to the order row's own id, NOT the broker
	// order id: a multi-lot SELL exit (#436) writes one order row per lot
	// sharing a single broker order id, and a blanket update keyed on the
	// broker id would stamp the broker's AGGREGATE filled qty onto every
	// lot's row.
	UpdateOrderFill(ctx context.Context, orderID string, filledPrice, filledQty float64, status string) error
	UpdateTradeExit(ctx context.Context, id string, exitPrice, pnl, pnlPct float64) error
	MarkOrderStatus(ctx context.Context, brokerOrderID, status string) error
}

// exitStoreAdapter adapts the package-level store funcs to exitReconcileStore.
type exitStoreAdapter struct{}

func (exitStoreAdapter) GetPendingExitOrders(ctx context.Context, limit int) ([]store.PendingExitOrder, error) {
	return store.GetPendingExitOrders(ctx, limit)
}
func (exitStoreAdapter) UpdateOrderFill(ctx context.Context, orderID string, filledPrice, filledQty float64, status string) error {
	return store.UpdateOrderFill(ctx, orderID, filledPrice, filledQty, status)
}
func (exitStoreAdapter) UpdateTradeExit(ctx context.Context, id string, exitPrice, pnl, pnlPct float64) error {
	return store.UpdateTradeExit(ctx, id, exitPrice, pnl, pnlPct)
}
func (exitStoreAdapter) MarkOrderStatus(ctx context.Context, brokerOrderID, status string) error {
	return store.MarkOrderStatus(ctx, brokerOrderID, status)
}

// ExitReconcileStats summarises one sweep of the exit-fill reconciler.
type ExitReconcileStats struct {
	Scanned      int // pending exit orders examined
	Corrected    int // broker fill differed from recorded exit — trade P&L rewritten
	Confirmed    int // broker fill matched recorded exit — order row backfilled only
	Terminal     int // broker reported cancelled/rejected — order marked, trade NOT touched
	StillPending int // broker still working the order (or partial fill)
	NotFound     int // broker no longer knows the order id
	Errors       int // broker/store errors — row left for the next sweep
}

// tradePnL computes P&L and P&L% for a trade closed at exitPrice, mirroring
// the sign convention used by the close paths in monitorPositions and the
// trailing-stop manager.
func tradePnL(direction string, entryPrice, quantity, exitPrice float64) (pnl, pnlPct float64) {
	pnl = (exitPrice - entryPrice) * quantity
	if direction == "short" {
		pnl = (entryPrice - exitPrice) * quantity
	}
	if entryPrice > 0 && quantity != 0 {
		pnlPct = (pnl / (entryPrice * quantity)) * 100
	}
	return pnl, pnlPct
}

// ReconcileExitOrderFills sweeps exit orders still marked 'pending' whose
// trade is already closed, asks the broker what actually happened, and makes
// the DB reflect broker truth:
//
//   - filled   → backfill filled_price/filled_qty on the order row; if the
//     confirmed fill differs from the exit_price recorded at close time,
//     rewrite the trade's exit_price/pnl/pnl_pct (closed_at is preserved).
//   - cancelled/rejected → mark the order row; ERROR log. The trade row says
//     closed but the broker never executed the exit — the position may still
//     be live at the broker (ReconcileBrokerVsDB surfaces that side).
//   - partial / still working → leave for the next sweep (filled-qty
//     accounting is tracked in #363/#369).
//   - not found at broker → leave pending, WARN. Alpaca purges terminal
//     orders from lookup only long after our sweep cadence, so this is rare.
//
// Exists because every close path records the *trigger* price when the broker
// fill is still pending at PlaceOrder-return time (Alpaca market orders fill
// async). With stale candle data that trigger price can be wildly wrong — a
// LAC trade was recorded as +$787 at 5.50 when the broker filled at 3.82 for
// −$19 (2026-07-02).
func ReconcileExitOrderFills(ctx context.Context, executor exitReconcileExecutor, db exitReconcileStore, limit int) ExitReconcileStats {
	var stats ExitReconcileStats
	if executor == nil {
		return stats
	}
	if limit <= 0 {
		limit = DefaultExitReconcileBatch
	}

	pending, err := db.GetPendingExitOrders(ctx, limit)
	if err != nil {
		logger.Warn("exit reconcile: fetch pending exit orders", "err", err)
		stats.Errors++
		return stats
	}

	for _, p := range pending {
		stats.Scanned++
		res, err := executor.GetOrderStatus(ctx, p.BrokerOrderID)
		if err != nil {
			if errors.Is(err, execution.ErrOrderNotFound) {
				stats.NotFound++
				logger.Warn("exit reconcile: order unknown at broker — cannot confirm fill",
					"ticker", p.Ticker, "trade_id", p.TradeID, "broker_order_id", p.BrokerOrderID)
			} else {
				stats.Errors++
				logger.Warn("exit reconcile: GetOrderStatus",
					"ticker", p.Ticker, "broker_order_id", p.BrokerOrderID, "err", err)
			}
			continue
		}

		switch execution.NormalizeOrderStatus(res.Status) {
		case "filled":
			if res.FilledPrice <= 0 {
				stats.Errors++
				logger.Warn("exit reconcile: broker says filled but returned no fill price",
					"ticker", p.Ticker, "broker_order_id", p.BrokerOrderID)
				continue
			}
			// Correct the trade BEFORE flipping the order row to 'filled':
			// the sweep only selects unfilled orders, so writing the order
			// first would make a failed trade update unretryable — the bad
			// exit price would stick forever. Trade-first is safe because
			// UpdateTradeExit is idempotent: if the order write then fails,
			// the next sweep redoes both.
			corrected := math.Abs(res.FilledPrice-p.ExitPrice) > exitPriceEpsilon
			if corrected {
				pnl, pnlPct := tradePnL(p.Direction, p.EntryPrice, p.Quantity, res.FilledPrice)
				if err := db.UpdateTradeExit(ctx, p.TradeID, res.FilledPrice, pnl, pnlPct); err != nil {
					stats.Errors++
					logger.Warn("exit reconcile: update trade exit", "trade_id", p.TradeID, "err", err)
					continue
				}
				logger.Info("exit reconcile: corrected trade exit to broker fill",
					"ticker", p.Ticker,
					"trade_id", p.TradeID,
					"recorded_exit", p.ExitPrice,
					"broker_fill", res.FilledPrice,
					"pnl", pnl,
				)
			}
			// Per-lot fill qty: the broker reports the AGGREGATE fill for
			// the order, which spans multiple lots on a #436 SELL exit —
			// each row records at most its own lot's quantity.
			lotQty := res.FilledQty
			if p.Quantity > 0 && lotQty > p.Quantity {
				lotQty = p.Quantity
			}
			if err := db.UpdateOrderFill(ctx, p.OrderID, res.FilledPrice, lotQty, "filled"); err != nil {
				stats.Errors++
				logger.Warn("exit reconcile: update order fill", "order_id", p.OrderID, "err", err)
				continue
			}
			if corrected {
				stats.Corrected++
			} else {
				stats.Confirmed++
			}
		case "cancelled", "rejected":
			if err := db.MarkOrderStatus(ctx, p.BrokerOrderID, execution.NormalizeOrderStatus(res.Status)); err != nil {
				stats.Errors++
				logger.Warn("exit reconcile: mark order status", "broker_order_id", p.BrokerOrderID, "err", err)
				continue
			}
			stats.Terminal++
			logger.Error("exit reconcile: closed trade's exit order never executed at broker — position may still be live",
				"ticker", p.Ticker,
				"trade_id", p.TradeID,
				"broker_order_id", p.BrokerOrderID,
				"broker_status", res.Status,
			)
		default: // pending / partial — leave for the next sweep
			stats.StillPending++
		}
	}

	if stats.Scanned > 0 {
		logger.Info("exit reconcile: sweep complete",
			"scanned", stats.Scanned,
			"corrected", stats.Corrected,
			"confirmed", stats.Confirmed,
			"terminal", stats.Terminal,
			"still_pending", stats.StillPending,
			"not_found", stats.NotFound,
			"errors", stats.Errors,
		)
	}
	return stats
}

// ReconcileExitOrderFillsWithStore is a convenience wrapper that uses the
// package-level store functions. Called from the engine's monitor loop and
// the one-shot cmd/reconcile-exits tool.
func ReconcileExitOrderFillsWithStore(ctx context.Context, executor exitReconcileExecutor, limit int) ExitReconcileStats {
	return ReconcileExitOrderFills(ctx, executor, exitStoreAdapter{}, limit)
}
