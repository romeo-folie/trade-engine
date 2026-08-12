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
	"math"
	"strings"
	"time"

	"github.com/romeo-folie/trade-engine/internal/execution"
	"github.com/romeo-folie/trade-engine/internal/store"
)

const clientIDPrefix = "te-stock-"

// Coordinator is an execution.Executor that persists a pre-submit intent and
// uses Alpaca's client_order_id lookup before any replay after an ambiguous
// response. It is intentionally installed inside ReadOnlyExecutor, so dry-run
// rejects mutation before an intent can be written.
type Coordinator struct{ inner execution.Executor }

func New(inner execution.Executor) *Coordinator { return &Coordinator{inner: inner} }

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
			if !isRecoveryContext(ctx) && isTerminalZeroFill(prior.Status, prior.FilledQty) {
				return execution.OrderResult{}, fmt.Errorf("%w: broker client order %s is %s", execution.ErrTerminalZeroFillPlacement, intent.ClientOrderID, prior.Status)
			}
			return c.finishSubmit(ctx, intent, req, prior)
		} else if !errors.Is(err, execution.ErrOrderNotFound) {
			_ = c.record(ctx, intent, "lookup", "error", execution.OrderResult{}, err.Error(), false)
			return execution.OrderResult{}, fmt.Errorf("lookup stock order client id before submit: %w", err)
		}
	}

	res, err := c.inner.PlaceOrder(ctx, req)
	if err == nil {
		_ = c.record(ctx, intent, "submit", "accepted", res, "", true)
		return c.finishSubmit(ctx, intent, req, res)
	}
	if execution.IsDefinitiveOrderRejection(err) && !execution.IsDuplicateClientOrderError(err) {
		_ = c.record(ctx, intent, "submit", "rejected", execution.OrderResult{Status: "rejected"}, err.Error(), true)
		return execution.OrderResult{}, fmt.Errorf("stock order definitively rejected: %w", err)
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
			_ = c.record(ctx, intent, "exact_replay", "rejected", execution.OrderResult{Status: "rejected"}, replayErr.Error(), true)
			return execution.OrderResult{}, fmt.Errorf("stock order definitively rejected: %w", replayErr)
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
		return execution.OrderResult{}, err
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
	ctx = context.WithValue(ctx, recoveryContextKey{}, true)
	intents, err := store.ListRecoverableStockOrderIntents(ctx)
	if err != nil {
		return err
	}
	for _, intent := range intents {
		// A prior intent in this same recovery snapshot can advance a mutable
		// stop row and supersede a later predecessor. Re-read it before acting
		// so the stale snapshot cannot recreate an active old-generation stop.
		current, err := store.GetStockOrderIntentByKey(ctx, intent.IdempotencyKey)
		if err != nil {
			return err
		}
		if current == nil || current.Purpose != "submit" {
			continue
		}
		intent = *current
		var req execution.OrderRequest
		if err := json.Unmarshal(intent.RequestPayload, &req); err != nil {
			return fmt.Errorf("decode stock intent %s: %w", intent.ClientOrderID, err)
		}
		if req.ClientOrderID != intent.ClientOrderID {
			return fmt.Errorf("stock intent %s payload client id mismatch", intent.ClientOrderID)
		}
		var res execution.OrderResult
		if req.ReplacesBrokerOrderID != "" {
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
		if err := c.projectRecoveredMarketExit(ctx, intent, req, res); err != nil {
			return err
		}
	}
	return nil
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
			return nil
		}
		return fmt.Errorf("analyst stock exit %s has no claimed trade links", intent.ClientOrderID)
	}

	// The broker's FilledQty is cumulative across the one aggregate order.
	// First reserve every existing lot row's original capacity. This preserves
	// allocation after a process dies halfway through projecting the lots, even
	// if an earlier lot has since been fully closed and is absent from `trades`.
	existingOrders, err := store.GetOrdersByStockIntent(ctx, intent.ID)
	if err != nil {
		return fmt.Errorf("load analyst exit projections: %w", err)
	}
	remainingFill := math.Max(0, res.FilledQty)
	allocations := make(map[string]float64, len(trades))
	for _, order := range existingOrders {
		allocation := math.Min(math.Max(0, order.Quantity), remainingFill)
		remainingFill -= allocation
		allocations[order.TradeID] = allocation
	}
	for _, trade := range trades {
		allocation, exists := allocations[trade.ID]
		if !exists {
			allocation = math.Min(math.Max(0, trade.Quantity), remainingFill)
			remainingFill -= allocation
		}
		if err := c.projectRecoveredMarketExitAllocation(ctx, intent, req, res, trade.ID, trade.Quantity, allocation); err != nil {
			return err
		}
	}
	return nil
}

