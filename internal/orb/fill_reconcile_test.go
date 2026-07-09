package orb

import (
	"testing"

	"github.com/romeo-folie/trade-engine/internal/execution"
	"github.com/romeo-folie/trade-engine/internal/store"
)

func pendingTrade(ticker, side string, qty int, orderID string) store.ORBTrade {
	return store.ORBTrade{Ticker: ticker, Side: side, Qty: qty, OrderID: orderID, Status: "pending"}
}

// A held SHORT position should match the pending 'sell' entry (the XAG/USD
// incident shape), not the opposing 'buy'.
func TestMatchOrphanEntry_ShortMatchesSell(t *testing.T) {
	f := orphanFinding{Ticker: "XAG/USD", BrokerQty: -800, ManagedQty: 0}
	pending := []store.ORBTrade{
		pendingTrade("XAG/USD", "buy", 800, "buy-id"),
		pendingTrade("XAG/USD", "sell", 800, "sell-id"),
	}
	got := matchOrphanEntry(f, pending, 0.5)
	if got == nil || got.OrderID != "sell-id" {
		t.Fatalf("expected sell-id, got %+v", got)
	}
}

func TestMatchOrphanEntry_LongMatchesBuy(t *testing.T) {
	f := orphanFinding{Ticker: "XAU/USD", BrokerQty: 23, ManagedQty: 0}
	pending := []store.ORBTrade{
		pendingTrade("XAU/USD", "buy", 23, "buy-id"),
		pendingTrade("XAU/USD", "sell", 23, "sell-id"),
	}
	got := matchOrphanEntry(f, pending, 0.5)
	if got == nil || got.OrderID != "buy-id" {
		t.Fatalf("expected buy-id, got %+v", got)
	}
}

func TestMatchOrphanEntry_QtyMismatchNoMatch(t *testing.T) {
	f := orphanFinding{Ticker: "XAG/USD", BrokerQty: -800, ManagedQty: 0}
	pending := []store.ORBTrade{pendingTrade("XAG/USD", "sell", 500, "sell-id")}
	if got := matchOrphanEntry(f, pending, 0.5); got != nil {
		t.Fatalf("qty 500 vs residue 800 should not match, got %+v", got)
	}
}

func TestMatchOrphanEntry_TickerMismatchNoMatch(t *testing.T) {
	f := orphanFinding{Ticker: "XAG/USD", BrokerQty: -800, ManagedQty: 0}
	pending := []store.ORBTrade{pendingTrade("XAU/USD", "sell", 800, "sell-id")}
	if got := matchOrphanEntry(f, pending, 0.5); got != nil {
		t.Fatalf("different ticker should not match, got %+v", got)
	}
}

// Partial: broker holds 800 but 300 is already managed → residue 500 should
// match a pending 500 entry.
func TestMatchOrphanEntry_PartialResidue(t *testing.T) {
	f := orphanFinding{Ticker: "XAG/USD", BrokerQty: -800, ManagedQty: -300}
	pending := []store.ORBTrade{pendingTrade("XAG/USD", "sell", 500, "sell-id")}
	got := matchOrphanEntry(f, pending, 0.5)
	if got == nil || got.OrderID != "sell-id" {
		t.Fatalf("expected sell-id for residue 500, got %+v", got)
	}
}

func TestOpposingPendingLeg(t *testing.T) {
	pending := []store.ORBTrade{
		pendingTrade("XAG/USD", "buy", 800, "buy-id"),
		pendingTrade("XAG/USD", "sell", 800, "sell-id"),
	}
	opp := opposingPendingLeg("XAG/USD", "sell", pending)
	if opp == nil || opp.OrderID != "buy-id" {
		t.Fatalf("opposing of sell should be buy-id, got %+v", opp)
	}
	// No opposing leg present.
	if opp := opposingPendingLeg("XAG/USD", "sell", pending[1:]); opp != nil {
		t.Fatalf("expected no opposing leg, got %+v", opp)
	}
}

func TestAvgCostFor(t *testing.T) {
	positions := []execution.Position{
		{Ticker: "XAU/USD", AvgCostBasis: 4137.2},
		{Ticker: "XAG/USD", AvgCostBasis: 60.1},
	}
	if got := avgCostFor(positions, "XAG/USD"); got != 60.1 {
		t.Errorf("avgCostFor XAG/USD = %v, want 60.1", got)
	}
	if got := avgCostFor(positions, "MISSING"); got != 0 {
		t.Errorf("missing ticker should return 0, got %v", got)
	}
}
