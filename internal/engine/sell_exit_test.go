package engine

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/execution"
	"github.com/romeo-folie/trade-engine/internal/store"
)

func insertSellSignal(t *testing.T, ticker string) string {
	t.Helper()
	id, err := store.InsertSignal(context.Background(), store.Signal{
		Ticker: ticker, Action: "SELL", Strategy: "momentum",
		SignalPrice: 119, Confidence: 0.7, SizePct: 2,
		Status: "pending", ExpiresAt: time.Now().Add(time.Hour),
		StalePriceMovePct: 2,
	})
	if err != nil {
		t.Fatalf("InsertSignal: %v", err)
	}
	return id
}

func signalStatus(t *testing.T, id string) string {
	t.Helper()
	sig, err := store.GetSignalByID(context.Background(), id)
	if err != nil || sig == nil {
		t.Fatalf("GetSignalByID: %v (nil=%v)", err, sig == nil)
	}
	return sig.Status
}

func tradeRowsForTicker(t *testing.T, ticker string) (open, closed, short int) {
	t.Helper()
	rows, err := store.GetDB().Query(context.Background(),
		`SELECT status::text, direction::text FROM trades WHERE ticker=$1`, ticker)
	if err != nil {
		t.Fatalf("query trades: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var status, direction string
		if err := rows.Scan(&status, &direction); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if direction == "short" {
			short++
		}
		switch status {
		case "open":
			open++
		case "closed":
			closed++
		}
	}
	return open, closed, short
}

// TestSellExit_ClosesAllLongLots pins the #436 semantics: a SELL signal
// closes EVERY open long lot at the confirmed fill with per-lot P&L, sized
// to the held quantity (never cash), cancels working legs first, and books
// NO short trade row — the pre-#436 behavior left the longs open and
// inserted a phantom short sized from cash.
func TestSellExit_ClosesAllLongLots(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("DATABASE_URL not set; skipping DB-backed engine test")
	}
	ctx := context.Background()
	const ticker = "SLXT"

	// Lot 1 carries bracket legs; lot 2 (a pyramid add) has none.
	trade1, tpID, slID := insertBracketFixture(t, ticker, 3)
	trade2, err := store.InsertTrade(ctx, store.Trade{
		Ticker: ticker, Direction: "long", Strategy: "momentum",
		EntryPrice: 110, Quantity: 2, StopLoss: 95, TakeProfit: 130, Status: "open",
	})
	if err != nil {
		t.Fatalf("InsertTrade lot2: %v", err)
	}

	exec := &fakeBracketExec{
		placeRes: execution.OrderResult{
			BrokerOrderID: "sell-1", Status: "filled", FilledPrice: 120, FilledQty: 5,
		},
	}
	e := newBracketTestEngine(exec)
	sigID := insertSellSignal(t, ticker)

	e.executeSellExit(ctx, sigID, store.Signal{
		ID: sigID, Ticker: ticker, Action: "SELL", SignalPrice: 119, SizePct: 2, Confidence: 0.7,
	})

	// One sell for the full HELD quantity (3+2), not a cash-derived size.
	if len(exec.placed) != 1 || exec.placed[0].Quantity != 5 {
		t.Fatalf("placed = %+v, want one sell of qty 5", exec.placed)
	}
	if exec.placed[0].Side != execution.OrderSideSell || exec.placed[0].Type != execution.OrderTypeMarket {
		t.Fatalf("order = %+v, want market sell", exec.placed[0])
	}
	// Legs cancelled before the sell.
	if len(exec.cancelled) != 2 {
		t.Fatalf("cancelled = %v, want both legs (%s, %s)", exec.cancelled, tpID, slID)
	}
	// Both lots closed, zero short rows.
	open, closed, short := tradeRowsForTicker(t, ticker)
	if open != 0 || closed != 2 || short != 0 {
		t.Fatalf("rows open=%d closed=%d short=%d, want 0/2/0", open, closed, short)
	}
	// Per-lot P&L at the confirmed 120 fill: lot1 entry 100 → +60; lot2 entry 110 → +20.
	for id, want := range map[string]float64{trade1: 60, trade2: 20} {
		var pnl float64
		if err := store.GetDB().QueryRow(ctx,
			`SELECT pnl FROM trades WHERE id=$1 AND status='closed'`, id).Scan(&pnl); err != nil {
			t.Fatalf("pnl lookup %s: %v", id, err)
		}
		if pnl < want-0.01 || pnl > want+0.01 {
			t.Fatalf("trade %s pnl = %.2f, want %.2f", id, pnl, want)
		}
	}
	if got := signalStatus(t, sigID); got != "executed" {
		t.Fatalf("signal status = %s, want executed", got)
	}
}

