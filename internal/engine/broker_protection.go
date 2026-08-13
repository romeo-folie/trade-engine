package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/romeo-folie/trade-engine/internal/execution"
	"github.com/romeo-folie/trade-engine/internal/logger"
	"github.com/romeo-folie/trade-engine/internal/store"
)

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
		if !execution.IsTerminalOrderStatus(status.Status) {
			continue
		}

		trade, err := store.GetTradeByID(ctx, tradeID)
		if err != nil || trade == nil || trade.Status != "open" || trade.Quantity <= 1e-9 {
			return
		}
		intent := newExitIntent(store.ExitReasonStopLoss, map[string]any{
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
			ExitReason: store.ExitReasonStopLoss, ExitEvidence: intent.Evidence,
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
			IsStopLoss: true, StockIntentID: stockIntentID, ExitReason: store.ExitReasonStopLoss,
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

func (e *Engine) containUnprotectedBrokerEntryAsync(tradeID, entryBrokerOrderID, ticker, reason string) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		e.containUnprotectedBrokerEntry(ctx, tradeID, entryBrokerOrderID, ticker, reason)
	}()
}

// verifyOrContainBrokerEntry gives a sparse placement response one fresh
// nested broker read before cancelling it. Alpaca normally includes legs in a
// bracket response, but a response that only supplies child IDs is not itself
// proof that the stop is inactive.
func (e *Engine) verifyOrContainBrokerEntryAsync(tradeID, entryBrokerOrderID, ticker string, requestedQty float64) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		status, err := e.executor.GetOrderStatus(ctx, entryBrokerOrderID)
		reason := "fresh broker protection snapshot is unavailable"
		if err == nil {
			trade, tradeErr := store.GetTradeByID(ctx, tradeID)
			if tradeErr == nil && trade != nil {
				state := brokerBracketProtectionState(status, trade.Quantity)
				if state.UnprotectedQty <= 1e-9 {
					return
				}
				logger.Error("broker protection: containment required", "trade_id", tradeID,
					"held_qty", state.HeldQty, "working_entry_qty", state.WorkingEntryQty,
					"protected_qty", state.ProtectedQty, "unprotected_qty", state.UnprotectedQty)
			}
			reason = brokerProtectionFailureReason(status, requestedQty)
		}
		e.containUnprotectedBrokerEntry(ctx, tradeID, entryBrokerOrderID, ticker, reason)
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
// exposure. It deliberately distinguishes a known child from active downside
// protection: Alpaca reports children while they are held under a partial
// parent, but those shares are still unprotected.
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
		if child.Status == "held" || execution.IsTerminalOrderStatus(child.Status) || child.RemainingQty <= 0 {
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

// brokerBracketProtectionVerified requires an active broker stop covering the
// exact locally held quantity. A bracket child in Alpaca's "held" state does
// not count: it will not protect a parent that is only partially filled.
func brokerBracketProtectionVerified(result execution.OrderResult, heldQty float64) bool {
	state := brokerBracketProtectionState(result, heldQty)
	return state.HeldQty <= 1e-9 || state.UnprotectedQty <= 1e-9
}

// containRecoveredUnprotectedBracket is called after a durable recovery
// projection (including the zero-fill entry poll). It keeps an accepted entry
// out of application-managed monitoring when broker protection cannot be
// verified from the nested parent snapshot.
func (e *Engine) containRecoveredUnprotectedBracket(ctx context.Context, intentKey string) {
	intent, err := store.GetStockOrderIntentByKey(ctx, intentKey)
	if err != nil || intent == nil || intent.TradeID == "" || intent.BrokerOrderID == "" {
		return
	}
	var req execution.OrderRequest
	if json.Unmarshal(intent.RequestPayload, &req) != nil || !req.RequireBrokerProtection {
		return
	}
	status, err := e.executor.GetOrderStatus(ctx, intent.BrokerOrderID)
	if err != nil {
		e.containUnprotectedBrokerEntryAsync(intent.TradeID, intent.BrokerOrderID, intent.Ticker,
			"fresh broker protection snapshot is unavailable during recovery")
		return
	}
	trade, err := store.GetTradeByID(ctx, intent.TradeID)
	if err != nil || trade == nil || trade.Status != "open" {
		return
	}
	if brokerBracketProtectionVerified(status, trade.Quantity) {
		return
	}
	e.containUnprotectedBrokerEntryAsync(trade.ID, intent.BrokerOrderID, trade.Ticker,
		brokerProtectionFailureReason(status, req.Quantity))
}
