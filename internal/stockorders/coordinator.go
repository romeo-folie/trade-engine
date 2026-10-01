// Package stockorders makes standard stock broker submissions durable and
// idempotent. It deliberately wraps only the normal stock engine; ORB/options,
// IBKR, and paper simulation retain their own lifecycles.
package stockorders

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"time"

	"github.com/romeo-folie/trade-engine/internal/execution"
	"github.com/romeo-folie/trade-engine/internal/store"
)

const clientIDPrefix = "te-stock-"

// ErrDurableStoreUnavailable prevents a standard-stock broker mutation when
// its pre-submit intent cannot be made durable. The engine may still start in
// read-only/degraded mode without a database; it must never bypass this guard
// and place an untracked stock order.
var ErrDurableStoreUnavailable = errors.New("stockorders: durable store unavailable")

// Coordinator is an execution.Executor that persists a pre-submit intent and
// uses Alpaca's client_order_id lookup before any replay after an ambiguous
// response. It is intentionally installed inside ReadOnlyExecutor, so dry-run
// rejects mutation before an intent can be written.
// RecoveryObserver restores in-process effects that cannot be reconstructed
// from the durable projections alone. It is optional so the coordinator stays
// usable by non-engine callers and tests.
type RecoveryObserver interface {
	RecoveredStockEntry(context.Context, string)
	RecoveredStockExit(context.Context, store.TradeRealization)
	PreparedStockEntryNeedsAdmission(context.Context, string)
}

// ProtectionRecoveryObserver is intentionally optional so recovery can notify
// an engine about a pre-existing protected-entry projection without replaying
// its one-time trade-open side effects.
type ProtectionRecoveryObserver interface {
	RecoveredStockEntryProtection(context.Context, string)
}

// DiscoveryTradeLeaseObserver renews any still-live source residency associated
// with an actually filled trade. It is invoked on recovery replays as well, so
// a crash after the trade projection cannot permanently skip the renewal.
type DiscoveryTradeLeaseObserver interface {
	RenewRecoveredEntryDiscoveryResidency(context.Context, string, time.Time) error
}

type Coordinator struct {
	inner    execution.Executor
	observer RecoveryObserver
}

func New(inner execution.Executor) *Coordinator { return &Coordinator{inner: inner} }

// SetRecoveryObserver wires the engine lifecycle effects for projections that
// recovery completes after a process restart.
func (c *Coordinator) SetRecoveryObserver(observer RecoveryObserver) { c.observer = observer }

func (c *Coordinator) recoveredClose(ctx context.Context, tradeID string) error {
	return store.WithTradeOrderLock(tradeID, func() error {
		return c.recoveredCloseLocked(ctx, tradeID)
	})
}

// recoveredCloseLocked completes a terminal stop while the caller already
// owns the per-trade lifecycle lock. Recovery and trailing replacement both
// enter that lock before applying a protective fill; acquiring it again here
// would deadlock the same goroutine.
func (c *Coordinator) recoveredCloseLocked(ctx context.Context, tradeID string) error {
	realized, err := store.FinalizeTradeExit(ctx, tradeID)
	if err != nil {
		return err
	}
	if realized.JustClosed && c.observer != nil {
		c.observer.RecoveredStockExit(ctx, realized)
	}
	return nil
}

func requireDurableStore() error {
	if !store.IsConnected() {
		return ErrDurableStoreUnavailable
	}
	return nil
}

// DurableStockOrderIntents marks this executor for callers that must not
// continue with a legacy, unlinked projection when the post-submit intent
// lookup is unavailable. Paper executors intentionally do not implement it.
func (*Coordinator) DurableStockOrderIntents() bool { return true }

func (c *Coordinator) BrokerName() string {
	if named, ok := c.inner.(interface{ BrokerName() string }); ok {
		return named.BrokerName()
	}
	return ""
}

func (c *Coordinator) PlaceOrder(ctx context.Context, req execution.OrderRequest) (execution.OrderResult, error) {
	if err := requireDurableStore(); err != nil {
		return execution.OrderResult{}, err
	}
	if req.IntentKey == "" {
		return execution.OrderResult{}, errors.New("standard stock order missing durable intent key")
	}
	if req.ClientOrderID == "" {
		req.ClientOrderID = clientIDForKey(req.IntentKey)
	}
	payload, err := json.Marshal(req)
	if err != nil {
		return execution.OrderResult{}, fmt.Errorf("marshal stock order intent: %w", err)
	}
	hash := fmt.Sprintf("%x", sha256.Sum256(payload))
	if existing, err := store.GetStockOrderIntentByKey(ctx, req.IntentKey); err != nil {
		return execution.OrderResult{}, err
	} else if existing != nil {
		// Terminal zero-fill generations are retired. Their original payload
		// may legitimately differ from the next market-exit tick's reference
		// price or residual quantity, so report the rotation sentinel before
		// enforcing the exact-payload replay contract.
		if isTerminalZeroFill(existing.Status, existing.FilledQty) {
			return execution.OrderResult{}, fmt.Errorf("%w: stock intent %s is %s", execution.ErrTerminalZeroFillPlacement, existing.ClientOrderID, existing.Status)
		}
		if existing.PayloadHash != hash {
			return execution.OrderResult{}, fmt.Errorf("stock order intent key %q reused with different payload", req.IntentKey)
		}
		var exact execution.OrderRequest
		if err := json.Unmarshal(existing.RequestPayload, &exact); err != nil {
			return execution.OrderResult{}, err
		}
		return c.submit(ctx, *existing, exact)
	}
	intent := store.StockOrderIntent{
		ID: intentUUID(), ClientOrderID: req.ClientOrderID, TradeID: req.TradeID,
		IdempotencyKey: req.IntentKey,
		Ticker:         req.Ticker, Side: string(req.Side), OrderType: string(req.Type),
		RequestedQty: req.Quantity, RequestPayload: payload, PayloadHash: hash, Status: "pending",
	}
	if err := store.CreateStockOrderIntent(ctx, intent); err != nil {
		return execution.OrderResult{}, fmt.Errorf("persist stock order intent before submit: %w", err)
	}
	return c.submit(ctx, intent, req)
}

func (c *Coordinator) submit(ctx context.Context, intent store.StockOrderIntent, req execution.OrderRequest) (execution.OrderResult, error) {
	lookup, hasLookup := c.inner.(execution.ClientOrderLookup)
	if hasLookup {
		if prior, err := lookup.GetOrderByClientID(ctx, intent.ClientOrderID); err == nil {
			_ = c.record(ctx, intent, "lookup", "found", prior, "", false)
			// Recovery must project an already-known terminal entry to close its
			// order-only row. Fresh callers, by contrast, must rotate before a
			// terminal zero-fill client id can satisfy their request.
			if !isRecoveryContext(ctx) && isTerminalZeroFillForRequest(req, prior) {
				return execution.OrderResult{}, fmt.Errorf("%w: broker client order %s is %s", execution.ErrTerminalZeroFillPlacement, intent.ClientOrderID, prior.Status)
			}
			return c.finishSubmit(ctx, intent, req, prior)
		} else if !errors.Is(err, execution.ErrOrderNotFound) {
			_ = c.record(ctx, intent, "lookup", "error", execution.OrderResult{}, err.Error(), false)
			return execution.OrderResult{}, fmt.Errorf("lookup stock order client id before submit: %w", err)
		} else if execution.IsTerminalOrderStatusForTimeInForce(intent.Status, req.TimeInForce) && intent.BrokerOrderID != "" {
			// Alpaca can purge a terminal client-id record before recovery runs.
			// The durable terminal snapshot is sufficient to finish its missing
			// projections, but never use a 404 to submit a second broker order.
			prior := execution.OrderResult{BrokerOrderID: intent.BrokerOrderID, Status: intent.Status, FilledPrice: intent.FilledPrice, FilledQty: intent.FilledQty}
			_ = c.record(ctx, intent, "lookup", "purged_terminal", prior, "", false)
			return c.finishSubmit(ctx, intent, req, prior)
		} else if intent.BrokerOrderID != "" || (intent.SubmittedAt != nil && !isRecoveryContext(ctx)) {
			_ = c.record(ctx, intent, "lookup", "missing_after_submit", execution.OrderResult{}, err.Error(), false)
			return execution.OrderResult{}, fmt.Errorf("stock order %s was submitted but its client id is unavailable", intent.ClientOrderID)
		}
	}

	res, err := c.inner.PlaceOrder(ctx, req)
	if err == nil {
		_ = c.record(ctx, intent, "submit", "accepted", res, "", true)
		return c.finishSubmit(ctx, intent, req, res)
	}
	if execution.IsDefinitiveOrderRejection(err) && !execution.IsDuplicateClientOrderError(err) {
		if recordErr := c.record(ctx, intent, "submit", "rejected", execution.OrderResult{Status: "rejected"}, err.Error(), true); recordErr != nil {
			return execution.OrderResult{}, fmt.Errorf("persist definitive stock order rejection: %w", recordErr)
		}
		return execution.OrderResult{}, terminalZeroFillRejectionError("stock order definitively rejected", err)
	}
	_ = c.record(ctx, intent, "submit", "ambiguous", execution.OrderResult{}, err.Error(), true)
	// A response failure may follow broker acceptance. Query by the durable id
	// before the one exact-payload replay; never use ticker/side attribution.
	if hasLookup {
		if prior, lookupErr := lookup.GetOrderByClientID(ctx, intent.ClientOrderID); lookupErr == nil {
			_ = c.record(ctx, intent, "lookup_after_ambiguous_submit", "found", prior, "", true)
			return c.finishSubmit(ctx, intent, req, prior)
		} else if !errors.Is(lookupErr, execution.ErrOrderNotFound) {
			_ = c.record(ctx, intent, "lookup_after_ambiguous_submit", "error", execution.OrderResult{}, lookupErr.Error(), true)
			return execution.OrderResult{}, fmt.Errorf("ambiguous stock order submit (%v); client-id lookup failed: %w", err, lookupErr)
		}
		res, replayErr := c.inner.PlaceOrder(ctx, req)
		if replayErr == nil {
			_ = c.record(ctx, intent, "exact_replay", "accepted", res, "", true)
			return c.finishSubmit(ctx, intent, req, res)
		}
		if execution.IsDefinitiveOrderRejection(replayErr) && !execution.IsDuplicateClientOrderError(replayErr) {
			if recordErr := c.record(ctx, intent, "exact_replay", "rejected", execution.OrderResult{Status: "rejected"}, replayErr.Error(), true); recordErr != nil {
				return execution.OrderResult{}, fmt.Errorf("persist definitive replay rejection: %w", recordErr)
			}
			return execution.OrderResult{}, terminalZeroFillRejectionError("stock order definitively rejected after exact replay", replayErr)
		}
		_ = c.record(ctx, intent, "exact_replay", "ambiguous", execution.OrderResult{}, replayErr.Error(), true)
		return execution.OrderResult{}, fmt.Errorf("stock order submit: %w", replayErr)
	}
	return execution.OrderResult{}, fmt.Errorf("stock order submit: %w", err)
}