func isRecoveredMarketExitKey(key string) bool {
	return strings.HasPrefix(key, "software-exit:") ||
		strings.HasPrefix(key, "trailing-market-exit:") ||
		strings.HasPrefix(key, "analyst-exit:")
}

func analystExitSignalID(key, ticker string) (string, bool) {
	parts := strings.SplitN(strings.TrimPrefix(key, "analyst-exit:"), ":", 2)
	return parts[0], len(parts) == 2 && parts[0] != "" && parts[1] == ticker
}

// projectRecoveredMarketExitAllocation completes one durable exit-order link.
// It first writes the assigned broker cumulative fill and then atomically
// applies it to the claimed trade. A retry sees the same row and is a no-op
// once the trade has already been finalized.
func (c *Coordinator) projectRecoveredMarketExitAllocation(
	ctx context.Context,
	intent store.StockOrderIntent,
	req execution.OrderRequest,
	res execution.OrderResult,
	tradeID string,
	quantity, allocatedFill float64,
) error {
	existing, err := store.GetOrderByStockIntentAndTrade(ctx, intent.ID, tradeID)
	if err != nil {
		return err
	}
	orderID := ""
	if existing != nil {
		orderID = existing.ID
	} else {
		brokerID := res.BrokerOrderID
		if brokerID == "" {
			brokerID = intent.BrokerOrderID
		}
		if brokerID == "" {
			return fmt.Errorf("recovered market exit %s has no broker order id", intent.ClientOrderID)
		}
		orderID, err = store.InsertOrderWithRetry(ctx, store.Order{
			TradeID: tradeID, BrokerOrderID: brokerID, Ticker: req.Ticker,
			Side: string(req.Side), OrderType: string(req.Type), Quantity: quantity,
			FilledPrice: res.FilledPrice, FilledQty: allocatedFill, FilledAt: filledAtPtr(res.FilledAt),
			Status: execution.NormalizeOrderStatus(res.Status), StockIntentID: intent.ID,
		})
		if err != nil {
			return fmt.Errorf("project recovered market exit order: %w", err)
		}
	}
	if allocatedFill <= 0 {
		return nil
	}
	if res.FilledPrice <= 0 {
		return fmt.Errorf("recovered market exit %s has fill without price", intent.ClientOrderID)
	}
	remaining, _, err := store.ApplyPartialExitAt(ctx, tradeID, orderID, res.FilledPrice, allocatedFill,
		execution.NormalizeOrderStatus(res.Status), res.FilledAt)
	if err != nil {
		return fmt.Errorf("apply recovered market exit fill: %w", err)
	}
	if execution.IsTerminalOrderStatus(res.Status) && remaining <= 1e-9 {
		if _, err := store.FinalizeTradeExit(ctx, tradeID); err != nil {
			return fmt.Errorf("finalize recovered market exit: %w", err)
		}
	}
	return nil
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
			Status: execution.NormalizeOrderStatus(res.Status), IsStopLoss: true,
			ExitReason: requestExitIntent(req).Reason, ExitEvidence: requestExitIntent(req).Evidence, StockIntentID: intent.ID})
		if err != nil {
			return err
		}
		existing = &store.Order{ID: orderID, BrokerOrderID: brokerID}
	}
	if res.FilledQty <= 0 {
		if execution.IsTerminalOrderStatus(res.Status) {
			return store.UpdateOrderStatusByStockIntent(ctx, intent.ID, execution.NormalizeOrderStatus(res.Status))
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
		res.FilledPrice, res.FilledQty, execution.NormalizeOrderStatus(res.Status), res.FilledAt,
		exitIntent)
	if err != nil {
		return fmt.Errorf("apply recovered stop fill: %w", err)
	}
	if execution.IsTerminalOrderStatus(res.Status) && remaining <= 1e-9 {
		if _, err := store.FinalizeTradeExit(ctx, req.TradeID); err != nil {
			return fmt.Errorf("finalize recovered stop fill: %w", err)
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
	if res.FilledQty <= 0 {
		if execution.IsTerminalOrderStatus(res.Status) {
			if err := store.UpdateOrderStatusByStockIntent(ctx, intent.ID, execution.NormalizeOrderStatus(res.Status)); err != nil {
				return fmt.Errorf("terminalize recovered zero-fill entry projection: %w", err)
			}
		}
		return nil
	}
	signalID := strings.TrimPrefix(intent.IdempotencyKey, "entry:")
	if signalID == "" || strings.Contains(signalID, ":") {
		return fmt.Errorf("invalid stock entry intent key %q", intent.IdempotencyKey)
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
	tradeID, err := store.InsertTradeForStockIntent(ctx, store.Trade{
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
	brokerID := res.BrokerOrderID
	if brokerID == "" {
		brokerID = intent.BrokerOrderID
	}
	if order == nil {
		if _, err := store.InsertOrderWithRetry(ctx, store.Order{
			TradeID: tradeID, BrokerOrderID: brokerID, Ticker: req.Ticker,
			Side: string(req.Side), OrderType: string(req.Type), Quantity: req.Quantity,
			FilledPrice: res.FilledPrice, FilledQty: res.FilledQty,
			Status: execution.NormalizeOrderStatus(res.Status), StockIntentID: intent.ID,
		}); err != nil {
			return fmt.Errorf("project recovered stock order: %w", err)
		}
	} else if err := store.AttachOrderToTradeByStockIntent(ctx, intent.ID, tradeID); err != nil {
		return fmt.Errorf("attach recovered stock order: %w", err)
	}
	if brokerID != "" {
		if err := store.ApplyEntryFill(ctx, tradeID, brokerID, res.FilledPrice, res.FilledQty, execution.NormalizeOrderStatus(res.Status)); err != nil {
			return fmt.Errorf("apply recovered stock entry fill: %w", err)
		}
	}
	if err := c.projectRecoveredEntryExits(ctx, intent, req, res, tradeID); err != nil {
		return err
	}
	if err := store.UpdateSignalStatus(ctx, signalID, "executed", ""); err != nil {
		return fmt.Errorf("mark recovered stock entry signal executed: %w", err)
	}
	return nil
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

// ensureRecoveredProtectiveStop maintains one active protection lineage for an
// uncapped recovered entry. A cumulative entry fill can increase after a
// restart: cancel and confirm the old generation before submitting the resized
// one, so protected working quantity never exceeds the held position.
func (c *Coordinator) ensureRecoveredProtectiveStop(ctx context.Context, parent store.StockOrderIntent, req execution.OrderRequest, res execution.OrderResult, tradeID string) error {
	trade, err := store.GetTradeByID(ctx, tradeID)
	if err != nil {
		return fmt.Errorf("load recovered protective-stop trade: %w", err)
	}
	if trade == nil || trade.Status == "closed" || trade.Quantity <= 1e-9 {
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
	if existing != nil && existing.Quantity == desiredQty && existing.StopPrice == req.PolicyStopLoss {
		return nil
	}
	key := fmt.Sprintf("recovered-protective-stop:%s:%s:%.8f", tradeID, parent.ID, req.PolicyStopLoss)
	remainingQty := desiredQty
	if existing != nil {
		if err := c.CancelOrder(ctx, existing.BrokerOrderID); err != nil {
			return fmt.Errorf("cancel prior recovered protective stop: %w", err)
		}
		status, err := c.GetOrderStatus(ctx, existing.BrokerOrderID)
		if err != nil {
			return fmt.Errorf("confirm cancellation of recovered protective stop: %w", err)
		}
		normalized := execution.NormalizeOrderStatus(status.Status)
		if !execution.IsTerminalOrderStatus(status.Status) {
			return fmt.Errorf("recovered protective stop %s remains %s after cancel", existing.BrokerOrderID, status.Status)
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
			FilledQty: stopRes.FilledQty, Status: execution.NormalizeOrderStatus(stopRes.Status), IsStopLoss: true,
			ExitReason: requestExitIntent(stopReq).Reason, ExitEvidence: requestExitIntent(stopReq).Evidence, StockIntentID: stopIntent.ID})
		return err
	}
	if err := store.UpdateOrderStopReplacementAndSupersede(ctx, existing.ID, existing.BrokerOrderID, stopRes.BrokerOrderID, stopIntent.ID,
		existing.FilledQty, remainingQty, req.PolicyStopLoss, remainingQty,
		execution.NormalizeOrderStatus(stopRes.Status), stopRes.FilledPrice, stopRes.FilledQty, stopRes.FilledAt, requestExitIntent(stopReq)); err != nil {
		return err
	}
	return nil
}

func (c *Coordinator) record(ctx context.Context, intent store.StockOrderIntent, operation, outcome string, res execution.OrderResult, errText string, submitted bool) error {
	status := execution.NormalizeOrderStatus(res.Status)
	if res.Status == "" {
		status = intent.Status
	}
	if err := store.UpdateStockOrderIntentBroker(ctx, intent.ClientOrderID, res.BrokerOrderID, status, res.FilledPrice, res.FilledQty, res.RequestID, errText, submitted); err != nil {
		return err
	}
	return store.RecordStockOrderAttempt(ctx, intent.ID, operation, outcome, res.BrokerOrderID, res.RequestID, errText)
}

func (c *Coordinator) CancelOrder(ctx context.Context, id string) error {
	return c.inner.CancelOrder(ctx, id)
}
func (c *Coordinator) GetOrderStatus(ctx context.Context, id string) (execution.OrderResult, error) {
	return c.inner.GetOrderStatus(ctx, id)
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
func (c *Coordinator) ReplaceStopOrder(ctx context.Context, id string, px float64) (execution.OrderResult, error) {
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
	key := fmt.Sprintf("replace:%s:%.8f", predecessor.IdempotencyKey, px)
	req := execution.OrderRequest{
		Ticker: predecessor.Ticker, Side: execution.OrderSide(predecessor.Side),
		Type: execution.OrderTypeStop, Quantity: predecessor.RequestedQty, StopPrice: px,
		TradeID: predecessor.TradeID, IntentKey: key, ClientOrderID: clientIDForKey(key),
		ReplacesBrokerOrderID: id,
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
		ExitReason: order.ExitReason, ExitEvidence: order.ExitEvidence}
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
	res, err := replacer.ReplaceStopOrderWithClientID(ctx, req.ReplacesBrokerOrderID, req.StopPrice, intent.ClientOrderID)
	if err == nil {
		_ = c.record(ctx, intent, "replace", "accepted", res, "", true)
		return c.maybeProjectReplacement(ctx, intent, req, res, project)
	}
	if execution.IsDefinitiveOrderRejection(err) && !execution.IsDuplicateClientOrderError(err) {
		if project {
			// Recovery has already observed no client-id record for this
			// persisted replacement. A 404 on its predecessor can mean the
			// original PATCH won before the process died, not that this durable
			// replacement was rejected. Retain it for a later client-id lookup.
			_ = c.record(ctx, intent, "replace", "ambiguous", execution.OrderResult{}, err.Error(), true)
			return execution.OrderResult{}, fmt.Errorf("%w: recovery replacement predecessor unavailable: %v", execution.ErrAmbiguousStopReplacement, err)
		}
		_ = c.record(ctx, intent, "replace", "rejected", execution.OrderResult{Status: "rejected"}, err.Error(), true)
		return execution.OrderResult{}, fmt.Errorf("stock stop replacement definitively rejected: %w", err)
	}
	_ = c.record(ctx, intent, "replace", "ambiguous", execution.OrderResult{}, err.Error(), true)
	if hasLookup {
		prior, lookupErr := lookup.GetOrderByClientID(ctx, intent.ClientOrderID)
		if lookupErr == nil {
			_ = c.record(ctx, intent, "replace_lookup_after_ambiguous", "found", prior, "", true)
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
	if err := store.UpdateOrderStopReplacement(ctx, order.ID, req.ReplacesBrokerOrderID, res.BrokerOrderID,
		order.FilledQty, req.Quantity, req.StopPrice, req.Quantity,
		execution.NormalizeOrderStatus(res.Status), res.FilledPrice, res.FilledQty, res.FilledAt, requestExitIntent(req)); err != nil {
		return execution.OrderResult{}, fmt.Errorf("project stock stop replacement: %w", err)
	}
	if err := store.RepointOrderStockIntentAndSupersede(ctx, order.ID, intent.ID); err != nil {
		return execution.OrderResult{}, fmt.Errorf("link stock stop replacement intent: %w", err)
	}
	return res, nil
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
