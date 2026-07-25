package store

import (
	"context"
	"math"
	"time"
)

// Trade mirrors a row from the trades table.
type Trade struct {
	ID            string
	SignalID      string
	Ticker        string
	Direction     string // long | short — value the engine actually persists
	Strategy      string
	EntryPrice    float64
	ExitPrice     float64
	Quantity      float64
	StopLoss      float64
	TakeProfit    float64
	PnL           float64
	PnLPct        float64
	Status        string // open | exiting | closed | cancelled
	ModelProvider string
	ModelName     string
	OpenedAt      time.Time
	ClosedAt      time.Time
	CreatedAt     time.Time
	// HighWaterMark is the running maximum price observed for an open trade,
	// used by the trailing-stop manager (#322). 0 when the manager has not
	// yet observed the trade (column is NULL in DB).
	HighWaterMark float64
}

// InsertTrade creates a new open trade record and returns its UUID.
func InsertTrade(ctx context.Context, t Trade) (string, error) {
	var id string
	err := GetDB().QueryRow(ctx, `
		INSERT INTO trades (
			signal_id, ticker, direction, strategy,
			entry_price, quantity, stop_loss, take_profit,
			status, model_provider, model_name
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,'open',$9,$10)
		RETURNING id`,
		nullableString(t.SignalID), t.Ticker, t.Direction, t.Strategy,
		t.EntryPrice, t.Quantity,
		nullableFloat(t.StopLoss), nullableFloat(t.TakeProfit),
		nullableString(t.ModelProvider), nullableString(t.ModelName),
	).Scan(&id)
	return id, err
}

