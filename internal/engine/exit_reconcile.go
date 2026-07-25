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

type orderlessExitBroker interface {
	GetPositions(ctx context.Context) ([]execution.Position, error)
}

type orderlessExitStore interface {
	GetOrderlessExitingTrades(ctx context.Context, limit int) ([]store.Trade, error)
	GetOwnedPositionTrades(ctx context.Context) ([]store.Trade, error)
	InsertOrderWithRetry(ctx context.Context, o store.Order) (string, error)
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
	UpdateExitingTradeQuantity(ctx context.Context, id string, quantity float64) error
	ReleaseTradeExitClaim(ctx context.Context, id string) error
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
func (exitStoreAdapter) UpdateExitingTradeQuantity(ctx context.Context, id string, quantity float64) error {
	return store.UpdateExitingTradeQuantity(ctx, id, quantity)
}
func (exitStoreAdapter) ReleaseTradeExitClaim(ctx context.Context, id string) error {
	return store.ReleaseTradeExitClaim(ctx, id)
}
func (exitStoreAdapter) MarkOrderStatus(ctx context.Context, brokerOrderID, status string) error {
	return store.MarkOrderStatus(ctx, brokerOrderID, status)
}
func (exitStoreAdapter) GetOrderlessExitingTrades(ctx context.Context, limit int) ([]store.Trade, error) {
	return store.GetOrderlessExitingTrades(ctx, limit)
}
func (exitStoreAdapter) GetOwnedPositionTrades(ctx context.Context) ([]store.Trade, error) {
	return store.GetOwnedPositionTrades(ctx)
}
func (exitStoreAdapter) InsertOrderWithRetry(ctx context.Context, o store.Order) (string, error) {
	return store.InsertOrderWithRetry(ctx, o)
}

// ExitReconcileStats summarises one sweep of the exit-fill reconciler.
type ExitReconcileStats struct {
	Scanned      int // pending exit orders examined
	Corrected    int // broker fill differed from recorded exit — trade P&L rewritten
	Confirmed    int // broker fill matched recorded exit — order row backfilled only
	Terminal     int // broker reported cancelled/rejected — order marked, trade NOT touched
	StillPending int // broker still working the order (or partial fill)
	NotFound     int // broker no longer knows the order id
	Recovered    int // orderless exiting trades reconstructed or released
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
func ReconcileExitOrderFills(ctx context.Context, executor exitReconcileExecutor, db exitReconcileStore, limit int, recordLoss ...func()) ExitReconcileStats {
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

	remainingTerminalFill := make(map[string]float64)
	terminalFillSeen := make(map[string]bool)
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
			corrected := p.TradeStatus == "exiting" ||
				math.Abs(res.FilledPrice-p.ExitPrice) > exitPriceEpsilon
			if corrected {
				lotQuantity := p.OrderQuantity
				if lotQuantity <= 0 {
					lotQuantity = p.Quantity
				}
				pnl, pnlPct := tradePnL(p.Direction, p.EntryPrice, lotQuantity, res.FilledPrice)
				if err := db.UpdateTradeExit(ctx, p.TradeID, res.FilledPrice, pnl, pnlPct); err != nil {
					stats.Errors++
					logger.Warn("exit reconcile: update trade exit", "trade_id", p.TradeID, "err", err)
					continue
				}
				if p.TradeStatus == "exiting" && pnl < 0 &&
					len(recordLoss) > 0 && recordLoss[0] != nil {
					recordLoss[0]()
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
			lotQty := p.OrderQuantity
			if lotQty <= 0 {
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
			terminalStatus := execution.NormalizeOrderStatus(res.Status)
			if !terminalFillSeen[p.BrokerOrderID] {
				// Broker FilledQty is cumulative for the shared aggregate
				// order. Subtract allocations already stored on every lot,
				// including rows outside this LIMIT window, so a later sweep
				// never assigns the same shares twice.
				remainingTerminalFill[p.BrokerOrderID] =
					math.Max(0, res.FilledQty-p.AggregateFill)
				terminalFillSeen[p.BrokerOrderID] = true
			}
			lotCapacity := p.OrderQuantity
			if lotCapacity <= 0 {
				lotCapacity = p.Quantity
			}
			unallocatedLot := math.Max(0, lotCapacity-p.RecordedFill)
			lotFillDelta := math.Min(unallocatedLot, remainingTerminalFill[p.BrokerOrderID])
			remainingTerminalFill[p.BrokerOrderID] =
				math.Max(0, remainingTerminalFill[p.BrokerOrderID]-lotFillDelta)
			lotFill := p.RecordedFill + lotFillDelta
			if lotFill > 0 {
				if err := db.UpdateOrderFill(
					ctx, p.OrderID, res.FilledPrice, lotFill, terminalStatus,
				); err != nil {
					stats.Errors++
					logger.Warn("exit reconcile: persist terminal partial fill",
						"order_id", p.OrderID, "err", err)
					continue
				}
			} else if err := db.UpdateOrderFill(
				ctx, p.OrderID, res.FilledPrice, 0, terminalStatus,
			); err != nil {
				stats.Errors++
				logger.Warn("exit reconcile: mark lot order status", "order_id", p.OrderID, "err", err)
				continue
			}
			stats.Terminal++
			if p.TradeStatus == "exiting" {
				if lotFill > 0 {
					// Set the absolute remaining lot quantity. This is
					// idempotent across either write failing: a retry cannot
					// subtract the same cumulative broker fill twice.
					remaining := math.Max(0, lotCapacity-lotFill)
					if remaining <= 1e-9 && res.FilledPrice > 0 {
						pnl, pnlPct := tradePnL(
							p.Direction, p.EntryPrice, lotCapacity, res.FilledPrice,
						)
						if err := db.UpdateTradeExit(
							ctx, p.TradeID, res.FilledPrice, pnl, pnlPct,
						); err != nil {
							stats.Errors++
							continue
						}
						if pnl < 0 && len(recordLoss) > 0 && recordLoss[0] != nil {
							recordLoss[0]()
						}
						continue
					}
					if err := db.UpdateExitingTradeQuantity(
						ctx, p.TradeID, remaining,
					); err != nil {
						stats.Errors++
						logger.Warn("exit reconcile: reduce quantity after partial exit",
							"trade_id", p.TradeID, "err", err)
						continue
					}
				}
				if err := db.ReleaseTradeExitClaim(ctx, p.TradeID); err != nil {
					stats.Errors++
					logger.Warn("exit reconcile: release failed exit claim",
						"trade_id", p.TradeID, "err", err)
					continue
				}
			}
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
	recoverOrderlessExits(
		ctx, executor, db, pending, limit, &stats, recordLoss...,
	)
	return stats
}

func recoverOrderlessExits(
	ctx context.Context,
	executor exitReconcileExecutor,
	db exitReconcileStore,
	durable []store.PendingExitOrder,
	limit int,
	stats *ExitReconcileStats,
	recordLoss ...func(),
) {
	broker, brokerOK := executor.(orderlessExitBroker)
	recoveryDB, storeOK := db.(orderlessExitStore)
	if !brokerOK || !storeOK {
		return
	}
	orderless, err := recoveryDB.GetOrderlessExitingTrades(ctx, limit)
	if err != nil {
		stats.Errors++
		logger.Warn("exit reconcile: fetch orderless exiting trades", "err", err)
		return
	}
	if len(orderless) == 0 {
		return
	}

	// First reconstruct still-working broker orders. This is the safest and
	// most precise recovery: the normal reconciler takes over next sweep.
	var working []execution.WorkingOrder
	if lister, ok := executor.(execution.WorkingOrdersLister); ok {
		working, err = lister.GetWorkingOrders(ctx)
		if err != nil {
			stats.Errors++
			logger.Warn("exit reconcile: list working orders for orderless recovery", "err", err)
			return
		}
	}
	recoveredTrade := make(map[string]bool)
	durableBrokerID := make(map[string]bool, len(durable))
	durableTicker := make(map[string]bool, len(durable))
	for _, p := range durable {
		durableBrokerID[p.BrokerOrderID] = true
		durableTicker[p.Ticker] = true
	}
	for _, t := range orderless {
		wantSide := "sell"
		if t.Direction == "short" {
			wantSide = "buy"
		}
		for _, w := range working {
			if w.Ticker != t.Ticker || w.Side != wantSide || w.OrderID == "" ||
				durableBrokerID[w.OrderID] {
				continue
			}
			if _, err := recoveryDB.InsertOrderWithRetry(ctx, store.Order{
				TradeID: t.ID, BrokerOrderID: w.OrderID, Ticker: t.Ticker,
				Side: wantSide, OrderType: string(execution.OrderTypeMarket),
				Quantity: t.Quantity, Status: w.Status, IsStopLoss: true,
			}); err != nil {
				stats.Errors++
				logger.Warn("exit reconcile: persist recovered working exit",
					"trade_id", t.ID, "broker_order_id", w.OrderID, "err", err)
				break
			}
			recoveredTrade[t.ID] = true
			stats.Recovered++
			logger.Warn("exit reconcile: reconstructed missing exit order row",
				"trade_id", t.ID, "broker_order_id", w.OrderID)
			break
		}
	}

	positions, err := broker.GetPositions(ctx)
	if err != nil {
		stats.Errors++
		logger.Warn("exit reconcile: fetch positions for orderless recovery", "err", err)
		return
	}
	owned, err := recoveryDB.GetOwnedPositionTrades(ctx)
	if err != nil {
		stats.Errors++
		logger.Warn("exit reconcile: fetch owned trades for orderless recovery", "err", err)
		return
	}
	brokerQty := make(map[string]float64)
	brokerPrice := make(map[string]float64)
	for _, p := range positions {
		qty := p.Quantity
		if p.Side == "short" && qty > 0 {
			qty = -qty
		}
		brokerQty[p.Ticker] += qty
		if p.CurrentPrice > 0 {
			brokerPrice[p.Ticker] = p.CurrentPrice
		}
	}
	dbQty := make(map[string]float64)
	for _, t := range owned {
		qty := t.Quantity
		if t.Direction == "short" {
			qty = -qty
		}
		dbQty[t.Ticker] += qty
	}

	shortage := make(map[string]float64)
	for ticker, qty := range dbQty {
		shortage[ticker] = math.Max(0, math.Abs(qty)-math.Abs(brokerQty[ticker]))
	}
	for _, t := range orderless {
		if recoveredTrade[t.ID] {
			continue
		}
		if durableTicker[t.Ticker] {
			// A known order can itself explain the broker/DB quantity delta.
			// Wait until its durable rows settle before attributing any
			// residual shortage to the orderless claim.
			continue
		}
		missing := shortage[t.Ticker]
		if missing <= 1e-9 {
			// Broker still holds the full DB-owned quantity and exposes no
			// working exit: no accepted/fill survived, so reopening is safe.
			if err := db.ReleaseTradeExitClaim(ctx, t.ID); err != nil {
				stats.Errors++
				continue
			}
			stats.Recovered++
			logger.Warn("exit reconcile: released orderless exit claim after broker position confirmation",
				"trade_id", t.ID, "ticker", t.Ticker)
			continue
		}
		sold := math.Min(t.Quantity, missing)
		remaining := math.Max(0, t.Quantity-sold)
		if remaining > 1e-9 {
			if err := db.UpdateExitingTradeQuantity(ctx, t.ID, remaining); err != nil {
				stats.Errors++
				continue
			}
			if err := db.ReleaseTradeExitClaim(ctx, t.ID); err != nil {
				stats.Errors++
				continue
			}
		} else {
			price := brokerPrice[t.Ticker]
			if price <= 0 {
				stats.Errors++
				logger.Error("exit reconcile: broker position proves orderless exit filled but no recovery price is available",
					"trade_id", t.ID, "ticker", t.Ticker)
				continue
			}
			pnl, pnlPct := tradePnL(t.Direction, t.EntryPrice, t.Quantity, price)
			if err := db.UpdateTradeExit(ctx, t.ID, price, pnl, pnlPct); err != nil {
				stats.Errors++
				continue
			}
			if pnl < 0 && len(recordLoss) > 0 && recordLoss[0] != nil {
				recordLoss[0]()
			}
		}
		shortage[t.Ticker] = math.Max(0, missing-sold)
		stats.Recovered++
		logger.Error("exit reconcile: recovered orderless exit from broker position delta",
			"trade_id", t.ID, "ticker", t.Ticker,
			"sold_qty", sold, "remaining_qty", remaining,
			"recovery_price", brokerPrice[t.Ticker])
	}
}

// ReconcileExitOrderFillsWithStore is a convenience wrapper that uses the
// package-level store functions. Called from the engine's monitor loop and
// the one-shot cmd/reconcile-exits tool.
func ReconcileExitOrderFillsWithStore(ctx context.Context, executor exitReconcileExecutor, limit int) ExitReconcileStats {
	return ReconcileExitOrderFills(ctx, executor, exitStoreAdapter{}, limit)
}

func ReconcileExitOrderFillsWithStoreAndLoss(
	ctx context.Context,
	executor exitReconcileExecutor,
	limit int,
	recordLoss func(),
) ExitReconcileStats {
	return ReconcileExitOrderFills(ctx, executor, exitStoreAdapter{}, limit, recordLoss)
}