func isTerminalZeroFill(status string, filledQty float64) bool {
	status = execution.NormalizeOrderStatus(status)
	return filledQty <= 0 && (status == "cancelled" || status == "rejected")
}

func isTerminalZeroFillForRequest(req execution.OrderRequest, res execution.OrderResult) bool {
	status := execution.NormalizeOrderStatusForTimeInForce(res.Status, req.TimeInForce)
	return res.FilledQty <= 0 && (status == "cancelled" || status == "rejected")
}

// terminalZeroFillRejectionError marks a broker-confirmed non-duplicate
// rejection as a conclusively dead generation while preserving the broker
// error for callers that need its HTTP/request context. Transport failures and
// duplicate-client responses remain ambiguous and must not use this sentinel.
func terminalZeroFillRejectionError(context string, err error) error {
	return fmt.Errorf("%w: %w: %s: %w", execution.ErrFreshTerminalZeroFillPlacement, execution.ErrTerminalZeroFillPlacement, context, err)
}

func requestExitIntent(req execution.OrderRequest) store.ExitIntent {
	if req.ExitReason == "" {
		return store.ExitIntent{Reason: store.ExitReasonStopLoss}
	}
	return store.ExitIntent{Reason: req.ExitReason, Evidence: req.ExitEvidence}
}

type recoveryContextKey struct{}

func isRecoveryContext(ctx context.Context) bool {
	v, _ := ctx.Value(recoveryContextKey{}).(bool)
	return v
}

func (c *Coordinator) finishSubmit(ctx context.Context, intent store.StockOrderIntent, req execution.OrderRequest, res execution.OrderResult) (execution.OrderResult, error) {
	if err := c.projectBracketChildren(ctx, intent, req, res); err != nil {
		// The broker has already accepted this parent. Preserve its identity for
		// callers so strict-entry recovery can verify or contain it immediately
		// even when the local child projection failed.
		return res, err
	}
	return res, nil
}

// projectBracketChildren persists broker-created bracket legs as an intent
// family immediately after their parent is known. It is safe to replay after
// a projection failure because its child idempotency keys are parent-derived.
func (c *Coordinator) projectBracketChildren(ctx context.Context, parent store.StockOrderIntent, req execution.OrderRequest, res execution.OrderResult) error {
	if req.OrderClass != "bracket" || len(res.ChildOrderIDs) < 2 || res.ChildOrderIDs[0] == "" || res.ChildOrderIDs[1] == "" {
		return nil
	}
	if _, err := store.UpsertBracketChildIntent(ctx, parent.ID, parent.TradeID, req.Ticker, "take-profit", res.ChildOrderIDs[0], string(execution.OrderTypeLimit), req.Quantity, req.TakeProfitPrice); err != nil {
		return fmt.Errorf("project take-profit child intent: %w", err)
	}
	if _, err := store.UpsertBracketChildIntent(ctx, parent.ID, parent.TradeID, req.Ticker, "stop-loss", res.ChildOrderIDs[1], string(execution.OrderTypeStop), req.Quantity, req.StopLossPrice); err != nil {
		return fmt.Errorf("project stop-loss child intent: %w", err)
	}
	return nil
}

// Recover replays nonterminal persisted intents using their exact stored
// payload and same client id. Callers run it before normal reconciliation.
func (c *Coordinator) Recover(ctx context.Context) error {
	if err := requireDurableStore(); err != nil {
		return err
	}
	ctx = context.WithValue(ctx, recoveryContextKey{}, true)
	intents, err := store.ListRecoverableStockOrderIntents(ctx)
	if err != nil {
		return err
	}
	var recoveryErrors []error
	for _, intent := range intents {
		if err := c.recoverIntent(ctx, intent); err != nil {
			recoveryErrors = append(recoveryErrors, fmt.Errorf("recover stock intent %s: %w", intent.ClientOrderID, err))
		}
	}
	return errors.Join(recoveryErrors...)
}

// recoverIntent isolates one immutable intent so a poisoned generation cannot
// prevent later, unrelated entries or exits from being reconciled in the same
// periodic pass.
func (c *Coordinator) recoverIntent(ctx context.Context, intent store.StockOrderIntent) error {
	// A prior intent in this same recovery snapshot can advance a mutable stop
	// row and supersede a later predecessor. Re-read it before acting so the
	// stale snapshot cannot recreate an active old-generation stop.
	current, err := store.GetStockOrderIntentByKey(ctx, intent.IdempotencyKey)
	if err != nil {
		return err
	}
	if current == nil || current.Purpose != "submit" {
		return nil
	}
	intent = *current
	var req execution.OrderRequest
	if err := json.Unmarshal(intent.RequestPayload, &req); err != nil {
		return fmt.Errorf("decode stock intent %s: %w", intent.ClientOrderID, err)
	}
	if req.ClientOrderID != intent.ClientOrderID {
		return fmt.Errorf("stock intent %s payload client id mismatch", intent.ClientOrderID)
	}
	if req.Type == execution.OrderTypeStop && req.TradeID != "" {
		return store.WithTradeOrderLock(req.TradeID, func() error {
			return c.recoverIntentLocked(ctx, intent, req)
		})
	}
	return c.recoverIntentLocked(ctx, intent, req)
}

// recoverIntentLocked submits and projects one intent. Stop intents enter via
// the per-trade lock above so recovery cannot race the live stop lifecycle.
func (c *Coordinator) recoverIntentLocked(ctx context.Context, intent store.StockOrderIntent, req execution.OrderRequest) error {
	var res execution.OrderResult
	var err error
	if strings.HasPrefix(intent.IdempotencyKey, "entry:") && intent.BrokerOrderID == "" {
		// Generic recovery has no fresh account/asset/ask admission context. A
		// prepared entry may still have reached Alpaca before its local attempt
		// record committed, so recovery always performs the harmless client-ID
		// lookup. A miss remains prepared (or ambiguous) and is never POSTed
		// again here without engine re-admission.
		lookup, ok := c.inner.(execution.ClientOrderLookup)
		if !ok {
			return fmt.Errorf("ambiguous stock entry %s requires client-id lookup", intent.ClientOrderID)
		}
		res, err = lookup.GetOrderByClientID(ctx, intent.ClientOrderID)
		if errors.Is(err, execution.ErrOrderNotFound) {
			if intent.SubmittedAt == nil {
				if c.observer != nil {
					c.observer.PreparedStockEntryNeedsAdmission(ctx, intent.IdempotencyKey)
				}
				return nil
			}
			return fmt.Errorf("ambiguous stock entry %s remains unavailable by client id", intent.ClientOrderID)
		}
		if err != nil {
			return fmt.Errorf("lookup ambiguous stock entry %s: %w", intent.ClientOrderID, err)
		}
		if err := c.record(ctx, intent, "recovery_lookup", "found", res, "", false); err != nil {
			return err
		}
		res, err = c.finishSubmit(ctx, intent, req, res)
	} else if req.ReplacesBrokerOrderID != "" {
		res, err = c.submitReplacement(ctx, intent, req, true)
	} else {
		res, err = c.submit(ctx, intent, req)
	}
	if err != nil {
		return err
	}
	if err := c.projectRecoveredEntry(ctx, intent, req, res); err != nil {
		return err
	}
	if err := c.projectRecoveredStandaloneStop(ctx, intent, req, res); err != nil {
		return err
	}
	return c.projectRecoveredMarketExit(ctx, intent, req, res)
}