// TestSellExit_NoLongBlocks: state changed since the ownership gate ran —
// the re-check must block without placing any order.
func TestSellExit_NoLongBlocks(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("DATABASE_URL not set; skipping DB-backed engine test")
	}
	exec := &fakeBracketExec{placeRes: execution.OrderResult{Status: "filled", FilledPrice: 1}}
	e := newBracketTestEngine(exec)
	sigID := insertSellSignal(t, "NOLG")

	e.executeSellExit(context.Background(), sigID, store.Signal{
		ID: sigID, Ticker: "NOLG", Action: "SELL", SignalPrice: 119,
	})
	if len(exec.placed) != 0 {
		t.Fatalf("no order must be placed without a long, got %+v", exec.placed)
	}
	if got := signalStatus(t, sigID); got != "blocked" {
		t.Fatalf("signal status = %s, want blocked", got)
	}
}

// TestSellExit_LegCancelFailureBlocks: an unconfirmable leg cancel must
// block the sell — a live leg would race it — and leave the lot open.
func TestSellExit_LegCancelFailureBlocks(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("DATABASE_URL not set; skipping DB-backed engine test")
	}
	const ticker = "SLCF"
	_, _, slID := insertBracketFixture(t, ticker, 3)
	exec := &fakeBracketExec{
		cancelErr: map[string]error{slID: fmt.Errorf("simulated cancel failure")},
		statuses:  map[string]execution.OrderResult{slID: {Status: "pending"}},
		placeRes:  execution.OrderResult{Status: "filled", FilledPrice: 120},
	}
	e := newBracketTestEngine(exec)
	sigID := insertSellSignal(t, ticker)

	e.executeSellExit(context.Background(), sigID, store.Signal{
		ID: sigID, Ticker: ticker, Action: "SELL", SignalPrice: 119,
	})
	if len(exec.placed) != 0 {
		t.Fatalf("no order must be placed while a leg cancel is unconfirmed, got %+v", exec.placed)
	}
	open, _, _ := tradeRowsForTicker(t, ticker)
	if open != 1 {
		t.Fatalf("open rows = %d, want 1 (lot untouched)", open)
	}
	if got := signalStatus(t, sigID); got != "blocked" {
		t.Fatalf("signal status = %s, want blocked", got)
	}
}

// TestSellExit_OrderErrorKeepsLongsOpen: broker rejects the sell — the lots
// must stay open (software monitoring resumes since the legs are gone).
func TestSellExit_OrderErrorKeepsLongsOpen(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("DATABASE_URL not set; skipping DB-backed engine test")
	}
	const ticker = "SLER"
	insertBracketFixture(t, ticker, 3)
	exec := &fakeBracketExec{placeErr: fmt.Errorf("simulated broker rejection")}
	e := newBracketTestEngine(exec)
	sigID := insertSellSignal(t, ticker)

	e.executeSellExit(context.Background(), sigID, store.Signal{
		ID: sigID, Ticker: ticker, Action: "SELL", SignalPrice: 119,
	})
	open, closed, short := tradeRowsForTicker(t, ticker)
	if open != 1 || closed != 0 || short != 0 {
		t.Fatalf("rows open=%d closed=%d short=%d, want 1/0/0", open, closed, short)
	}
	if got := signalStatus(t, sigID); got != "blocked" {
		t.Fatalf("signal status = %s, want blocked", got)
	}
}
