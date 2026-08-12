package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// StockOrderIntent is the immutable pre-submit record for a standard stock
// order. RequestedQty and FilledQty are intentionally distinct: the latter is
// the broker's cumulative fill and must never overwrite the requested payload.
type StockOrderIntent struct {
	ID              string
	IdempotencyKey  string
	ClientOrderID   string
	TradeID         string
	ParentIntentID  string
	Purpose         string
	Ticker          string
	Side            string
	OrderType       string
	RequestedQty    float64
	RequestPayload  json.RawMessage
	PayloadHash     string
	BrokerOrderID   string
	Status          string
	FilledPrice     float64
	FilledQty       float64
	BrokerError     string
	BrokerRequestID string
	SubmittedAt     *time.Time
	LastCheckedAt   *time.Time
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// maxStockOrderIntentRetryGenerations bounds traversal of terminal stop
// generations. Each successor key is derived from its immediate predecessor's
// durable broker id, so a cycle is not expected; the limit still turns corrupt
// or manually-created lineage into a safe error instead of an unbounded DB
// walk.
const maxStockOrderIntentRetryGenerations = 32

// ResolveStockOrderIntentRetryGeneration returns the durable generation that
// should be submitted after baseKey is rejected as terminal zero-fill. If the
// latest generation is terminal, SubmitKey is the next deterministic key. If a
// newer generation already exists but is pending/accepted/ambiguous, SubmitKey
// replays that exact generation rather than creating a second live stop.
//
// A successor must be exactly "<prior key>:retry:<prior broker id>". This
// makes lineage self-validating and keeps a stale base key from cycling back
// to an earlier client id after several cancellations.
func ResolveStockOrderIntentRetryGeneration(ctx context.Context, baseKey string) (latest *StockOrderIntent, submitKey string, err error) {
	key := baseKey
	seen := make(map[string]struct{}, maxStockOrderIntentRetryGenerations)
	for generation := 0; generation < maxStockOrderIntentRetryGenerations; generation++ {
		if _, duplicate := seen[key]; duplicate {
			return nil, "", fmt.Errorf("stock order intent retry lineage cycles at %q", key)
		}
		seen[key] = struct{}{}
		intent, err := GetStockOrderIntentByKey(ctx, key)
		if err != nil {
			return nil, "", err
		}
		if intent == nil {
			return nil, "", fmt.Errorf("stock order intent retry generation %q is missing", key)
		}
		if !isTerminalZeroFillStockOrderIntent(intent) {
			return intent, intent.IdempotencyKey, nil
		}
		if intent.BrokerOrderID == "" {
			return nil, "", fmt.Errorf("terminal stock order intent %q has no broker order id", intent.IdempotencyKey)
		}
		nextKey := StockOrderIntentRetryKey(intent.IdempotencyKey, intent.BrokerOrderID)
		next, err := GetStockOrderIntentByKey(ctx, nextKey)
		if err != nil {
			return nil, "", err
		}
		if next == nil {
			return intent, nextKey, nil
		}
		key = next.IdempotencyKey
	}
	return nil, "", fmt.Errorf("stock order intent retry lineage exceeds %d generations from %q", maxStockOrderIntentRetryGenerations, baseKey)
}

// StockOrderIntentRetryKey advances a terminal generation without ever
// reusing its client id.
func StockOrderIntentRetryKey(previousKey, terminalBrokerOrderID string) string {
	return fmt.Sprintf("%s:retry:%s", previousKey, terminalBrokerOrderID)
}

func isTerminalZeroFillStockOrderIntent(intent *StockOrderIntent) bool {
	return intent != nil && intent.FilledQty <= 0 && (intent.Status == "cancelled" || intent.Status == "rejected" || intent.Status == "expired")
}

func CreateStockOrderIntent(ctx context.Context, in StockOrderIntent) error {
	_, err := GetDB().Exec(ctx, `
        INSERT INTO stock_order_intents (
            id, idempotency_key, client_order_id, trade_id, parent_intent_id, ticker, side,
            order_type, requested_qty, request_payload, payload_hash, status, purpose
        ) VALUES ($1,$2,$3,NULLIF($4,'')::uuid,NULLIF($5,'')::uuid,$6,$7,$8,$9,$10,$11,$12,$13)`,
		in.ID, in.IdempotencyKey, in.ClientOrderID, in.TradeID, in.ParentIntentID, in.Ticker, in.Side,
		in.OrderType, in.RequestedQty, in.RequestPayload, in.PayloadHash, in.Status, purposeOrSubmit(in.Purpose))
	return err
}

func GetStockOrderIntentByKey(ctx context.Context, key string) (*StockOrderIntent, error) {
	rows, err := GetDB().Query(ctx, `
        SELECT id, idempotency_key, client_order_id, COALESCE(trade_id::text,''), COALESCE(parent_intent_id::text,''), purpose, ticker, side, order_type, requested_qty, request_payload, payload_hash, COALESCE(broker_order_id,''), status, COALESCE(filled_price,0), filled_qty, COALESCE(broker_error,''), COALESCE(broker_request_id,''), submitted_at, last_checked_at, created_at, updated_at
        FROM stock_order_intents WHERE idempotency_key=$1`, key)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, nil
	}
	var in StockOrderIntent
	if err := rows.Scan(&in.ID, &in.IdempotencyKey, &in.ClientOrderID, &in.TradeID, &in.ParentIntentID, &in.Purpose, &in.Ticker, &in.Side, &in.OrderType, &in.RequestedQty, &in.RequestPayload, &in.PayloadHash, &in.BrokerOrderID, &in.Status, &in.FilledPrice, &in.FilledQty, &in.BrokerError, &in.BrokerRequestID, &in.SubmittedAt, &in.LastCheckedAt, &in.CreatedAt, &in.UpdatedAt); err != nil {
		return nil, err
	}
	return &in, rows.Err()
}

