package engine

import (
	"context"
	"errors"
	"testing"

	"github.com/romeo-folie/trade-engine/internal/execution"
	"github.com/romeo-folie/trade-engine/internal/store"
)

func TestEnsureUncappedProtectiveStopSeedsImmediately(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("DATABASE_URL not set; skipping DB-backed engine test")
	}
	ctx := context.Background()
	tradeID, err := store.InsertTrade(ctx, store.Trade{
		Ticker: "UCPS", Direction: "long", Strategy: "momentum",
		EntryPrice: 100, Quantity: 5, StopLoss: 95, Status: "open",
	})
	if err != nil {
		t.Fatalf("InsertTrade: %v", err)
	}
	exec := &fakeBracketExec{placeRes: execution.OrderResult{
		BrokerOrderID: "uncap-stop-1", Status: "accepted",
	}}
	e := newBracketTestEngine(exec)

	if err := e.ensureUncappedProtectiveStop(ctx, tradeID, "UCPS", 5, 95); err != nil {
		t.Fatalf("ensureUncappedProtectiveStop: %v", err)
	}
	if len(exec.placed) != 1 {
		t.Fatalf("placed = %d, want one immediate stop", len(exec.placed))
	}
	req := exec.placed[0]
	if req.Type != execution.OrderTypeStop || req.Side != execution.OrderSideSell ||
		req.Quantity != 5 || req.StopPrice != 95 || req.TimeInForce != "gtc" {
		t.Fatalf("protective request = %+v", req)
	}
	stop, err := store.LatestStopLossOrderForTrade(ctx, tradeID)
	if err != nil || stop == nil {
		t.Fatalf("LatestStopLossOrderForTrade: %v (nil=%v)", err, stop == nil)
	}
	if stop.Quantity != 5 || stop.BrokerOrderID != "uncap-stop-1" {
		t.Fatalf("stored stop = %+v", stop)
	}

	// Idempotent when the trailing manager or fill poll reaches the same fill.
	if err := e.ensureUncappedProtectiveStop(ctx, tradeID, "UCPS", 5, 95); err != nil {
		t.Fatalf("second ensure: %v", err)
	}
	if len(exec.placed) != 1 {
		t.Fatalf("idempotent ensure placed %d stops, want 1", len(exec.placed))
	}
}

func TestEnsureUncappedProtectiveStopGrowsWithPartialFill(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("DATABASE_URL not set; skipping DB-backed engine test")
	}
	ctx := context.Background()
	tradeID, err := store.InsertTrade(ctx, store.Trade{
		Ticker: "UCPG", Direction: "long", Strategy: "momentum",
		EntryPrice: 100, Quantity: 5, StopLoss: 95, Status: "open",
	})
	if err != nil {
		t.Fatalf("InsertTrade: %v", err)
	}
	if _, err := store.InsertOrder(ctx, store.Order{
		TradeID: tradeID, BrokerOrderID: "partial-stop", Ticker: "UCPG",
		Side: "sell", OrderType: "stop", Quantity: 2, StopPrice: 95,
		Status: "pending", IsStopLoss: true,
	}); err != nil {
		t.Fatalf("InsertOrder: %v", err)
	}
	exec := &fakeBracketExec{
		statuses: map[string]execution.OrderResult{
			"partial-stop": {Status: "cancelled"},
		},
		placeRes: execution.OrderResult{BrokerOrderID: "full-stop", Status: "accepted"},
	}
	e := newBracketTestEngine(exec)

	if err := e.ensureUncappedProtectiveStop(ctx, tradeID, "UCPG", 5, 95); err != nil {
		t.Fatalf("resize protective stop: %v", err)
	}
	if len(exec.cancelled) != 1 || exec.cancelled[0] != "partial-stop" {
		t.Fatalf("cancelled = %v, want partial-stop", exec.cancelled)
	}
	if len(exec.placed) != 1 || exec.placed[0].Quantity != 5 {
		t.Fatalf("replacement = %+v, want qty 5", exec.placed)
	}
	stop, err := store.LatestStopLossOrderForTrade(ctx, tradeID)
	if err != nil || stop == nil {
		t.Fatalf("LatestStopLossOrderForTrade: %v (nil=%v)", err, stop == nil)
	}
	if stop.BrokerOrderID != "full-stop" || stop.Quantity != 5 {
		t.Fatalf("full stop = %+v", stop)
	}
}

func TestAdvanceProtectedQuantityRetriesFailedCoverage(t *testing.T) {
	attempts := 0
	ensure := func() error {
		attempts++
		if attempts == 1 {
			return errors.New("temporary cancel confirmation failure")
		}
		return nil
	}

	protected, err := advanceProtectedQuantity(2, 5, ensure)
	if err == nil {
		t.Fatal("first coverage attempt unexpectedly succeeded")
	}
	if protected != 2 {
		t.Fatalf("failed coverage advanced quantity to %v, want 2", protected)
	}

	protected, err = advanceProtectedQuantity(protected, 5, ensure)
	if err != nil {
		t.Fatalf("retry coverage: %v", err)
	}
	if protected != 5 || attempts != 2 {
		t.Fatalf("retry produced protected=%v attempts=%d, want 5/2", protected, attempts)
	}

	protected, err = advanceProtectedQuantity(protected, 5, ensure)
	if err != nil || protected != 5 || attempts != 2 {
		t.Fatalf("already-covered fill retried: protected=%v attempts=%d err=%v", protected, attempts, err)
	}
}

func TestShouldPollEntryFill(t *testing.T) {
	tests := []struct {
		name   string
		result execution.OrderResult
		want   bool
	}{
		{
			name: "immediate partial fill with price",
			result: execution.OrderResult{
				BrokerOrderID: "partial-1",
				Status:        "partially_filled",
				FilledPrice:   100,
				FilledQty:     2,
			},
			want: true,
		},
		{
			name:   "accepted with no fill",
			result: execution.OrderResult{BrokerOrderID: "pending-1", Status: "accepted"},
			want:   true,
		},
		{
			name:   "fully filled",
			result: execution.OrderResult{BrokerOrderID: "filled-1", Status: "filled", FilledPrice: 100, FilledQty: 5},
			want:   false,
		},
		{
			name:   "cancelled",
			result: execution.OrderResult{BrokerOrderID: "cancelled-1", Status: "cancelled"},
			want:   false,
		},
		{
			name:   "no broker order",
			result: execution.OrderResult{Status: "accepted"},
			want:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldPollEntryFill(tt.result); got != tt.want {
				t.Fatalf("shouldPollEntryFill(%+v) = %v, want %v", tt.result, got, tt.want)
			}
		})
	}
}
