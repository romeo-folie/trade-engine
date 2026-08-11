package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"
)

// ErrOrderBrokerIDChanged means an order row was repointed to a replacement
// broker order after the caller loaded it. Pollers must not apply the stale
// broker result to the replacement row.
var ErrOrderBrokerIDChanged = errors.New("order broker id changed")

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
	ExitReason    string
	ExitEvidence  json.RawMessage
	// ParentOrderID is the broker order id of the bracket PARENT when this
	// row is an OCO-linked child exit leg (migration 044). Non-empty means:
	// PATCH-replace only (cancel+resubmit orphans the sibling leg), and both
	// legs must be cancelled before selling the shares they hold.
	ParentOrderID string
	FilledAt      *time.Time
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
			parent_order_id, filled_at, exit_reason, exit_evidence
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)
		RETURNING id`,
		nullableString(o.TradeID), nullableString(o.BrokerOrderID),
		o.Ticker, o.Side, o.OrderType,
		o.Quantity, nullableFloat(o.LimitPrice), nullableFloat(o.StopPrice),
		o.Status, o.IsStopLoss, o.IsTakeProfit,
		nullableFloat(o.FilledPrice), nullableFloat(o.FilledQty),
		nullableString(o.ParentOrderID), nullableTimePtr(o.FilledAt),
		nullableString(o.ExitReason), nullableRawJSON(o.ExitEvidence),
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
	return UpdateOrderFillAt(ctx, id, filledPrice, filledQty, status, time.Time{})
}

// UpdateOrderFillForBrokerOrder is UpdateOrderFill guarded by the broker id
// that was polled. It prevents a concurrent stop replacement from being
// overwritten by the stale predecessor's status.
func UpdateOrderFillForBrokerOrder(
	ctx context.Context,
	id, expectedBrokerOrderID string,
	filledPrice, filledQty float64,
	status string,
) error {
	return updateOrderFillAt(ctx, id, expectedBrokerOrderID, filledPrice, filledQty, status, time.Time{})
}

// UpdateOrderStatusForBrokerOrder terminalizes the broker order that was
// polled without discarding any durable fill price or quantity already stored.
func UpdateOrderStatusForBrokerOrder(ctx context.Context, id, expectedBrokerOrderID, status string) error {
	tag, err := GetDB().Exec(ctx, `
		UPDATE orders SET status=$1, updated_at=NOW()
		WHERE id=$2 AND broker_order_id=$3`, status, id, expectedBrokerOrderID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: order %s no longer references %s", ErrOrderBrokerIDChanged, id, expectedBrokerOrderID)
	}
	return nil
}

// UpdateOrderFillAt persists broker execution time when the adapter supplies
// it. A zero timestamp deliberately leaves an existing broker timestamp intact.
func UpdateOrderFillAt(ctx context.Context, id string, filledPrice, filledQty float64, status string, filledAt time.Time) error {
	return updateOrderFillAt(ctx, id, "", filledPrice, filledQty, status, filledAt)
}

// UpdateOrderFillAtForBrokerOrder is UpdateOrderFillAt guarded by the broker
// id that produced the status response.
func UpdateOrderFillAtForBrokerOrder(
	ctx context.Context,
	id, expectedBrokerOrderID string,
	filledPrice, filledQty float64,
	status string,
	filledAt time.Time,
) error {
	return updateOrderFillAt(ctx, id, expectedBrokerOrderID, filledPrice, filledQty, status, filledAt)
}

func updateOrderFillAt(
	ctx context.Context,
	id, expectedBrokerOrderID string,
	filledPrice, filledQty float64,
	status string,
	filledAt time.Time,
) error {
	tag, err := GetDB().Exec(ctx, `
		UPDATE orders
		SET filled_price=$1, filled_qty=$2, status=$3,
		    filled_at=CASE
		        WHEN $4::timestamptz IS NULL THEN filled_at
		        WHEN filled_at IS NULL THEN $4::timestamptz
		        ELSE GREATEST(filled_at,$4::timestamptz)
		    END, updated_at=NOW()
		WHERE id=$5
		  AND ($6='' OR broker_order_id=$6)`,
		filledPrice, filledQty, status, nullableTime(filledAt), id, expectedBrokerOrderID)
	if err != nil {
		return err
	}
	if expectedBrokerOrderID != "" && tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: order %s no longer references %s", ErrOrderBrokerIDChanged, id, expectedBrokerOrderID)
	}
	return nil
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
	return ApplyPartialExitAt(ctx, tradeID, orderID, filledPrice, cumulativeFilledQty, status, time.Time{})
}

// ApplyPartialExitAt is ApplyPartialExit with an optional broker execution
// timestamp. The latest known fill time is carried onto the trade so final
// reconciliation never substitutes its own wall clock for broker truth.
func ApplyPartialExitAt(
	ctx context.Context,
	tradeID, orderID string,
	filledPrice, cumulativeFilledQty float64,
	status string,
	filledAt time.Time,
) (remaining, applied float64, err error) {
	return applyPartialExitAt(ctx, tradeID, orderID, "", filledPrice, cumulativeFilledQty, status, filledAt, nil)
}

// ApplyPartialExitAtForBrokerOrder is ApplyPartialExitAt guarded by the broker
// id that produced the cumulative fill. The order row is locked before the id
// comparison, so a concurrent replacement either happens wholly before this
// transaction (and is rejected here) or waits until the fill is durable.
func ApplyPartialExitAtForBrokerOrder(
	ctx context.Context,
	tradeID, orderID, expectedBrokerOrderID string,
	filledPrice, cumulativeFilledQty float64,
	status string,
	filledAt time.Time,
) (remaining, applied float64, err error) {
	return applyPartialExitAt(
		ctx, tradeID, orderID, expectedBrokerOrderID,
		filledPrice, cumulativeFilledQty, status, filledAt, nil,
	)
}

// ApplyProtectiveExitAtForBrokerOrder atomically applies a broker fill and
// replaces a previously claimed software intent with the protective outcome
// that actually won the broker race.
func ApplyProtectiveExitAtForBrokerOrder(
	ctx context.Context,
	tradeID, orderID, expectedBrokerOrderID string,
	filledPrice, cumulativeFilledQty float64,
	status string,
	filledAt time.Time,
	intent ExitIntent,
) (remaining, applied float64, err error) {
	return applyPartialExitAt(
		ctx, tradeID, orderID, expectedBrokerOrderID,
		filledPrice, cumulativeFilledQty, status, filledAt, &intent,
	)
}

func applyPartialExitAt(
	ctx context.Context,
	tradeID, orderID, expectedBrokerOrderID string,
	filledPrice, cumulativeFilledQty float64,
	status string,
	filledAt time.Time,
	intent *ExitIntent,
) (remaining, applied float64, err error) {
	if cumulativeFilledQty > 0 && filledPrice <= 0 {
		return 0, 0, fmt.Errorf("partial exit fill price must be positive")
	}
	tx, err := GetDB().Begin(ctx)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	var tradeQuantity float64
	if err = tx.QueryRow(ctx, `
		SELECT quantity
		FROM trades
		WHERE id=$1 AND status IN ('open','exiting')
		FOR UPDATE`, tradeID).Scan(&tradeQuantity); err != nil {
		return 0, 0, err
	}
	var recorded, recordedPrice, appliedAlready, orderQuantity float64
	var currentBrokerOrderID string
	if err = tx.QueryRow(ctx, `
		SELECT COALESCE(filled_qty,0), COALESCE(filled_price,0),
		       COALESCE(exit_fill_applied_qty,0), quantity,
		       COALESCE(broker_order_id,'')
		FROM orders
		WHERE id=$1
		FOR UPDATE`, orderID).Scan(
		&recorded, &recordedPrice, &appliedAlready, &orderQuantity,
		&currentBrokerOrderID,
	); err != nil {
		return 0, 0, err
	}
	if expectedBrokerOrderID != "" && currentBrokerOrderID != expectedBrokerOrderID {
		return 0, 0, fmt.Errorf(
			"%w: order %s references %s, polled %s",
			ErrOrderBrokerIDChanged, orderID, currentBrokerOrderID, expectedBrokerOrderID,
		)
	}
	observedFill := math.Max(appliedAlready, math.Max(recorded, cumulativeFilledQty))
	if orderQuantity > 0 {
		// Broker responses for aggregate orders carry the aggregate fill on
		// every lookup. A per-lot row can never own more than its assigned
		// quantity, even if a caller accidentally forwards that aggregate.
		observedFill = math.Min(observedFill, math.Max(orderQuantity, appliedAlready))
	}
	// Two OCO legs can both report terminal fills during a broker race. The
	// first leg owns the remaining trade quantity; later legs may still be
	// persisted for audit, but cannot increase realized quantity/P&L beyond
	// what remains on the trade.
	appliedTarget := math.Min(observedFill, appliedAlready+math.Max(0, tradeQuantity))
	applied = math.Max(0, appliedTarget-appliedAlready)
	// FilledPrice is the broker's cumulative average for this order. A later
	// response can revise that average without advancing FilledQty, so replace
	// the contribution of every previously applied share as well as adding any
	// newly observed shares.
	proceedsDelta := appliedTarget*filledPrice - appliedAlready*recordedPrice

	if _, err = tx.Exec(ctx, `
		UPDATE orders
		SET filled_price=$1, filled_qty=$2, status=$3,
		    exit_fill_applied_qty=$4,
		    filled_at=CASE
		        WHEN $5::timestamptz IS NULL THEN filled_at
		        WHEN filled_at IS NULL THEN $5::timestamptz
		        ELSE GREATEST(filled_at,$5::timestamptz)
		    END, updated_at=NOW()
		WHERE id=$6`,
		filledPrice, observedFill, status, appliedTarget, nullableTime(filledAt), orderID,
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
		    END,
		    exit_filled_at=CASE
		        WHEN $4::timestamptz IS NULL THEN exit_filled_at
		        WHEN exit_filled_at IS NULL THEN $4::timestamptz
		        ELSE GREATEST(exit_filled_at, $4::timestamptz)
		    END
		WHERE id=$2 AND status IN ('open','exiting')
		RETURNING quantity`,
		applied, tradeID, proceedsDelta, nullableTime(filledAt),
	).Scan(&remaining); err != nil {
		return 0, 0, err
	}
	if intent != nil {
		if _, err = tx.Exec(ctx, `
			UPDATE trades
			SET exit_reason=NULLIF($2,''), exit_evidence=$3::jsonb
			WHERE id=$1 AND status='exiting'`,
			tradeID, intent.Reason, nullableRawJSON(intent.Evidence),
		); err != nil {
			return 0, 0, err
		}
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
		       COALESCE(parent_order_id,''), filled_at,
		       COALESCE(exit_reason,''), exit_evidence,
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
			&o.ParentOrderID, &o.FilledAt,
			&o.ExitReason, &o.ExitEvidence,
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
		       COALESCE(parent_order_id,''), filled_at,
		       COALESCE(exit_reason,''), exit_evidence,
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

// ExitLegsForMarketExit returns one consistent snapshot of every broker-linked
// protective leg that can still account for shares: working pending/partial
// rows and terminal rows with a recorded fill. Callers preparing a replacement
// market exit use this combined query to avoid a fill racing between separate
// working and terminal lookups.
func ExitLegsForMarketExit(ctx context.Context, tradeID string) ([]Order, error) {
	rows, err := GetDB().Query(ctx, `
		SELECT id, COALESCE(trade_id::text,''), COALESCE(broker_order_id,''),
		       ticker, side, order_type, quantity,
		       COALESCE(limit_price,0), COALESCE(stop_price,0),
		       COALESCE(filled_price,0), COALESCE(filled_qty,0),
		       status, is_stop_loss, is_take_profit,
		       COALESCE(parent_order_id,''), filled_at,
		       COALESCE(exit_reason,''), exit_evidence,
		       created_at, updated_at
		FROM orders
		WHERE trade_id = $1
		  AND (is_stop_loss = TRUE OR is_take_profit = TRUE)
		  AND COALESCE(broker_order_id,'') <> ''
		  AND (
		        status IN ('pending','partial')
		     OR (order_type IN ('stop','stop_limit','limit')
		         AND status IN ('filled','cancelled','rejected')
		         AND (status='filled' OR COALESCE(filled_qty,0) > 0))
		      )
		ORDER BY created_at ASC`, tradeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanOrders(rows)
}

func nullableTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC()
}

func nullableTimePtr(t *time.Time) any {
	if t == nil || t.IsZero() {
		return nil
	}
	return t.UTC()
}
