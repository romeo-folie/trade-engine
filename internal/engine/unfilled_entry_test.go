package engine

import (
	"context"
	"testing"

	"github.com/romeo-folie/trade-engine/internal/execution"
	"github.com/romeo-folie/trade-engine/internal/store"
)

func insertUnfilledEntry(t *testing.T, ticker, brokerID string) string {
	t.Helper()
	ctx := context.Background()
	tradeID, err := store.InsertTrade(ctx, store.Trade{
		Ticker: ticker, Direction: "long", Strategy: "news_sentiment",
		EntryPrice: 0, Quantity: 0, Status: "open",
	})
	if err != nil {
		t.Fatalf("InsertTrade: %v", err)
	}
	if _, err := store.InsertOrder(ctx, store.Order{
		TradeID: tradeID, BrokerOrderID: brokerID, Ticker: ticker,
		Side: "buy", OrderType: "market", Quantity: 0, Status: "pending",
	}); err != nil {
		t.Fatalf("InsertOrder: %v", err)
	}
	return tradeID
}

func tradeStatusQty(t *testing.T, id string) (string, float64) {
	t.Helper()
	var status string
	var qty float64
	if err := store.GetDB().QueryRow(context.Background(),
		`SELECT status::text, quantity FROM trades WHERE id=$1`, id).Scan(&status, &qty); err != nil {
		t.Fatalf("trade lookup: %v", err)
	}
	return status, qty
}

// TestCancelUnfilledEntry_CancelsZeroFillRow pins the #437 cleanup: an entry
// order that died with zero fill (expired after-hours order — the IREN/MU/
// TSLA rows) must cancel the qty-0 trade row instead of leaving it "open"
// and occupying a position slot forever.
func TestCancelUnfilledEntry_CancelsZeroFillRow(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("DATABASE_URL not set; skipping DB-backed engine test")
	}
	tradeID := insertUnfilledEntry(t, "UNFL", "dead-order-1")
	// GetOrderStatus: not in the statuses map → ErrOrderNotFound (purged),
	// the strongest "it's gone" signal.
	e := newBracketTestEngine(&fakeBracketExec{})

	e.cancelUnfilledEntry(tradeID, "dead-order-1", "UNFL")

	status, _ := tradeStatusQty(t, tradeID)
	if status != "cancelled" {
		t.Fatalf("trade status = %s, want cancelled", status)
	}
}

// TestCancelUnfilledEntry_LateFillBackfills: a fill that races in between
// the last poll and the cleanup must be RECORDED, not cancelled — cancelling
// the row would orphan real shares at the broker.
func TestCancelUnfilledEntry_LateFillBackfills(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("DATABASE_URL not set; skipping DB-backed engine test")
	}
	tradeID := insertUnfilledEntry(t, "LATE", "late-order-1")
	e := newBracketTestEngine(&fakeBracketExec{
		statuses: map[string]execution.OrderResult{
			"late-order-1": {Status: "filled", FilledPrice: 42.5, FilledQty: 7},
		},
	})

	e.cancelUnfilledEntry(tradeID, "late-order-1", "LATE")

	status, qty := tradeStatusQty(t, tradeID)
	if status != "open" || qty != 7 {
		t.Fatalf("trade = %s qty %.1f, want open qty 7 (late fill backfilled)", status, qty)
	}
}

// TestCancelUnfilledTrade_GuardedToZeroQty: the store helper must refuse to
// cancel a trade that received any fill.
func TestCancelUnfilledTrade_GuardedToZeroQty(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("DATABASE_URL not set; skipping DB-backed engine test")
	}
	ctx := context.Background()
	tradeID, err := store.InsertTrade(ctx, store.Trade{
		Ticker: "GRDD", Direction: "long", Strategy: "momentum",
		EntryPrice: 100, Quantity: 5, Status: "open",
	})
	if err != nil {
		t.Fatalf("InsertTrade: %v", err)
	}
	cancelled, err := store.CancelUnfilledTrade(ctx, tradeID)
	if err != nil {
		t.Fatalf("CancelUnfilledTrade: %v", err)
	}
	if cancelled {
		t.Fatal("a filled trade must never be cancelled by the zero-fill cleanup")
	}
	if status, _ := tradeStatusQty(t, tradeID); status != "open" {
		t.Fatalf("trade status = %s, want open", status)
	}
}
