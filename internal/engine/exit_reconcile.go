package engine

import (
	"context"
	"errors"
	"math"
	"strings"
	"time"

	"github.com/romeo-folie/trade-engine/internal/execution"
	"github.com/romeo-folie/trade-engine/internal/logger"
	"github.com/romeo-folie/trade-engine/internal/store"
)

// DefaultExitReconcileBatch caps how many pending exit orders a single sweep
// polls at the broker, bounding API load per run. The sweep runs repeatedly,
// so a backlog larger than the cap drains across consecutive runs.
const DefaultExitReconcileBatch = 100

// StartupExitReconcileBatch is a bounded best-effort startup batch, deliberately
// much larger than the periodic sweep. It prevents ordinary backlogs from
// leaving trade 101 hidden before drift/adoption without making startup issue
// unbounded broker calls; the periodic reconciler owns any remainder.
const StartupExitReconcileBatch = 10_000

// ExitReconcileInterval is independent of market-data ticks so an exit can be
// finalized and announced even when the market loop is idle or blocked.
const ExitReconcileInterval = 10 * time.Minute

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
	GetDurableExitOrderOwnership(ctx context.Context) ([]store.DurableExitOrderOwnership, error)
	InsertOrderWithRetry(ctx context.Context, o store.Order) (string, error)
	ApplyRecoveredExit(ctx context.Context, id string, exitPrice, soldQty float64) (store.TradeRealization, error)
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
	UpdateOrderFillAt(ctx context.Context, orderID string, filledPrice, filledQty float64, status string, filledAt time.Time) error
	ApplyPartialExitAt(ctx context.Context, tradeID, orderID string, filledPrice, cumulativeFilledQty float64, status string, filledAt time.Time) (remaining, applied float64, err error)
	UpdateTradeExitAt(ctx context.Context, id string, exitPrice, pnl, pnlPct float64, filledAt time.Time) error
	UpdateTradeExitFilledAt(ctx context.Context, id string, filledAt time.Time) error
	FinalizeTradeExit(ctx context.Context, id string, exitPrice, pnl, pnlPct float64) (store.TradeRealization, error)
	UpdateExitingTradeQuantity(ctx context.Context, id string, quantity float64) error
	ReleaseTradeExitClaim(ctx context.Context, id string) error
	MarkOrderStatus(ctx context.Context, brokerOrderID, status string) error
}

// exitStoreAdapter adapts the package-level store funcs to exitReconcileStore.
type exitStoreAdapter struct{}