func (c *Coordinator) projectRecoveredMarketExit(ctx context.Context, intent store.StockOrderIntent, req execution.OrderRequest, res execution.OrderResult) error {
	if req.Type != execution.OrderTypeMarket || !isRecoveredMarketExitKey(intent.IdempotencyKey) {
		return nil
	}
	if req.TradeID != "" {
		return c.projectRecoveredMarketExitAllocation(ctx, intent, req, res, req.TradeID, req.Quantity, res.FilledQty)
	}
	if !strings.HasPrefix(intent.IdempotencyKey, "analyst-exit:") {
		return nil
	}
	signalID, ok := analystExitSignalID(intent.IdempotencyKey, req.Ticker)
	if !ok {
		return fmt.Errorf("invalid analyst stock exit intent key %q", intent.IdempotencyKey)
	}
	trades, err := store.GetExitingTradesForSellSignal(ctx, req.Ticker, signalID)
	if err != nil {
		return fmt.Errorf("load claimed analyst exit trades: %w", err)
	}
	if len(trades) == 0 {
		projected, err := store.CountOrdersForStockIntent(ctx, intent.ID)
		if err != nil {
			return fmt.Errorf("count analyst exit projections: %w", err)
		}
		if projected > 0 {
			// A prior recovery finalized every claimed lot. The immutable intent
			// remains selectable so it can heal a partial projection, but once
			// every trade is closed there is deliberately no live claim to find.
			if res.FilledQty <= 0 && execution.IsTerminalOrderStatusForTimeInForce(res.Status, req.TimeInForce) {
				status := execution.NormalizeOrderStatusForTimeInForce(res.Status, req.TimeInForce)
				if err := store.UpdateSignalStatus(ctx, signalID, "rejected", "sell order "+status+" without a broker fill"); err != nil {
					return fmt.Errorf("reject recovered zero-fill analyst exit signal: %w", err)
				}
				return nil
			}
			if err := store.UpdateSignalStatus(ctx, signalID, "executed", ""); err != nil {
				return fmt.Errorf("mark recovered analyst exit signal executed: %w", err)
			}
			return nil
		}
		if res.FilledQty <= 0 && execution.IsTerminalOrderStatus(res.Status) {
			signal, signalErr := store.GetSignalByID(ctx, signalID)
			if signalErr != nil {
				return fmt.Errorf("load terminal analyst exit signal: %w", signalErr)
			}
			if signal != nil && signal.Status == "executed" {
				// A newer retry already completed this signal. The retired
				// terminal generation has no remaining claim to release.
				return nil
			}
		}
		return fmt.Errorf("analyst stock exit %s has no claimed trade links", intent.ClientOrderID)
	}

	// The broker's FilledQty is cumulative across the one aggregate order. Build
	// the complete live claim list in its original claim order before assigning
	// any capacity. An existing projection for a later lot must not consume
	// shares that belong to an earlier lot whose row was lost in a crash window.
	existingOrders, err := store.GetOrdersByStockIntent(ctx, intent.ID)
	if err != nil {
		return fmt.Errorf("load analyst exit projections: %w", err)
	}
	existingByTrade := make(map[string]store.Order, len(existingOrders))
	for _, order := range existingOrders {
		existingByTrade[order.TradeID] = order
	}

	// Make every currently claimed lot durable before applying this broker
	// snapshot. This makes fill allocation independent of sparse inserts left by
	// a process crash between individual projection rows.
	for _, trade := range trades {
		if _, exists := existingByTrade[trade.ID]; exists {
			continue
		}
		if _, err := c.ensureRecoveredMarketExitProjection(ctx, intent, req, res, trade.ID, trade.Quantity); err != nil {
			return err
		}
		existingByTrade[trade.ID] = store.Order{TradeID: trade.ID, Quantity: trade.Quantity}
	}

	// Fully closed lots no longer appear in `trades`; preserve their capacity
	// ahead of the live claim list on a later cumulative broker snapshot.
	liveTrades := make(map[string]struct{}, len(trades))
	for _, trade := range trades {
		liveTrades[trade.ID] = struct{}{}
	}
	remainingFill := math.Max(0, res.FilledQty)
	for _, order := range existingOrders {
		if _, live := liveTrades[order.TradeID]; live {
			continue
		}
		allocation := math.Min(math.Max(0, order.Quantity), remainingFill)
		remainingFill -= allocation
	}
	for _, trade := range trades {
		quantity := existingByTrade[trade.ID].Quantity
		allocation := math.Min(math.Max(0, quantity), remainingFill)
		remainingFill -= allocation
		if err := c.projectRecoveredMarketExitAllocation(ctx, intent, req, res, trade.ID, quantity, allocation); err != nil {
			return err
		}
	}
	if res.FilledQty <= 0 && execution.IsTerminalOrderStatus(res.Status) {
		if err := store.UpdateSignalStatus(ctx, signalID, "rejected", "sell order "+execution.NormalizeOrderStatus(res.Status)+" without a broker fill"); err != nil {
			return fmt.Errorf("reject recovered zero-fill analyst exit signal: %w", err)
		}
		return nil
	}
	if err := store.UpdateSignalStatus(ctx, signalID, "executed", ""); err != nil {
		return fmt.Errorf("mark recovered analyst exit signal executed: %w", err)
	}
	return nil
}

func isRecoveredMarketExitKey(key string) bool {
	return strings.HasPrefix(key, "software-exit:") ||
		strings.HasPrefix(key, "trailing-market-exit:") ||
		strings.HasPrefix(key, "analyst-exit:") ||
		strings.HasPrefix(key, "broker-protection-flatten:")
}

func analystExitSignalID(key, ticker string) (string, bool) {
	key, ok := strings.CutPrefix(key, "analyst-exit:")
	if !ok || ticker == "" {
		return "", false
	}
	parts := strings.Split(key, ":")
	if len(parts) < 2 || parts[0] == "" || parts[1] != ticker {
		return "", false
	}
	// Terminal zero-fill retries append `:retry:<terminal identity>` to the
	// preceding immutable key. Accept any number of complete generations, but
	// never attribute a malformed suffix to this signal's claimed lots.
	for i := 2; i < len(parts); i += 2 {
		if i+1 >= len(parts) || parts[i] != "retry" || parts[i+1] == "" {
			return "", false
		}
	}
	return parts[0], true
}

// projectRecoveredMarketExitAllocation completes one durable exit-order link
// and atomically applies its assigned cumulative fill to the claimed trade. A
// retry sees the same row and is a no-op once the trade has been finalized.
func (c *Coordinator) projectRecoveredMarketExitAllocation(
	ctx context.Context,
	intent store.StockOrderIntent,
	req execution.OrderRequest,
	res execution.OrderResult,
	tradeID string,
	quantity, allocatedFill float64,
) error {
	orderID, err := c.ensureRecoveredMarketExitProjection(ctx, intent, req, res, tradeID, quantity)
	if err != nil {
		return err
	}
	if allocatedFill <= 0 {
		if execution.IsTerminalOrderStatusForTimeInForce(res.Status, req.TimeInForce) {
			status := execution.NormalizeOrderStatusForTimeInForce(res.Status, req.TimeInForce)
			if err := store.UpdateOrderStatusByStockIntent(ctx, intent.ID, status); err != nil {
				return fmt.Errorf("terminalize recovered zero-fill market exit projection: %w", err)
			}
			if err := store.ReleaseTradeExitClaim(ctx, tradeID); err != nil {
				return fmt.Errorf("release recovered zero-fill market exit claim: %w", err)
			}
		}
		return nil
	}
	if res.FilledPrice <= 0 {
		return fmt.Errorf("recovered market exit %s has fill without price", intent.ClientOrderID)
	}
	remaining, _, err := store.ApplyPartialExitAt(ctx, tradeID, orderID, res.FilledPrice, allocatedFill,
		execution.NormalizeOrderStatusForTimeInForce(res.Status, req.TimeInForce), res.FilledAt)
	if err != nil {
		return fmt.Errorf("apply recovered market exit fill: %w", err)
	}
	if execution.IsTerminalOrderStatusForTimeInForce(res.Status, req.TimeInForce) && remaining <= 1e-9 {
		if err := c.recoveredClose(ctx, tradeID); err != nil {
			return fmt.Errorf("finalize recovered market exit: %w", err)
		}
	}
	return nil
}

// ensureRecoveredMarketExitProjection creates one per-lot order row without
// consuming aggregate broker fill. Allocation happens only after every claimed
// lot has a durable projection, so sparse crash states keep claim ordering.
func (c *Coordinator) ensureRecoveredMarketExitProjection(
	ctx context.Context,
	intent store.StockOrderIntent,
	req execution.OrderRequest,
	res execution.OrderResult,
	tradeID string,
	quantity float64,
) (string, error) {
	existing, err := store.GetOrderByStockIntentAndTrade(ctx, intent.ID, tradeID)
	if err != nil {
		return "", err
	}
	if existing != nil {
		return existing.ID, nil
	}
	brokerID := res.BrokerOrderID
	if brokerID == "" {
		brokerID = intent.BrokerOrderID
	}
	if brokerID == "" {
		return "", fmt.Errorf("recovered market exit %s has no broker order id", intent.ClientOrderID)
	}
	orderID, err := store.InsertOrderWithRetry(ctx, store.Order{
		TradeID: tradeID, BrokerOrderID: brokerID, Ticker: req.Ticker,
		Side: string(req.Side), OrderType: string(req.Type), Quantity: quantity,
		Status: "pending", StockIntentID: intent.ID,
	})
	if err != nil {
		return "", fmt.Errorf("project recovered market exit order: %w", err)
	}
	return orderID, nil
}

func filledAtPtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

// projectRecoveredStandaloneStop records an accepted normal protective or
// trailing stop if the process died between broker acceptance and order-row
// persistence. It is intent-idempotent, so replay never creates duplicate
// broker protection.
func (c *Coordinator) projectRecoveredStandaloneStop(ctx context.Context, intent store.StockOrderIntent, req execution.OrderRequest, res execution.OrderResult) error {
	if req.Type != execution.OrderTypeStop || req.TradeID == "" || req.OrderClass != "" || req.ReplacesBrokerOrderID != "" {
		return nil
	}
	existing, err := store.GetOrderByStockIntentID(ctx, intent.ID)
	if err != nil {
		return err
	}
	brokerID := res.BrokerOrderID
	if brokerID == "" {
		brokerID = intent.BrokerOrderID
	}
	if existing == nil {
		orderID, err := store.InsertOrderWithRetry(ctx, store.Order{TradeID: req.TradeID, BrokerOrderID: brokerID,
			Ticker: req.Ticker, Side: string(req.Side), OrderType: string(req.Type), Quantity: req.Quantity,
			StopPrice: req.StopPrice, FilledPrice: res.FilledPrice, FilledQty: res.FilledQty,
			Status: execution.NormalizeOrderStatusForTimeInForce(res.Status, req.TimeInForce), IsStopLoss: true,
			ExitReason: requestExitIntent(req).Reason, ExitEvidence: requestExitIntent(req).Evidence, StockIntentID: intent.ID})
		if err != nil {
			return err
		}
		existing = &store.Order{ID: orderID, BrokerOrderID: brokerID}
	}
	if res.FilledQty <= 0 {
		if execution.IsTerminalOrderStatusForTimeInForce(res.Status, req.TimeInForce) {
			return store.UpdateOrderStatusByStockIntent(ctx, intent.ID, execution.NormalizeOrderStatusForTimeInForce(res.Status, req.TimeInForce))
		}
		return nil
	}
	if res.FilledPrice <= 0 {
		return fmt.Errorf("recovered stop %s has fill without price", intent.ClientOrderID)
	}
	exitIntent := requestExitIntent(req)
	if _, err := store.ClaimTradeExitWithIntent(ctx, req.TradeID, exitIntent); err != nil {
		return fmt.Errorf("claim recovered stop exit: %w", err)
	}
	expectedBrokerID := existing.BrokerOrderID
	if expectedBrokerID == "" {
		expectedBrokerID = brokerID
	}
	remaining, _, err := store.ApplyProtectiveExitAtForBrokerOrder(ctx, req.TradeID, existing.ID, expectedBrokerID,
		res.FilledPrice, res.FilledQty, execution.NormalizeOrderStatusForTimeInForce(res.Status, req.TimeInForce), res.FilledAt,
		exitIntent)
	if err != nil {
		return fmt.Errorf("apply recovered stop fill: %w", err)
	}
	if execution.IsTerminalOrderStatusForTimeInForce(res.Status, req.TimeInForce) {
		if remaining <= 1e-9 {
			if err := c.recoveredCloseLocked(ctx, req.TradeID); err != nil {
				return fmt.Errorf("finalize recovered stop fill: %w", err)
			}
		} else if err := store.ReleaseTradeExitClaim(ctx, req.TradeID); err != nil {
			// This stop is terminal, so it can no longer own the residual shares.
			// Releasing its claim lets normal reconciliation immediately restore
			// protection or submit the next exit generation.
			return fmt.Errorf("release terminal recovered partial-stop claim: %w", err)
		}
	}
	return nil
}

