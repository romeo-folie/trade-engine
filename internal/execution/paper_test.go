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
		Ticker:     "AAPL",
		Side:       OrderSideBuy,
		Type:       OrderTypeMarket,
		Quantity:   10,
		LimitPrice: 150.50,
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
		Quantity: 5, LimitPrice: 200,
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