func (exitStoreAdapter) GetPendingExitOrders(ctx context.Context, limit int) ([]store.PendingExitOrder, error) {
	return store.GetPendingExitOrders(ctx, limit)
}
func (exitStoreAdapter) UpdateOrderFillAt(ctx context.Context, orderID string, filledPrice, filledQty float64, status string, filledAt time.Time) error {
	return store.UpdateOrderFillAt(ctx, orderID, filledPrice, filledQty, status, filledAt)
}
func (exitStoreAdapter) ApplyPartialExitAt(ctx context.Context, tradeID, orderID string, filledPrice, cumulativeFilledQty float64, status string, filledAt time.Time) (float64, float64, error) {
	return store.ApplyPartialExitAt(ctx, tradeID, orderID, filledPrice, cumulativeFilledQty, status, filledAt)
}
func (exitStoreAdapter) UpdateTradeExitAt(ctx context.Context, id string, exitPrice, pnl, pnlPct float64, filledAt time.Time) error {
	return store.UpdateTradeExitAt(ctx, id, exitPrice, pnl, pnlPct, filledAt)
}
func (exitStoreAdapter) UpdateTradeExitFilledAt(ctx context.Context, id string, filledAt time.Time) error {
	return store.UpdateTradeExitFilledAt(ctx, id, filledAt)
}
func (exitStoreAdapter) FinalizeTradeExit(ctx context.Context, id string, _, _, _ float64) (store.TradeRealization, error) {
	return store.FinalizeTradeExit(ctx, id)
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
func (exitStoreAdapter) GetDurableExitOrderOwnership(ctx context.Context) ([]store.DurableExitOrderOwnership, error) {
	return store.GetDurableExitOrderOwnership(ctx)
}
func (exitStoreAdapter) ApplyRecoveredExit(ctx context.Context, id string, exitPrice, soldQty float64) (store.TradeRealization, error) {
	return store.ApplyRecoveredExit(ctx, id, exitPrice, soldQty)
}
func (exitStoreAdapter) InsertOrderWithRetry(ctx context.Context, o store.Order) (string, error) {
	return store.InsertOrderWithRetry(ctx, o)
}

// ExitReconcileStats summarises one sweep of the exit-fill reconciler.
type ExitReconcileStats struct {
	Scanned          int                      // pending exit orders examined
	Corrected        int                      // broker fill differed from recorded exit — trade P&L rewritten
	Confirmed        int                      // broker fill matched recorded exit — order row backfilled only
	Terminal         int                      // broker reported cancelled/rejected — order marked, trade NOT touched
	StillPending     int                      // broker still working the order (or partial fill)
	NotFound         int                      // broker no longer knows the order id
	DurableRecovered int                      // a purged terminal order was recovered from its DB fill
	Recovered        int                      // orderless exiting trades reconstructed or released
	Errors           int                      // broker/store errors — row left for the next sweep
	Closed           []store.TradeRealization // trades newly finalized by this sweep
}

func recordedFillTime(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
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
	type terminalTradeState struct {
		ticker      string
		direction   string
		entryPrice  float64
		remaining   float64
		fillQty     float64
		proceeds    float64
		canFinalize bool
	}
	terminalTrades := make(map[string]*terminalTradeState)
	stateFor := func(p store.PendingExitOrder) *terminalTradeState {
		s := terminalTrades[p.TradeID]
		if s == nil {
			s = &terminalTradeState{
				ticker: p.Ticker, direction: p.Direction,
				entryPrice: p.EntryPrice, remaining: p.Quantity,
				canFinalize: true,
			}
			terminalTrades[p.TradeID] = s
		}
		return s
	}
	for _, p := range pending {
		stats.Scanned++
		var tradeState *terminalTradeState
		if p.TradeStatus == "exiting" {
			tradeState = stateFor(p)
		}
		res, err := executor.GetOrderStatus(ctx, p.BrokerOrderID)
		if err != nil {
			if errors.Is(err, execution.ErrOrderNotFound) {
				stats.NotFound++
				if tradeState != nil {
					// A terminal order can disappear from broker history after its
					// fill. Re-apply the durable cumulative fill so a restart does
					// not strand an exiting trade behind ClaimOwned. A filled row
					// without a durable quantity is ambiguous and remains fail-closed.
					terminalStatus := execution.NormalizeOrderStatus(p.OrderStatus)
					if terminalStatus == "filled" && p.RecordedFill <= 0 {
						tradeState.canFinalize = false
						stats.Errors++
						logger.Warn("exit reconcile: purged filled order has no durable fill quantity",
							"ticker", p.Ticker, "trade_id", p.TradeID, "broker_order_id", p.BrokerOrderID)
						continue
					}
					if terminalStatus == "filled" || terminalStatus == "cancelled" || terminalStatus == "rejected" {
						if p.RecordedFill > 0 {
							fillPrice := p.RecordedPrice
							if fillPrice <= 0 {
								if p.LimitPrice > 0 {
									fillPrice = p.LimitPrice
								} else {
									fillPrice = p.StopPrice
								}
							}
							if fillPrice <= 0 {
								tradeState.canFinalize = false
								stats.Errors++
								logger.Warn("exit reconcile: purged terminal order has no durable fill price",
									"ticker", p.Ticker, "trade_id", p.TradeID, "broker_order_id", p.BrokerOrderID)
								continue
							}
							remaining, _, applyErr := db.ApplyPartialExitAt(
								ctx, p.TradeID, p.OrderID, fillPrice, p.RecordedFill, terminalStatus, recordedFillTime(p.RecordedFilledAt),
							)
							if applyErr != nil {
								tradeState.canFinalize = false
								stats.Errors++
								logger.Warn("exit reconcile: apply durable purged fill",
									"trade_id", p.TradeID, "order_id", p.OrderID, "err", applyErr)
								continue
							}
							tradeState.remaining = remaining
							tradeState.fillQty += p.RecordedFill
							tradeState.proceeds += p.RecordedFill * fillPrice
							stats.DurableRecovered++
						}
					} else {
						tradeState.canFinalize = false
					}
				}
				logger.Warn("exit reconcile: order unknown at broker — used durable fill if available",
					"ticker", p.Ticker, "trade_id", p.TradeID, "broker_order_id", p.BrokerOrderID)
			} else {
				if tradeState != nil {
					tradeState.canFinalize = false
				}
				stats.Errors++
				logger.Warn("exit reconcile: GetOrderStatus",
					"ticker", p.Ticker, "broker_order_id", p.BrokerOrderID, "err", err)
			}
			continue
		}

		switch execution.NormalizeOrderStatus(res.Status) {
		case "filled":
			fillPrice := res.FilledPrice
			if fillPrice <= 0 {
				fillPrice = p.RecordedPrice
			}
			if fillPrice <= 0 {
				if p.LimitPrice > 0 {
					fillPrice = p.LimitPrice
				} else {
					fillPrice = p.StopPrice
				}
			}
			if fillPrice <= 0 {
				if tradeState != nil {
					tradeState.canFinalize = false
				}
				stats.Errors++
				logger.Warn("exit reconcile: broker says filled but returned no fill price",
					"ticker", p.Ticker, "broker_order_id", p.BrokerOrderID)
				continue
			}
			lotQty := p.OrderQuantity
			if lotQty <= 0 {
				lotQty = p.Quantity
			}
			if p.TradeStatus == "exiting" {
				remaining, _, err := db.ApplyPartialExitAt(
					ctx, p.TradeID, p.OrderID, fillPrice, lotQty, "filled", res.FilledAt,
				)
				if err != nil {
					tradeState.canFinalize = false
					stats.Errors++
					logger.Warn("exit reconcile: atomically apply filled exit",
						"trade_id", p.TradeID, "order_id", p.OrderID, "err", err)
					continue
				}
				tradeState.remaining = remaining
				tradeState.fillQty += lotQty
				tradeState.proceeds += lotQty * fillPrice
				stats.Corrected++
				continue
			}

			// Legacy already-closed rows only need broker-price correction.
			corrected := math.Abs(fillPrice-p.ExitPrice) > exitPriceEpsilon
			pnl, pnlPct := tradePnL(p.Direction, p.EntryPrice, lotQty, fillPrice)
			if corrected {
				if err := db.UpdateTradeExitAt(ctx, p.TradeID, fillPrice, pnl, pnlPct, res.FilledAt); err != nil {
					stats.Errors++
					logger.Warn("exit reconcile: update trade exit", "trade_id", p.TradeID, "err", err)
					continue
				}
			} else if !res.FilledAt.IsZero() {
				if err := db.UpdateTradeExitFilledAt(ctx, p.TradeID, res.FilledAt); err != nil {
					stats.Errors++
					logger.Warn("exit reconcile: update trade fill time", "trade_id", p.TradeID, "err", err)
					continue
				}
			}
			if err := db.UpdateOrderFillAt(ctx, p.OrderID, fillPrice, lotQty, "filled", res.FilledAt); err != nil {
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
			fillPrice := res.FilledPrice
			if fillPrice <= 0 {
				fillPrice = p.RecordedPrice
			}
			if fillPrice <= 0 {
				if p.LimitPrice > 0 {
					fillPrice = p.LimitPrice
				} else {
					fillPrice = p.StopPrice
				}
			}
			if p.TradeStatus == "exiting" {
				remaining, _, err := db.ApplyPartialExitAt(
					ctx, p.TradeID, p.OrderID, fillPrice, lotFill, terminalStatus, res.FilledAt,
				)
				if err != nil {
					tradeState.canFinalize = false
					stats.Errors++
					logger.Warn("exit reconcile: atomically apply terminal exit",
						"trade_id", p.TradeID, "order_id", p.OrderID, "err", err)
					continue
				}
				tradeState.remaining = remaining
				if lotFill > 0 {
					tradeState.fillQty += lotFill
					tradeState.proceeds += lotFill * fillPrice
				}
			} else {
				if err := db.UpdateOrderFillAt(
					ctx, p.OrderID, fillPrice, lotFill, terminalStatus, res.FilledAt,
				); err != nil {
					stats.Errors++
					logger.Warn("exit reconcile: persist closed-trade terminal exit",
						"order_id", p.OrderID, "err", err)
					continue
				}
			}
			stats.Terminal++
			logMessage := "exit reconcile: claimed exit ended without a complete fill; claim will be released"
			isError := false
			switch {
			case p.TradeStatus == "closed" && lotFill <= 0:
				logMessage = "exit reconcile: closed trade's exit order never executed at broker — position may still be live"
				isError = true
			case p.TradeStatus == "closed":
				logMessage = "exit reconcile: closed trade's exit order terminated partially filled — broker position may remain"
			}
			if isError {
				logger.Error(logMessage,
					"ticker", p.Ticker, "trade_id", p.TradeID,
					"broker_order_id", p.BrokerOrderID, "broker_status", res.Status)
			} else {
				logger.Warn(logMessage,
					"ticker", p.Ticker, "trade_id", p.TradeID,
					"broker_order_id", p.BrokerOrderID, "broker_status", res.Status)
			}
		default: // pending / partial — leave for the next sweep
			if tradeState != nil {
				tradeState.canFinalize = false
			}
			stats.StillPending++
		}
	}

	for tradeID, state := range terminalTrades {
		if !state.canFinalize {
			continue
		}
		if state.remaining <= 1e-9 {
			if state.fillQty <= 0 || state.proceeds <= 0 {
				stats.Errors++
				logger.Error("exit reconcile: flattened trade lacks durable fill price",
					"trade_id", tradeID, "ticker", state.ticker)
				continue
			}
			exitPrice := state.proceeds / state.fillQty
			pnl, pnlPct := tradePnL(
				state.direction, state.entryPrice, state.fillQty, exitPrice,
			)
			realized, err := db.FinalizeTradeExit(
				ctx, tradeID, exitPrice, pnl, pnlPct,
			)
			if err != nil {
				stats.Errors++
				continue
			}
			if realized.PnL < 0 && len(recordLoss) > 0 && recordLoss[0] != nil {
				recordLoss[0]()
			}
			stats.Closed = append(stats.Closed, realized)
			continue
		}
		if err := db.ReleaseTradeExitClaim(ctx, tradeID); err != nil {
			stats.Errors++
			logger.Warn("exit reconcile: release settled exit claim",
				"trade_id", tradeID, "err", err)
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
			"durable_recovered", stats.DurableRecovered,
			"errors", stats.Errors,
		)
	}
	recoverOrderlessExits(
		ctx, executor, db, limit, &stats, recordLoss...,
	)
	return stats
}

func recoverOrderlessExits(
	ctx context.Context,
	executor exitReconcileExecutor,
	db exitReconcileStore,
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
	ownership, err := recoveryDB.GetDurableExitOrderOwnership(ctx)
	if err != nil {
		stats.Errors++
		logger.Warn("exit reconcile: fetch complete durable exit ownership", "err", err)
		return
	}
	durableBrokerID := make(map[string]bool, len(ownership))
	durableExitGroup := make(map[string]bool, len(ownership))
	for _, owner := range ownership {
		durableBrokerID[owner.BrokerOrderID] = true
		if owner.TradeStatus == "exiting" && owner.ClaimOwned {
			durableExitGroup[owner.Ticker+"\x00"+owner.Side] = true
		}
	}
	type recoveryGroup struct {
		trades     []store.Trade
		candidates []execution.WorkingOrder
	}
	groups := make(map[string]*recoveryGroup)
	for _, t := range orderless {
		wantSide := "sell"
		if t.Direction == "short" {
			wantSide = "buy"
		}
		key := t.Ticker + "\x00" + wantSide
		group := groups[key]
		if group == nil {
			group = &recoveryGroup{}
			groups[key] = group
		}
		group.trades = append(group.trades, t)
	}
	for key, group := range groups {
		for _, w := range working {
			wantTicker, wantSide, _ := strings.Cut(key, "\x00")
			if w.Ticker != wantTicker || w.Side != wantSide ||
				w.OrderID == "" || durableBrokerID[w.OrderID] {
				continue
			}
			group.candidates = append(group.candidates, w)
		}
	}
	ambiguousWorkingTicker := make(map[string]bool)
	for _, group := range groups {
		if len(group.trades) != 1 || len(group.candidates) != 1 {
			if len(group.candidates) > 0 {
				ambiguousWorkingTicker[group.trades[0].Ticker] = true
				logger.Error("exit reconcile: ambiguous working-order attribution; retaining claims",
					"ticker", group.trades[0].Ticker,
					"orderless_trades", len(group.trades),
					"candidate_orders", len(group.candidates))
			}
			continue
		}
		t, w := group.trades[0], group.candidates[0]
		wantSide := "sell"
		if t.Direction == "short" {
			wantSide = "buy"
		}
		if _, err := recoveryDB.InsertOrderWithRetry(ctx, store.Order{
			TradeID: t.ID, BrokerOrderID: w.OrderID, Ticker: t.Ticker,
			Side: wantSide, OrderType: string(execution.OrderTypeMarket),
			Quantity: t.Quantity, Status: w.Status,
			IsStopLoss:   t.ExitReason == store.ExitReasonStopLoss || t.ExitReason == store.ExitReasonTrailingATR,
			IsTakeProfit: t.ExitReason == store.ExitReasonTakeProfit,
		}); err != nil {
			stats.Errors++
			logger.Warn("exit reconcile: persist recovered working exit",
				"trade_id", t.ID, "broker_order_id", w.OrderID, "err", err)
			continue
		}
		recoveredTrade[t.ID] = true
		stats.Recovered++
		logger.Warn("exit reconcile: reconstructed missing exit order row",
			"trade_id", t.ID, "broker_order_id", w.OrderID)
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
		wantSide := "sell"
		if t.Direction == "short" {
			wantSide = "buy"
		}
		if durableExitGroup[t.Ticker+"\x00"+wantSide] {
			// A known order can itself explain the broker/DB quantity delta.
			// Wait until its durable rows settle before attributing any
			// residual shortage to the orderless claim.
			continue
		}
		if ambiguousWorkingTicker[t.Ticker] {
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
		recoveryPrice := brokerPrice[t.Ticker]
		if recoveryPrice <= 0 {
			// A flat position has no live quote in GetPositions. Realize the
			// inferred slice conservatively at cost; the ERROR log preserves
			// the accounting exception for broker-activity backfill.
			recoveryPrice = t.EntryPrice
		}
		realized, err := recoveryDB.ApplyRecoveredExit(
			ctx, t.ID, recoveryPrice, sold,
		)
		if err != nil {
			stats.Errors++
			continue
		}
		if realized.JustClosed {
			stats.Closed = append(stats.Closed, realized)
		}
		remaining := realized.Remaining
		if realized.Status == "closed" && realized.PnL < 0 &&
			len(recordLoss) > 0 && recordLoss[0] != nil {
			recordLoss[0]()
		}
		shortage[t.Ticker] = math.Max(0, missing-sold)
		stats.Recovered++
		logger.Error("exit reconcile: recovered orderless exit from broker position delta",
			"trade_id", t.ID, "ticker", t.Ticker,
			"sold_qty", sold, "remaining_qty", remaining,
			"recovery_price", recoveryPrice)
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

func (e *Engine) runExitReconcileSweep(ctx context.Context) ExitReconcileStats {
	return e.runExitReconcileSweepLimit(ctx, DefaultExitReconcileBatch)
}

func (e *Engine) runExitReconcileSweepLimit(ctx context.Context, limit int) ExitReconcileStats {
	return runExitReconcilePhases(
		ctx,
		5*time.Minute,
		e.reconcileBrokerManagedExitFills,
		func(claimedCtx context.Context) ExitReconcileStats {
			return e.runClaimedExitReconcileSweepLimit(claimedCtx, limit)
		},
	)
}

func runExitReconcilePhases(
	ctx context.Context,
	phaseTimeout time.Duration,
	brokerManaged func(context.Context) int,
	claimed func(context.Context) ExitReconcileStats,
) ExitReconcileStats {
	// Protective bracket/standalone-stop fills belong to open trades, so they
	// are intentionally absent from GetPendingExitOrders. Book those first: the
	// startup caller compares broker positions with DB ownership immediately
	// after this sweep, including while the market is closed.
	legCtx, cancelLegs := context.WithTimeout(ctx, phaseTimeout)
	brokerManagedErrors := brokerManaged(legCtx)
	cancelLegs()

	// Give claimed exits their own budget. A large/slow protective-leg set must
	// not consume the deadline and starve already-exiting trades every sweep.
	claimedCtx, cancelClaimed := context.WithTimeout(ctx, phaseTimeout)
	defer cancelClaimed()
	stats := claimed(claimedCtx)
	stats.Errors += brokerManagedErrors
	return stats
}

func (e *Engine) runClaimedExitReconcileSweepLimit(ctx context.Context, limit int) ExitReconcileStats {
	stats := ReconcileExitOrderFillsWithStoreAndLoss(
		ctx, e.executor, limit, e.portfolio.RecordLoss,
	)
	for _, realized := range stats.Closed {
		e.publishTradeClosed(realized)
	}
	return stats
}

func (e *Engine) reconcileBrokerManagedExitFills(ctx context.Context) int {
	trades, err := store.GetOpenTrades(ctx)
	if err != nil {
		logger.Warn("broker-managed exit reconcile: load open trades", "err", err)
		return 1
	}
	for _, trade := range trades {
		e.reconcileBracketTrade(ctx, trade)
	}
	return 0
}

// ReconcileExitsNow performs a bounded best-effort startup sweep. The command
// invokes it before broker-drift alerting; Engine.Run invokes it as a safety
// net for other callers before their first trading tick. Successful sweeps are
// idempotent, while transient failures remain retryable by the safety call and
// any backlog beyond the startup cap remains owned by the periodic reconciler.
func (e *Engine) ReconcileExitsNow(ctx context.Context) {
	if !store.IsConnected() {
		return
	}
	// Always retry broker-managed open legs on the Engine.Run safety call, even
	// when the command's first pass completed its claimed-exit backlog. The leg
	// status API is best effort and reconcileBracketTrade deliberately treats a
	// transient broker read as unknown rather than closing from stale state.
	legCtx, cancelLegs := context.WithTimeout(ctx, 5*time.Minute)
	e.reconcileBrokerManagedExitFills(legCtx)
	cancelLegs()
	e.exitStartupMu.Lock()
	defer e.exitStartupMu.Unlock()
	if e.exitStartupPrepared {
		return
	}
	claimedCtx, cancelClaimed := context.WithTimeout(ctx, 5*time.Minute)
	defer cancelClaimed()
	stats := e.runClaimedExitReconcileSweepLimit(claimedCtx, StartupExitReconcileBatch)
	e.exitStartupPrepared = stats.Errors == 0
}

func runExitReconcileTicker(ctx context.Context, interval time.Duration, sweep func(context.Context)) {
	if interval <= 0 || sweep == nil {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sweep(ctx)
		}
	}
}

func (e *Engine) runExitReconcileLoop(ctx context.Context) {
	runExitReconcileTicker(ctx, ExitReconcileInterval, func(sweepCtx context.Context) {
		e.runExitReconcileSweep(sweepCtx)
	})
}
