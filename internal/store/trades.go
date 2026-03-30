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

func CloseTrade(ctx context.Context, id string, exitPrice, pnl, pnlPct float64) error {
	_, err := GetDB().Exec(ctx, `
		UPDATE trades
		SET exit_price=$1, pnl=$2, pnl_pct=$3, status='closed', closed_at=NOW()
		WHERE id=$4`,
		exitPrice, pnl, pnlPct, id)
	return err
}

// GetOpenTrades returns all trades with status = 'open'.
func GetOpenTrades(ctx context.Context) ([]Trade, error) {
	rows, err := GetDB().Query(ctx, `
		SELECT id, COALESCE(signal_id::text,''), ticker, direction, strategy,
		       entry_price, COALESCE(exit_price,0),
		       quantity, COALESCE(stop_loss,0), COALESCE(take_profit,0),
		       COALESCE(pnl,0), COALESCE(pnl_pct,0), status,
		       COALESCE(model_provider,''), COALESCE(model_name,''),
		       opened_at, COALESCE(closed_at, opened_at), created_at
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
		       opened_at, COALESCE(closed_at, opened_at), created_at
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
		       opened_at, COALESCE(closed_at, opened_at), created_at
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
		); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
