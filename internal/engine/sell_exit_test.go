package engine

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/execution"
	"github.com/romeo-folie/trade-engine/internal/stockorders"
	"github.com/romeo-folie/trade-engine/internal/store"
)

// definitiveThenFilledExitBroker rejects the first request before creating a
// broker order, then accepts and fills the durable successor generation.
type definitiveThenFilledExitBroker struct {
	posts []execution.OrderRequest
	err   error
}

func (b *definitiveThenFilledExitBroker) PlaceOrder(_ context.Context, req execution.OrderRequest) (execution.OrderResult, error) {
	b.posts = append(b.posts, req)
	if len(b.posts) == 1 {
		return execution.OrderResult{}, b.err
	}
	return execution.OrderResult{BrokerOrderID: "accepted-retry-exit", Status: "filled", FilledPrice: 120, FilledQty: req.Quantity}, nil
}
func (*definitiveThenFilledExitBroker) GetOrderByClientID(context.Context, string) (execution.OrderResult, error) {
	return execution.OrderResult{}, execution.ErrOrderNotFound
}
func (*definitiveThenFilledExitBroker) CancelOrder(context.Context, string) error { return nil }
func (*definitiveThenFilledExitBroker) GetOrderStatus(context.Context, string) (execution.OrderResult, error) {
	return execution.OrderResult{}, errors.New("unused")
}
func (*definitiveThenFilledExitBroker) GetPositions(context.Context) ([]execution.Position, error) {
	return nil, nil
}
func (*definitiveThenFilledExitBroker) GetAccount(context.Context) (execution.AccountInfo, error) {
	return execution.AccountInfo{}, nil
}
func (*definitiveThenFilledExitBroker) ReplaceStopOrder(context.Context, string, float64) (execution.OrderResult, error) {
	return execution.OrderResult{}, errors.New("unused")
}

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
	ticker := fmt.Sprintf("SLCF_%d", time.Now().UnixNano())
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

func TestSellExit_LegCancellationFillRetainsClaim(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("DATABASE_URL not set; skipping DB-backed engine test")
	}
	const ticker = "SLFF"
	tradeID, _, slID := insertBracketFixture(t, ticker, 3)
	exec := &fakeBracketExec{statuses: map[string]execution.OrderResult{
		slID: {Status: "cancelled", FilledPrice: 95, FilledQty: 3},
	}}
	e := newBracketTestEngine(exec)
	sigID := insertSellSignal(t, ticker)

	e.executeSellExit(context.Background(), sigID, store.Signal{
		ID: sigID, Ticker: ticker, Action: "SELL", SignalPrice: 94,
	})
	if len(exec.placed) != 0 {
		t.Fatalf("market sell placed after leg flattened lot: %+v", exec.placed)
	}
	trade, err := store.GetTradeByID(context.Background(), tradeID)
	if err != nil || trade == nil {
		t.Fatalf("GetTradeByID: %v", err)
	}
	if trade.Status != "exiting" || trade.Quantity != 0 {
		t.Fatalf("trade = status %s qty %v, want exiting/0 for reconciliation",
			trade.Status, trade.Quantity)
	}
}

// TestSellExit_OrderErrorKeepsClaimForRecovery: placement errors are
// ambiguous, so the lot remains exiting until broker recovery proves whether
// the POST survived.
func TestSellExit_OrderErrorKeepsClaimForRecovery(t *testing.T) {
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
	if open != 0 || closed != 0 || short != 0 {
		t.Fatalf("rows open=%d closed=%d short=%d, want 0/0/0 while claim is retained", open, closed, short)
	}
	owned, err := store.GetOwnedPositionTrades(context.Background())
	if err != nil {
		t.Fatalf("GetOwnedPositionTrades: %v", err)
	}
	foundExiting := false
	for _, trade := range owned {
		foundExiting = foundExiting ||
			(trade.Ticker == ticker && trade.Status == "exiting")
	}
	if !foundExiting {
		t.Fatal("ambiguous placement released the exit claim")
	}
	if got := signalStatus(t, sigID); got != "blocked" {
		t.Fatalf("signal status = %s, want blocked", got)
	}
}

func TestSellExit_DefinitiveCoordinatorRejectionRotatesAndCloses(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("DATABASE_URL not set; skipping DB-backed engine test")
	}
	ctx := context.Background()
	ticker := fmt.Sprintf("SLDR_%d", time.Now().UnixNano())
	tradeID, err := store.InsertTrade(ctx, store.Trade{Ticker: ticker, Direction: "long", Strategy: "momentum", EntryPrice: 100, Quantity: 3, Status: "open"})
	if err != nil {
		t.Fatal(err)
	}
	broker := &definitiveThenFilledExitBroker{err: &execution.BrokerRequestError{StatusCode: 422, Message: "market exit rejected"}}
	e := newBracketTestEngine(stockorders.New(broker))
	sigID := insertSellSignal(t, ticker)
	e.executeSellExit(ctx, sigID, store.Signal{ID: sigID, Ticker: ticker, Action: "SELL", SignalPrice: 119, Confidence: 0.7})

	if len(broker.posts) != 2 || broker.posts[0].ClientOrderID == broker.posts[1].ClientOrderID {
		t.Fatalf("definitive rejection did not advance client generation: %+v", broker.posts)
	}
	baseKey := "analyst-exit:" + sigID + ":" + ticker
	base, err := store.GetStockOrderIntentByKey(ctx, baseKey)
	if err != nil || base == nil || base.Status != "rejected" || base.BrokerOrderID != "" {
		t.Fatalf("rejected base intent=%#v err=%v", base, err)
	}
	_, retryKey, err := store.ResolveStockOrderIntentRetryGeneration(ctx, baseKey)
	if err != nil {
		t.Fatal(err)
	}
	retry, err := store.GetStockOrderIntentByKey(ctx, retryKey)
	if err != nil || retry == nil || retry.Status != "filled" || retry.BrokerOrderID != "accepted-retry-exit" {
		t.Fatalf("accepted retry intent=%#v err=%v", retry, err)
	}
	trade, err := store.GetTradeByID(ctx, tradeID)
	if err != nil || trade == nil || trade.Status != "closed" || trade.Quantity != 3 {
		t.Fatalf("retried definitive analyst exit trade=%#v err=%v", trade, err)
	}
	if got := signalStatus(t, sigID); got != "executed" {
		t.Fatalf("signal status=%q, want executed", got)
	}
}
