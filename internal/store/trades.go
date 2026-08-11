package store

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"time"
)

const (
	ExitReasonStopLoss    = "stop_loss"
	ExitReasonTakeProfit  = "take_profit"
	ExitReasonSellSignal  = "sell_signal"
	ExitReasonTrailingATR = "trailing_atr"
	ExitReasonEMA9Close   = "ema9_close"
	ExitReasonRecovery    = "broker_recovery"
)

// ExitIntent is the durable policy decision that initiated an exit. Evidence
// is JSON so each policy can preserve its own numerical trigger inputs without
// growing the trades table for every new rule.
type ExitIntent struct {
	Reason   string
	Evidence json.RawMessage
}

// Trade mirrors a row from the trades table.
type Trade struct {
	ID                 string
	SignalID           string
	Ticker             string
	Direction          string // long | short — value the engine actually persists
	Strategy           string
	EntryPrice         float64
	ExitPrice          float64
	Quantity           float64
	StopLoss           float64
	TakeProfit         float64
	PnL                float64
	PnLPct             float64
	Status             string // open | exiting | closed | cancelled
	ModelProvider      string
	ModelName          string
	EntrySetup         string
	EntrySetupEvidence string
	OpenedAt           time.Time
	ClosedAt           time.Time
	CreatedAt          time.Time
	ExitClaimedAt      *time.Time
	ExitReason         string
	ExitEvidence       json.RawMessage
	ExitFilledAt       *time.Time
	// HighWaterMark is the running maximum price observed for an open trade,
	// used by the trailing-stop manager (#322). 0 when the manager has not
	// yet observed the trade (column is NULL in DB).
	HighWaterMark float64
}

