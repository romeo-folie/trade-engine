package engine

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/romeo-folie/trade-engine/internal/execution"
	"github.com/romeo-folie/trade-engine/internal/store"
)

var errProtectiveStopFilledDuringRollback = errors.New("protective stop filled before rollback completed")

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

	res, err := e.executor.PlaceOrder(ctx, execution.OrderRequest{
		Ticker:      ticker,
		Side:        execution.OrderSideSell,
		Type:        execution.OrderTypeStop,
		Quantity:    quantity,
		StopPrice:   stopPrice,
		TradeID:     tradeID,
		TimeInForce: "gtc", // uncapped entries are whole-share only
	})
	if err != nil {
		return fmt.Errorf("place protective stop: %w", err)
	}
	if _, err := store.InsertOrder(ctx, store.Order{
		TradeID:       tradeID,
		BrokerOrderID: res.BrokerOrderID,
		Ticker:        ticker,
		Side:          string(execution.OrderSideSell),
		OrderType:     string(execution.OrderTypeStop),
		Quantity:      quantity,
		StopPrice:     stopPrice,
		Status:        execution.NormalizeOrderStatus(res.Status),
		IsStopLoss:    true,
	}); err != nil {
		if rollbackErr := rollbackUnrecordedProtectiveStop(ctx, e.executor, res.BrokerOrderID); rollbackErr != nil {
			if errors.Is(rollbackErr, errProtectiveStopFilledDuringRollback) {
				_, _ = store.ClaimTradeExit(ctx, tradeID)
				if _, persistErr := store.InsertOrder(ctx, store.Order{
					TradeID: tradeID, BrokerOrderID: res.BrokerOrderID,
					Ticker: ticker, Side: string(execution.OrderSideSell),
					OrderType: string(execution.OrderTypeStop), Quantity: quantity,
					StopPrice: stopPrice, Status: "filled", IsStopLoss: true,
				}); persistErr != nil {
					return fmt.Errorf("record protective stop: %w; filled rollback order also unpersisted: %v", err, persistErr)
				}
			}
			return fmt.Errorf("record protective stop: %w; rollback unrecorded broker stop: %v", err, rollbackErr)
		}
		return fmt.Errorf("record protective stop: %w (broker stop rolled back)", err)
	}
	return nil
}

// rollbackUnrecordedProtectiveStop prevents a broker-accepted stop whose DB
// insert failed from surviving invisibly. A later retry must not place another
// full-quantity stop until the unrecorded one is confirmed gone.
func rollbackUnrecordedProtectiveStop(
	ctx context.Context,
	executor execution.Executor,
	brokerOrderID string,
) error {
	cancelErr := executor.CancelOrder(ctx, brokerOrderID)
	status, statusErr := executor.GetOrderStatus(ctx, brokerOrderID)
	if statusErr != nil {
		if errors.Is(statusErr, execution.ErrOrderNotFound) {
			return nil
		}
		return fmt.Errorf("cancel=%v status=%w", cancelErr, statusErr)
	}
	switch execution.NormalizeOrderStatus(status.Status) {
	case "cancelled", "rejected":
		return nil
	case "filled":
		return errProtectiveStopFilledDuringRollback
	default:
		return fmt.Errorf("cancel=%v order remains %s", cancelErr, status.Status)
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
				if _, err := store.ClaimTradeExit(ctx, existing.TradeID); err != nil {
					return 0, fmt.Errorf("claim partial stop exit: %w", err)
				}
				if err := store.UpdateOrderFill(
					ctx, existing.ID, status.FilledPrice, status.FilledQty, "cancelled",
				); err != nil {
					return 0, fmt.Errorf("record partial protective-stop fill: %w", err)
				}
				remaining := math.Max(0, targetQuantity-status.FilledQty)
				if err := store.UpdateExitingTradeQuantity(
					ctx, existing.TradeID, remaining,
				); err != nil {
					return 0, fmt.Errorf("reduce trade after partial stop fill: %w", err)
				}
				if err := store.ReleaseTradeExitClaim(ctx, existing.TradeID); err != nil {
					return 0, fmt.Errorf("release reconciled partial stop claim: %w", err)
				}
				return status.FilledQty, nil
			}
			// Safe to replace below.
		case "filled":
			if _, err := store.ClaimTradeExit(ctx, existing.TradeID); err != nil {
				return 0, fmt.Errorf("claim filled stop exit: %w", err)
			}
			if err := store.UpdateOrderFill(
				ctx, existing.ID, status.FilledPrice, status.FilledQty, "filled",
			); err != nil {
				return 0, fmt.Errorf("record protective-stop fill: %w", err)
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