func AttachStockOrderIntentTrade(ctx context.Context, clientOrderID, tradeID string) error {
	_, err := GetDB().Exec(ctx, `
        UPDATE stock_order_intents SET trade_id=$2::uuid, updated_at=NOW()
        WHERE client_order_id=$1 AND trade_id IS NULL`, clientOrderID, tradeID)
	return err
}

func AttachStockOrderIntentTradeByKey(ctx context.Context, key, tradeID string) error {
	_, err := GetDB().Exec(ctx, `UPDATE stock_order_intents SET trade_id=$2::uuid, updated_at=NOW() WHERE idempotency_key=$1 AND (trade_id IS NULL OR trade_id=$2::uuid)`, key, tradeID)
	return err
}

// UpsertBracketChildIntent records the broker-created legs of a bracket under
// its pre-submit parent intent. Alpaca creates these children atomically with
// the parent, so they have no separately submitted client id; this projection
// gives each leg a durable identity for later recovery/reconciliation.
func UpsertBracketChildIntent(ctx context.Context, parentIntentID, tradeID, ticker, role, brokerOrderID, orderType string, quantity, price float64) (string, error) {
	key := fmt.Sprintf("bracket-child:%s:%s", parentIntentID, role)
	payload, err := json.Marshal(map[string]any{
		"parent_intent_id": parentIntentID, "role": role, "ticker": ticker,
		"broker_order_id": brokerOrderID, "order_type": orderType,
		"requested_qty": quantity, "price": price,
	})
	if err != nil {
		return "", err
	}
	var id string
	err = GetDB().QueryRow(ctx, `
		INSERT INTO stock_order_intents (
			id, idempotency_key, client_order_id, trade_id, parent_intent_id,
			ticker, side, order_type, requested_qty, request_payload, payload_hash,
			broker_order_id, status, submitted_at, purpose
		) VALUES (
			uuid_generate_v4(), $1, $2, NULLIF($3,'')::uuid, $4::uuid,
			$5, 'sell', $6::order_type, $7, $8::jsonb, $9,
			$10, 'pending', NOW(), 'broker_projection'
		)
		ON CONFLICT (idempotency_key) DO UPDATE
		SET broker_order_id=EXCLUDED.broker_order_id,
		    trade_id=EXCLUDED.trade_id, updated_at=NOW()
		RETURNING id`,
		key, key, tradeID, parentIntentID, ticker, orderType, quantity,
		string(payload), key, brokerOrderID,
	).Scan(&id)
	return id, err
}

