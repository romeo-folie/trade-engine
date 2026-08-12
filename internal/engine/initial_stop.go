package engine

import (
	"context"
	"errors"
	"fmt"

	"github.com/romeo-folie/trade-engine/internal/execution"
	"github.com/romeo-folie/trade-engine/internal/store"
)

var errProtectiveStopFilledDuringRollback = errors.New("protective stop filled before rollback completed")

// Kept as a narrow seam for the post-acceptance rollback regression. Normal
// production behavior is the store helper; the seam never changes execution
// policy or broker-facing state.
var insertUncappedProtectiveOrder = store.InsertOrderWithRetry

// shouldPollEntryFill keeps tracking every broker order that can still add
// fills. FilledPrice may already be nonzero for a partial fill, so status—not
// price—is the completion signal.
func shouldPollEntryFill(result execution.OrderResult) bool {
	return result.BrokerOrderID != "" &&
		!execution.IsTerminalOrderStatus(result.Status)
}

// ensureUncappedProtectiveStop establishes broker protection as soon as an
// uncapped entry fill is known. It also grows a stop seeded against a partial
// fill when later polls report more shares.
func (e *Engine) ensureUncappedProtectiveStop(
	ctx context.Context,
	tradeID, ticker string,
	quantity, stopPrice float64,
) error {
	if quantity <= 0 {
		return fmt.Errorf("protective stop requires positive quantity, got %f", quantity)
	}
	if stopPrice <= 0 {
		return fmt.Errorf("protective stop requires positive price, got %f", stopPrice)
	}

	existing, err := store.LatestStopLossOrderForTrade(ctx, tradeID)
	if err != nil {
		return fmt.Errorf("lookup protective stop: %w", err)
	}
	if existing != nil {
		if existing.Quantity >= quantity {
			return nil
		}
		soldQty, err := e.cancelProtectiveStopForResize(ctx, *existing, quantity)
		if err != nil {
			return err
		}
		quantity -= soldQty
		if quantity <= 0 {
			return errors.New("protective stop sold the full entry while resizing")
		}
	}

	intentKey := protectiveStopIntentKey(tradeID, stopPrice, quantity)
	stopReq := execution.OrderRequest{
		Ticker:      ticker,
		Side:        execution.OrderSideSell,
		Type:        execution.OrderTypeStop,
		Quantity:    quantity,
		StopPrice:   stopPrice,
		TradeID:     tradeID,
		IntentKey:   intentKey,
		TimeInForce: "gtc", // uncapped entries are whole-share only
	}
	res, err := e.executor.PlaceOrder(ctx, stopReq)
	if errors.Is(err, execution.ErrTerminalZeroFillPlacement) {
		prior, lookupErr := store.GetStockOrderIntentByKey(ctx, intentKey)
		if lookupErr != nil {
			return fmt.Errorf("load terminal protective-stop intent: %w", lookupErr)
		}
		if prior == nil || prior.BrokerOrderID == "" || !isTerminalZeroFillStockIntent(prior.Status, prior.FilledQty) {
			return fmt.Errorf("terminal protective-stop retry lacks confirmed broker generation: %w", err)
		}
		intentKey = protectiveStopRetryIntentKey(intentKey, prior.BrokerOrderID)
		stopReq.IntentKey = intentKey
		stopReq.ClientOrderID = ""
		res, err = e.executor.PlaceOrder(ctx, stopReq)
	}
	if err != nil {
		return fmt.Errorf("place protective stop: %w", err)
	}
	stockIntentID, err := stockIntentIDForAcceptedOrder(ctx, e.executor, intentKey)
	if err != nil {
		return fmt.Errorf("resolve accepted protective-stop intent: %w", err)
	}
	status := execution.NormalizeOrderStatus(res.Status)
	fillQty := res.FilledQty
	if status == "filled" && fillQty <= 0 {
		fillQty = quantity
	}
	fillPrice := res.FilledPrice
	if fillQty > 0 && fillPrice <= 0 {
		fillPrice = stopPrice
	}
	fillObserved := fillQty > 0
	stopIntent := newExitIntent(store.ExitReasonStopLoss, map[string]any{"stop_loss": stopPrice})
	if fillObserved {
		// Transfer ownership before making the terminal/partial order row
		// durable. The exit reconciler selects such rows only for claimed
		// trades; leaving this trade open would make an immediate stop fill
		// invisible and a later manager tick could submit another sell.
		if _, claimErr := store.ClaimTradeExitWithIntent(ctx, tradeID, stopIntent); claimErr != nil {
			return fmt.Errorf("claim immediate protective-stop fill: %w", claimErr)
		}
	}
	if _, err := insertUncappedProtectiveOrder(ctx, store.Order{
		TradeID:       tradeID,
		BrokerOrderID: res.BrokerOrderID,
		Ticker:        ticker,
		Side:          string(execution.OrderSideSell),
		OrderType:     string(execution.OrderTypeStop),
		Quantity:      quantity,
		StopPrice:     stopPrice,
		FilledPrice:   fillPrice,
		FilledQty:     fillQty,
		Status:        status,
		IsStopLoss:    true,
		ExitReason:    stopIntent.Reason,
		ExitEvidence:  stopIntent.Evidence,
		FilledAt:      filledAtPtr(res.FilledAt),
		StockIntentID: stockIntentID,
	}); err != nil {
		rollbackRes, rollbackErr :=
			rollbackUnrecordedProtectiveStop(ctx, e.executor, res.BrokerOrderID)
		if rollbackRes.FilledQty > 0 {
			_, _ = store.ClaimTradeExitWithIntent(ctx, tradeID, stopIntent)
			// Preserve the fill itself before returning. If the broker purges
			// this terminal order before the next sweep, the reconciler can
			// still replay the durable quantity, price, and execution time.
			rollbackStatus := execution.NormalizeOrderStatus(rollbackRes.Status)
			if rollbackStatus == "" {
				rollbackStatus = "filled"
			}
			if _, persistErr := store.InsertOrderWithRetry(ctx, store.Order{
				TradeID: tradeID, BrokerOrderID: res.BrokerOrderID,
				Ticker: ticker, Side: string(execution.OrderSideSell),
				OrderType: string(execution.OrderTypeStop), Quantity: quantity,
				StopPrice: stopPrice, Status: rollbackStatus, IsStopLoss: true,
				FilledPrice: rollbackRes.FilledPrice, FilledQty: rollbackRes.FilledQty,
				FilledAt:   filledAtPtr(rollbackRes.FilledAt),
				ExitReason: stopIntent.Reason, ExitEvidence: stopIntent.Evidence,
				StockIntentID: stockIntentID,
			}); persistErr != nil {
				return fmt.Errorf("record protective stop: %w; rollback fill also unpersisted: %v", err, persistErr)
			}
			return fmt.Errorf("record protective stop: %w; rollback observed broker fill queued for reconciliation", err)
		}
		if rollbackErr != nil {
			return fmt.Errorf("record protective stop: %w; rollback unrecorded broker stop: %v", err, rollbackErr)
		}
		return fmt.Errorf("record protective stop: %w (broker stop rolled back)", err)
	}
	if fillObserved {
		return errProtectiveStopFilledDuringRollback
	}
	if status == "cancelled" || status == "rejected" {
		return fmt.Errorf("protective stop became %s before providing coverage", status)
	}
	return nil
}

