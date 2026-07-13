package store_test

import (
	"context"
	"testing"

	"github.com/romeo-folie/trade-engine/internal/store"
)

// TestBracketLegs_ParentRoundTripAndOpenLegs verifies migration 044:
// parent_order_id persists and reads back through every order reader, and
// OpenExitLegsForTrade returns exactly the still-working exit legs — the
// predicate the bracket monitor, the SELL leg-cancel, and the trailing
// manager's exit path all key on.
func TestBracketLegs_ParentRoundTripAndOpenLegs(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("DATABASE_URL not set; skipping store integration test")
	}
	ctx := context.Background()

	tradeID, err := store.InsertTrade(ctx, store.Trade{
		Ticker: "BRKT", Direction: "long", Strategy: "momentum",
		EntryPrice: 100, Quantity: 3, StopLoss: 95, TakeProfit: 110,
		Status: "open",
	})
	if err != nil {
		t.Fatalf("InsertTrade: %v", err)
	}

	mkLeg := func(brokerID string, tp bool, status string) store.Order {
		o := store.Order{
			TradeID: tradeID, BrokerOrderID: brokerID, Ticker: "BRKT",
			Side: "sell", Quantity: 3, Status: status,
			ParentOrderID: "parent-abc",
		}
		if tp {
			o.OrderType, o.LimitPrice, o.IsTakeProfit = "limit", 110, true
		} else {
			o.OrderType, o.StopPrice, o.IsStopLoss = "stop", 95, true
		}
		return o
	}
	if _, err := store.InsertOrder(ctx, mkLeg("leg-tp", true, "pending")); err != nil {
		t.Fatalf("insert tp leg: %v", err)
	}
	slID, err := store.InsertOrder(ctx, mkLeg("leg-sl", false, "pending"))
	if err != nil {
		t.Fatalf("insert sl leg: %v", err)
	}
	// A filled prior order for the same trade must NOT count as a working leg.
	if _, err := store.InsertOrder(ctx, store.Order{
		TradeID: tradeID, BrokerOrderID: "entry-1", Ticker: "BRKT",
		Side: "buy", OrderType: "market", Quantity: 3, Status: "filled",
	}); err != nil {
		t.Fatalf("insert entry order: %v", err)
	}

	legs, err := store.OpenExitLegsForTrade(ctx, tradeID)
	if err != nil {
		t.Fatalf("OpenExitLegsForTrade: %v", err)
	}
	if len(legs) != 2 {
		t.Fatalf("got %d open legs, want 2", len(legs))
	}
	for _, leg := range legs {
		if leg.ParentOrderID != "parent-abc" {
			t.Fatalf("leg %s ParentOrderID = %q, want parent-abc (migration 044 roundtrip)", leg.BrokerOrderID, leg.ParentOrderID)
		}
	}

	// The trailing manager's stop lookup must see the SL leg with its parent
	// marker — that is what forbids cancel+resubmit on OCO children.
	stop, err := store.LatestStopLossOrderForTrade(ctx, tradeID)
	if err != nil || stop == nil {
		t.Fatalf("LatestStopLossOrderForTrade: %v (nil=%v)", err, stop == nil)
	}
	if stop.ParentOrderID != "parent-abc" || stop.BrokerOrderID != "leg-sl" {
		t.Fatalf("stop leg = %+v, want leg-sl with parent-abc", stop)
	}

	// Cancelling the SL leg removes it from the working set.
	if err := store.UpdateOrderFill(ctx, slID, 0, 0, "cancelled"); err != nil {
		t.Fatalf("cancel sl leg: %v", err)
	}
	legs, err = store.OpenExitLegsForTrade(ctx, tradeID)
	if err != nil {
		t.Fatalf("OpenExitLegsForTrade after cancel: %v", err)
	}
	if len(legs) != 1 || !legs[0].IsTakeProfit {
		t.Fatalf("got %+v, want only the TP leg after SL cancel", legs)
	}
}
