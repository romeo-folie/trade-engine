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

// ActiveStockEntryReservation is the restart-safe admission projection for a
// nonterminal entry intent. FilledQty is cumulative broker truth; RemainingQty
// is only the still-working portion and therefore never double counts a trade
// created for a partial fill.
type ActiveStockEntryReservation struct {
	IntentID       string
	IdempotencyKey string
	ClientOrderID  string
	BrokerOrderID  string
	TradeID        string
	Ticker         string
	Status         string
	RequestedQty   float64
	FilledQty      float64
	RemainingQty   float64
	ReferencePrice float64
	BrokerError    string
	SubmittedAt    *time.Time
	CreatedAt      time.Time
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
		terminalIdentity := intent.BrokerOrderID
		if terminalIdentity == "" {
			// A definitive pre-submit rejection has no broker order id, but it
			// is still a durable terminal generation. Its immutable intent id
			// yields a stable successor key across retries and restarts.
			terminalIdentity = "intent-" + intent.ID
		}
		nextKey := StockOrderIntentRetryKey(intent.IdempotencyKey, terminalIdentity)
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

// ResolveTerminalPartialStockOrderIntentRetryGeneration advances only a
// terminal market-exit generation that already applied some fill. The residual
// position is a distinct broker order and therefore cannot reuse the original
// immutable payload or client id.
func ResolveTerminalPartialStockOrderIntentRetryGeneration(ctx context.Context, baseKey string) (string, error) {
	key := baseKey
	for generation := 0; generation < maxStockOrderIntentRetryGenerations; generation++ {
		intent, err := GetStockOrderIntentByKey(ctx, key)
		if err != nil {
			return "", err
		}
		if intent == nil || !isTerminalPartialStockOrderIntent(intent) {
			return key, nil
		}
		identity := intent.BrokerOrderID
		if identity == "" {
			identity = "intent-" + intent.ID
		}
		nextKey := StockOrderIntentRetryKey(intent.IdempotencyKey, identity)
		next, err := GetStockOrderIntentByKey(ctx, nextKey)
		if err != nil {
			return "", err
		}
		if next == nil {
			return nextKey, nil
		}
		key = next.IdempotencyKey
	}
	return "", fmt.Errorf("stock order intent terminal partial retry lineage exceeds %d generations from %q", maxStockOrderIntentRetryGenerations, baseKey)
}

// StockOrderIntentRetryKey advances a terminal generation without ever
// reusing its client id.
func StockOrderIntentRetryKey(previousKey, terminalBrokerOrderID string) string {
	return fmt.Sprintf("%s:retry:%s", previousKey, terminalBrokerOrderID)
}

func isTerminalZeroFillStockOrderIntent(intent *StockOrderIntent) bool {
	return intent != nil && intent.FilledQty <= 0 && (intent.Status == "cancelled" || intent.Status == "rejected" || intent.Status == "expired")
}

func isTerminalPartialStockOrderIntent(intent *StockOrderIntent) bool {
	return intent != nil && intent.FilledQty > 0 && intent.FilledQty < intent.RequestedQty-0.000000001 &&
		(intent.Status == "cancelled" || intent.Status == "rejected" || intent.Status == "expired")
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

// StockEntryBrokerProtection returns whether the durable entry that owns a
// trade requires broker-resident protection, plus its parent broker order ID.
// Keeping the query here lets reconciliation enforce the immutable admission
// policy without guessing from mutable exit-leg rows.
func StockEntryBrokerProtection(ctx context.Context, tradeID string) (required bool, brokerOrderID string, err error) {
	var raw json.RawMessage
	err = GetDB().QueryRow(ctx, `
		SELECT request_payload, COALESCE(broker_order_id,'')
		FROM stock_order_intents
		WHERE trade_id=$1::uuid AND purpose='submit' AND side='buy'
		ORDER BY created_at DESC LIMIT 1`, tradeID).Scan(&raw, &brokerOrderID)
	if err != nil {
		return false, "", err
	}
	var policy struct {
		RequireBrokerProtection bool
	}
	if err := json.Unmarshal(raw, &policy); err != nil {
		return false, "", err
	}
	return policy.RequireBrokerProtection, brokerOrderID, nil
}

// HasPreparedStockEntry reports whether a signal owns a pending entry intent
// that has no recorded broker submission or identity.
func HasPreparedStockEntry(ctx context.Context, signalID string) (bool, error) {
	var found bool
	err := GetDB().QueryRow(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM stock_order_intents
			WHERE idempotency_key LIKE 'entry:' || $1 || '%'
			  AND purpose='submit' AND status='pending'
			  AND submitted_at IS NULL AND broker_order_id IS NULL
		)`, signalID).Scan(&found)
	return found, err
}

// RetirePreparedStockEntryAfterBrokerMiss releases one never-recorded entry
// generation. Callers must invoke it only immediately after an exact client-ID
// lookup returned not found and the owning signal is independently terminal.
func RetirePreparedStockEntryAfterBrokerMiss(ctx context.Context, intentKey, reason string) error {
	_, err := GetDB().Exec(ctx, `
		UPDATE stock_order_intents
		SET status='rejected', broker_error=$2, last_checked_at=NOW(), updated_at=NOW()
		WHERE idempotency_key=$1 AND purpose='submit' AND status='pending'
		  AND submitted_at IS NULL AND broker_order_id IS NULL`, intentKey, reason)
	return err
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
		"requested_qty": quantity, "price": price, "time_in_force": "gtc",
	})
	if err != nil {
		return "", err
	}
	var id string
	err = GetDB().QueryRow(ctx, `
		WITH RECURSIVE upserted AS (
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
			SET trade_id=COALESCE(stock_order_intents.trade_id, EXCLUDED.trade_id),
			    updated_at=NOW()
			RETURNING id
		), lineage(id, depth) AS (
			SELECT id, 0 FROM upserted
			UNION ALL
			SELECT child.id, lineage.depth + 1
			FROM stock_order_intents child
			JOIN lineage ON child.parent_intent_id=lineage.id
		), active_lineage AS (
			SELECT lineage.id
			FROM lineage
			JOIN orders ON orders.stock_intent_id=lineage.id
			ORDER BY lineage.depth DESC
			LIMIT 1
		)
		SELECT COALESCE(
			(SELECT id FROM active_lineage),
			(SELECT id FROM upserted)
		)`,
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

// HasAmbiguousStockOrderAttempt reports whether the most recent replacement
// submission has an unknown outcome. The pre-submission marker is written
// before PATCH, so a process/database failure after the broker request cannot
// erase the fact that the predecessor may already have been replaced.
func HasAmbiguousStockOrderAttempt(ctx context.Context, intentID string) (bool, error) {
	var found bool
	err := GetDB().QueryRow(ctx, `
		SELECT COALESCE((
			SELECT outcome IN ('submitting','ambiguous')
			FROM stock_order_attempts
			WHERE intent_id=$1::uuid
			  AND operation IN ('replace_submission','replace')
			ORDER BY created_at DESC, id DESC
			LIMIT 1
		), FALSE)`, intentID).Scan(&found)
	return found, err
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

// ListUnprojectedStockStopIntents returns durable stops whose broker state was
// recorded before their local order row. Positive fills sort first so recovery
// reduces the trade before deciding whether residual fallback protection is
// needed; accepted working stops are then projected instead of duplicated.
func ListUnprojectedStockStopIntents(ctx context.Context, tradeID string) ([]StockOrderIntent, error) {
	rows, err := GetDB().Query(ctx, `
		SELECT i.idempotency_key
		FROM stock_order_intents i
		WHERE i.trade_id=$1::uuid
		  AND i.purpose='submit'
		  AND i.side='sell'
		  AND i.order_type IN ('stop','stop_limit')
		  AND (
		      i.status IN ('pending','partial')
		      OR (i.status IN ('filled','cancelled','rejected') AND i.filled_qty > 0.000000001)
		  )
		  AND NOT EXISTS (
			  SELECT 1 FROM orders o WHERE o.stock_intent_id=i.id
		  )
		ORDER BY CASE WHEN i.filled_qty > 0.000000001 THEN 0 ELSE 1 END,
		         i.created_at`, tradeID)
	if err != nil {
		return nil, err
	}
	var keys []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			rows.Close()
			return nil, err
		}
		keys = append(keys, key)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	out := make([]StockOrderIntent, 0, len(keys))
	for _, key := range keys {
		intent, err := GetStockOrderIntentByKey(ctx, key)
		if err != nil {
			return nil, err
		}
		if intent != nil {
			out = append(out, *intent)
		}
	}
	return out, nil
}

func UpdateStockOrderIntentBroker(ctx context.Context, clientOrderID, brokerOrderID, status string, filledPrice, filledQty float64, requestID, errText string, submitted bool) error {
	_, err := GetDB().Exec(ctx, `
        UPDATE stock_order_intents
        SET broker_order_id=COALESCE(NULLIF($2,''), broker_order_id),
		    -- Lifecycle snapshots can arrive out of order. Once terminal, an
		    -- intent must never be regressed by an older working snapshot; a
		    -- later terminal snapshot is still allowed to record the broker's
		    -- final outcome (for example, a cancellation that raced a fill).
		    status=CASE
		        WHEN status IN ('filled','cancelled','rejected') AND $3 IN ('pending','partial') THEN status
		        -- Error-only attempt records carry an empty broker result. Keep
		        -- the last observed lifecycle and cumulative fill rather than
		        -- regressing a durable partial fill to pending/zero.
		        WHEN $7 <> '' AND $3 IN ('pending','partial') AND $2='' AND $4::numeric IS NULL AND $5=0 THEN status
		        ELSE $3::order_status
		    END,
            filled_price=CASE
                WHEN $4 IS NULL THEN filled_price
                WHEN $5 < filled_qty THEN filled_price
                WHEN $4 = 0 AND filled_price IS NOT NULL THEN filled_price
                ELSE $4
            END,
            filled_qty=GREATEST(filled_qty, $5),
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
               AND (
                   NOT EXISTS (
                       SELECT 1 FROM orders o
                       JOIN trades t ON t.id=o.trade_id
                       WHERE o.stock_intent_id=stock_order_intents.id
                         AND o.trade_id=stock_order_intents.trade_id
                         AND t.stock_intent_id=stock_order_intents.id
                         AND COALESCE(o.filled_qty,0) >= stock_order_intents.filled_qty - 0.000000001
                   )
                   -- Bracket children are broker-created projections, so only
                   -- their durable parent can restore a child row that was
                   -- lost after the parent projection otherwise completed.
	                   OR (
	                       COALESCE(stock_order_intents.request_payload->>'OrderClass','') = 'bracket'
                       AND (
                           SELECT COUNT(*)
                           FROM stock_order_intents child
                           WHERE child.parent_intent_id=stock_order_intents.id
                             AND child.purpose='broker_projection'
                             AND EXISTS (
                                 WITH RECURSIVE replacement_lineage(id) AS (
                                     SELECT child.id
                                     UNION ALL
                                     SELECT replacement.id
                                     FROM stock_order_intents replacement
                                     JOIN replacement_lineage lineage ON lineage.id=replacement.parent_intent_id
                                 )
                                 SELECT 1 FROM orders child_order
                                 JOIN replacement_lineage lineage ON lineage.id=child_order.stock_intent_id
                             )
                       ) < 2
	                   )
	                   -- An uncapped entry is not complete until its standalone
	                   -- protective stop covers the durable position. Without
	                   -- this, a crash after the entry projection but before stop
	                   -- submission would leave a live position unprotected.
	                   OR (
	                       COALESCE(stock_order_intents.request_payload->>'UncappedTakeProfit','false') = 'true'
	                       AND EXISTS (
	                           SELECT 1 FROM trades protected_trade
	                           WHERE protected_trade.id=stock_order_intents.trade_id
	                             AND protected_trade.status='open'
	                       )
	                       AND NOT EXISTS (
	                           SELECT 1 FROM orders stop_order
	                           JOIN trades protected_trade ON protected_trade.id=stop_order.trade_id
	                           WHERE stop_order.trade_id=stock_order_intents.trade_id
	                             AND stop_order.is_stop_loss
	                             AND stop_order.order_type IN ('stop','stop_limit')
	                             AND stop_order.status IN ('pending','partial')
	                             AND ABS(stop_order.quantity-protected_trade.quantity) <= 0.000000001
	                             -- A trailing manager may already have ratcheted
	                             -- the sell stop above the immutable policy floor.
	                             -- That is stronger protection, not a mismatch.
	                             AND stop_order.stop_price >= COALESCE(NULLIF(stock_order_intents.request_payload->>'PolicyStopLoss','')::numeric,0) - 0.000000001
	                       )
	                   )
	                   -- A completed fill must also finish the signal lifecycle.
	                   -- Keep the intent selectable if a crash happened after its
	                   -- projections but before that final update.
	                   OR NOT EXISTS (
	                       SELECT 1 FROM signals signal
	                       WHERE signal.id::text=split_part(substring(stock_order_intents.idempotency_key from 7), ':retry:', 1)
	                         AND signal.status='executed'
	                   )
	               ))
           -- A terminal zero-fill entry can have no projection at all, or an
           -- order-only row, after a crash between acceptance and terminal
           -- polling. Select it once to reject the signal and persist the
           -- terminal order status when present.
           OR (status IN ('cancelled','rejected') AND filled_qty <= 0.000000001
               AND idempotency_key LIKE 'entry:%'
               -- A locally definitive rejection has no broker order to project;
               -- only a broker-identified terminal order can be the crash case.
               AND broker_order_id IS NOT NULL
               AND NOT EXISTS (
                   SELECT 1 FROM signals signal
	                   WHERE signal.id::text=split_part(substring(stock_order_intents.idempotency_key from 7), ':retry:', 1)
                     AND signal.status='rejected'
               )
               AND (
                   NOT EXISTS (
                       SELECT 1 FROM orders o
                       WHERE o.stock_intent_id=stock_order_intents.id
                   )
                   OR EXISTS (
                       SELECT 1 FROM orders o
                       WHERE o.stock_intent_id=stock_order_intents.id
                         AND o.status IN ('pending','partial')
                   )
               ))
           -- A filled exit stays recoverable only until every required
           -- projection is present, its cumulative fill is durably applied,
           -- and its trade is terminal. Aggregate analyst exits have one
           -- order row per claimed lot, so completion is based on their
           -- aggregate allocation and every linked lot rather than one row.
           OR (status IN ('filled','cancelled','rejected')
               AND (filled_qty > 0 OR (
                   status IN ('cancelled','rejected') AND filled_qty <= 0.000000001
	                   -- A locally definitive rejection has no broker order
	                   -- to recover. Replaying it after its caller rotates a
	                   -- successor could create a second live exit.
	                   AND COALESCE(broker_order_id,'') <> ''
                   -- Once a successor exists it owns the same claimed lots;
                   -- never reopen the retired generation after rotation.
                   AND NOT EXISTS (
                       SELECT 1 FROM stock_order_intents newer
                       WHERE newer.idempotency_key LIKE stock_order_intents.idempotency_key || ':retry:%'
                   )
               ))
               AND order_type='market' AND (
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
                     AND (stock_order_intents.filled_qty < stock_order_intents.requested_qty - 0.000000001 OR t.status='closed')
               ))
               OR
	               (trade_id IS NULL AND (
	                   NOT EXISTS (SELECT 1 FROM orders o WHERE o.stock_intent_id=stock_order_intents.id)
                   OR COALESCE((SELECT SUM(COALESCE(o.filled_qty,0)) FROM orders o WHERE o.stock_intent_id=stock_order_intents.id),0)
                        < stock_order_intents.filled_qty - 0.000000001
                   OR (stock_order_intents.filled_qty >= stock_order_intents.requested_qty - 0.000000001 AND EXISTS (
                       SELECT 1 FROM orders o
                       JOIN trades t ON t.id=o.trade_id
                       WHERE o.stock_intent_id=stock_order_intents.id
	                         AND (COALESCE(o.filled_qty,0) < o.quantity - 0.000000001 OR t.status <> 'closed')
	                   ))
	               ))
	               OR (trade_id IS NULL AND idempotency_key LIKE 'analyst-exit:%' AND NOT EXISTS (
	                   SELECT 1 FROM signals signal
	                   WHERE signal.id::text=split_part(stock_order_intents.idempotency_key, ':', 2)
	                     AND signal.status IN ('executed','rejected')
	               ))
	           ))
           OR (status IN ('filled','cancelled','rejected') AND filled_qty > 0 AND order_type='stop' AND trade_id IS NOT NULL
               AND NOT EXISTS (
                   SELECT 1 FROM orders o
                   JOIN trades t ON t.id=o.trade_id
                   WHERE o.stock_intent_id=stock_order_intents.id
                     AND o.trade_id=stock_order_intents.trade_id
                     AND COALESCE(o.filled_qty,0) >= stock_order_intents.filled_qty - 0.000000001
	                     -- A terminal partial stop cannot protect a residual
	                     -- position. Keep it recoverable while its exit claim
	                     -- is still held, so a transient failure after applying
	                     -- the fill can retry ReleaseTradeExitClaim.
                     AND ((stock_order_intents.filled_qty < stock_order_intents.requested_qty - 0.000000001 AND t.status='open') OR t.status='closed')
               ))
           -- Same order-only completion rule for DAY/fractional standalone
           -- stops. done_for_day is normalized to cancelled before storage.
           OR (status IN ('cancelled','rejected') AND filled_qty <= 0.000000001
               AND order_type='stop' AND trade_id IS NOT NULL AND EXISTS (
                   SELECT 1 FROM orders o
                   WHERE o.stock_intent_id=stock_order_intents.id
                     AND o.status IN ('pending','partial')
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

// ListActiveStockEntryReservations returns every nonterminal standard-stock
// entry, including prepared/ambiguous intents and partial entries already
// attached to a trade. The projection is reconstructed directly from durable
// intents after restart.
func ListActiveStockEntryReservations(ctx context.Context) ([]ActiveStockEntryReservation, error) {
	rows, err := GetDB().Query(ctx, `
		SELECT id::text, idempotency_key, client_order_id,
		       COALESCE(broker_order_id,''), COALESCE(trade_id::text,''),
		       ticker, status, requested_qty, filled_qty,
		       GREATEST(requested_qty-filled_qty, 0),
		       COALESCE(NULLIF(request_payload->>'ReferencePrice','')::numeric, 0),
		       COALESCE(broker_error,''), submitted_at, created_at
		FROM stock_order_intents
		WHERE purpose='submit'
		  AND idempotency_key LIKE 'entry:%'
		  AND side='buy'
		  AND status IN ('pending','partial')
		  AND requested_qty-filled_qty > 0.000000001
		ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var reservations []ActiveStockEntryReservation
	for rows.Next() {
		var reservation ActiveStockEntryReservation
		if err := rows.Scan(
			&reservation.IntentID, &reservation.IdempotencyKey,
			&reservation.ClientOrderID, &reservation.BrokerOrderID,
			&reservation.TradeID, &reservation.Ticker, &reservation.Status,
			&reservation.RequestedQty, &reservation.FilledQty,
			&reservation.RemainingQty, &reservation.ReferencePrice,
			&reservation.BrokerError, &reservation.SubmittedAt,
			&reservation.CreatedAt,
		); err != nil {
			return nil, err
		}
		reservations = append(reservations, reservation)
	}
	return reservations, rows.Err()
}

// GetWorkingStockEntryReservations preserves the risk-check adapter while the
// typed projection above remains the source of reservation truth.
func GetWorkingStockEntryReservations(ctx context.Context) ([]Trade, error) {
	active, err := ListActiveStockEntryReservations(ctx)
	if err != nil {
		return nil, err
	}
	reservations := make([]Trade, 0, len(active))
	for _, reservation := range active {
		// Callers of this legacy risk adapter already supply durable owned
		// trades. An attached partial entry is represented by that trade; only
		// its remainder belongs in the typed admission ledger above, not as a
		// second synthetic position row here.
		if reservation.TradeID != "" {
			continue
		}
		reservations = append(reservations, Trade{
			Ticker: reservation.Ticker, Direction: "long",
			Quantity: reservation.RemainingQty, EntryPrice: reservation.ReferencePrice,
			Status: "open",
		})
	}
	return reservations, nil
}

func purposeOrSubmit(purpose string) string {
	if purpose == "" {
		return "submit"
	}
	return purpose
}