func protectiveStopIntentKey(tradeID string, stopPrice, quantity float64) string {
	return "protective-stop:" + tradeID + ":" + fmt.Sprintf("%.8f:%.8f", stopPrice, quantity)
}

func protectiveStopRetryIntentKey(previousKey, cancelledBrokerOrderID string) string {
	return fmt.Sprintf("%s:retry:%s", previousKey, cancelledBrokerOrderID)
}

func isTerminalZeroFillStockIntent(status string, filledQty float64) bool {
	status = execution.NormalizeOrderStatus(status)
	return filledQty <= 0 && (status == "cancelled" || status == "rejected")
}

// rollbackUnrecordedProtectiveStop prevents a broker-accepted stop whose DB
// insert failed from surviving invisibly. A later retry must not place another
// full-quantity stop until the unrecorded one is confirmed gone.
func rollbackUnrecordedProtectiveStop(
	ctx context.Context,
	executor execution.Executor,
	brokerOrderID string,
) (execution.OrderResult, error) {
	cancelErr := executor.CancelOrder(ctx, brokerOrderID)
	status, statusErr := executor.GetOrderStatus(ctx, brokerOrderID)
	if statusErr != nil {
		if errors.Is(statusErr, execution.ErrOrderNotFound) {
			return execution.OrderResult{}, nil
		}
		return execution.OrderResult{}, fmt.Errorf("cancel=%v status=%w", cancelErr, statusErr)
	}
	switch execution.NormalizeOrderStatus(status.Status) {
	case "cancelled", "rejected":
		return status, nil
	case "filled":
		return status, errProtectiveStopFilledDuringRollback
	default:
		return status, fmt.Errorf("cancel=%v order remains %s", cancelErr, status.Status)
	}
}

