package store

import (
	"context"
	"time"
)

// Trade mirrors a row from the trades table.
type Trade struct {
	ID            string
	SignalID      string
	Ticker        string
	Direction     string // buy | sell
	Strategy      string
	EntryPrice    float64
	ExitPrice     float64
	Quantity      float64
	StopLoss      float64
	TakeProfit    float64
	PnL           float64
	PnLPct        float64
	Status        string // open | closed | cancelled
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

// CloseTrade updates a trade with exit price, P&L, and closed_at.
// UpdateTradeEntryPrice backfills the entry_price and quantity for a trade
// after an async broker fill confirmation arrives.
// UpdateTradeEntryPrice backfills the entry_price and quantity for a trade
// after an async broker fill confirmation arrives.
func UpdateTradeEntryPrice(ctx context.Context, id string, entryPrice, quantity float64) error {
	_, err := GetDB().Exec(ctx, `
		UPDATE trades
		SET entry_price=$1, quantity=$2
		WHERE id=$3`,
		entryPrice, quantity, id)
	return err
}

// UpdateOrderFillPrice backfills filled_price and filled_qty on an order row.
func UpdateOrderFillPrice(ctx context.Context, brokerOrderID string, filledPrice, filledQty float64) error {
	_, err := GetDB().Exec(ctx, `
		UPDATE orders
		SET filled_price=$1, filled_qty=$2, status='filled'
		WHERE broker_order_id=$3`,
		filledPrice, filledQty, brokerOrderID)
	return err
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

// LatestStopLossOrderForTrade returns the most-recently-created open stop-loss
// order for a trade, or nil if none exists. Used by the trailing-stop
// manager (#322) to decide whether to PATCH/replace an existing broker stop
// or submit a new one.
func LatestStopLossOrderForTrade(ctx context.Context, tradeID string) (*Order, error) {
	rows, err := GetDB().Query(ctx, `
		SELECT id, COALESCE(trade_id::text,''), COALESCE(broker_order_id,''),
		       ticker, side, order_type, quantity,
		       COALESCE(limit_price,0), COALESCE(stop_price,0),
		       COALESCE(filled_price,0), COALESCE(filled_qty,0),
		       status, is_stop_loss, is_take_profit,
		       created_at, updated_at
		FROM orders
		WHERE trade_id = $1
		  AND is_stop_loss = TRUE
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

// UpdateOrderStopPriceAndID updates the stop price and broker order id of an
// existing order row. The trailing-stop manager (#322) calls this after
// successfully replacing the broker STOP — Alpaca returns a fresh order id
// on PATCH so we have to track the new id.
func UpdateOrderStopPriceAndID(ctx context.Context, orderID, newBrokerOrderID string, newStopPrice float64) error {
	_, err := GetDB().Exec(ctx, `
		UPDATE orders
		SET stop_price = $1,
		    broker_order_id = COALESCE(NULLIF($2, ''), broker_order_id),
		    updated_at = NOW()
		WHERE id = $3`,
		newStopPrice, newBrokerOrderID, orderID)
	return err
}

func CloseTrade(ctx context.Context, id string, exitPrice, pnl, pnlPct float64) error {
	_, err := GetDB().Exec(ctx, `
		UPDATE trades
		SET exit_price=$1, pnl=$2, pnl_pct=$3, status='closed', closed_at=NOW()
		WHERE id=$4`,
		exitPrice, pnl, pnlPct, id)
	return err
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
