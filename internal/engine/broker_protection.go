package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/romeo-folie/trade-engine/internal/execution"
	"github.com/romeo-folie/trade-engine/internal/logger"
	"github.com/romeo-folie/trade-engine/internal/store"
)

var brokerProtectionVerificationRetryDelay = 2 * time.Second

const brokerProtectionVerificationAttempts = 5

// containUnprotectedBrokerEntry stops a bracket parent before flattening an
// accepted position that has not obtained verifiable broker protection. Alpaca
// keeps bracket children held until the parent has fully filled, so flattening
// while that parent can still fill would risk selling short on a late fill.
//
// It intentionally reuses the ordinary durable market-exit and exit-claim
// path. An ambiguous cancellation keeps the trade owned by this containment
// attempt and is retried; it never falls back to application-managed exits.
func (e *Engine) containUnprotectedBrokerEntry(ctx context.Context, tradeID, entryBrokerOrderID, ticker, reason string) {
	const attempts = 3
	for attempt := 0; attempt < attempts; attempt++ {
		if err := e.executor.CancelOrder(ctx, entryBrokerOrderID); err != nil {
			logger.Warn("broker protection: cancel entry before containment", "trade_id", tradeID,
				"broker_order_id", entryBrokerOrderID, "attempt", attempt+1, "err", err)
		}
		status, err := e.executor.GetOrderStatus(ctx, entryBrokerOrderID)
		if err != nil {
			logger.Warn("broker protection: cannot confirm entry before containment", "trade_id", tradeID,
				"broker_order_id", entryBrokerOrderID, "attempt", attempt+1, "err", err)
			continue
		}
		if status.FilledQty > 0 {
			if err := store.ApplyEntryFill(ctx, tradeID, entryBrokerOrderID, status.FilledPrice,
				status.FilledQty, execution.NormalizeOrderStatus(status.Status)); err != nil {
				logger.Warn("broker protection: persist entry fill before containment", "trade_id", tradeID,
					"broker_order_id", entryBrokerOrderID, "err", err)
				return
			}
		}
		// Strict bracket parents are GTC. Alpaca reports a resting GTC order
		// as done_for_day outside the session; it remains eligible to resume
		// and must not be treated as safely cancelled.
		if !execution.IsTerminalOrderStatusForTimeInForce(status.Status, "gtc") {
			continue
		}

		trade, err := store.GetTradeByID(ctx, tradeID)
		if err != nil || trade == nil || trade.Status != "open" || trade.Quantity <= 1e-9 {
			return
		}
		// The parent snapshot is the only authoritative source for bracket
		// children when projection failed. Clear it before claiming the trade:
		// if confirmation is uncertain, leave the trade open so the next sweep
		// can retry instead of stranding it under an exit claim.
		if !e.cancelBrokerBracketChildren(ctx, tradeID, status.ChildOrders) {
			return
		}
		intent := newExitIntent(store.ExitReasonBrokerProtectionFailure, map[string]any{
			"broker_protection_failure": reason,
			"entry_broker_order_id":     entryBrokerOrderID,
		})
		claimed, err := store.ClaimTradeExitWithIntent(ctx, tradeID, intent)
		if err != nil || !claimed {
			return // another durable exit owner now owns the position
		}
		remainingByTrade, _, clear, releaseSafe := e.cancelClaimedExitLegs(ctx, []store.Trade{*trade}, ticker)
		if !clear {
			if releaseSafe {
				_ = store.ReleaseTradeExitClaim(ctx, tradeID)
			}
			return
		}
		quantity := remainingByTrade[tradeID]
		if quantity <= 1e-9 {
			return
		}
		exitReq := execution.OrderRequest{
			Ticker: ticker, Side: execution.OrderSideSell, Type: execution.OrderTypeMarket,
			Quantity: quantity, ReferencePrice: status.FilledPrice, TradeID: tradeID,
			IntentKey:  "broker-protection-flatten:" + tradeID,
			ExitReason: store.ExitReasonBrokerProtectionFailure, ExitEvidence: intent.Evidence,
		}
		result, intentKey, terminalZero, err := e.placeMarketExitWithTerminalRetries(ctx, exitReq)
		if err != nil {
			if terminalZero {
				_ = store.ReleaseTradeExitClaim(ctx, tradeID)
			}
			logger.Error("broker protection: flatten placement failed", "trade_id", tradeID, "err", err)
			return
		}
		stockIntentID, err := stockIntentIDForAcceptedOrder(ctx, e.executor, intentKey)
		if err != nil {
			logger.Error("broker protection: accepted flatten projection unavailable", "trade_id", tradeID, "err", err)
			return
		}
		if _, err := store.InsertOrderWithRetry(ctx, store.Order{
			TradeID: tradeID, BrokerOrderID: result.BrokerOrderID, Ticker: ticker,
			Side: string(execution.OrderSideSell), OrderType: string(execution.OrderTypeMarket),
			Quantity: quantity, FilledPrice: result.FilledPrice, FilledQty: result.FilledQty,
			FilledAt: filledAtPtr(result.FilledAt), Status: execution.NormalizeOrderStatus(result.Status),
			StockIntentID: stockIntentID, ExitReason: store.ExitReasonBrokerProtectionFailure,
			ExitEvidence: intent.Evidence,
		}); err != nil {
			logger.Error("broker protection: persist accepted flatten", "trade_id", tradeID, "err", err)
		}
		logger.Error("broker protection: flattening unprotected position", "trade_id", tradeID,
			"ticker", ticker, "quantity", quantity, "reason", reason)
		return
	}
	logger.Error("broker protection: entry cancellation remains unresolved; retaining failed-closed ownership",
		"trade_id", tradeID, "broker_order_id", entryBrokerOrderID, "reason", reason)
}

