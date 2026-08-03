package execution

import (
	"context"
	"strings"
	"testing"
)

func TestPaperExecutor_PlaceAndGetStatus(t *testing.T) {
	pe := NewPaperExecutor()
	ctx := context.Background()

	result, err := pe.PlaceOrder(ctx, OrderRequest{
		Ticker:         "AAPL",
		Side:           OrderSideBuy,
		Type:           OrderTypeMarket,
		Quantity:       10,
		ReferencePrice: 150.50,
	})
	if err != nil {
		t.Fatalf("PlaceOrder: %v", err)
	}
	if result.Status != "filled" {
		t.Fatalf("expected filled, got %s", result.Status)
	}
	if result.FilledPrice != 150.50 {
		t.Fatalf("expected 150.50, got %.2f", result.FilledPrice)
	}
	if result.FilledQty != 10 {
		t.Fatalf("expected qty 10, got %.2f", result.FilledQty)
	}
	if !strings.HasPrefix(result.BrokerOrderID, "PAPER-") {
		t.Fatalf("expected PAPER- prefix, got %s", result.BrokerOrderID)
	}

	// GetOrderStatus
	status, err := pe.GetOrderStatus(ctx, result.BrokerOrderID)
	if err != nil {
		t.Fatalf("GetOrderStatus: %v", err)
	}
	if status.Status != "filled" {
		t.Fatalf("expected filled, got %s", status.Status)
	}
}

func TestPaperExecutor_Cancel(t *testing.T) {
	pe := NewPaperExecutor()
	ctx := context.Background()

	result, _ := pe.PlaceOrder(ctx, OrderRequest{
		Ticker: "TSLA", Side: OrderSideSell, Type: OrderTypeMarket,
		Quantity: 5, ReferencePrice: 200,
	})
	if err := pe.CancelOrder(ctx, result.BrokerOrderID); err != nil {
		t.Fatalf("CancelOrder: %v", err)
	}
	status, _ := pe.GetOrderStatus(ctx, result.BrokerOrderID)
	if status.Status != "cancelled" {
		t.Fatalf("expected cancelled, got %s", status.Status)
	}
}

func TestPaperExecutor_CancelNotFound(t *testing.T) {
	pe := NewPaperExecutor()
	err := pe.CancelOrder(context.Background(), "NOPE")
	if err == nil {
		t.Fatal("expected error for unknown order")
	}
}

// ─── Stop order simulation ───────────────────────────────────────────────────

func TestPaperExecutor_StopOrderStaysPending(t *testing.T) {
	pe := NewPaperExecutor()
	ctx := context.Background()

	result, err := pe.PlaceOrder(ctx, OrderRequest{
		Ticker:    "EUR/USD",
		Side:      OrderSideBuy,
		Type:      OrderTypeStop,
		Quantity:  5000,
		StopPrice: 1.095,
	})
	if err != nil {
		t.Fatalf("PlaceOrder: %v", err)
	}
	if result.Status != "pending" {
		t.Errorf("stop order status = %q; want pending", result.Status)
	}
	if result.FilledPrice != 0 {
		t.Errorf("stop order FilledPrice = %v; want 0", result.FilledPrice)
	}

	// GetOrderStatus confirms it stays pending.
	status, _ := pe.GetOrderStatus(ctx, result.BrokerOrderID)
	if status.Status != "pending" {
		t.Errorf("GetOrderStatus = %q; want pending", status.Status)
	}
}

func TestPaperExecutor_SimulateFill(t *testing.T) {
	pe := NewPaperExecutor()
	ctx := context.Background()

	result, _ := pe.PlaceOrder(ctx, OrderRequest{
		Ticker:    "GBP/USD",
		Side:      OrderSideSell,
		Type:      OrderTypeStop,
		Quantity:  5000,
		StopPrice: 1.260,
	})

	if err := pe.SimulateFill(result.BrokerOrderID, 1.2598); err != nil {
		t.Fatalf("SimulateFill: %v", err)
	}

	status, _ := pe.GetOrderStatus(ctx, result.BrokerOrderID)
	if status.Status != "filled" {
		t.Errorf("status after SimulateFill = %q; want filled", status.Status)
	}
	if status.FilledPrice != 1.2598 {
		t.Errorf("FilledPrice = %v; want 1.2598", status.FilledPrice)
	}
	if status.FilledQty != 5000 {
		t.Errorf("FilledQty = %v; want 5000", status.FilledQty)
	}
}