// projectRecoveredEntry completes only an accepted standard entry whose
// process died before the normal engine persisted its projections. A working
// zero-fill order remains order-only: it is not a trade or a phantom position.
func (c *Coordinator) projectRecoveredEntry(ctx context.Context, intent store.StockOrderIntent, req execution.OrderRequest, res execution.OrderResult) error {
	if !strings.HasPrefix(intent.IdempotencyKey, "entry:") {
		return nil
	}
	signalID, err := stockEntrySignalID(intent.IdempotencyKey)
	if err != nil {
		return err
	}
	if res.FilledQty <= 0 {
		if execution.IsTerminalOrderStatusForTimeInForce(res.Status, req.TimeInForce) {
			status := execution.NormalizeOrderStatusForTimeInForce(res.Status, req.TimeInForce)
			if err := store.UpdateOrderStatusByStockIntent(ctx, intent.ID, status); err != nil {
				return fmt.Errorf("terminalize recovered zero-fill entry projection: %w", err)
			}
			if err := store.UpdateSignalStatus(ctx, signalID, "rejected", "entry order "+status+" without fill"); err != nil {
				return fmt.Errorf("reject recovered zero-fill entry signal: %w", err)
			}
		}
		return nil
	}
	sig, err := store.GetSignalByID(ctx, signalID)
	if err != nil {
		return fmt.Errorf("load signal for recovered stock entry: %w", err)
	}
	if sig == nil {
		return fmt.Errorf("signal %s missing for recovered stock entry", signalID)
	}
	direction := "long"
	if req.Side == execution.OrderSideSell {
		direction = "short"
	}
	// The intent's convenience trade link can lag the actual projection after
	// a transient attach failure. Let the idempotent trade insert report whether
	// this call really created the row before deciding to emit process effects.
	tradeID, newProjection, err := store.EnsureTradeForStockIntent(ctx, store.Trade{
		SignalID: sig.ID, Ticker: sig.Ticker, Direction: direction, Strategy: sig.Strategy,
		EntryPrice: res.FilledPrice, Quantity: res.FilledQty, StopLoss: req.PolicyStopLoss,
		TakeProfit: recoveredTakeProfit(req), EntrySetup: sig.EntrySetup,
		EntrySetupEvidence: sig.EntrySetupEvidence, StockIntentID: intent.ID,
	})
	if err != nil {
		return fmt.Errorf("project recovered stock trade: %w", err)
	}
	if err := store.AttachStockOrderIntentTradeByKey(ctx, intent.IdempotencyKey, tradeID); err != nil {
		return fmt.Errorf("attach recovered stock intent: %w", err)
	}
	order, err := store.GetOrderByStockIntentID(ctx, intent.ID)
	if err != nil {
		return fmt.Errorf("load recovered stock order projection: %w", err)
	}
	if res.FilledAt.IsZero() {
		filledAt, err := store.GetStockOrderIntentFillTime(ctx, intent.ID)
		if err != nil {
			return fmt.Errorf("load durable stock entry fill time: %w", err)
		}
		if filledAt != nil {
			res.FilledAt = *filledAt
		}
	}
	recordedEntryQty := 0.0
	if order != nil {
		recordedEntryQty = order.FilledQty
	}
	brokerID := res.BrokerOrderID
	if brokerID == "" {
		brokerID = intent.BrokerOrderID
	}
	if order == nil {
		if _, err := store.InsertOrderWithRetry(ctx, store.Order{
			TradeID: tradeID, BrokerOrderID: brokerID, Ticker: req.Ticker,
			Side: string(req.Side), OrderType: string(req.Type), Quantity: req.Quantity,
			FilledPrice: res.FilledPrice, FilledQty: res.FilledQty,
			Status: execution.NormalizeOrderStatusForTimeInForce(res.Status, req.TimeInForce), StockIntentID: intent.ID,
			FilledAt: filledAtPtr(res.FilledAt),
		}); err != nil {
			return fmt.Errorf("project recovered stock order: %w", err)
		}
	} else if err := store.AttachOrderToTradeByStockIntent(ctx, intent.ID, tradeID); err != nil {
		return fmt.Errorf("attach recovered stock order: %w", err)
	}
	exitOwned := false
	if brokerID != "" {
		trade, err := store.GetTradeByID(ctx, tradeID)
		if err != nil {
			return fmt.Errorf("load recovered stock entry trade: %w", err)
		}
		if trade == nil {
			return fmt.Errorf("recovered stock entry trade %s is missing", tradeID)
		}
		if trade.Status != "open" {
			exitOwned = true
			// Once an exit owns this trade, a still-working entry must not be
			// allowed to add shares behind that exit. Cancel first and use the
			// terminal broker snapshot below, so no later fill can become an
			// unprotected position.
			terminal, err := c.cancelRecoveredEntryAfterExitOwnership(ctx, brokerID)
			if err != nil {
				return fmt.Errorf("cancel recovered entry after exit ownership: %w", err)
			}
			res = terminal
			if trade.Status == "closed" && res.FilledQty > recordedEntryQty+1e-9 {
				return fmt.Errorf("recovered entry %s filled after its trade closed; cancellation confirmed but reconciliation is required", intent.ClientOrderID)
			}
		}
		if err := store.ApplyEntryFill(ctx, tradeID, brokerID, res.FilledPrice, res.FilledQty, execution.NormalizeOrderStatusForTimeInForce(res.Status, req.TimeInForce)); err != nil {
			return fmt.Errorf("apply recovered stock entry fill: %w", err)
		}
	}
	if !exitOwned {
		if err := c.projectRecoveredEntryExits(ctx, intent, req, res, tradeID); err != nil {
			return err
		}
	}
	if observer, ok := c.observer.(ProtectionRecoveryObserver); ok {
		observer.RecoveredStockEntryProtection(ctx, intent.IdempotencyKey)
	}
	if err := store.UpdateSignalStatus(ctx, signalID, "executed", ""); err != nil {
		return fmt.Errorf("mark recovered stock entry signal executed: %w", err)
	}
	if newProjection && c.observer != nil {
		c.observer.RecoveredStockEntry(ctx, intent.IdempotencyKey)
	}
	if observer, ok := c.observer.(DiscoveryTradeLeaseObserver); ok {
		fillAt := res.FilledAt
		if fillAt.IsZero() && order != nil && order.FilledAt != nil {
			fillAt = *order.FilledAt
		}
		protectionConfirmed := !exitOwned
		if protectionConfirmed && req.OrderClass == "bracket" && req.RequireBrokerProtection {
			var verifyErr error
			protectionConfirmed, verifyErr = c.recoveredBracketProtectionVerified(ctx, intent, req, res)
			if verifyErr != nil {
				slog.Warn("verify recovered bracket protection before discovery renewal",
					"ticker", sig.Ticker, "intent_id", intent.ID, "err", verifyErr)
			}
		}
		if !protectionConfirmed {
			slog.Info("defer discovery residency renewal until recovered entry protection is verified",
				"ticker", sig.Ticker, "intent_id", intent.ID)
			if exitOwned {
				// The trade is already exiting or closed; its order recovery must
				// not be held open solely for a residency extension that no longer
				// protects a live position.
				if err := store.MarkStockOrderIntentDiscoveryResidencyRenewed(ctx, intent.ID); err != nil {
					slog.Warn("acknowledge discovery renewal for exit-owned trade",
						"ticker", sig.Ticker, "intent_id", intent.ID, "err", err)
				}
			}
		} else if fillAt.IsZero() {
			slog.Info("skip discovery residency renewal without authoritative fill timestamp", "ticker", sig.Ticker)
			if err := store.MarkStockOrderIntentDiscoveryResidencyRenewed(ctx, intent.ID); err != nil {
				slog.Warn("mark unavailable discovery fill time acknowledged",
					"ticker", sig.Ticker, "intent_id", intent.ID, "err", err)
			}
		} else if err := observer.RenewRecoveredEntryDiscoveryResidency(ctx, sig.Ticker, fillAt); err != nil {
			// Discovery residency is ancillary. The broker-confirmed fill, order,
			// and protective exits above are already durable; a lease write must
			// not make recovery appear incomplete or gate a protected position.
			slog.Warn("renew discovery residency after recovered stock entry",
				"ticker", sig.Ticker, "filled_at", fillAt, "err", err)
		} else if err := store.MarkStockOrderIntentDiscoveryResidencyRenewed(ctx, intent.ID); err != nil {
			// Keep the intent recoverable if the completion marker cannot be
			// committed; renewal is idempotent and recovery can safely retry it.
			slog.Warn("mark recovered discovery residency renewal complete",
				"ticker", sig.Ticker, "intent_id", intent.ID, "err", err)
		}
	} else {
		// Without the optional engine observer there is no lease owner capable
		// of performing this ancillary action. Acknowledge it so generic
		// coordinator users do not replay an otherwise-complete entry forever.
		if err := store.MarkStockOrderIntentDiscoveryResidencyRenewed(ctx, intent.ID); err != nil {
			slog.Warn("mark discovery residency renewal acknowledged without observer",
				"ticker", sig.Ticker, "intent_id", intent.ID, "err", err)
		}
	}
	return nil
}

