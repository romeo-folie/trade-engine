package store

import (
	"context"
	"fmt"
	"math"
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
	// ParentOrderID is the broker order id of the bracket PARENT when this
	// row is an OCO-linked child exit leg (migration 044). Non-empty means:
	// PATCH-replace only (cancel+resubmit orphans the sibling leg), and both
	// legs must be cancelled before selling the shares they hold.
	ParentOrderID string
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// InsertOrder creates a new order record and returns its UUID. Fill columns
// are persisted when set — an order inserted as already-filled (sync fills,
// or the exit path's inline fill poll) must carry its fill, otherwise a
// status='filled' row with empty fill columns is invisible to the exit-fill
// reconciler and stays priceless forever.
func InsertOrder(ctx context.Context, o Order) (string, error) {
	var id string
	err := GetDB().QueryRow(ctx, `
		INSERT INTO orders (
			trade_id, broker_order_id, ticker, side, order_type,
			quantity, limit_price, stop_price, status,
			is_stop_loss, is_take_profit, filled_price, filled_qty,
			parent_order_id
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
		RETURNING id`,
		nullableString(o.TradeID), nullableString(o.BrokerOrderID),
		o.Ticker, o.Side, o.OrderType,
		o.Quantity, nullableFloat(o.LimitPrice), nullableFloat(o.StopPrice),
		o.Status, o.IsStopLoss, o.IsTakeProfit,
		nullableFloat(o.FilledPrice), nullableFloat(o.FilledQty),
		nullableString(o.ParentOrderID),
	).Scan(&id)
	return id, err
}

// InsertOrderWithRetry closes the small but safety-critical window after a
// broker accepts an exit and before its identity is durable. The same broker
// ID makes retries idempotent at the lifecycle level; callers keep the trade
// claimed if all attempts fail.
func InsertOrderWithRetry(ctx context.Context, o Order) (string, error) {
	var id string
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		id, err = InsertOrder(ctx, o)
		if err == nil {
			return id, nil
		}
		if o.BrokerOrderID != "" {
			lookupErr := GetDB().QueryRow(ctx, `
				SELECT id FROM orders
				WHERE broker_order_id=$1
				  AND COALESCE(trade_id::text,'')=$2
				ORDER BY created_at DESC LIMIT 1`,
				o.BrokerOrderID, o.TradeID,
			).Scan(&id)
			if lookupErr == nil {
				return id, nil
			}
		}
		if attempt < 2 {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(50 * time.Millisecond):
			}
		}
	}
	return "", err
}

// UpdateOrderFill sets filled_price, filled_qty, and status after a fill event.
func UpdateOrderFill(ctx context.Context, id string, filledPrice, filledQty float64, status string) error {
	_, err := GetDB().Exec(ctx, `
		UPDATE orders SET filled_price=$1, filled_qty=$2, status=$3, updated_at=NOW()
		WHERE id=$4`,
		filledPrice, filledQty, status, id)
	return err
}

// ApplyPartialExit atomically advances an order's cumulative fill and reduces
// its trade by only the newly observed shares. Replaying the same broker
// result is quantity-idempotent; a revised cumulative average reprices the
// order's existing contribution without selling shares twice. This keeps
// cancellation-race recovery safe across process restarts and failures
// between otherwise separate writes.
func ApplyPartialExit(
	ctx context.Context,
	tradeID, orderID string,
	filledPrice, cumulativeFilledQty float64,
	status string,
) (remaining, applied float64, err error) {
	if cumulativeFilledQty > 0 && filledPrice <= 0 {
		return 0, 0, fmt.Errorf("partial exit fill price must be positive")
	}
	tx, err := GetDB().Begin(ctx)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	var recorded, recordedPrice, appliedAlready, orderQuantity float64
	if err = tx.QueryRow(ctx, `
		SELECT COALESCE(filled_qty,0), COALESCE(filled_price,0),
		       COALESCE(exit_fill_applied_qty,0), quantity
		FROM orders
		WHERE id=$1
		FOR UPDATE`, orderID).Scan(
		&recorded, &recordedPrice, &appliedAlready, &orderQuantity,
	); err != nil {
		return 0, 0, err
	}
	targetFill := math.Max(appliedAlready, math.Max(recorded, cumulativeFilledQty))
	if orderQuantity > 0 {
		// Broker responses for aggregate orders carry the aggregate fill on
		// every lookup. A per-lot row can never own more than its assigned
		// quantity, even if a caller accidentally forwards that aggregate.
		targetFill = math.Min(targetFill, math.Max(orderQuantity, appliedAlready))
	}
	applied = math.Max(0, targetFill-appliedAlready)
	// FilledPrice is the broker's cumulative average for this order. A later
	// response can revise that average without advancing FilledQty, so replace
	// the contribution of every previously applied share as well as adding any
	// newly observed shares.
	proceedsDelta := targetFill*filledPrice - appliedAlready*recordedPrice

	if _, err = tx.Exec(ctx, `
		UPDATE orders
		SET filled_price=$1, filled_qty=$2, status=$3,
		    exit_fill_applied_qty=$2, updated_at=NOW()
		WHERE id=$4`,
		filledPrice, targetFill, status, orderID,
	); err != nil {
		return 0, 0, err
	}
	if err = tx.QueryRow(ctx, `
		UPDATE trades
		SET quantity=GREATEST(0, quantity-$1),
		    realized_exit_qty=realized_exit_qty+$1,
		    realized_exit_proceeds=realized_exit_proceeds+$3,
		    realized_pnl=realized_pnl+CASE
		        WHEN direction='short' THEN entry_price*$1-$3
		        ELSE $3-entry_price*$1
		    END
		WHERE id=$2 AND status IN ('open','exiting')
		RETURNING quantity`,
		applied, tradeID, proceedsDelta,
	).Scan(&remaining); err != nil {
		return 0, 0, err
	}
	if err = tx.Commit(ctx); err != nil {
		return 0, 0, err
	}
	return remaining, applied, nil
}

// GetOrdersByTrade retrieves all orders linked to a trade UUID.
func GetOrdersByTrade(ctx context.Context, tradeID string) ([]Order, error) {
	rows, err := GetDB().Query(ctx, `
		SELECT id, COALESCE(trade_id::text,''), COALESCE(broker_order_id,''),
		       ticker, side, order_type, quantity,
		       COALESCE(limit_price,0), COALESCE(stop_price,0),
		       COALESCE(filled_price,0), COALESCE(filled_qty,0),
		       status, is_stop_loss, is_take_profit,
		       COALESCE(parent_order_id,''),
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
			&o.ParentOrderID,
			&o.CreatedAt, &o.UpdatedAt,
		); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// OpenExitLegsForTrade returns the trade's still-working exit legs — pending
// or partially-filled stop-loss / take-profit orders with a broker id. Used
// by the bracket-exit paths: a non-empty result means the trade's exits are
// broker-resident (the software monitor must not double-sell) and the legs
// hold the trade's shares (they must be cancelled before any other sell).
func OpenExitLegsForTrade(ctx context.Context, tradeID string) ([]Order, error) {
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
		  AND (is_stop_loss = TRUE OR is_take_profit = TRUE)
		  AND status IN ('pending','partial')
		  AND COALESCE(broker_order_id,'') <> ''
		ORDER BY created_at ASC`, tradeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanOrders(rows)
}
