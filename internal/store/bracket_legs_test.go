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

	tpLegID := legs[0].ID
	if err := store.UpdateOrderFill(ctx, tpLegID, 110, 3, "filled"); err != nil {
		t.Fatalf("fill TP leg: %v", err)
	}
	legs, err = store.OpenExitLegsForTrade(ctx, tradeID)
	if err != nil {
		t.Fatalf("OpenExitLegsForTrade after fill: %v", err)
	}
	if len(legs) != 0 {
		t.Fatalf("working legs after TP fill = %+v, want none", legs)
	}
	filled, err := store.ExitLegsForMarketExit(ctx, tradeID)
	if err != nil {
		t.Fatalf("ExitLegsForMarketExit: %v", err)
	}
	if len(filled) != 1 || filled[0].ID != tpLegID {
		t.Fatalf("filled legs = %+v, want the terminal TP leg", filled)
	}
	if _, err := store.InsertOrder(ctx, store.Order{
		TradeID: tradeID, BrokerOrderID: "leg-partial", Ticker: "BRKT",
		Side: "sell", OrderType: "stop", Quantity: 3, StopPrice: 95,
		FilledPrice: 96, FilledQty: 1, Status: "cancelled", IsStopLoss: true,
	}); err != nil {
		t.Fatalf("insert partial terminal leg: %v", err)
	}
	if _, err := store.InsertOrder(ctx, store.Order{
		TradeID: tradeID, BrokerOrderID: "leg-zero", Ticker: "BRKT",
		Side: "sell", OrderType: "stop", Quantity: 3, StopPrice: 95,
		Status: "cancelled", IsStopLoss: true,
	}); err != nil {
		t.Fatalf("insert zero-fill terminal leg: %v", err)
	}
	workingID, err := store.InsertOrder(ctx, store.Order{
		TradeID: tradeID, BrokerOrderID: "leg-working", Ticker: "BRKT",
		Side: "sell", OrderType: "stop", Quantity: 3, StopPrice: 95,
		Status: "pending", IsStopLoss: true,
	})
	if err != nil {
		t.Fatalf("insert working leg: %v", err)
	}
	if _, err := store.InsertOrder(ctx, store.Order{
		TradeID: tradeID, BrokerOrderID: "entry-2", Ticker: "BRKT",
		Side: "buy", OrderType: "market", Quantity: 3, Status: "filled",
	}); err != nil {
		t.Fatalf("insert second entry: %v", err)
	}
	all, err := store.ExitLegsForMarketExit(ctx, tradeID)
	if err != nil {
		t.Fatalf("ExitLegsForMarketExit: %v", err)
	}
	seen := map[string]bool{}
	for _, leg := range all {
		seen[leg.BrokerOrderID] = true
	}
	for _, id := range []string{"leg-partial", "leg-working", "leg-tp"} {
		if !seen[id] {
			t.Fatalf("combined leg snapshot missing %s: %+v", id, all)
		}
	}
	if seen["leg-zero"] || seen["entry-2"] || seen["leg-sl"] || workingID == "" {
		t.Fatalf("combined leg snapshot included excluded rows: %+v", all)
	}
}
