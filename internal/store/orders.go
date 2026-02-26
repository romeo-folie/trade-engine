package store

import (
	"context"
	"time"
)

// Order mirrors a row from the orders table.
type Order struct {
	ID            string
	TradeID       string
	BrokerOrderID string
	Ticker        string
	Side          string // buy | sell
	OrderType     string // market | limit | stop
	Quantity      float64
	LimitPrice    float64
	StopPrice     float64
	FilledPrice   float64
	FilledQty     float64
	Status        string // pending | filled | partial | cancelled
	IsStopLoss    bool
	IsTakeProfit  bool
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// InsertOrder creates a new order record and returns its UUID.
func InsertOrder(ctx context.Context, o Order) (string, error) {
	var id string
	err := GetDB().QueryRow(ctx, `
		INSERT INTO orders (
			trade_id, broker_order_id, ticker, side, order_type,
			quantity, limit_price, stop_price, status,
			is_stop_loss, is_take_profit
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
		RETURNING id`,
		nullableString(o.TradeID), nullableString(o.BrokerOrderID),
		o.Ticker, o.Side, o.OrderType,
		o.Quantity, nullableFloat(o.LimitPrice), nullableFloat(o.StopPrice),
		o.Status, o.IsStopLoss, o.IsTakeProfit,
	).Scan(&id)
	return id, err
}

// UpdateOrderFill sets filled_price, filled_qty, and status after a fill event.
func UpdateOrderFill(ctx context.Context, id string, filledPrice, filledQty float64, status string) error {
	_, err := GetDB().Exec(ctx, `
		UPDATE orders SET filled_price=$1, filled_qty=$2, status=$3, updated_at=NOW()
		WHERE id=$4`,
		filledPrice, filledQty, status, id)
	return err
}

// GetOrdersByTrade retrieves all orders linked to a trade UUID.
func GetOrdersByTrade(ctx context.Context, tradeID string) ([]Order, error) {
	rows, err := GetDB().Query(ctx, `
		SELECT id, COALESCE(trade_id::text,''), COALESCE(broker_order_id,''),
		       ticker, side, order_type, quantity,
		       COALESCE(limit_price,0), COALESCE(stop_price,0),
		       COALESCE(filled_price,0), COALESCE(filled_qty,0),
		       status, is_stop_loss, is_take_profit,
		       created_at, updated_at
		FROM orders WHERE trade_id=$1 ORDER BY created_at ASC`, tradeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanOrders(rows)
}

func scanOrders(rows interface {
	Next() bool
	Scan(...any) error
	Err() error
}) ([]Order, error) {
	var out []Order
	for rows.Next() {
		var o Order
		if err := rows.Scan(
			&o.ID, &o.TradeID, &o.BrokerOrderID,
			&o.Ticker, &o.Side, &o.OrderType, &o.Quantity,
			&o.LimitPrice, &o.StopPrice,
			&o.FilledPrice, &o.FilledQty,
			&o.Status, &o.IsStopLoss, &o.IsTakeProfit,
			&o.CreatedAt, &o.UpdatedAt,
		); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}
