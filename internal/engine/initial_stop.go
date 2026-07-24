package engine

import (
	"context"
	"errors"
	"fmt"

	"github.com/romeo-folie/trade-engine/internal/execution"
	"github.com/romeo-folie/trade-engine/internal/store"
)

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
		if err := e.cancelProtectiveStopForResize(ctx, *existing); err != nil {
			return err
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
		return fmt.Errorf("record protective stop: %w", err)
	}
	return nil
}

func (e *Engine) cancelProtectiveStopForResize(ctx context.Context, existing store.Order) error {
	cancelErr := e.executor.CancelOrder(ctx, existing.BrokerOrderID)
	status, statusErr := e.executor.GetOrderStatus(ctx, existing.BrokerOrderID)
	if statusErr != nil {
		if !errors.Is(statusErr, execution.ErrOrderNotFound) {
			return fmt.Errorf("confirm protective-stop cancellation: cancel=%v status=%w", cancelErr, statusErr)
		}
	} else {
		switch execution.NormalizeOrderStatus(status.Status) {
		case "cancelled", "rejected":
			// Safe to replace below.
		case "filled":
			return errors.New("protective stop filled while resizing; refusing replacement")
		default:
			return fmt.Errorf("protective stop remains %s after cancel; refusing duplicate", status.Status)
		}
	}
	if err := store.MarkOrderStatus(ctx, existing.BrokerOrderID, "cancelled"); err != nil {
		return fmt.Errorf("mark old protective stop cancelled: %w", err)
	}
	return nil
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