// cancelBrokerBracketChildren cancels and confirms every child reported by a
// nested bracket-parent snapshot. The local projection can be incomplete
// precisely when this containment path is needed, so local orders alone are
// not a sufficient cancellation inventory.
func (e *Engine) cancelBrokerBracketChildren(ctx context.Context, tradeID string, children []execution.OrderLeg) bool {
	for _, child := range children {
		if child.BrokerOrderID == "" {
			continue
		}
		// The nested parent snapshot can report a child fill even after the
		// child's own endpoint has been purged. Never flatten against the
		// stale local trade quantity in that case.
		if child.FilledQty > 1e-9 || execution.NormalizeOrderStatusForTimeInForce(child.Status, "gtc") == "filled" {
			if err := persistParentReportedBracketChildFill(ctx, tradeID, child); err != nil {
				logger.Warn("broker protection: persist parent-reported child fill", "trade_id", tradeID,
					"broker_order_id", child.BrokerOrderID, "err", err)
			}
			logger.Warn("broker protection: parent reports broker child fill during containment; awaiting reconciliation",
				"broker_order_id", child.BrokerOrderID, "filled_qty", child.FilledQty)
			return false
		}
		if err := e.executor.CancelOrder(ctx, child.BrokerOrderID); err != nil {
			logger.Warn("broker protection: cancel broker child before containment",
				"broker_order_id", child.BrokerOrderID, "err", err)
		}
		status, err := e.executor.GetOrderStatus(ctx, child.BrokerOrderID)
		if errors.Is(err, execution.ErrOrderNotFound) {
			continue
		}
		if err != nil {
			logger.Warn("broker protection: cannot confirm broker child cancellation",
				"broker_order_id", child.BrokerOrderID, "err", err)
			return false
		}
		// A filled or partially filled unprojected child changes the held
		// quantity, which cannot safely be inferred from the parent alone.
		// Keep this trade out of a market flatten until reconciliation projects
		// the broker fill; that is safer than risking an over-sell.
		if status.FilledQty > 1e-9 || execution.NormalizeOrderStatusForTimeInForce(status.Status, "gtc") == "filled" {
			logger.Warn("broker protection: broker child filled during containment; awaiting reconciliation",
				"broker_order_id", child.BrokerOrderID, "filled_qty", status.FilledQty)
			return false
		}
		if !execution.IsTerminalOrderStatusForTimeInForce(status.Status, "gtc") {
			logger.Warn("broker protection: broker child remains live after cancellation",
				"broker_order_id", child.BrokerOrderID, "status", status.Status)
			return false
		}
	}
	return true
}

