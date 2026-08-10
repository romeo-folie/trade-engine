package orb

import (
	"testing"

	"github.com/romeo-folie/trade-engine/internal/execution"
	"github.com/romeo-folie/trade-engine/internal/store"
)

func pendingTrade(ticker, side string, qty int, orderID string) store.ORBTrade {
	return store.ORBTrade{Ticker: ticker, Side: side, Qty: qty, OrderID: orderID, Status: "pending"}
}

func ids(entries []*store.ORBTrade) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.OrderID
	}
	return out
}

// A held SHORT position should match the pending 'sell' entry (the XAG/USD
// incident shape), not the opposing 'buy'.
func TestMatchOrphanEntries_ShortMatchesSell(t *testing.T) {
	f := orphanFinding{Ticker: "XAG/USD", BrokerQty: -800, ManagedQty: 0}
	pending := []store.ORBTrade{
		pendingTrade("XAG/USD", "buy", 800, "buy-id"),
		pendingTrade("XAG/USD", "sell", 800, "sell-id"),
	}
	got := matchOrphanEntries(f, pending, 0.5)
	if len(got) != 1 || got[0].OrderID != "sell-id" {
		t.Fatalf("expected [sell-id], got %v", ids(got))
	}
}

func TestMatchOrphanEntries_LongMatchesBuy(t *testing.T) {
	f := orphanFinding{Ticker: "XAU/USD", BrokerQty: 23, ManagedQty: 0}
	pending := []store.ORBTrade{
		pendingTrade("XAU/USD", "buy", 23, "buy-id"),
		pendingTrade("XAU/USD", "sell", 23, "sell-id"),
	}
	got := matchOrphanEntries(f, pending, 0.5)
	if len(got) != 1 || got[0].OrderID != "buy-id" {
		t.Fatalf("expected [buy-id], got %v", ids(got))
	}
}

// Split bracket: runner + partial are two same-side pending rows summing to the
// residue. Both must be returned so the whole recovered position is managed
// (Codex P1 — per-row matching would leave a split-bracket orphan pending).
func TestMatchOrphanEntries_SplitBracketGroup(t *testing.T) {
	f := orphanFinding{Ticker: "XAG/USD", BrokerQty: -10000, ManagedQty: 0}
	pending := []store.ORBTrade{
		pendingTrade("XAG/USD", "sell", 6000, "runner-id"),
		pendingTrade("XAG/USD", "sell", 4000, "partial-id"),
		pendingTrade("XAG/USD", "buy", 10000, "opp-id"),
	}
	got := matchOrphanEntries(f, pending, 0.5)
	if len(got) != 2 {
		t.Fatalf("expected both split rows, got %v", ids(got))
	}
	sum := got[0].Qty + got[1].Qty
	if sum != 10000 {
		t.Errorf("split group qty = %d, want 10000", sum)
	}
}

func TestMatchOrphanEntries_QtyMismatchNoMatch(t *testing.T) {
	f := orphanFinding{Ticker: "XAG/USD", BrokerQty: -800, ManagedQty: 0}
	pending := []store.ORBTrade{pendingTrade("XAG/USD", "sell", 500, "sell-id")}
	if got := matchOrphanEntries(f, pending, 0.5); got != nil {
		t.Fatalf("qty 500 vs residue 800 should not match, got %v", ids(got))
	}
}

func TestMatchOrphanEntries_TickerMismatchNoMatch(t *testing.T) {
	f := orphanFinding{Ticker: "XAG/USD", BrokerQty: -800, ManagedQty: 0}
	pending := []store.ORBTrade{pendingTrade("XAU/USD", "sell", 800, "sell-id")}
	if got := matchOrphanEntries(f, pending, 0.5); got != nil {
		t.Fatalf("different ticker should not match, got %v", ids(got))
	}
}

// Partial: broker holds 800 but 300 is already managed → residue 500 should
// match a single pending 500 entry.
func TestMatchOrphanEntries_PartialResidueSingle(t *testing.T) {
	f := orphanFinding{Ticker: "XAG/USD", BrokerQty: -800, ManagedQty: -300}
	pending := []store.ORBTrade{pendingTrade("XAG/USD", "sell", 500, "sell-id")}
	got := matchOrphanEntries(f, pending, 0.5)
	if len(got) != 1 || got[0].OrderID != "sell-id" {
		t.Fatalf("expected [sell-id] for residue 500, got %v", ids(got))
	}
}