func stockEntrySignalID(key string) (string, error) {
	if !strings.HasPrefix(key, "entry:") {
		return "", fmt.Errorf("invalid stock entry intent key %q", key)
	}
	remainder := strings.TrimPrefix(key, "entry:")
	if retryAt := strings.Index(remainder, ":retry:"); retryAt >= 0 {
		remainder = remainder[:retryAt]
	}
	if remainder == "" || strings.Contains(remainder, ":") {
		return "", fmt.Errorf("invalid stock entry intent key %q", key)
	}
	return remainder, nil
}

// cancelRecoveredEntryAfterExitOwnership proves that a residual entry cannot
// fill after an exit has taken the trade. Cancellation responses are not
// authoritative (an already-terminal Alpaca order can reject DELETE), so the
// follow-up status read is required and is persisted through GetOrderStatus.
func (c *Coordinator) cancelRecoveredEntryAfterExitOwnership(ctx context.Context, brokerOrderID string) (execution.OrderResult, error) {
	if err := c.CancelOrder(ctx, brokerOrderID); err != nil {
		// Status confirmation below decides whether the entry is actually dead.
	}
	res, err := c.GetOrderStatus(ctx, brokerOrderID)
	if err != nil {
		return execution.OrderResult{}, err
	}
	if !execution.IsTerminalOrderStatus(res.Status) {
		return execution.OrderResult{}, fmt.Errorf("entry broker order %s remains %s after cancel", brokerOrderID, res.Status)
	}
	return res, nil
}

func recoveredTakeProfit(req execution.OrderRequest) float64 {
	if req.UncappedTakeProfit {
		return 0
	}
	return req.PolicyTakeProfit
}

// projectRecoveredEntryExits restores only the broker-resident policy that
// was part of the immutable entry request. Bracket children are projections of
// the accepted parent; uncapped entries receive a standalone durable stop
// before recovery reports success, so a filled position is never treated as
// fully recovered without its required protection.
func (c *Coordinator) projectRecoveredEntryExits(ctx context.Context, parent store.StockOrderIntent, req execution.OrderRequest, res execution.OrderResult, tradeID string) error {
	if req.OrderClass == "bracket" && len(res.ChildOrderIDs) >= 2 && res.ChildOrderIDs[0] != "" && res.ChildOrderIDs[1] != "" {
		legs := []struct {
			role, brokerID, orderType string
			price                     float64
			stop, take                bool
		}{
			{"take-profit", res.ChildOrderIDs[0], string(execution.OrderTypeLimit), req.TakeProfitPrice, false, true},
			{"stop-loss", res.ChildOrderIDs[1], string(execution.OrderTypeStop), req.StopLossPrice, true, false},
		}
		for _, leg := range legs {
			child := recoveredBracketChild(res.ChildOrders, leg.brokerID)
			childID, err := store.UpsertBracketChildIntent(ctx, parent.ID, tradeID, req.Ticker, leg.role, leg.brokerID, leg.orderType, req.Quantity, leg.price)
			if err != nil {
				return fmt.Errorf("project recovered bracket child intent: %w", err)
			}
			existing, err := store.GetOrderByStockIntentID(ctx, childID)
			if err != nil {
				return fmt.Errorf("load recovered bracket child projection: %w", err)
			}
			if existing == nil {
				order := store.Order{TradeID: tradeID, BrokerOrderID: leg.brokerID, Ticker: req.Ticker,
					Side: string(execution.OrderSideSell), OrderType: leg.orderType, Quantity: req.Quantity,
					Status: "pending", IsStopLoss: leg.stop, IsTakeProfit: leg.take,
					ParentOrderID: res.BrokerOrderID, StockIntentID: childID}
				if child != nil && child.FilledQty > 0 {
					// Persist this nested snapshot before the parent becomes terminal
					// and ceases to be recoverable. A later direct child lookup may
					// already be purged; bracket reconciliation will atomically apply
					// this durable cumulative fill to the trade.
					order.FilledPrice = child.FilledPrice
					order.FilledQty = child.FilledQty
					order.Status = execution.NormalizeOrderStatusForTimeInForce(child.Status, "gtc")
					if !child.FilledAt.IsZero() {
						filledAt := child.FilledAt
						order.FilledAt = &filledAt
					}
				}
				if leg.stop {
					order.StopPrice = leg.price
				} else {
					order.LimitPrice = leg.price
				}
				if _, err := store.InsertOrderWithRetry(ctx, order); err != nil {
					return fmt.Errorf("project recovered bracket child order: %w", err)
				}
			}
		}
	}
	if req.UncappedTakeProfit && res.FilledQty > 0 {
		if err := c.ensureRecoveredProtectiveStop(ctx, parent, req, res, tradeID); err != nil {
			return fmt.Errorf("recover uncapped entry protective stop: %w", err)
		}
	}
	return nil
}

func recoveredBracketChild(children []execution.OrderLeg, brokerOrderID string) *execution.OrderLeg {
	for i := range children {
		if children[i].BrokerOrderID == brokerOrderID {
			return &children[i]
		}
	}
	return nil
}

func (c *Coordinator) recoveredBracketProtectionVerified(
	ctx context.Context, parentIntent store.StockOrderIntent, req execution.OrderRequest, parent execution.OrderResult,
) (bool, error) {
	if parent.FilledQty <= 1e-9 || !execution.IsTerminalOrderStatusForTimeInForce(parent.Status, req.TimeInForce) {
		return false, nil
	}
	stopID := ""
	if parentIntent.ID != "" {
		projection, err := store.GetBracketStopProjectionForParent(ctx, parentIntent.ID)
		if err != nil {
			return false, fmt.Errorf("load durable recovered stop projection: %w", err)
		}
		if projection != nil {
			stopID = projection.BrokerOrderID
		}
	}
	if stopID == "" {
		for _, child := range parent.ChildOrders {
			if child.Type == "stop" || child.Type == "stop_limit" {
				stopID = child.BrokerOrderID
				break
			}
		}
	}
	if stopID == "" && len(parent.ChildOrderIDs) >= 2 {
		// Alpaca's bracket result contract orders children [take-profit, stop-loss].
		stopID = parent.ChildOrderIDs[1]
	}
	if stopID == "" {
		return false, nil
	}
	stop, err := c.inner.GetOrderStatus(ctx, stopID)
	if err != nil {
		return false, fmt.Errorf("load recovered stop %s: %w", stopID, err)
	}
	if execution.IsTerminalOrderStatusForTimeInForce(stop.Status, "gtc") {
		return false, nil
	}
	remaining := stop.RemainingQty
	if remaining <= 0 && stop.Quantity > 0 {
		remaining = math.Max(0, stop.Quantity-stop.FilledQty)
	}
	// Bracket children may already have filled part of the position. The parent
	// retains its cumulative entry fill, while the active stop only needs to
	// cover the residual quantity still held by the trade.
	heldQty := parent.FilledQty
	if parentIntent.TradeID != "" {
		trade, err := store.GetTradeByID(ctx, parentIntent.TradeID)
		if err != nil {
			return false, fmt.Errorf("load recovered bracket trade quantity: %w", err)
		}
		if trade != nil {
			heldQty = trade.Quantity
		}
	}
	if heldQty <= 1e-9 {
		return true, nil
	}
	return remaining+1e-9 >= heldQty, nil
}

// ensureRecoveredProtectiveStop maintains one active protection lineage for an
// uncapped recovered entry. A cumulative entry fill can increase after a
// restart: cancel and confirm the old generation before submitting the resized
// one, so protected working quantity never exceeds the held position.
func (c *Coordinator) ensureRecoveredProtectiveStop(ctx context.Context, parent store.StockOrderIntent, req execution.OrderRequest, res execution.OrderResult, tradeID string) error {
	return store.WithTradeOrderLock(tradeID, func() error {
		return c.ensureRecoveredProtectiveStopLocked(ctx, parent, req, res, tradeID)
	})
}