func RecordStockOrderAttempt(ctx context.Context, intentID, operation, outcome, brokerOrderID, requestID, errText string) error {
	_, err := GetDB().Exec(ctx, `
        INSERT INTO stock_order_attempts (intent_id, operation, outcome, broker_order_id, broker_request_id, error)
        VALUES ($1::uuid,$2,$3,NULLIF($4,''),NULLIF($5,''),NULLIF($6,''))`,
		intentID, operation, outcome, brokerOrderID, requestID, errText)
	return err
}

func GetStockOrderIntentByBrokerOrderID(ctx context.Context, brokerOrderID string) (*StockOrderIntent, error) {
	rows, err := GetDB().Query(ctx, `SELECT id, idempotency_key, client_order_id, COALESCE(trade_id::text,''), COALESCE(parent_intent_id::text,''), purpose, ticker, side, order_type, requested_qty, request_payload, payload_hash, COALESCE(broker_order_id,''), status, COALESCE(filled_price,0), filled_qty, COALESCE(broker_error,''), COALESCE(broker_request_id,''), submitted_at, last_checked_at, created_at, updated_at FROM stock_order_intents WHERE broker_order_id=$1`, brokerOrderID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, nil
	}
	var in StockOrderIntent
	if err := rows.Scan(&in.ID, &in.IdempotencyKey, &in.ClientOrderID, &in.TradeID, &in.ParentIntentID, &in.Purpose, &in.Ticker, &in.Side, &in.OrderType, &in.RequestedQty, &in.RequestPayload, &in.PayloadHash, &in.BrokerOrderID, &in.Status, &in.FilledPrice, &in.FilledQty, &in.BrokerError, &in.BrokerRequestID, &in.SubmittedAt, &in.LastCheckedAt, &in.CreatedAt, &in.UpdatedAt); err != nil {
		return nil, err
	}
	return &in, rows.Err()
}

func UpdateStockOrderIntentBroker(ctx context.Context, clientOrderID, brokerOrderID, status string, filledPrice, filledQty float64, requestID, errText string, submitted bool) error {
	_, err := GetDB().Exec(ctx, `
        UPDATE stock_order_intents
        SET broker_order_id=COALESCE(NULLIF($2,''), broker_order_id),
            status=$3, filled_price=$4, filled_qty=$5,
            broker_request_id=COALESCE(NULLIF($6,''), broker_request_id),
            broker_error=NULLIF($7,''),
            submitted_at=CASE WHEN $8 THEN COALESCE(submitted_at, NOW()) ELSE submitted_at END,
            last_checked_at=NOW(), updated_at=NOW()
        WHERE client_order_id=$1`,
		clientOrderID, brokerOrderID, status, nullableFloat(filledPrice), filledQty, requestID, errText, submitted)
	return err
}