// persistParentReportedBracketChildFill applies a nested parent snapshot to an
// already-projected child row before containment defers. Alpaca can purge the
// child's direct endpoint after a fill, leaving this parent response as the
// only evidence capable of reducing the durable trade quantity.
func persistParentReportedBracketChildFill(ctx context.Context, tradeID string, child execution.OrderLeg) error {
	if tradeID == "" || child.BrokerOrderID == "" || child.FilledQty <= 0 {
		return nil
	}
	order, err := store.GetOrderByBrokerOrderID(ctx, child.BrokerOrderID)
	if err != nil || order == nil {
		return err
	}
	if order.TradeID != tradeID {
		return fmt.Errorf("broker child is linked to trade %s", order.TradeID)
	}
	_, _, err = store.ApplyPartialExitAtForBrokerOrder(
		ctx, tradeID, order.ID, child.BrokerOrderID,
		child.FilledPrice, child.FilledQty,
		execution.NormalizeOrderStatusForTimeInForce(child.Status, "gtc"), child.FilledAt,
	)
	return err
}

func (e *Engine) containUnprotectedBrokerEntryAsync(tradeID, entryBrokerOrderID, ticker, reason string) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		e.containUnprotectedBrokerEntry(ctx, tradeID, entryBrokerOrderID, ticker, reason)
	}()
}

// cancelLegacyBrokerHeldEntry retires a working BUY that was accepted before
// broker protection became mandatory. It does not mark the signal terminal
// until broker truth is terminal: recovery owns a cancellation-race fill and
// applies the currently active strict policy to it.
func (e *Engine) cancelLegacyBrokerHeldEntry(ctx context.Context, intentKey string) {
	intent, err := store.GetStockOrderIntentByKey(ctx, intentKey)
	if err != nil || intent == nil || intent.BrokerOrderID == "" {
		logger.Error("broker protection: cannot locate legacy broker-held entry",
			"intent_key", intentKey, "err", err)
		return
	}
	var req execution.OrderRequest
	if err := json.Unmarshal(intent.RequestPayload, &req); err != nil {
		logger.Error("broker protection: decode legacy broker-held entry request",
			"intent_key", intentKey, "err", err)
		return
	}
	timeInForce := req.TimeInForce

	var terminal execution.OrderResult
	confirmed := false
	for attempt := 0; attempt < 3; attempt++ {
		if err := e.executor.CancelOrder(ctx, intent.BrokerOrderID); err != nil {
			logger.Debug("broker protection: cancel legacy broker-held entry",
				"intent_key", intentKey, "broker_order_id", intent.BrokerOrderID,
				"attempt", attempt+1, "err", err)
		}
		status, err := e.executor.GetOrderStatus(ctx, intent.BrokerOrderID)
		if errors.Is(err, execution.ErrOrderNotFound) {
			// A purged parent may have filled during the cancellation race. Unlike
			// an exit sibling, this entry's fill cannot be inferred from a local
			// trade or order row, so a 404 is not proof of a zero-fill cancel.
			// Keep the durable reservation for recovery rather than losing an
			// unprojected, now-unprotected position.
			logger.Warn("broker protection: legacy entry lookup purged before fill confirmation",
				"intent_key", intentKey, "broker_order_id", intent.BrokerOrderID,
				"attempt", attempt+1)
			continue
		}
		if err != nil {
			continue
		}
		if !execution.IsTerminalOrderStatusForTimeInForce(status.Status, timeInForce) {
			continue
		}
		terminal = status
		confirmed = true
		break
	}
	if !confirmed {
		logger.Error("broker protection: legacy broker-held entry remains unresolved; retaining durable reservation",
			"intent_key", intentKey, "broker_order_id", intent.BrokerOrderID)
		return
	}
	if err := store.UpdateStockOrderIntentBroker(ctx, intent.ClientOrderID, intent.BrokerOrderID,
		execution.NormalizeOrderStatusForTimeInForce(terminal.Status, timeInForce), terminal.FilledPrice, terminal.FilledQty,
		terminal.RequestID, "", false); err != nil {
		logger.Error("broker protection: persist retired legacy entry", "intent_key", intentKey, "err", err)
		return
	}
	// If the cancellation race filled, later generic recovery must not revert to
	// the legacy request's app-managed policy when this immediate Recover is
	// transiently unavailable. Record the current strict-policy obligation next
	// to the immutable original intent before attempting recovery.
	if terminal.FilledQty > 0 {
		if err := store.RequireBrokerProtectionForStockEntry(ctx, intent.ID); err != nil {
			logger.Error("broker protection: persist legacy fill containment obligation", "intent_key", intentKey, "err", err)
			return
		}
	}
	recoverer, ok := e.executor.(interface{ Recover(context.Context) error })
	if !ok {
		logger.Error("broker protection: legacy entry has no durable recovery executor", "intent_key", intentKey)
		return
	}
	if err := recoverer.Recover(ctx); err != nil {
		logger.Error("broker protection: recover retired legacy entry", "intent_key", intentKey, "err", err)
		return
	}
	// Recovery normally invokes this observer. Invoke it again defensively for
	// executor implementations that project but do not publish observers.
	e.containRecoveredUnprotectedBracketForced(ctx, intentKey)
}