// InsertTrade creates a new open trade record and returns its UUID.
func InsertTrade(ctx context.Context, t Trade) (string, error) {
	entrySetup := t.EntrySetup
	if entrySetup == "" {
		entrySetup = EntrySetupStandard
	}
	var id string
	err := GetDB().QueryRow(ctx, `
		INSERT INTO trades (
			signal_id, ticker, direction, strategy,
			entry_price, quantity, stop_loss, take_profit,
			status, model_provider, model_name, entry_setup, entry_setup_evidence
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,'open',$9,$10,$11,$12)
		RETURNING id`,
		nullableString(t.SignalID), t.Ticker, t.Direction, t.Strategy,
		t.EntryPrice, t.Quantity,
		nullableFloat(t.StopLoss), nullableFloat(t.TakeProfit),
		nullableString(t.ModelProvider), nullableString(t.ModelName),
		entrySetup, nullableString(t.EntrySetupEvidence),
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
	return ClaimTradeExitWithIntent(ctx, id, ExitIntent{})
}

// ClaimTradeExitWithIntent atomically transfers exit ownership and persists
// why it was taken. The intent therefore survives broker latency and process
// restarts until the exit reconciler finishes the trade.
func ClaimTradeExitWithIntent(ctx context.Context, id string, intent ExitIntent) (bool, error) {
	tag, err := GetDB().Exec(ctx, `
		UPDATE trades
		SET status='exiting', exit_claimed_at=NOW(),
		    exit_reason=NULLIF($2,''), exit_evidence=$3::jsonb
		WHERE id=$1 AND status='open'`, id, intent.Reason, nullableRawJSON(intent.Evidence))
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

func ReleaseTradeExitClaim(ctx context.Context, id string) error {
	_, err := GetDB().Exec(ctx, `
		UPDATE trades
		SET status='open', exit_claimed_at=NULL,
		    exit_reason=NULL, exit_evidence=NULL, exit_filled_at=NULL
		WHERE id=$1 AND status='exiting'`, id)
	return err
}

func nullableRawJSON(v json.RawMessage) any {
	if len(v) == 0 || string(v) == "null" {
		return nil
	}
	// Bind as text: a bare []byte is encoded by pgx as bytea, which cannot be
	// cast to jsonb. The explicit SQL cast validates this text as JSON.
	return string(v)
}

func UpdateExitingTradeQuantity(ctx context.Context, id string, quantity float64) error {
	_, err := GetDB().Exec(ctx, `
		UPDATE trades SET quantity=$1
		WHERE id=$2 AND status='exiting'`, quantity, id)
	return err
}

// TradeRealization is the cumulative realized result across every exit slice
// assigned to a trade, including cancellation-race partials.
type TradeRealization struct {
	TradeID      string
	Ticker       string
	Direction    string
	EntryPrice   float64
	ExitPrice    float64
	PnL          float64
	PnLPct       float64
	ExitQty      float64
	Remaining    float64
	Status       string
	ExitReason   string
	ExitEvidence json.RawMessage
	ExitFilledAt *time.Time
	JustClosed   bool
}

// CloseTradeRealized adds the trade's currently-held quantity as the final
// realized slice, then closes it with a fill-weighted exit price and cumulative
// P&L derived from the locked trade row.
func CloseTradeRealized(
	ctx context.Context,
	id string,
	exitPrice float64,
) (TradeRealization, error) {
	return CloseTradeRealizedWithIntent(ctx, id, exitPrice, time.Time{}, ExitIntent{})
}

// CloseTradeRealizedWithIntent closes a synchronously-filled trade while
// preserving the policy decision and broker execution time. Existing intent
// from ClaimTradeExitWithIntent wins when callers pass an empty value.
func CloseTradeRealizedWithIntent(
	ctx context.Context,
	id string,
	exitPrice float64,
	filledAt time.Time,
	intent ExitIntent,
) (TradeRealization, error) {
	var result TradeRealization
	err := GetDB().QueryRow(ctx, `
		UPDATE trades
		SET realized_exit_qty = realized_exit_qty + quantity,
		    realized_exit_proceeds = realized_exit_proceeds + quantity * $1,
		    realized_pnl = realized_pnl + CASE
		        WHEN direction = 'short'
		        THEN (entry_price - $1) * quantity
		        ELSE ($1 - entry_price) * quantity
		    END,
		    exit_price = CASE
		        WHEN realized_exit_qty + quantity > 0
		        THEN (realized_exit_proceeds + quantity * $1)
		             / (realized_exit_qty + quantity)
		        ELSE $1
		    END,
		    pnl = realized_pnl + CASE
		        WHEN direction = 'short'
		        THEN (entry_price - $1) * quantity
		        ELSE ($1 - entry_price) * quantity
		    END,
		    pnl_pct = CASE
		        WHEN entry_price > 0 AND realized_exit_qty + quantity > 0
		        THEN (
		            realized_pnl + CASE
		                WHEN direction = 'short'
		                THEN (entry_price - $1) * quantity
		                ELSE ($1 - entry_price) * quantity
		            END
		        )
		             / (entry_price * (realized_exit_qty + quantity)) * 100
		        ELSE 0
		    END,
		    quantity = realized_exit_qty + quantity,
		    status = 'closed',
		    closed_at = NOW(),
		    exit_reason = COALESCE(NULLIF($3,''), exit_reason),
		    exit_evidence = COALESCE($4::jsonb, exit_evidence),
		    exit_filled_at = CASE
		        WHEN $5::timestamptz IS NULL THEN exit_filled_at
		        WHEN exit_filled_at IS NULL THEN $5::timestamptz
		        ELSE GREATEST(exit_filled_at, $5::timestamptz)
		    END,
		    exit_claimed_at = NULL
		WHERE id = $2 AND status IN ('open','exiting')
		RETURNING id, ticker, direction, entry_price,
		          exit_price, pnl, pnl_pct, realized_exit_qty,
		          0::NUMERIC, status, COALESCE(exit_reason,''),
		          exit_evidence, exit_filled_at`,
		exitPrice, id, intent.Reason, nullableRawJSON(intent.Evidence), nullableTime(filledAt),
	).Scan(
		&result.TradeID, &result.Ticker, &result.Direction, &result.EntryPrice,
		&result.ExitPrice, &result.PnL, &result.PnLPct,
		&result.ExitQty, &result.Remaining, &result.Status,
		&result.ExitReason, &result.ExitEvidence, &result.ExitFilledAt,
	)
	result.JustClosed = err == nil
	return result, err
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
			    quantity=GREATEST(0::NUMERIC, $2::NUMERIC-$3::NUMERIC),
			    realized_pnl=CASE
			        WHEN direction='short'
			        THEN (
			            COALESCE(NULLIF($1::NUMERIC,0::NUMERIC),entry_price)
			            * realized_exit_qty
			        ) - realized_exit_proceeds
			        ELSE realized_exit_proceeds - (
			            COALESCE(NULLIF($1::NUMERIC,0::NUMERIC),entry_price)
			            * realized_exit_qty
			        )
			    END
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
//
// The ::NUMERIC casts are load-bearing. A bare `NULLIF($1, 0)` makes Postgres
// infer the parameter's type from the integer literal 0, so both placeholders
// resolve to `integer` and pgx encodes the float64 stop into an int4 — silently
// dropping the cents. Every trailing write landed as a whole dollar
// (stop_loss 8.50 → 8, high_water_mark 9.13 → 9), which both understated the
// stop against the broker's real resting leg and stopped the high-water mark
// from ever accumulating. Casting pins the parameters to NUMERIC, matching the
// columns.
func UpdateTradeTrailing(ctx context.Context, id string, stopLoss, highWaterMark float64) error {
	_, err := GetDB().Exec(ctx, `
		UPDATE trades
		SET stop_loss = COALESCE(NULLIF($1::NUMERIC, 0::NUMERIC), stop_loss),
		    high_water_mark = COALESCE(NULLIF($2::NUMERIC, 0::NUMERIC), high_water_mark)
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
		       COALESCE(parent_order_id,''), filled_at,
		       COALESCE(exit_reason,''), exit_evidence,
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

// UpdateOrderStopReplacement atomically repoints an existing protective order
// row and preserves any fill returned with the broker replacement. A terminal
// order may be purged before the next sweep, so its execution truth must be
// durable in the same write as its new broker id. If a concurrent reconciler
// applied a fill while the broker replacement was in flight, adopt the live
// replacement using the freshly locked trade remainder instead of rejecting
// the write and leaving the position without durable stop coverage.
func UpdateOrderStopReplacement(
	ctx context.Context,
	orderID, expectedBrokerOrderID, newBrokerOrderID string,
	expectedFilledQty, expectedTradeQuantity, newStopPrice, newQuantity float64,
	status string,
	filledPrice, filledQty float64,
	filledAt time.Time,
	intent ExitIntent,
) error {
	tx, err := GetDB().Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var actualBrokerOrderID string
	var actualFilledQty, actualTradeQuantity float64
	if err := tx.QueryRow(ctx, `
		SELECT COALESCE(o.broker_order_id,''), COALESCE(o.filled_qty,0), t.quantity
		FROM trades t
		JOIN orders o ON o.trade_id=t.id
		WHERE o.id=$1
		FOR UPDATE OF t, o`, orderID).Scan(
		&actualBrokerOrderID, &actualFilledQty, &actualTradeQuantity,
	); err != nil {
		return err
	}
	if actualBrokerOrderID != expectedBrokerOrderID {
		return fmt.Errorf("%w: stop row %s or its trade changed while broker replacement was in flight", ErrOrderBrokerIDChanged, orderID)
	}
	if math.Abs(actualFilledQty-expectedFilledQty) > 1e-9 ||
		math.Abs(actualTradeQuantity-expectedTradeQuantity) > 1e-9 {
		if actualTradeQuantity <= 1e-9 {
			return fmt.Errorf("%w: trade for stop row %s has no remaining position after concurrent fill", ErrOrderBrokerIDChanged, orderID)
		}
		// Replacing a partially-filled broker order preserves its unfilled
		// remainder. Mirror that remainder here; the old order's applied fill
		// is already represented in the trade's cumulative realization fields.
		newQuantity = actualTradeQuantity
	}
	if _, err = tx.Exec(ctx, `
		UPDATE orders
		SET stop_price = $1,
		    broker_order_id = COALESCE(NULLIF($2, ''), broker_order_id),
		    quantity = $3,
		    status = $4,
		    filled_price = NULLIF($5::NUMERIC,0::NUMERIC),
		    filled_qty = $6::NUMERIC,
		    exit_fill_applied_qty = 0,
		    filled_at = $7::timestamptz,
		    exit_reason = NULLIF($8, ''),
		    exit_evidence = $9::jsonb,
		    updated_at = NOW()
		WHERE id = $10`,
		newStopPrice, newBrokerOrderID, newQuantity, status,
		filledPrice, filledQty, nullableTime(filledAt), intent.Reason,
		nullableRawJSON(intent.Evidence), orderID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func CloseTrade(ctx context.Context, id string, exitPrice, pnl, pnlPct float64) error {
	_, err := CloseTradeRealized(ctx, id, exitPrice)
	return err
}

// FinalizeTradeExit closes an exiting trade after ApplyPartialExit has
// assigned every sold share to the cumulative realized totals.
func FinalizeTradeExit(ctx context.Context, id string) (TradeRealization, error) {
	var result TradeRealization
	err := GetDB().QueryRow(ctx, `
		UPDATE trades
		SET exit_price = realized_exit_proceeds / realized_exit_qty,
		    pnl = realized_pnl,
		    pnl_pct = CASE
		        WHEN entry_price > 0
		        THEN realized_pnl / (entry_price * realized_exit_qty) * 100
		        ELSE 0
		    END,
		    quantity = realized_exit_qty,
		    status = 'closed',
		    closed_at = COALESCE(closed_at, NOW()),
		    exit_claimed_at = NULL
		WHERE id = $1
		  AND status = 'exiting'
		  AND quantity <= 0.000000001
		  AND realized_exit_qty > 0
		RETURNING id, ticker, direction, entry_price,
		          exit_price, pnl, pnl_pct, realized_exit_qty,
		          0::NUMERIC, status, COALESCE(exit_reason,''),
		          exit_evidence, exit_filled_at`,
		id,
	).Scan(
		&result.TradeID, &result.Ticker, &result.Direction, &result.EntryPrice,
		&result.ExitPrice, &result.PnL, &result.PnLPct,
		&result.ExitQty, &result.Remaining, &result.Status,
		&result.ExitReason, &result.ExitEvidence, &result.ExitFilledAt,
	)
	result.JustClosed = err == nil
	return result, err
}

// ApplyRecoveredExit records broker-position-delta recovery when no durable
// order row survived. Partial recovery reopens the remaining position; a full
// recovery finalizes the cumulative trade result.
func ApplyRecoveredExit(
	ctx context.Context,
	id string,
	exitPrice, soldQty float64,
) (TradeRealization, error) {
	var result TradeRealization
	err := GetDB().QueryRow(ctx, `
		WITH target AS (
		    SELECT id, LEAST(quantity, GREATEST(0, $2::NUMERIC)) AS sold
		    FROM trades
		    WHERE id = $1 AND status = 'exiting'
		    FOR UPDATE
		)
		UPDATE trades t
		SET realized_exit_qty = t.realized_exit_qty + target.sold,
		    realized_exit_proceeds = t.realized_exit_proceeds + target.sold * $3,
		    realized_pnl = t.realized_pnl + CASE
		        WHEN t.direction = 'short'
		        THEN (t.entry_price - $3) * target.sold
		        ELSE ($3 - t.entry_price) * target.sold
		    END,
		    quantity = CASE
		        WHEN t.quantity - target.sold <= 0.000000001
		        THEN t.realized_exit_qty + target.sold
		        ELSE t.quantity - target.sold
		    END,
		    exit_price = CASE
		        WHEN t.quantity - target.sold <= 0.000000001
		        THEN (t.realized_exit_proceeds + target.sold * $3)
		             / NULLIF(t.realized_exit_qty + target.sold, 0)
		        ELSE t.exit_price
		    END,
		    pnl = CASE
		        WHEN t.quantity - target.sold <= 0.000000001
		        THEN t.realized_pnl + CASE
		            WHEN t.direction = 'short'
		            THEN (t.entry_price - $3) * target.sold
		            ELSE ($3 - t.entry_price) * target.sold
		        END
		        ELSE t.pnl
		    END,
		    pnl_pct = CASE
		        WHEN t.quantity - target.sold <= 0.000000001
		             AND t.entry_price > 0
		             AND t.realized_exit_qty + target.sold > 0
		        THEN (
		            t.realized_pnl + CASE
		                WHEN t.direction = 'short'
		                THEN (t.entry_price - $3) * target.sold
		                ELSE ($3 - t.entry_price) * target.sold
		            END
		        ) / (t.entry_price * (t.realized_exit_qty + target.sold)) * 100
		        ELSE t.pnl_pct
		    END,
		    status = CASE
		        WHEN t.quantity - target.sold <= 0.000000001
		        THEN 'closed'::trade_status
		        ELSE 'open'::trade_status
		    END,
		    closed_at = CASE
		        WHEN t.quantity - target.sold <= 0.000000001
		        THEN COALESCE(t.closed_at, NOW())
		        ELSE t.closed_at
		    END,
		    exit_reason = COALESCE(NULLIF(t.exit_reason, ''), $4),
		    exit_evidence = COALESCE(
		        t.exit_evidence,
		        jsonb_build_object(
		            'source', 'broker_position_delta',
		            'recovered_quantity', target.sold,
		            'recovery_price', $3::NUMERIC
		        )
		    ),
		    exit_claimed_at = NULL
		FROM target
		WHERE t.id = target.id
		RETURNING t.id, t.ticker, t.direction, t.entry_price,
		          COALESCE(
		              t.realized_exit_proceeds / NULLIF(t.realized_exit_qty,0),
		              0
		          ),
		          t.realized_pnl,
		          CASE
		              WHEN t.entry_price > 0 AND t.realized_exit_qty > 0
		              THEN t.realized_pnl
		                   / (t.entry_price * t.realized_exit_qty) * 100
		              ELSE 0
		          END,
		          t.realized_exit_qty,
		          CASE WHEN t.status='closed' THEN 0 ELSE t.quantity END,
		          t.status, COALESCE(t.exit_reason,''),
		          t.exit_evidence, t.exit_filled_at`,
		id, soldQty, exitPrice, ExitReasonRecovery,
	).Scan(
		&result.TradeID, &result.Ticker, &result.Direction, &result.EntryPrice,
		&result.ExitPrice, &result.PnL, &result.PnLPct,
		&result.ExitQty, &result.Remaining, &result.Status,
		&result.ExitReason, &result.ExitEvidence, &result.ExitFilledAt,
	)
	result.JustClosed = err == nil && result.Status == "closed"
	return result, err
}

// UpdateTradeExit corrects legacy already-closed exits whose original broker
// fill was asynchronous. Exiting trades must use ApplyPartialExit followed by
// FinalizeTradeExit so cumulative realized slices cannot be discarded.
func UpdateTradeExit(ctx context.Context, id string, exitPrice, pnl, pnlPct float64) error {
	return UpdateTradeExitAt(ctx, id, exitPrice, pnl, pnlPct, time.Time{})
}

// UpdateTradeExitAt corrects a legacy already-closed asynchronous exit while
// retaining closed_at as the database finalization timestamp and recording the
// broker's execution timestamp separately when available.
func UpdateTradeExitAt(ctx context.Context, id string, exitPrice, pnl, pnlPct float64, filledAt time.Time) error {
	_, err := GetDB().Exec(ctx, `
		UPDATE trades
		SET exit_price=$1, pnl=$2, pnl_pct=$3,
		    realized_pnl=$2,
		    realized_exit_proceeds=realized_exit_qty*$1,
		    exit_filled_at=CASE
		        WHEN $5::timestamptz IS NULL THEN exit_filled_at
		        WHEN exit_filled_at IS NULL THEN $5::timestamptz
		        ELSE GREATEST(exit_filled_at,$5::timestamptz)
		    END,
		    status='closed',
		    closed_at=COALESCE(closed_at, NOW()),
		    exit_claimed_at=NULL
		WHERE id=$4 AND status='closed'`,
		exitPrice, pnl, pnlPct, id, nullableTime(filledAt))
	return err
}

// UpdateTradeExitFilledAt enriches an already-correct closed trade without
// recalculating its lifecycle P&L from one order lot.
func UpdateTradeExitFilledAt(ctx context.Context, id string, filledAt time.Time) error {
	if filledAt.IsZero() {
		return nil
	}
	_, err := GetDB().Exec(ctx, `
		UPDATE trades
		SET exit_filled_at=CASE
		    WHEN exit_filled_at IS NULL THEN $2::timestamptz
		    ELSE GREATEST(exit_filled_at,$2::timestamptz)
		END
		WHERE id=$1 AND status='closed'`, id, filledAt.UTC())
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
	OrderID           string
	BrokerOrderID     string
	Ticker            string
	OrderStatus       string // broker order status last durably recorded in the DB
	TradeID           string
	Direction         string // long | short
	EntryPrice        float64
	Quantity          float64 // current trade quantity
	OrderQuantity     float64 // lot quantity assigned to this broker order row
	LimitPrice        float64 // fallback when a terminal limit fill omits price
	StopPrice         float64 // fallback when a terminal stop fill omits price
	RecordedFill      float64 // fill already durably assigned to this lot
	RecordedPrice     float64 // fill price already durably assigned to this lot
	RecordedFilledAt  *time.Time
	AggregateFill     float64 // fill assigned across every row sharing the broker id
	ExitPrice         float64 // exit price currently recorded on the trade row
	TradeStatus       string  // closed (price correction) | exiting (finalization)
	ExitReason        string
	ExitEvidence      json.RawMessage
	IsStopLoss        bool
	IsTakeProfit      bool
	OrderExitReason   string
	OrderExitEvidence json.RawMessage
}

// DurableExitOrderOwnership is the complete broker-order identity set that
// can explain an in-flight exit. Orderless recovery must use an unbounded
// ownership snapshot rather than the reconciler's limited work batch; a
// durable order just outside that batch must never be attributed to another
// trade that happens to share its ticker and side.
type DurableExitOrderOwnership struct {
	TradeID       string
	BrokerOrderID string
	Ticker        string
	Side          string
	TradeStatus   string
	TradeQuantity float64
	ExitClaimedAt *time.Time
	// ClaimOwned is true when this order was created by the current exit claim,
	// or when an unapplied terminal fill can account for shares already missing
	// at the broker. Settled pre-existing protective stops remain known broker
	// IDs but do not strand orderless recovery.
	ClaimOwned bool
}

func GetDurableExitOrderOwnership(ctx context.Context) ([]DurableExitOrderOwnership, error) {
	rows, err := GetDB().Query(ctx, `
		SELECT DISTINCT t.id, o.broker_order_id, o.ticker, o.side, t.status,
		       t.quantity, t.exit_claimed_at,
		       CASE
		         WHEN t.status='closed' THEN TRUE
		         WHEN t.status='exiting'
		              AND o.status IN ('filled','cancelled','rejected')
		              AND (o.created_at >= t.exit_claimed_at OR
		                   COALESCE(o.filled_qty,0) > COALESCE(o.exit_fill_applied_qty,0)) THEN TRUE
		         WHEN t.status='exiting'
		              AND o.status IN ('pending','partial')
		              AND (o.created_at >= t.exit_claimed_at OR COALESCE(o.filled_qty,0) > 0) THEN TRUE
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
		          (o.status IN ('pending','partial') AND
		           (o.created_at >= t.exit_claimed_at OR COALESCE(o.filled_qty,0) > 0
		            OR o.is_stop_loss OR o.is_take_profit))
		       OR (o.status IN ('filled','cancelled','rejected') AND
		           (o.status='filled' OR COALESCE(o.filled_qty,0) > 0 OR
		            o.created_at >= t.exit_claimed_at))
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
			&owner.TradeID, &owner.BrokerOrderID, &owner.Ticker, &owner.Side, &owner.TradeStatus,
			&owner.TradeQuantity, &owner.ExitClaimedAt,
			&owner.ClaimOwned,
		); err != nil {
			return nil, err
		}
		out = append(out, owner)
	}
	return out, rows.Err()
}

// GetPendingExitOrders returns pending/partial exit orders plus terminal exit
// rows eligible for reconciliation whose trade is closed or durably exiting
// and whose side opposes direction —
// i.e. the exit order for that trade. This shape covers every close path
// (stop/TP monitor, trailing-stop market exits) regardless of the
// is_stop_loss/is_take_profit flags. Entry orders (side matching direction)
// and working broker stops on open trades are excluded by construction.
// 'partial' is included because PlaceOrder can return partially_filled and
// the row must stay sweepable until the broker reports the fill complete.
// Filled terminal rows are included regardless of claim time because a
// protective stop may have filled just before the worker claimed the trade.
// Working protective rows remain eligible after a concurrent exit claim even
// when they predate it: a stop can fill while the new owner is cancelling it.
// Pre-claim terminal rows remain eligible only with unapplied durable fill
// evidence; stale zero-fill cancellations must not suppress orderless recovery.
func GetPendingExitOrders(ctx context.Context, limit int) ([]PendingExitOrder, error) {
	rows, err := GetDB().Query(ctx, `
		WITH selected_trades AS (
		  SELECT t.id, MIN(o.created_at) AS first_order_at
		  FROM orders o
		  JOIN trades t ON t.id=o.trade_id
		  WHERE (
		          (t.status='closed' AND o.status IN ('pending','partial'))
		       OR (t.status='exiting' AND (
		            (o.status IN ('pending','partial') AND
		             (o.created_at >= t.exit_claimed_at OR COALESCE(o.filled_qty,0) > 0 OR
		              o.is_stop_loss OR o.is_take_profit))
		         OR (o.status IN ('filled','cancelled','rejected') AND
		             (COALESCE(o.filled_qty,0) > COALESCE(o.exit_fill_applied_qty,0) OR
		              o.created_at >= t.exit_claimed_at OR
		              (t.quantity <= 0.000000001 AND COALESCE(o.filled_qty,0) > 0)))
		          ))
		        )
		    AND COALESCE(o.broker_order_id,'') <> ''
		    AND ((t.direction='long' AND o.side='sell')
		      OR (t.direction='short' AND o.side='buy'))
		  GROUP BY t.id
		  ORDER BY first_order_at
		  LIMIT $1
		)
		SELECT o.id, o.broker_order_id, o.ticker, o.status,
		       t.id, t.direction, t.entry_price, t.quantity,
		       o.quantity, COALESCE(o.limit_price,0),
		       COALESCE(o.stop_price,0), COALESCE(o.filled_qty,0),
		       COALESCE(o.filled_price,0), o.filled_at,
		       COALESCE((
		         SELECT SUM(COALESCE(allocated.filled_qty,0))
		         FROM orders allocated
		         WHERE allocated.broker_order_id=o.broker_order_id
		       ),0),
		       COALESCE(t.exit_price,0), t.status,
		       COALESCE(t.exit_reason,''), t.exit_evidence,
		       o.is_stop_loss, o.is_take_profit,
		       COALESCE(o.exit_reason,''), o.exit_evidence
		FROM orders o
		JOIN trades t ON t.id = o.trade_id
		JOIN selected_trades selected ON selected.id=t.id
		WHERE (
		        (t.status='closed' AND o.status IN ('pending','partial'))
		     OR (t.status='exiting' AND (
		          (o.status IN ('pending','partial') AND
		           (o.created_at >= t.exit_claimed_at OR COALESCE(o.filled_qty,0) > 0 OR
		            o.is_stop_loss OR o.is_take_profit))
		       OR (o.status IN ('filled','cancelled','rejected') AND
		           (COALESCE(o.filled_qty,0) > COALESCE(o.exit_fill_applied_qty,0) OR
		            o.created_at >= t.exit_claimed_at OR
		            (t.quantity <= 0.000000001 AND COALESCE(o.filled_qty,0) > 0)))
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
		if err := rows.Scan(&p.OrderID, &p.BrokerOrderID, &p.Ticker, &p.OrderStatus,
			&p.TradeID, &p.Direction, &p.EntryPrice, &p.Quantity,
			&p.OrderQuantity, &p.LimitPrice, &p.StopPrice,
			&p.RecordedFill, &p.RecordedPrice, &p.RecordedFilledAt,
			&p.AggregateFill,
			&p.ExitPrice, &p.TradeStatus, &p.ExitReason, &p.ExitEvidence,
			&p.IsStopLoss, &p.IsTakeProfit, &p.OrderExitReason, &p.OrderExitEvidence); err != nil {
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
		       entry_setup, COALESCE(entry_setup_evidence,''),
		       opened_at, COALESCE(closed_at, opened_at), created_at,
		       COALESCE(high_water_mark,0), exit_claimed_at,
		       COALESCE(exit_reason,''), exit_evidence, exit_filled_at
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
		       entry_setup, COALESCE(entry_setup_evidence,''),
		       opened_at, COALESCE(closed_at, opened_at), created_at,
		       COALESCE(high_water_mark,0), exit_claimed_at,
		       COALESCE(exit_reason,''), exit_evidence, exit_filled_at
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
		       entry_setup, COALESCE(entry_setup_evidence,''),
		       opened_at, COALESCE(closed_at, opened_at), created_at,
		       COALESCE(high_water_mark,0), exit_claimed_at,
		       COALESCE(exit_reason,''), exit_evidence, exit_filled_at
		FROM trades t
		WHERE t.status='exiting'
		  AND t.exit_claimed_at IS NOT NULL
		  AND t.exit_claimed_at <= NOW() - INTERVAL '2 minutes'
		  AND NOT EXISTS (
		    SELECT 1 FROM orders o
		    WHERE o.trade_id=t.id
		      AND COALESCE(o.broker_order_id,'') <> ''
		      AND ((t.direction='long' AND o.side='sell')
		        OR (t.direction='short' AND o.side='buy'))
		      AND o.created_at >= t.exit_claimed_at
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
		       entry_setup, COALESCE(entry_setup_evidence,''),
		       opened_at, COALESCE(closed_at, opened_at), created_at,
		       COALESCE(high_water_mark,0), exit_claimed_at,
		       COALESCE(exit_reason,''), exit_evidence, exit_filled_at
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
		       entry_setup, COALESCE(entry_setup_evidence,''),
		       opened_at, COALESCE(closed_at, opened_at), created_at,
		       COALESCE(high_water_mark,0), exit_claimed_at,
		       COALESCE(exit_reason,''), exit_evidence, exit_filled_at
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
			&t.EntrySetup, &t.EntrySetupEvidence,
			&t.OpenedAt, &t.ClosedAt, &t.CreatedAt,
			&t.HighWaterMark, &t.ExitClaimedAt,
			&t.ExitReason, &t.ExitEvidence, &t.ExitFilledAt,
		); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