func (c *Coordinator) ensureRecoveredProtectiveStopLocked(ctx context.Context, parent store.StockOrderIntent, req execution.OrderRequest, res execution.OrderResult, tradeID string) error {
	trade, err := store.GetTradeByID(ctx, tradeID)
	if err != nil {
		return fmt.Errorf("load recovered protective-stop trade: %w", err)
	}
	if trade == nil || trade.Status != "open" || trade.Quantity <= 1e-9 {
		return nil
	}
	desiredQty := trade.Quantity
	existing, err := store.GetRecoveredProtectiveStop(ctx, tradeID, parent.ID)
	if err != nil {
		return err
	}
	// A normal engine projection may already have installed its uncapped hard
	// stop before the process died. Adopt that live row instead of creating a
	// second recovered-only lineage over the same shares.
	if existing == nil {
		existing, err = store.LatestStopLossOrderForTrade(ctx, tradeID)
		if err != nil {
			return fmt.Errorf("load normal protective stop for recovery: %w", err)
		}
	}
	if existing == nil {
		unprojected, err := store.ListUnprojectedStockStopIntents(ctx, tradeID)
		if err != nil {
			return fmt.Errorf("load unprojected protective stop intents: %w", err)
		}
		for _, candidate := range unprojected {
			var stopReq execution.OrderRequest
			if err := json.Unmarshal(candidate.RequestPayload, &stopReq); err != nil {
				return fmt.Errorf("decode unprojected protective stop intent: %w", err)
			}
			if err := c.recoverIntentLocked(ctx, candidate, stopReq); err != nil {
				return fmt.Errorf("project protective stop before recovery fallback: %w", err)
			}
			trade, err = store.GetTradeByID(ctx, tradeID)
			if err != nil {
				return fmt.Errorf("reload recovered protective-stop trade: %w", err)
			}
			if trade == nil || trade.Status != "open" || trade.Quantity <= 1e-9 {
				return nil
			}
			desiredQty = trade.Quantity
			existing, err = store.GetOrderByStockIntentID(ctx, candidate.ID)
			if err != nil {
				return fmt.Errorf("load projected protective stop intent: %w", err)
			}
			if existing != nil && existing.Status != "pending" && existing.Status != "partial" {
				existing = nil
			}
			if existing != nil {
				break
			}
		}
	}
	if existing != nil && math.Abs(existing.Quantity-desiredQty) <= 1e-9 &&
		existing.StopPrice >= req.PolicyStopLoss-1e-9 {
		return nil
	}
	key := fmt.Sprintf("recovered-protective-stop:%s:%s:%.8f", tradeID, parent.ID, req.PolicyStopLoss)
	remainingQty := desiredQty
	if existing != nil {
		cancelErr := c.CancelOrder(ctx, existing.BrokerOrderID)
		status, statusErr := c.GetOrderStatus(ctx, existing.BrokerOrderID)
		stopTIF := standaloneStopTimeInForce(existing.Quantity)
		normalized := "cancelled"
		if statusErr != nil {
			if !errors.Is(statusErr, execution.ErrOrderNotFound) {
				return fmt.Errorf("confirm cancellation of recovered protective stop: cancel=%v status=%w", cancelErr, statusErr)
			}
			// A purged broker id is terminal. Preserve any cumulative fill
			// already recorded locally, then replace protection for the durable
			// residual position.
			status = execution.OrderResult{
				BrokerOrderID: existing.BrokerOrderID,
				Status:        normalized,
				FilledPrice:   existing.FilledPrice,
				FilledQty:     existing.FilledQty,
			}
			if existing.FilledAt != nil {
				status.FilledAt = *existing.FilledAt
			}
		} else {
			normalized = execution.NormalizeOrderStatusForTimeInForce(status.Status, stopTIF)
			if !execution.IsTerminalOrderStatusForTimeInForce(status.Status, stopTIF) {
				return fmt.Errorf("recovered protective stop %s remains %s after cancel: cancel=%v", existing.BrokerOrderID, status.Status, cancelErr)
			}
		}
		exitIntent := store.ExitIntent{Reason: existing.ExitReason, Evidence: existing.ExitEvidence}
		if exitIntent.Reason == "" {
			exitIntent = store.ExitIntent{Reason: store.ExitReasonStopLoss}
		}
		if status.FilledQty > 0 {
			if _, err := store.ClaimTradeExitWithIntent(ctx, tradeID, exitIntent); err != nil {
				return fmt.Errorf("claim recovered protective-stop cancellation fill: %w", err)
			}
			remaining, _, err := store.ApplyProtectiveExitAtForBrokerOrder(ctx,
				tradeID, existing.ID, existing.BrokerOrderID,
				status.FilledPrice, status.FilledQty, normalized, status.FilledAt, exitIntent)
			if err != nil {
				return fmt.Errorf("apply recovered protective-stop cancellation fill: %w", err)
			}
			remainingQty = remaining
		} else if err := store.UpdateOrderStatusForBrokerOrder(ctx, existing.ID, existing.BrokerOrderID, normalized); err != nil {
			return fmt.Errorf("terminalize recovered protective stop after cancellation: %w", err)
		}
		if remainingQty <= 1e-9 {
			// A cancellation can race a complete stop fill. Do not create a
			// fresh protective order for a position that no longer exists; the
			// durable exit reconciler/finalizer owns the terminal trade state.
			return nil
		}
		key += fmt.Sprintf(":resize:%s:%.8f", existing.BrokerOrderID, remainingQty)
	}
	stopReq := execution.OrderRequest{Ticker: req.Ticker, Side: execution.OrderSideSell,
		Type: execution.OrderTypeStop, Quantity: remainingQty, StopPrice: req.PolicyStopLoss,
		TradeID: tradeID, TimeInForce: "gtc", IntentKey: key,
		ExitReason: store.ExitReasonStopLoss, ExitEvidence: json.RawMessage(fmt.Sprintf(`{"stop_loss":%g}`, req.PolicyStopLoss))}
	stopRes, err := c.PlaceOrder(ctx, stopReq)
	if err != nil {
		return err
	}
	stopIntent, err := store.GetStockOrderIntentByKey(ctx, key)
	if err != nil || stopIntent == nil {
		return fmt.Errorf("load durable recovered protective stop intent: %w", err)
	}
	if existing == nil {
		_, err = store.InsertOrderWithRetry(ctx, store.Order{TradeID: tradeID, BrokerOrderID: stopRes.BrokerOrderID,
			Ticker: req.Ticker, Side: string(execution.OrderSideSell), OrderType: string(execution.OrderTypeStop),
			Quantity: remainingQty, StopPrice: req.PolicyStopLoss, FilledPrice: stopRes.FilledPrice,
			FilledQty: stopRes.FilledQty, Status: execution.NormalizeOrderStatusForTimeInForce(stopRes.Status, stopReq.TimeInForce), IsStopLoss: true,
			ExitReason: requestExitIntent(stopReq).Reason, ExitEvidence: requestExitIntent(stopReq).Evidence, StockIntentID: stopIntent.ID})
		return err
	}
	if err := store.UpdateOrderStopReplacementAndSupersede(ctx, existing.ID, existing.BrokerOrderID, stopRes.BrokerOrderID, stopIntent.ID,
		existing.FilledQty, remainingQty, req.PolicyStopLoss, remainingQty,
		execution.NormalizeOrderStatusForTimeInForce(stopRes.Status, stopReq.TimeInForce), stopRes.FilledPrice, stopRes.FilledQty, stopRes.FilledAt, requestExitIntent(stopReq)); err != nil {
		return err
	}
	return nil
}

func (c *Coordinator) record(ctx context.Context, intent store.StockOrderIntent, operation, outcome string, res execution.OrderResult, errText string, submitted bool) error {
	status := execution.NormalizeOrderStatus(res.Status)
	var req execution.OrderRequest
	if err := json.Unmarshal(intent.RequestPayload, &req); err == nil {
		status = execution.NormalizeOrderStatusForTimeInForce(res.Status, req.TimeInForce)
	}
	if res.Status == "" {
		status = intent.Status
	}
	if err := store.UpdateStockOrderIntentBrokerWithFillTime(
		ctx, intent.ClientOrderID, res.BrokerOrderID, status,
		res.FilledPrice, res.FilledQty, res.RequestID, errText, submitted, res.FilledAt,
	); err != nil {
		return err
	}
	return store.RecordStockOrderAttempt(ctx, intent.ID, operation, outcome, res.BrokerOrderID, res.RequestID, errText)
}

func (c *Coordinator) CancelOrder(ctx context.Context, id string) error {
	if err := requireDurableStore(); err != nil {
		return err
	}
	return c.inner.CancelOrder(ctx, id)
}
func (c *Coordinator) GetOrderStatus(ctx context.Context, id string) (execution.OrderResult, error) {
	res, err := c.inner.GetOrderStatus(ctx, id)
	if err != nil {
		return res, err
	}
	intent, err := store.GetStockOrderIntentByBrokerOrderID(ctx, id)
	if err != nil {
		return res, fmt.Errorf("load durable stock intent for broker status %s: %w", id, err)
	}
	if intent != nil {
		if err := c.record(ctx, *intent, "status_poll", "observed", res, "", false); err != nil {
			return res, fmt.Errorf("persist durable stock intent status %s: %w", id, err)
		}
	}
	return res, nil
}
func (c *Coordinator) GetPositions(ctx context.Context) ([]execution.Position, error) {
	return c.inner.GetPositions(ctx)
}
func (c *Coordinator) GetAccount(ctx context.Context) (execution.AccountInfo, error) {
	return c.inner.GetAccount(ctx)
}
func (c *Coordinator) AccountSnapshot(ctx context.Context) (execution.BrokerAccount, error) {
	b, ok := c.inner.(execution.BrokerView)
	if !ok {
		return execution.BrokerAccount{}, execution.ErrBrokerUnavailable
	}
	return b.AccountSnapshot(ctx)
}
func (c *Coordinator) PositionsSnapshot(ctx context.Context) ([]execution.BrokerPosition, error) {
	b, ok := c.inner.(execution.BrokerView)
	if !ok {
		return nil, execution.ErrBrokerUnavailable
	}
	return b.PositionsSnapshot(ctx)
}
func (c *Coordinator) EquityCurve(ctx context.Context, p, tf string) (execution.EquityHistory, error) {
	b, ok := c.inner.(execution.BrokerView)
	if !ok {
		return execution.EquityHistory{}, execution.ErrBrokerUnavailable
	}
	return b.EquityCurve(ctx, p, tf)
}
func (c *Coordinator) RecentFills(ctx context.Context) ([]execution.BrokerOrder, error) {
	b, ok := c.inner.(execution.BrokerView)
	if !ok {
		return nil, execution.ErrBrokerUnavailable
	}
	return b.RecentFills(ctx)
}
func (c *Coordinator) GetWorkingOrders(ctx context.Context) ([]execution.WorkingOrder, error) {
	l, ok := c.inner.(execution.WorkingOrdersLister)
	if !ok {
		return nil, execution.ErrBrokerUnavailable
	}
	return l.GetWorkingOrders(ctx)
}
func (c *Coordinator) GetOrderByClientID(ctx context.Context, clientOrderID string) (execution.OrderResult, error) {
	lookup, ok := c.inner.(execution.ClientOrderLookup)
	if !ok {
		return execution.OrderResult{}, execution.ErrBrokerUnavailable
	}
	return lookup.GetOrderByClientID(ctx, clientOrderID)
}
func (c *Coordinator) StockAssetSnapshot(ctx context.Context, symbol string) (execution.StockAsset, error) {
	provider, ok := c.inner.(execution.StockAssetProvider)
	if !ok {
		return execution.StockAsset{}, execution.ErrBrokerUnavailable
	}
	return provider.StockAssetSnapshot(ctx, symbol)
}
func (c *Coordinator) ReplaceStopOrder(ctx context.Context, id string, px float64) (execution.OrderResult, error) {
	if err := requireDurableStore(); err != nil {
		return execution.OrderResult{}, err
	}
	predecessor, err := store.GetStockOrderIntentByBrokerOrderID(ctx, id)
	if err != nil {
		return execution.OrderResult{}, err
	}
	if predecessor == nil {
		predecessor, err = c.adoptLegacyStop(ctx, id)
		if err != nil {
			return execution.OrderResult{}, err
		}
	}
	provenance := store.ExitIntent{Reason: store.ExitReasonStopLoss}
	var predecessorRequest execution.OrderRequest
	if err := json.Unmarshal(predecessor.RequestPayload, &predecessorRequest); err == nil {
		provenance = requestExitIntent(predecessorRequest)
	}
	if predecessorRequest.TimeInForce == "" {
		predecessorRequest.TimeInForce = bracketChildTimeInForce(*predecessor)
	}
	key := fmt.Sprintf("replace:%s:%.8f", predecessor.IdempotencyKey, px)
	req := execution.OrderRequest{
		Ticker: predecessor.Ticker, Side: execution.OrderSide(predecessor.Side),
		Type: execution.OrderTypeStop, Quantity: predecessor.RequestedQty, StopPrice: px,
		TradeID: predecessor.TradeID, IntentKey: key, ClientOrderID: clientIDForKey(key),
		ReplacesBrokerOrderID: id,
		TimeInForce:           predecessorRequest.TimeInForce,
		ExitReason:            provenance.Reason, ExitEvidence: provenance.Evidence,
	}
	payload, err := json.Marshal(req)
	if err != nil {
		return execution.OrderResult{}, fmt.Errorf("marshal stock stop replacement intent: %w", err)
	}
	hash := fmt.Sprintf("%x", sha256.Sum256(payload))
	intent, err := store.GetStockOrderIntentByKey(ctx, key)
	if err != nil {
		return execution.OrderResult{}, err
	}
	if intent != nil {
		if intent.PayloadHash != hash {
			return execution.OrderResult{}, fmt.Errorf("stock replacement intent key %q reused with different payload", key)
		}
		if err := json.Unmarshal(intent.RequestPayload, &req); err != nil {
			return execution.OrderResult{}, err
		}
		return c.submitReplacement(ctx, *intent, req, false)
	}
	intent = &store.StockOrderIntent{
		ID: intentUUID(), IdempotencyKey: key, ClientOrderID: req.ClientOrderID,
		TradeID: predecessor.TradeID, ParentIntentID: predecessor.ID,
		Ticker: req.Ticker, Side: string(req.Side), OrderType: string(req.Type),
		RequestedQty: req.Quantity, RequestPayload: payload, PayloadHash: hash, Status: "pending",
	}
	if err := store.CreateStockOrderIntent(ctx, *intent); err != nil {
		return execution.OrderResult{}, fmt.Errorf("persist stock stop replacement intent before submit: %w", err)
	}
	return c.submitReplacement(ctx, *intent, req, false)
}