func TestPaperExecutor_SimulateFill_AlreadyFilled(t *testing.T) {
	pe := NewPaperExecutor()
	ctx := context.Background()

	result, _ := pe.PlaceOrder(ctx, OrderRequest{
		Ticker: "EUR/USD", Side: OrderSideBuy, Type: OrderTypeStop,
		Quantity: 5000, StopPrice: 1.095,
	})
	_ = pe.SimulateFill(result.BrokerOrderID, 1.095)

	// Second fill should error.
	if err := pe.SimulateFill(result.BrokerOrderID, 1.096); err == nil {
		t.Error("expected error when filling already-filled order")
	}
}

func TestPaperExecutor_SimulateFill_Cancelled(t *testing.T) {
	pe := NewPaperExecutor()
	ctx := context.Background()

	result, _ := pe.PlaceOrder(ctx, OrderRequest{
		Ticker: "EUR/USD", Side: OrderSideBuy, Type: OrderTypeStop,
		Quantity: 5000, StopPrice: 1.095,
	})
	_ = pe.CancelOrder(ctx, result.BrokerOrderID)

	// Fill after cancel should error.
	if err := pe.SimulateFill(result.BrokerOrderID, 1.095); err == nil {
		t.Error("expected error when filling cancelled order")
	}
}

func TestPaperExecutor_PendingStopOrders(t *testing.T) {
	pe := NewPaperExecutor()
	ctx := context.Background()

	// Place a market order (fills immediately) and two stop orders.
	pe.PlaceOrder(ctx, OrderRequest{
		Ticker: "AAPL", Side: OrderSideBuy, Type: OrderTypeMarket,
		Quantity: 10, ReferencePrice: 150,
	})
	buyResult, _ := pe.PlaceOrder(ctx, OrderRequest{
		Ticker: "EUR/USD", Side: OrderSideBuy, Type: OrderTypeStop,
		Quantity: 5000, StopPrice: 1.095,
	})
	sellResult, _ := pe.PlaceOrder(ctx, OrderRequest{
		Ticker: "EUR/USD", Side: OrderSideSell, Type: OrderTypeStop,
		Quantity: 5000, StopPrice: 1.085,
	})

	pending := pe.PendingStopOrders()
	if len(pending) != 2 {
		t.Fatalf("PendingStopOrders: got %d, want 2", len(pending))
	}
	if _, ok := pending[buyResult.BrokerOrderID]; !ok {
		t.Error("buy stop order not in PendingStopOrders")
	}
	if _, ok := pending[sellResult.BrokerOrderID]; !ok {
		t.Error("sell stop order not in PendingStopOrders")
	}

	// Fill one → should drop to 1 pending.
	pe.SimulateFill(buyResult.BrokerOrderID, 1.095)
	pending = pe.PendingStopOrders()
	if len(pending) != 1 {
		t.Fatalf("PendingStopOrders after fill: got %d, want 1", len(pending))
	}
}

func TestPaperExecutor_CancelPendingStop(t *testing.T) {
	pe := NewPaperExecutor()
	ctx := context.Background()

	result, _ := pe.PlaceOrder(ctx, OrderRequest{
		Ticker: "EUR/USD", Side: OrderSideBuy, Type: OrderTypeStop,
		Quantity: 5000, StopPrice: 1.095,
	})
	if err := pe.CancelOrder(ctx, result.BrokerOrderID); err != nil {
		t.Fatalf("CancelOrder on pending stop: %v", err)
	}
	status, _ := pe.GetOrderStatus(ctx, result.BrokerOrderID)
	if status.Status != "cancelled" {
		t.Errorf("status after cancel = %q; want cancelled", status.Status)
	}

	// Should no longer appear in pending stops.
	if len(pe.PendingStopOrders()) != 0 {
		t.Error("cancelled stop still in PendingStopOrders")
	}
}

func TestPaperExecutor_MarketOrderStillFillsImmediately(t *testing.T) {
	pe := NewPaperExecutor()
	ctx := context.Background()

	result, err := pe.PlaceOrder(ctx, OrderRequest{
		Ticker: "AAPL", Side: OrderSideBuy, Type: OrderTypeMarket,
		Quantity: 10, ReferencePrice: 150.50,
	})
	if err != nil {
		t.Fatalf("PlaceOrder: %v", err)
	}
	if result.Status != "filled" {
		t.Errorf("market order status = %q; want filled", result.Status)
	}
}

func TestPaperExecutor_GetPositionsEmpty(t *testing.T) {
	pe := NewPaperExecutor()
	pos, err := pe.GetPositions(context.Background())
	if err != nil {
		t.Fatalf("GetPositions: %v", err)
	}
	if len(pos) != 0 {
		t.Fatalf("expected 0 positions, got %d", len(pos))
	}
}