// verifyOrContainBrokerEntry gives a sparse placement response one fresh
// nested broker read before cancelling it. Alpaca normally includes legs in a
// bracket response, but a response that only supplies child IDs is not itself
// proof that the stop is inactive.
func (e *Engine) verifyOrContainBrokerEntryAsync(tradeID, entryBrokerOrderID, ticker string, requestedQty float64) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		for attempt := 0; attempt < brokerProtectionVerificationAttempts; attempt++ {
			status, err := e.executor.GetOrderStatus(ctx, entryBrokerOrderID)
			if err != nil {
				logger.Warn("broker protection: fresh parent snapshot unavailable; retrying",
					"trade_id", tradeID, "broker_order_id", entryBrokerOrderID,
					"attempt", attempt+1, "err", err)
				if !waitForBrokerProtectionRetry(ctx) {
					return
				}
				continue
			}
			trade, err := store.GetTradeByID(ctx, tradeID)
			if err != nil || trade == nil {
				logger.Warn("broker protection: trade projection unavailable; retrying verification",
					"trade_id", tradeID, "broker_order_id", entryBrokerOrderID,
					"attempt", attempt+1, "err", err)
				if !waitForBrokerProtectionRetry(ctx) {
					return
				}
				continue
			}
			// The broker's cumulative parent fill is newer than the asynchronous
			// local projection. Coverage must include either value's larger held
			// quantity so a newly filled remainder cannot be left unprotected.
			heldQty := brokerCoverageHeldQty(trade.Quantity, status)
			state := brokerBracketProtectionState(status, heldQty)
			if state.UnprotectedQty <= 1e-9 {
				return
			}
			if attempt < brokerProtectionVerificationAttempts-1 {
				logger.Info("broker protection: bracket unprotected; re-verifying",
					"trade_id", tradeID, "attempt", attempt+1, "status", status.Status,
					"transient", brokerBracketProtectionTransient(status))
				if !waitForBrokerProtectionRetry(ctx) {
					return
				}
				continue
			}
			if !e.brokerBracketProtectionDefinitivelyUnprotected(ctx, status, heldQty) {
				// A parent snapshot can lag a child transition.  In particular, do
				// not flatten a live position merely because a direct child read is
				// temporarily unavailable or reports a newer working stop.
				logger.Error("broker protection: coverage remains uncertain; retaining broker ownership",
					"trade_id", tradeID, "broker_order_id", entryBrokerOrderID,
					"held_qty", heldQty)
				return
			}
			logger.Error("broker protection: containment required", "trade_id", tradeID,
				"held_qty", state.HeldQty, "working_entry_qty", state.WorkingEntryQty,
				"protected_qty", state.ProtectedQty, "unprotected_qty", state.UnprotectedQty)
			e.containUnprotectedBrokerEntry(ctx, tradeID, entryBrokerOrderID, ticker,
				brokerProtectionFailureReason(status, requestedQty))
			return
		}
		logger.Error("broker protection: verification remains uncertain; retaining broker ownership",
			"trade_id", tradeID, "broker_order_id", entryBrokerOrderID)
	}()
}

func waitForBrokerProtectionRetry(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(brokerProtectionVerificationRetryDelay):
		return true
	}
}