// CancelUnfilledTrade marks an open, zero-quantity trade row cancelled — the
// cleanup for an entry order that died (expired/cancelled/rejected) or went
// permanently silent with no fill. Guarded to quantity=0 AND status='open'
// so it can never cancel a trade that received any fill; returns whether a
// row was actually cancelled.
func CancelUnfilledTrade(ctx context.Context, id string) (bool, error) {
	tag, err := GetDB().Exec(ctx, `
		UPDATE trades SET status='cancelled', closed_at=NOW()
		WHERE id=$1 AND status='open' AND quantity=0`, id)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// ClaimTradeExit atomically transfers exit ownership from any worker that
// observed the trade as open. Only the winner may cancel legs or submit a
// broker sell; all other exit paths see zero affected rows and stand down.
func ClaimTradeExit(ctx context.Context, id string) (bool, error) {
	tag, err := GetDB().Exec(ctx, `
		UPDATE trades SET status='exiting', exit_claimed_at=NOW()
		WHERE id=$1 AND status='open'`, id)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

func ReleaseTradeExitClaim(ctx context.Context, id string) error {
	_, err := GetDB().Exec(ctx, `
		UPDATE trades SET status='open', exit_claimed_at=NULL
		WHERE id=$1 AND status='exiting'`, id)
	return err
}

func UpdateExitingTradeQuantity(ctx context.Context, id string, quantity float64) error {
	_, err := GetDB().Exec(ctx, `
		UPDATE trades SET quantity=$1
		WHERE id=$2 AND status='exiting'`, quantity, id)
	return err
}

// ApplyEntryFill atomically records a cumulative entry fill and derives the
// trade's currently-held quantity after subtracting every exit fill already
// applied to that trade. A late entry poll must never resurrect shares that a
// protective stop sold between two partial entry fills.
func ApplyEntryFill(
	ctx context.Context,
	tradeID, brokerOrderID string,
	filledPrice, cumulativeFilledQty float64,
	status string,
) error {
	tx, err := GetDB().Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	var direction, tradeStatus string
	if err := tx.QueryRow(ctx, `
		SELECT direction, status
		FROM trades
		WHERE id=$1
		FOR UPDATE`, tradeID).Scan(&direction, &tradeStatus); err != nil {
		return err
	}
	var recordedEntryQty float64
	if err := tx.QueryRow(ctx, `
		SELECT COALESCE(MAX(filled_qty),0)
		FROM orders
		WHERE broker_order_id=$1 AND trade_id=$2`,
		brokerOrderID, tradeID,
	).Scan(&recordedEntryQty); err != nil {
		return err
	}
	targetEntryQty := math.Max(recordedEntryQty, cumulativeFilledQty)
	if _, err := tx.Exec(ctx, `
		UPDATE orders
		SET filled_price=COALESCE(NULLIF($1::NUMERIC,0::NUMERIC),filled_price),
		    filled_qty=$2, status=$3, updated_at=NOW()
		WHERE broker_order_id=$4 AND trade_id=$5`,
		filledPrice, targetEntryQty, status, brokerOrderID, tradeID,
	); err != nil {
		return err
	}
	if tradeStatus == "open" || tradeStatus == "exiting" {
		exitSide := "sell"
		if direction == "short" {
			exitSide = "buy"
		}
		var appliedExits float64
		if err := tx.QueryRow(ctx, `
			SELECT COALESCE(SUM(exit_fill_applied_qty),0)
			FROM orders
			WHERE trade_id=$1
			  AND side::text=$2`,
			tradeID, exitSide,
		).Scan(&appliedExits); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE trades
			SET entry_price=COALESCE(NULLIF($1::NUMERIC,0::NUMERIC),entry_price),
			    quantity=GREATEST(0::NUMERIC, $2::NUMERIC-$3::NUMERIC)
			WHERE id=$4 AND status IN ('open','exiting')`,
			filledPrice, targetEntryQty, appliedExits, tradeID,
		); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// UpdateTradeTrailing persists the latest trailing-stop computation for a
// trade: the new stop_loss and the high_water_mark. Used by the trailing-stop
// manager (#322). Either field may be 0 to skip updating that column.
func UpdateTradeTrailing(ctx context.Context, id string, stopLoss, highWaterMark float64) error {
	_, err := GetDB().Exec(ctx, `
		UPDATE trades
		SET stop_loss = COALESCE(NULLIF($1, 0), stop_loss),
		    high_water_mark = COALESCE(NULLIF($2, 0), high_water_mark)
		WHERE id = $3`,
		stopLoss, highWaterMark, id)
	return err
}

// LatestStopLossOrderForTrade returns the most-recently-created open STOP
// (or stop-limit) order for a trade, or nil if none exists. Used by the
// trailing-stop manager (#322) to decide whether to PATCH/replace an
// existing broker stop or submit a new one.
//
// The order_type filter is load-bearing: the codebase records market exit
// orders with is_stop_loss=true (the flag tags the order's purpose, not
// its type). Without the order_type clause this query could return a
// market exit and the manager would try to PATCH/cancel-and-replace it as
// if it were a STOP — racing the broker's market fill.
func LatestStopLossOrderForTrade(ctx context.Context, tradeID string) (*Order, error) {
	rows, err := GetDB().Query(ctx, `
		SELECT id, COALESCE(trade_id::text,''), COALESCE(broker_order_id,''),
		       ticker, side, order_type, quantity,
		       COALESCE(limit_price,0), COALESCE(stop_price,0),
		       COALESCE(filled_price,0), COALESCE(filled_qty,0),
		       status, is_stop_loss, is_take_profit,
		       COALESCE(parent_order_id,''),
		       created_at, updated_at
		FROM orders
		WHERE trade_id = $1
		  AND is_stop_loss = TRUE
		  AND order_type IN ('stop','stop_limit')
		  AND status IN ('pending','partial')
		ORDER BY created_at DESC
		LIMIT 1`, tradeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out, err := scanOrders(rows)
	if err != nil || len(out) == 0 {
		return nil, err
	}
	return &out[0], nil
}

// UpdateOrderStopPriceAndID updates the stop price, quantity, and broker order
// id of an existing order row. The trailing-stop manager (#322) calls this
// after replacing or resizing the broker STOP.
func UpdateOrderStopPriceAndID(
	ctx context.Context,
	orderID, newBrokerOrderID string,
	newStopPrice, newQuantity float64,
) error {
	_, err := GetDB().Exec(ctx, `
		UPDATE orders
		SET stop_price = $1,
		    broker_order_id = COALESCE(NULLIF($2, ''), broker_order_id),
		    quantity = $3,
		    updated_at = NOW()
		WHERE id = $4`,
		newStopPrice, newBrokerOrderID, newQuantity, orderID)
	return err
}

func CloseTrade(ctx context.Context, id string, exitPrice, pnl, pnlPct float64) error {
	_, err := GetDB().Exec(ctx, `
		UPDATE trades
		SET exit_price=$1, pnl=$2, pnl_pct=$3, status='closed',
		    closed_at=NOW(), exit_claimed_at=NULL
		WHERE id=$4`,
		exitPrice, pnl, pnlPct, id)
	return err
}

// UpdateTradeExit corrects an already-closed exit or finalizes a durably
// claimed (`exiting`) trade once the broker confirms the full fill.
func UpdateTradeExit(ctx context.Context, id string, exitPrice, pnl, pnlPct float64) error {
	_, err := GetDB().Exec(ctx, `
		UPDATE trades
		SET exit_price=$1, pnl=$2, pnl_pct=$3,
		    status='closed',
		    closed_at=COALESCE(closed_at, NOW()),
		    exit_claimed_at=NULL
		WHERE id=$4 AND status IN ('closed','exiting')`,
		exitPrice, pnl, pnlPct, id)
	return err
}

// MarkOrderStatus sets a terminal status on an order row without touching the
// fill columns. Used by the exit-fill reconciler when the broker reports an
// order cancelled/rejected rather than filled.
func MarkOrderStatus(ctx context.Context, brokerOrderID, status string) error {
	_, err := GetDB().Exec(ctx, `
		UPDATE orders
		SET status=$1, updated_at=NOW()
		WHERE broker_order_id=$2`,
		status, brokerOrderID)
	return err
}

// PendingExitOrder is a pending exit order row joined with the trade fields
// needed to recompute P&L once the broker confirms the fill.
type PendingExitOrder struct {
	OrderID       string
	BrokerOrderID string
	Ticker        string
	TradeID       string
	Direction     string // long | short
	EntryPrice    float64
	Quantity      float64 // current trade quantity
	OrderQuantity float64 // lot quantity assigned to this broker order row
	LimitPrice    float64 // fallback when a terminal limit fill omits price
	StopPrice     float64 // fallback when a terminal stop fill omits price
	RecordedFill  float64 // fill already durably assigned to this lot
	RecordedPrice float64 // fill price already durably assigned to this lot
	AggregateFill float64 // fill assigned across every row sharing the broker id
	ExitPrice     float64 // exit price currently recorded on the trade row
	TradeStatus   string  // closed (price correction) | exiting (finalization)
}

// DurableExitOrderOwnership is the complete broker-order identity set that
// can explain an in-flight exit. Orderless recovery must use an unbounded
// ownership snapshot rather than the reconciler's limited work batch; a
// durable order just outside that batch must never be attributed to another
// trade that happens to share its ticker and side.
type DurableExitOrderOwnership struct {
	BrokerOrderID string
	Ticker        string
	Side          string
	TradeStatus   string
	// ClaimOwned is true when this order was created by, or became terminal
	// during, the trade's current exit claim. Pre-existing protective stops
	// are known broker IDs but must not strand an orderless claim forever.
	ClaimOwned bool
}

func GetDurableExitOrderOwnership(ctx context.Context) ([]DurableExitOrderOwnership, error) {
	rows, err := GetDB().Query(ctx, `
		SELECT DISTINCT o.broker_order_id, o.ticker, o.side, t.status,
		       CASE
		         WHEN t.status='closed' THEN TRUE
		         WHEN o.created_at >= t.exit_claimed_at THEN TRUE
		         WHEN o.status IN ('filled','cancelled','rejected')
		              AND o.updated_at >= t.exit_claimed_at THEN TRUE
		         ELSE FALSE
		       END AS claim_owned
		FROM orders o
		JOIN trades t ON t.id=o.trade_id
		WHERE COALESCE(o.broker_order_id,'') <> ''
		  AND ((t.direction='long' AND o.side='sell')
		    OR (t.direction='short' AND o.side='buy'))
		  AND (
		        (t.status='closed' AND o.status IN ('pending','partial'))
		     OR (t.status='exiting' AND (
		          o.status IN ('pending','partial')
		       OR (o.status IN ('filled','cancelled','rejected')
		           AND o.updated_at >= t.exit_claimed_at)
		        ))
		      )`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DurableExitOrderOwnership
	for rows.Next() {
		var owner DurableExitOrderOwnership
		if err := rows.Scan(
			&owner.BrokerOrderID, &owner.Ticker, &owner.Side, &owner.TradeStatus,
			&owner.ClaimOwned,
		); err != nil {
			return nil, err
		}
		out = append(out, owner)
	}
	return out, rows.Err()
}

// GetPendingExitOrders returns orders still marked 'pending' or 'partial'
// whose trade is closed or durably exiting and whose side opposes direction —
// i.e. the exit order for that trade. This shape covers every close path
// (stop/TP monitor, trailing-stop market exits) regardless of the
// is_stop_loss/is_take_profit flags. Entry orders (side matching direction)
// and working broker stops on open trades are excluded by construction.
// 'partial' is included because PlaceOrder can return partially_filled and
// the row must stay sweepable until the broker reports the fill complete.
func GetPendingExitOrders(ctx context.Context, limit int) ([]PendingExitOrder, error) {
	rows, err := GetDB().Query(ctx, `
		WITH selected_trades AS (
		  SELECT t.id, MIN(o.created_at) AS first_order_at
		  FROM orders o
		  JOIN trades t ON t.id=o.trade_id
		  WHERE (
		          (t.status='closed' AND o.status IN ('pending','partial'))
		       OR (t.status='exiting' AND (
		            o.status IN ('pending','partial')
		         OR (o.status IN ('filled','cancelled','rejected')
		             AND o.updated_at >= t.exit_claimed_at)
		          ))
		        )
		    AND COALESCE(o.broker_order_id,'') <> ''
		    AND ((t.direction='long' AND o.side='sell')
		      OR (t.direction='short' AND o.side='buy'))
		  GROUP BY t.id
		  ORDER BY first_order_at
		  LIMIT $1
		)
		SELECT o.id, o.broker_order_id, o.ticker,
		       t.id, t.direction, t.entry_price, t.quantity,
		       o.quantity, COALESCE(o.limit_price,0),
		       COALESCE(o.stop_price,0), COALESCE(o.filled_qty,0),
		       COALESCE(o.filled_price,0),
		       COALESCE((
		         SELECT SUM(COALESCE(allocated.filled_qty,0))
		         FROM orders allocated
		         WHERE allocated.broker_order_id=o.broker_order_id
		       ),0),
		       COALESCE(t.exit_price,0), t.status
		FROM orders o
		JOIN trades t ON t.id = o.trade_id
		JOIN selected_trades selected ON selected.id=t.id
		WHERE (
		        (t.status='closed' AND o.status IN ('pending','partial'))
		     OR (t.status='exiting' AND (
		          o.status IN ('pending','partial')
		       OR (o.status IN ('filled','cancelled','rejected')
		           AND o.updated_at >= t.exit_claimed_at)
		        ))
		      )
		  AND COALESCE(o.broker_order_id,'') <> ''
		  AND ((t.direction = 'long'  AND o.side = 'sell')
		    OR (t.direction = 'short' AND o.side = 'buy'))
		ORDER BY o.created_at
		`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []PendingExitOrder
	for rows.Next() {
		var p PendingExitOrder
		if err := rows.Scan(&p.OrderID, &p.BrokerOrderID, &p.Ticker,
			&p.TradeID, &p.Direction, &p.EntryPrice, &p.Quantity,
			&p.OrderQuantity, &p.LimitPrice, &p.StopPrice,
			&p.RecordedFill, &p.RecordedPrice,
			&p.AggregateFill,
			&p.ExitPrice, &p.TradeStatus); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// CountOpenTrades returns the number of trade rows with status='open'.
// Cheaper than GetOpenTrades when only the count is needed: avoids loading
// every column of every open trade just to call len(...).
func CountOpenTrades(ctx context.Context) (int, error) {
	var n int
	err := GetDB().QueryRow(ctx, `SELECT COUNT(*) FROM trades WHERE status='open'`).Scan(&n)
	return n, err
}

// GetOpenTrades returns all trades with status = 'open'.
func GetOpenTrades(ctx context.Context) ([]Trade, error) {
	rows, err := GetDB().Query(ctx, `
		SELECT id, COALESCE(signal_id::text,''), ticker, direction, strategy,
		       entry_price, COALESCE(exit_price,0),
		       quantity, COALESCE(stop_loss,0), COALESCE(take_profit,0),
		       COALESCE(pnl,0), COALESCE(pnl_pct,0), status,
		       COALESCE(model_provider,''), COALESCE(model_name,''),
		       opened_at, COALESCE(closed_at, opened_at), created_at,
		       COALESCE(high_water_mark,0)
		FROM trades WHERE status='open'
		ORDER BY opened_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTrades(rows)
}

// GetOwnedPositionTrades includes durable in-flight exits so startup broker
// reconciliation never mistakes their still-held shares for adoptable orphans.
func GetOwnedPositionTrades(ctx context.Context) ([]Trade, error) {
	rows, err := GetDB().Query(ctx, `
		SELECT id, COALESCE(signal_id::text,''), ticker, direction, strategy,
		       entry_price, COALESCE(exit_price,0),
		       quantity, COALESCE(stop_loss,0), COALESCE(take_profit,0),
		       COALESCE(pnl,0), COALESCE(pnl_pct,0), status,
		       COALESCE(model_provider,''), COALESCE(model_name,''),
		       opened_at, COALESCE(closed_at, opened_at), created_at,
		       COALESCE(high_water_mark,0)
		FROM trades WHERE status IN ('open','exiting')
		ORDER BY opened_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTrades(rows)
}

// GetOrderlessExitingTrades returns claimed trades whose claim has no exit
// order created after it. These are the crash/failure window between winning
// exit ownership and making the broker order identity durable.
func GetOrderlessExitingTrades(ctx context.Context, limit int) ([]Trade, error) {
	rows, err := GetDB().Query(ctx, `
		SELECT id, COALESCE(signal_id::text,''), ticker, direction, strategy,
		       entry_price, COALESCE(exit_price,0),
		       quantity, COALESCE(stop_loss,0), COALESCE(take_profit,0),
		       COALESCE(pnl,0), COALESCE(pnl_pct,0), status,
		       COALESCE(model_provider,''), COALESCE(model_name,''),
		       opened_at, COALESCE(closed_at, opened_at), created_at,
		       COALESCE(high_water_mark,0)
		FROM trades t
		WHERE t.status='exiting'
		  AND t.exit_claimed_at IS NOT NULL
		  AND t.exit_claimed_at <= NOW() - INTERVAL '2 minutes'
		  AND NOT EXISTS (
		    SELECT 1 FROM orders o
		    WHERE o.trade_id=t.id
		      AND o.created_at >= t.exit_claimed_at
		      AND COALESCE(o.broker_order_id,'') <> ''
		      AND ((t.direction='long' AND o.side='sell')
		        OR (t.direction='short' AND o.side='buy'))
		  )
		ORDER BY t.exit_claimed_at
		LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTrades(rows)
}

// GetRecentTrades returns the most-recent `limit` trades regardless of status.
func GetRecentTrades(ctx context.Context, limit int) ([]Trade, error) {
	rows, err := GetDB().Query(ctx, `
		SELECT id, COALESCE(signal_id::text,''), ticker, direction, strategy,
		       entry_price, COALESCE(exit_price,0),
		       quantity, COALESCE(stop_loss,0), COALESCE(take_profit,0),
		       COALESCE(pnl,0), COALESCE(pnl_pct,0), status,
		       COALESCE(model_provider,''), COALESCE(model_name,''),
		       opened_at, COALESCE(closed_at, opened_at), created_at,
		       COALESCE(high_water_mark,0)
		FROM trades ORDER BY opened_at DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTrades(rows)
}

// GetTradeByID retrieves a single trade by UUID.
func GetTradeByID(ctx context.Context, id string) (*Trade, error) {
	rows, err := GetDB().Query(ctx, `
		SELECT id, COALESCE(signal_id::text,''), ticker, direction, strategy,
		       entry_price, COALESCE(exit_price,0),
		       quantity, COALESCE(stop_loss,0), COALESCE(take_profit,0),
		       COALESCE(pnl,0), COALESCE(pnl_pct,0), status,
		       COALESCE(model_provider,''), COALESCE(model_name,''),
		       opened_at, COALESCE(closed_at, opened_at), created_at,
		       COALESCE(high_water_mark,0)
		FROM trades WHERE id=$1`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	trades, err := scanTrades(rows)
	if err != nil || len(trades) == 0 {
		return nil, err
	}
	return &trades[0], nil
}

func scanTrades(rows interface {
	Next() bool
	Scan(...any) error
	Err() error
}) ([]Trade, error) {
	var out []Trade
	for rows.Next() {
		var t Trade
		if err := rows.Scan(
			&t.ID, &t.SignalID, &t.Ticker, &t.Direction, &t.Strategy,
			&t.EntryPrice, &t.ExitPrice,
			&t.Quantity, &t.StopLoss, &t.TakeProfit,
			&t.PnL, &t.PnLPct, &t.Status,
			&t.ModelProvider, &t.ModelName,
			&t.OpenedAt, &t.ClosedAt, &t.CreatedAt,
			&t.HighWaterMark,
		); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
