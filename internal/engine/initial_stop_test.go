package engine

import (
	"context"
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