// brokerBracketProtectionTransient reports expected Alpaca bracket transitions.
// A partial parent keeps its children held until the parent completes; that
// snapshot is not active protection for the filled shares, but it merits a
// bounded recheck before containment cancels the parent.
func brokerBracketProtectionTransient(result execution.OrderResult) bool {
	if result.Status == "partial" || (result.Quantity > 0 && result.FilledQty+1e-9 < result.Quantity) {
		return true
	}
	for _, child := range result.ChildOrders {
		if (child.Type == "stop" || child.Type == "stop_limit") && child.Status == "held" {
			return true
		}
	}
	return false
}

func brokerCoverageHeldQty(localQty float64, parent execution.OrderResult) float64 {
	return math.Max(localQty, parent.FilledQty)
}

// recoverAndVerifyAcceptedBrokerEntryAsync closes the post-acceptance gap
// before the local trade/child projections complete. Strict entries are
// durable before the broker request, so recovery can create or attach the
// trade and then apply the same broker-coverage verification.
func (e *Engine) recoverAndVerifyAcceptedBrokerEntryAsync(intentKey string) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		recoverer, ok := e.executor.(interface{ Recover(context.Context) error })
		if !ok {
			logger.Error("broker protection: accepted strict entry has no durable recovery executor",
				"intent_key", intentKey)
			return
		}
		for attempt := 0; attempt < 3; attempt++ {
			if err := recoverer.Recover(ctx); err != nil {
				logger.Warn("broker protection: recover accepted strict entry before verification",
					"intent_key", intentKey, "attempt", attempt+1, "err", err)
				continue
			}
			e.containRecoveredUnprotectedBracket(ctx, intentKey)
			return
		}
		// Retain the durable intent for the coordinator's next recovery sweep;
		// an unverified strict entry must never be downgraded to app ownership.
		logger.Error("broker protection: accepted strict entry recovery remains uncertain",
			"intent_key", intentKey)
	}()
}

func brokerProtectionFailureReason(result execution.OrderResult, requestedQty float64) string {
	if len(result.ChildOrderIDs) < 2 {
		return "broker did not return both bracket child identities"
	}
	if result.FilledQty > 0 && result.FilledQty+1e-9 < requestedQty {
		return "bracket parent partially filled before child protection could activate"
	}
	return fmt.Sprintf("broker protection is not verified for %.6f shares", requestedQty)
}

// brokerProtectionState is a broker-snapshot accounting of a bracket's
// exposure. Alpaca uses "held" for a bracket's secondary orders while they
// wait for their trigger. Once the parent is completely filled, a held stop is
// therefore broker-resident downside protection, not an absent stop. A held
// child under a partially-filled parent remains insufficient because Alpaca
// does not activate bracket exits until the parent completes.
type brokerProtectionState struct {
	HeldQty         float64
	WorkingEntryQty float64
	ProtectedQty    float64
	UnprotectedQty  float64
}

func brokerBracketProtectionState(result execution.OrderResult, heldQty float64) brokerProtectionState {
	state := brokerProtectionState{HeldQty: heldQty, WorkingEntryQty: result.RemainingQty}
	if heldQty <= 1e-9 || result.Status == "partial" ||
		(result.Quantity > 0 && result.FilledQty+1e-9 < result.Quantity) {
		state.UnprotectedQty = heldQty
		return state
	}
	for _, child := range result.ChildOrders {
		if child.Type != "stop" && child.Type != "stop_limit" {
			continue
		}
		if execution.IsTerminalOrderStatusForTimeInForce(child.Status, "gtc") || child.RemainingQty <= 0 {
			continue
		}
		// Only the still-working remainder protects the position. A partially
		// filled stop's cumulative original quantity includes shares already
		// sold and must not be counted as coverage.
		state.ProtectedQty = child.RemainingQty
		break
	}
	state.UnprotectedQty = heldQty - state.ProtectedQty
	if state.UnprotectedQty < 0 {
		state.UnprotectedQty = 0
	}
	return state
}

// brokerBracketProtectionVerified requires a broker-resident stop covering the
// exact locally held quantity. A held stop counts only after the parent has
// completely filled; brokerBracketProtectionState enforces that distinction.
func brokerBracketProtectionVerified(result execution.OrderResult, heldQty float64) bool {
	state := brokerBracketProtectionState(result, heldQty)
	return state.HeldQty <= 1e-9 || state.UnprotectedQty <= 1e-9
}