func (e *Engine) cancelProtectiveStopForResize(
	ctx context.Context,
	existing store.Order,
	targetQuantity float64,
) (float64, error) {
	cancelErr := e.executor.CancelOrder(ctx, existing.BrokerOrderID)
	status, statusErr := e.executor.GetOrderStatus(ctx, existing.BrokerOrderID)
	if statusErr != nil {
		if !errors.Is(statusErr, execution.ErrOrderNotFound) {
			return 0, fmt.Errorf("confirm protective-stop cancellation: cancel=%v status=%w", cancelErr, statusErr)
		}
	} else {
		switch execution.NormalizeOrderStatus(status.Status) {
		case "cancelled", "rejected":
			if status.FilledQty > 0 {
				fillPrice := status.FilledPrice
				if fillPrice <= 0 {
					fillPrice = existing.StopPrice
				}
				intent := newExitIntent(store.ExitReasonStopLoss, map[string]any{"stop_loss": existing.StopPrice})
				claimed, err := store.ClaimTradeExitWithIntent(ctx, existing.TradeID, intent)
				if err != nil {
					return 0, fmt.Errorf("claim partial stop exit: %w", err)
				}
				if !claimed {
					return 0, errors.New("partial stop filled while another exit worker owns the trade")
				}
				remaining, applied, err := store.ApplyPartialExitAt(
					ctx, existing.TradeID, existing.ID,
					fillPrice, status.FilledQty, "cancelled", status.FilledAt,
				)
				if err != nil {
					return 0, fmt.Errorf("atomically record partial protective-stop fill: %w", err)
				}
				if remaining <= 1e-9 {
					return 0, errors.New("protective stop flattened trade while resizing; exit reconciler owns trade")
				}
				if err := store.ReleaseTradeExitClaim(ctx, existing.TradeID); err != nil {
					return 0, fmt.Errorf("release reconciled partial stop claim: %w", err)
				}
				return applied, nil
			}
			// Safe to replace below.
		case "filled":
			fillPrice := status.FilledPrice
			if fillPrice <= 0 {
				fillPrice = existing.StopPrice
			}
			intent := newExitIntent(store.ExitReasonStopLoss, map[string]any{"stop_loss": existing.StopPrice})
			claimed, err := store.ClaimTradeExitWithIntent(ctx, existing.TradeID, intent)
			if err != nil {
				return 0, fmt.Errorf("claim filled stop exit: %w", err)
			}
			if !claimed {
				return 0, errors.New("protective stop filled while another exit worker owns the trade")
			}
			fillQty := status.FilledQty
			if fillQty <= 0 {
				fillQty = existing.Quantity
			}
			remaining, applied, err := store.ApplyPartialExitAt(
				ctx, existing.TradeID, existing.ID,
				fillPrice, fillQty, "filled", status.FilledAt,
			)
			if err != nil {
				return 0, fmt.Errorf("atomically record protective-stop fill: %w", err)
			}
			if remaining > 1e-9 {
				if err := store.ReleaseTradeExitClaim(ctx, existing.TradeID); err != nil {
					return 0, fmt.Errorf("release partially filled stop claim: %w", err)
				}
				return applied, nil
			}
			return 0, errors.New("protective stop filled while resizing; exit reconciler owns trade")
		default:
			return 0, fmt.Errorf("protective stop remains %s after cancel; refusing duplicate", status.Status)
		}
	}
	if err := store.MarkOrderStatus(ctx, existing.BrokerOrderID, "cancelled"); err != nil {
		return 0, fmt.Errorf("mark old protective stop cancelled: %w", err)
	}
	return 0, nil
}

// advanceProtectedQuantity records coverage only after the broker operation
// succeeds. Leaving the prior quantity unchanged on error makes the next fill
// poll retry an otherwise-identical partial or terminal fill.
func advanceProtectedQuantity(
	protectedQty, filledQty float64,
	ensure func() error,
) (float64, error) {
	if filledQty <= protectedQty {
		return protectedQty, nil
	}
	if err := ensure(); err != nil {
		return protectedQty, err
	}
	return filledQty, nil
}