// A wrongly-'cancelled' entry the broker still holds must still match by
// ticker+side+qty (matching is status-agnostic), so recordReconciledFill can
// reopen it to 'filled' — today's XAG/XAU trap.
func TestMatchOrphanEntries_MatchesCancelledRow(t *testing.T) {
	f := orphanFinding{Ticker: "XAG/USD", BrokerQty: -800, ManagedQty: 0}
	cancelled := store.ORBTrade{Ticker: "XAG/USD", Side: "sell", Qty: 800, OrderID: "sell-id", Status: "cancelled"}
	got := matchOrphanEntries(f, []store.ORBTrade{cancelled}, 0.5)
	if len(got) != 1 || got[0].OrderID != "sell-id" {
		t.Fatalf("expected cancelled sell-id to match, got %v", ids(got))
	}
}

// Two equal-qty split legs where only one filled (residue = one leg): the
// single-row fallback must NOT guess — different TP/SL/order ids mean recording
// the wrong bracket. Ambiguous → return nothing (Codex P2).
func TestMatchOrphanEntries_AmbiguousEqualQtySplit(t *testing.T) {
	f := orphanFinding{Ticker: "XAG/USD", BrokerQty: -500, ManagedQty: 0}
	pending := []store.ORBTrade{
		pendingTrade("XAG/USD", "sell", 500, "runner-id"),
		pendingTrade("XAG/USD", "sell", 500, "partial-id"),
	}
	if got := matchOrphanEntries(f, pending, 0.5); got != nil {
		t.Fatalf("equal-qty split with residue 500 must be ambiguous, got %v", ids(got))
	}
}

func TestOppositeSide(t *testing.T) {
	if oppositeSide("buy") != "sell" || oppositeSide("BUY") != "sell" {
		t.Error("opposite of buy should be sell")
	}
	if oppositeSide("sell") != "buy" || oppositeSide("SELL") != "buy" {
		t.Error("opposite of sell should be buy")
	}
}

func TestAvgCostFor(t *testing.T) {
	positions := []execution.Position{
		{Ticker: "XAU/USD", AvgCostBasis: 4137.2},
		{Ticker: "XAG/USD", AvgCostBasis: 60.1},
	}
	if got := avgCostFor(positions, "XAG/USD", ""); got != 60.1 {
		t.Errorf("avgCostFor XAG/USD = %v, want 60.1", got)
	}
	if got := avgCostFor(positions, "MISSING", ""); got != 0 {
		t.Errorf("missing ticker should return 0, got %v", got)
	}
}

func TestFillReconcileKeepsFuturesExpiriesSeparate(t *testing.T) {
	positions := []execution.Position{
		{Ticker: "MYM", ConID: "old", Quantity: 2, AvgCostBasis: 42000},
		{Ticker: "MYM", ConID: "new", Quantity: 3, AvgCostBasis: 43000},
	}
	filled := []store.ORBTrade{{Ticker: "MYM", ConID: "old", Side: "buy", Qty: 2}}
	pending := []store.ORBTrade{
		{Ticker: "MYM", ConID: "old", Side: "buy", Qty: 3, OrderID: "wrong-expiry"},
		{Ticker: "MYM", ConID: "new", Side: "buy", Qty: 3, OrderID: "right-expiry"},
	}

	findings := findOrphanedPositions(positions, filled, orphanPositionTolerance)
	if len(findings) != 1 || findings[0].ConID != "new" {
		t.Fatalf("findings = %+v; want only new expiry", findings)
	}
	matched := matchOrphanEntries(findings[0], pending, orphanPositionTolerance)
	if len(matched) != 1 || matched[0].OrderID != "right-expiry" {
		t.Fatalf("matched = %v; want right-expiry", ids(matched))
	}
	if got := avgCostFor(positions, findings[0].Ticker, findings[0].ConID); got != 43000 {
		t.Fatalf("fill cost = %v; want new-expiry cost 43000", got)
	}
}