// brokerBracketProtectionDefinitivelyUnprotected adds a direct child read to
// the final containment decision. Nested parent responses are eventually
// consistent, so a zero-fill terminal/undersized stop shown by the parent is
// not enough to flatten until the child endpoint agrees. A partial parent is
// deliberately different: Alpaca's exits cannot protect its filled shares
// until the parent completes, so containment remains the fail-safe outcome.
func (e *Engine) brokerBracketProtectionDefinitivelyUnprotected(
	ctx context.Context,
	parent execution.OrderResult,
	heldQty float64,
) bool {
	if heldQty <= 1e-9 {
		return false
	}
	if parent.Status == "partial" || (parent.Quantity > 0 && parent.FilledQty+1e-9 < parent.Quantity) {
		return true
	}

	var stop *execution.OrderLeg
	for i := range parent.ChildOrders {
		child := &parent.ChildOrders[i]
		if child.Type == "stop" || child.Type == "stop_limit" {
			stop = child
			break
		}
	}
	// Repeated terminal full-parent snapshots with no stop identity are the
	// only case where a direct child confirmation is impossible.
	if stop == nil || stop.BrokerOrderID == "" {
		return true
	}
	direct, err := e.executor.GetOrderStatus(ctx, stop.BrokerOrderID)
	if err != nil {
		logger.Warn("broker protection: direct stop verification unavailable; retaining broker ownership",
			"broker_order_id", stop.BrokerOrderID, "err", err)
		return false
	}
	return !brokerBracketProtectionVerified(execution.OrderResult{
		Status: "filled", Quantity: heldQty, FilledQty: heldQty,
		ChildOrders: []execution.OrderLeg{directOrderLeg(stop.BrokerOrderID, direct)},
	}, heldQty)
}

func directOrderLeg(brokerOrderID string, result execution.OrderResult) execution.OrderLeg {
	return execution.OrderLeg{
		BrokerOrderID: brokerOrderID,
		Type:          "stop",
		Status:        result.Status,
		Quantity:      result.Quantity,
		RemainingQty:  result.RemainingQty,
		FilledQty:     result.FilledQty,
	}
}

// containRecoveredUnprotectedBracket is called after a durable recovery
// projection (including the zero-fill entry poll). It keeps an accepted entry
// out of application-managed monitoring when broker protection cannot be
// verified from the nested parent snapshot.
func (e *Engine) containRecoveredUnprotectedBracket(ctx context.Context, intentKey string) {
	e.containRecoveredUnprotectedBracketWithPolicy(ctx, intentKey, false)
}

// containRecoveredUnprotectedBracketForced is used only after a pre-policy
// broker-held entry was explicitly cancelled under the current strict policy.
// Ordinary recovery must honor the immutable request flag instead.
func (e *Engine) containRecoveredUnprotectedBracketForced(ctx context.Context, intentKey string) {
	e.containRecoveredUnprotectedBracketWithPolicy(ctx, intentKey, true)
}

func (e *Engine) containRecoveredUnprotectedBracketWithPolicy(ctx context.Context, intentKey string, forceProtection bool) {
	intent, err := store.GetStockOrderIntentByKey(ctx, intentKey)
	if err != nil || intent == nil || intent.TradeID == "" || intent.BrokerOrderID == "" {
		return
	}
	var req execution.OrderRequest
	if json.Unmarshal(intent.RequestPayload, &req) != nil {
		return
	}
	required, _, policyErr := store.StockEntryBrokerProtection(ctx, intent.TradeID)
	if policyErr != nil || (!required && !forceProtection) {
		return
	}
	trade, err := store.GetTradeByID(ctx, intent.TradeID)
	if err != nil || trade == nil || trade.Status != "open" {
		return
	}
	// Recovery used to perform one snapshot and immediately contain a held
	// bracket child. Route it through the same stateful verifier as normal
	// placement so recovery cannot bypass the protection semantics above.
	e.verifyOrContainBrokerEntryAsync(trade.ID, intent.BrokerOrderID, trade.Ticker, req.Quantity)
}