// bracketChildTimeInForce reads the compact broker-projection payload rather
// than assuming it has the shape of an OrderRequest. Existing projections were
// GTC bracket legs too, so retain that invariant for rows written before the
// field was added.
func bracketChildTimeInForce(intent store.StockOrderIntent) string {
	var metadata struct {
		TimeInForce string `json:"time_in_force"`
	}
	if err := json.Unmarshal(intent.RequestPayload, &metadata); err == nil && metadata.TimeInForce != "" {
		return metadata.TimeInForce
	}
	if intent.Purpose == "broker_projection" && strings.HasPrefix(intent.IdempotencyKey, "bracket-child:") {
		return "gtc"
	}
	return ""
}

// adoptLegacyStop backfills the immutable predecessor intent for a stop row
// created before durable stock intents existed. The stable row id is its
// lineage key, so all later PATCH attempts retain one deterministic client id.
func (c *Coordinator) adoptLegacyStop(ctx context.Context, brokerOrderID string) (*store.StockOrderIntent, error) {
	order, err := store.GetOrderByBrokerOrderID(ctx, brokerOrderID)
	if err != nil {
		return nil, err
	}
	if order == nil || !order.IsStopLoss || order.OrderType != string(execution.OrderTypeStop) {
		return nil, fmt.Errorf("durable stock intent missing for stop replacement %s", brokerOrderID)
	}
	key := "legacy-stop:" + order.ID
	if existing, err := store.GetStockOrderIntentByKey(ctx, key); err != nil {
		return nil, err
	} else if existing != nil {
		return existing, store.RepointOrderStockIntent(ctx, order.ID, existing.ID)
	}
	req := execution.OrderRequest{Ticker: order.Ticker, Side: execution.OrderSide(order.Side), Type: execution.OrderTypeStop,
		Quantity: order.Quantity, StopPrice: order.StopPrice, TradeID: order.TradeID,
		IntentKey: key, ClientOrderID: clientIDForKey(key),
		TimeInForce: standaloneStopTimeInForce(order.Quantity),
		ExitReason:  order.ExitReason, ExitEvidence: order.ExitEvidence}
	payload, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	intent := &store.StockOrderIntent{ID: intentUUID(), IdempotencyKey: key, ClientOrderID: req.ClientOrderID,
		TradeID: order.TradeID, Ticker: order.Ticker, Side: order.Side, OrderType: order.OrderType,
		RequestedQty: order.Quantity, RequestPayload: payload, PayloadHash: fmt.Sprintf("%x", sha256.Sum256(payload)), Status: order.Status,
		Purpose: "broker_projection"}
	if err := store.CreateStockOrderIntent(ctx, *intent); err != nil {
		return nil, fmt.Errorf("adopt legacy stop intent: %w", err)
	}
	if err := store.UpdateStockOrderIntentBroker(ctx, intent.ClientOrderID, brokerOrderID, order.Status, order.FilledPrice, order.FilledQty, "", "", true); err != nil {
		return nil, err
	}
	if err := store.RepointOrderStockIntent(ctx, order.ID, intent.ID); err != nil {
		return nil, err
	}
	return intent, nil
}

func (c *Coordinator) submitReplacement(ctx context.Context, intent store.StockOrderIntent, req execution.OrderRequest, project bool) (execution.OrderResult, error) {
	lookup, hasLookup := c.inner.(execution.ClientOrderLookup)
	if hasLookup {
		prior, err := lookup.GetOrderByClientID(ctx, intent.ClientOrderID)
		if err == nil {
			_ = c.record(ctx, intent, "replace_lookup", "found", prior, "", false)
			if isTerminalZeroFillForRequest(req, prior) && !project {
				// A replacement client id proves that this PATCH generation was
				// attempted, but its terminal zero-fill result cannot provide
				// protection. Let the trailing manager take its established
				// cancel-and-resubmit path instead of repointing the mutable stop
				// row to a cancelled broker id.
				return execution.OrderResult{}, fmt.Errorf("%w: replacement client order %s is %s", execution.ErrTerminalZeroFillPlacement, intent.ClientOrderID, prior.Status)
			}
			return c.maybeProjectReplacement(ctx, intent, req, prior, project)
		}
		if !errors.Is(err, execution.ErrOrderNotFound) {
			_ = c.record(ctx, intent, "replace_lookup", "error", execution.OrderResult{}, err.Error(), false)
			return execution.OrderResult{}, fmt.Errorf("lookup stock replacement client id before submit: %w", err)
		}
	}
	replacer, ok := c.inner.(interface {
		ReplaceStopOrderWithClientID(context.Context, string, float64, string) (execution.OrderResult, error)
	})
	if !ok {
		return execution.OrderResult{}, fmt.Errorf("stock broker cannot durably replace stop")
	}
	// Do not send another PATCH after an earlier one lost its result. A missing
	// client-id response does not prove the first replacement lost, and a second
	// one could create another live protective stop. The marker is committed
	// before the request so the ambiguity survives a crash immediately after it.
	hadPriorAmbiguity, err := store.HasAmbiguousStockOrderAttempt(ctx, intent.ID)
	if err != nil {
		return execution.OrderResult{}, fmt.Errorf("check replacement ambiguity history before submit: %w", err)
	}
	if hadPriorAmbiguity {
		return execution.OrderResult{}, fmt.Errorf("%w: prior replacement submission remains unresolved", execution.ErrAmbiguousStopReplacement)
	}
	if err := store.RecordStockOrderAttempt(ctx, intent.ID, "replace_submission", "submitting", "", "", ""); err != nil {
		return execution.OrderResult{}, fmt.Errorf("record replacement submission before broker request: %w", err)
	}
	res, err := replacer.ReplaceStopOrderWithClientID(ctx, req.ReplacesBrokerOrderID, req.StopPrice, intent.ClientOrderID)
	if err == nil {
		_ = c.record(ctx, intent, "replace", "accepted", res, "", true)
		return c.maybeProjectReplacement(ctx, intent, req, res, project)
	}
	if execution.IsDefinitiveOrderRejection(err) && !execution.IsDuplicateClientOrderError(err) {
		if project || hadPriorAmbiguity {
			// Recovery has already observed no client-id record for this
			// persisted replacement. The same is true of a normal retry after a
			// prior ambiguous PATCH: a 404 on its predecessor can mean that first
			// PATCH won before its response was lost, not that this generation was
			// rejected. Retain it for a later client-id lookup.
			_ = c.record(ctx, intent, "replace", "ambiguous", execution.OrderResult{}, err.Error(), true)
			return execution.OrderResult{}, fmt.Errorf("%w: replacement predecessor unavailable after ambiguous submission: %v", execution.ErrAmbiguousStopReplacement, err)
		}
		_ = c.record(ctx, intent, "replace", "rejected", execution.OrderResult{Status: "rejected"}, err.Error(), true)
		return execution.OrderResult{}, terminalZeroFillRejectionError("stock stop replacement definitively rejected", err)
	}
	_ = c.record(ctx, intent, "replace", "ambiguous", execution.OrderResult{}, err.Error(), true)
	if hasLookup {
		prior, lookupErr := lookup.GetOrderByClientID(ctx, intent.ClientOrderID)
		if lookupErr == nil {
			_ = c.record(ctx, intent, "replace_lookup_after_ambiguous", "found", prior, "", true)
			if isTerminalZeroFillForRequest(req, prior) && !project {
				return execution.OrderResult{}, fmt.Errorf("%w: replacement client order %s is %s", execution.ErrTerminalZeroFillPlacement, intent.ClientOrderID, prior.Status)
			}
			return c.maybeProjectReplacement(ctx, intent, req, prior, project)
		}
		if !errors.Is(lookupErr, execution.ErrOrderNotFound) {
			return execution.OrderResult{}, fmt.Errorf("%w: client-id lookup after ambiguous replacement failed: %v", execution.ErrAmbiguousStopReplacement, lookupErr)
		}
		res, replayErr := replacer.ReplaceStopOrderWithClientID(ctx, req.ReplacesBrokerOrderID, req.StopPrice, intent.ClientOrderID)
		if replayErr == nil {
			_ = c.record(ctx, intent, "replace_exact_replay", "accepted", res, "", true)
			return c.maybeProjectReplacement(ctx, intent, req, res, project)
		}
		// The first PATCH may have replaced the predecessor before its response
		// was lost. A transient client-id 404 followed by a replay 404 against
		// that now-gone predecessor proves neither outcome; terminalizing it
		// would invite the trailing manager to cancel+resubmit a second stop.
		if errors.Is(replayErr, execution.ErrOrderNotFound) || execution.IsDefinitiveOrderRejection(replayErr) {
			_ = c.record(ctx, intent, "replace_exact_replay", "ambiguous", execution.OrderResult{}, replayErr.Error(), true)
			return execution.OrderResult{}, fmt.Errorf("%w: first replacement response was ambiguous; exact replay failed: %v", execution.ErrAmbiguousStopReplacement, replayErr)
		}
		_ = c.record(ctx, intent, "replace_exact_replay", "ambiguous", execution.OrderResult{}, replayErr.Error(), true)
		return execution.OrderResult{}, fmt.Errorf("%w: stock stop replacement: %v", execution.ErrAmbiguousStopReplacement, replayErr)
	}
	return execution.OrderResult{}, fmt.Errorf("%w: broker cannot resolve ambiguous replacement: %v", execution.ErrAmbiguousStopReplacement, err)
}