func ListRecoverableStockOrderIntents(ctx context.Context) ([]StockOrderIntent, error) {
	rows, err := GetDB().Query(ctx, `
        SELECT id, idempotency_key, client_order_id, COALESCE(trade_id::text,''), COALESCE(parent_intent_id::text,''), purpose,
               ticker, side, order_type, requested_qty, request_payload, payload_hash,
               COALESCE(broker_order_id,''), status, COALESCE(filled_price,0), filled_qty,
               COALESCE(broker_error,''), COALESCE(broker_request_id,''), submitted_at,
               last_checked_at, created_at, updated_at
        FROM stock_order_intents
        WHERE purpose='submit' AND (
              status IN ('pending','partial')
           OR (status IN ('filled','cancelled','rejected') AND filled_qty > 0 AND idempotency_key LIKE 'entry:%'
               AND NOT EXISTS (
                   SELECT 1 FROM orders o
                   JOIN trades t ON t.id=o.trade_id
                   WHERE o.stock_intent_id=stock_order_intents.id
                     AND o.trade_id=stock_order_intents.trade_id
                     AND t.stock_intent_id=stock_order_intents.id
                     AND COALESCE(o.filled_qty,0) >= stock_order_intents.filled_qty - 0.000000001
               ))
           -- A filled exit stays recoverable only until every required
           -- projection is present, its cumulative fill is durably applied,
           -- and its trade is terminal. Aggregate analyst exits have one
           -- order row per claimed lot, so completion is based on their
           -- aggregate allocation and every linked lot rather than one row.
           OR (status IN ('filled','cancelled','rejected') AND filled_qty > 0 AND order_type='market' AND (
                  idempotency_key LIKE 'software-exit:%'
               OR idempotency_key LIKE 'trailing-market-exit:%'
               OR idempotency_key LIKE 'analyst-exit:%'
           ) AND (
               (trade_id IS NOT NULL AND NOT EXISTS (
                   SELECT 1 FROM orders o
                   JOIN trades t ON t.id=o.trade_id
                   WHERE o.stock_intent_id=stock_order_intents.id
                     AND o.trade_id=stock_order_intents.trade_id
                     AND COALESCE(o.filled_qty,0) >= stock_order_intents.filled_qty - 0.000000001
                     AND t.status='closed'
               ))
               OR
               (trade_id IS NULL AND (
                   NOT EXISTS (SELECT 1 FROM orders o WHERE o.stock_intent_id=stock_order_intents.id)
                   OR COALESCE((SELECT SUM(COALESCE(o.filled_qty,0)) FROM orders o WHERE o.stock_intent_id=stock_order_intents.id),0)
                        < stock_order_intents.filled_qty - 0.000000001
                   OR EXISTS (
                       SELECT 1 FROM orders o
                       JOIN trades t ON t.id=o.trade_id
                       WHERE o.stock_intent_id=stock_order_intents.id
                         AND (COALESCE(o.filled_qty,0) < o.quantity - 0.000000001 OR t.status <> 'closed')
                   )
               ))
           ))
           OR (status IN ('filled','cancelled','rejected') AND filled_qty > 0 AND order_type='stop' AND trade_id IS NOT NULL
               AND NOT EXISTS (
                   SELECT 1 FROM orders o
                   JOIN trades t ON t.id=o.trade_id
                   WHERE o.stock_intent_id=stock_order_intents.id
                     AND o.trade_id=stock_order_intents.trade_id
                     AND COALESCE(o.filled_qty,0) >= stock_order_intents.filled_qty - 0.000000001
                     AND t.status='closed'
               ))
        )
        ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StockOrderIntent
	for rows.Next() {
		var in StockOrderIntent
		if err := rows.Scan(&in.ID, &in.IdempotencyKey, &in.ClientOrderID, &in.TradeID, &in.ParentIntentID, &in.Purpose,
			&in.Ticker, &in.Side, &in.OrderType, &in.RequestedQty, &in.RequestPayload, &in.PayloadHash,
			&in.BrokerOrderID, &in.Status, &in.FilledPrice, &in.FilledQty, &in.BrokerError,
			&in.BrokerRequestID, &in.SubmittedAt, &in.LastCheckedAt, &in.CreatedAt, &in.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, in)
	}
	return out, rows.Err()
}

func purposeOrSubmit(purpose string) string {
	if purpose == "" {
		return "submit"
	}
	return purpose
}