func (c *Coordinator) maybeProjectReplacement(ctx context.Context, intent store.StockOrderIntent, req execution.OrderRequest, res execution.OrderResult, project bool) (execution.OrderResult, error) {
	if !project {
		return res, nil
	}
	// A recovery lookup can find a replacement generation that the broker
	// already cancelled/rejected before this process could repoint the mutable
	// predecessor row. It is not a successful replacement projection; leave the
	// predecessor untouched so the manager can safely cancel/confirm and submit
	// a fresh generation on its next protection pass.
	if isTerminalZeroFillForRequest(req, res) {
		// Recovery owns this replacement generation, and broker truth proves it
		// can no longer provide coverage. Retire the generation so an order row
		// that has already advanced beyond it cannot keep making the stale
		// replacement recoverable on every startup. The predecessor projection
		// deliberately remains untouched for the manager's normal fallback.
		if err := store.SupersedeStockOrderIntent(ctx, intent.ID); err != nil {
			return execution.OrderResult{}, fmt.Errorf("retire terminal replacement intent: %w", err)
		}
		return res, nil
	}
	return c.projectReplacement(ctx, intent, req, res)
}

// projectReplacement atomically advances the one mutable stop row after an
// accepted PATCH (including lookup-only crash recovery). The predecessor
// intent retains its broker id, so a fill race on that old id remains
// independently reconcilable while the active row points at the replacement.
func (c *Coordinator) projectReplacement(ctx context.Context, intent store.StockOrderIntent, req execution.OrderRequest, res execution.OrderResult) (execution.OrderResult, error) {
	if intent.ParentIntentID == "" {
		return res, nil
	}
	order, err := store.GetOrderByStockIntentID(ctx, intent.ID)
	if err != nil {
		return execution.OrderResult{}, fmt.Errorf("load replacement projection: %w", err)
	}
	if order != nil && order.BrokerOrderID == res.BrokerOrderID {
		if err := store.SupersedeStockOrderIntent(ctx, intent.ParentIntentID); err != nil {
			return execution.OrderResult{}, fmt.Errorf("supersede already-linked replacement predecessor: %w", err)
		}
		return res, nil
	}
	order, err = store.GetOrderByStockIntentID(ctx, intent.ParentIntentID)
	if err != nil {
		return execution.OrderResult{}, fmt.Errorf("load predecessor replacement projection: %w", err)
	}
	if order == nil {
		return execution.OrderResult{}, fmt.Errorf("missing predecessor order projection for replacement %s", intent.ClientOrderID)
	}
	if order.BrokerOrderID == res.BrokerOrderID && order.StockIntentID == intent.ID {
		return res, nil
	}
	if order.BrokerOrderID == res.BrokerOrderID {
		if err := store.RepointOrderStockIntentAndSupersede(ctx, order.ID, intent.ID); err != nil {
			return execution.OrderResult{}, fmt.Errorf("complete replacement intent repoint: %w", err)
		}
		return res, nil
	}
	// A replacement response can be visible even though the predecessor filled
	// while its PATCH was in flight. Reconcile that old broker identity before
	// overwriting the sole mutable order row; otherwise the fill would become
	// unreachable once the predecessor intent is superseded.
	expectedFilledQty, expectedTradeQty := order.FilledQty, req.Quantity
	predecessor, err := c.GetOrderStatus(ctx, order.BrokerOrderID)
	if err != nil {
		return execution.OrderResult{}, fmt.Errorf("confirm predecessor stop before replacement projection: %w", err)
	}
	if predecessor.FilledQty > 0 {
		predecessorIntent := store.ExitIntent{Reason: order.ExitReason, Evidence: order.ExitEvidence}
		if predecessorIntent.Reason == "" {
			predecessorIntent = requestExitIntent(req)
		}
		if _, err := store.ClaimTradeExitWithIntent(ctx, order.TradeID, predecessorIntent); err != nil {
			return execution.OrderResult{}, fmt.Errorf("claim predecessor replacement fill: %w", err)
		}
		remaining, _, err := store.ApplyProtectiveExitAtForBrokerOrder(ctx,
			order.TradeID, order.ID, order.BrokerOrderID,
			predecessor.FilledPrice, predecessor.FilledQty,
			execution.NormalizeOrderStatusForTimeInForce(predecessor.Status, req.TimeInForce), predecessor.FilledAt,
			predecessorIntent)
		if err != nil {
			return execution.OrderResult{}, fmt.Errorf("apply predecessor replacement fill: %w", err)
		}
		if execution.IsTerminalOrderStatusForTimeInForce(predecessor.Status, req.TimeInForce) {
			if remaining <= 1e-9 {
				if err := c.recoveredCloseLocked(ctx, order.TradeID); err != nil {
					return execution.OrderResult{}, fmt.Errorf("finalize predecessor replacement fill: %w", err)
				}
				// The PATCH may already have created a replacement stop even though
				// the predecessor flattened the position in the same race. Do not
				// retain a sell order with no shares behind it.
				_ = c.CancelOrder(ctx, res.BrokerOrderID)
				replacementStatus, err := c.GetOrderStatus(ctx, res.BrokerOrderID)
				if err != nil {
					return execution.OrderResult{}, fmt.Errorf("confirm replacement after predecessor flattened trade: %w", err)
				}
				if !execution.IsTerminalOrderStatusForTimeInForce(replacementStatus.Status, req.TimeInForce) {
					return execution.OrderResult{}, fmt.Errorf("replacement stop %s remains %s after predecessor flattened trade", res.BrokerOrderID, replacementStatus.Status)
				}
				if replacementStatus.FilledQty > 0 {
					return execution.OrderResult{}, fmt.Errorf("replacement stop %s filled after predecessor flattened trade", res.BrokerOrderID)
				}
				if err := store.SupersedeStockOrderIntent(ctx, intent.ID); err != nil {
					return execution.OrderResult{}, fmt.Errorf("retire replacement after predecessor flattened trade: %w", err)
				}
				return res, nil
			} else if err := store.ReleaseTradeExitClaim(ctx, order.TradeID); err != nil {
				return execution.OrderResult{}, fmt.Errorf("release terminal predecessor replacement claim: %w", err)
			}
		}
	}
	if err := store.UpdateOrderStopReplacement(ctx, order.ID, req.ReplacesBrokerOrderID, res.BrokerOrderID,
		expectedFilledQty, expectedTradeQty, req.StopPrice, req.Quantity,
		execution.NormalizeOrderStatusForTimeInForce(res.Status, req.TimeInForce), res.FilledPrice, res.FilledQty, res.FilledAt, requestExitIntent(req)); err != nil {
		return execution.OrderResult{}, fmt.Errorf("project stock stop replacement: %w", err)
	}
	if err := store.RepointOrderStockIntentAndSupersede(ctx, order.ID, intent.ID); err != nil {
		return execution.OrderResult{}, fmt.Errorf("link stock stop replacement intent: %w", err)
	}
	return res, nil
}

// standaloneStopTimeInForce matches the broker policy used for standalone
// protective stops: Alpaca permits fractional stop orders only as DAY, while
// whole-share stops retain overnight protection as GTC.
func standaloneStopTimeInForce(quantity float64) string {
	if quantity != math.Trunc(quantity) {
		return "day"
	}
	return "gtc"
}

func newClientOrderID() string { return clientIDPrefix + strings.ReplaceAll(intentUUID(), "-", "") }
func clientIDForKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return clientIDPrefix + hex.EncodeToString(sum[:16])
}
func intentUUID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand: " + err.Error())
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b)
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}
