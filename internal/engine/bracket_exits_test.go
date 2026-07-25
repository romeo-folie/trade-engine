package engine

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/cost"
	"github.com/romeo-folie/trade-engine/internal/data"
	"github.com/romeo-folie/trade-engine/internal/events"
	"github.com/romeo-folie/trade-engine/internal/execution"
	"github.com/romeo-folie/trade-engine/internal/portfolio"
	"github.com/romeo-folie/trade-engine/internal/store"
)

// fakeBracketExec serves canned order statuses and records cancels/orders.
type fakeBracketExec struct {
	statuses  map[string]execution.OrderResult
	statusErr map[string]error
	cancelErr map[string]error
	cancelled []string
	placed    []execution.OrderRequest
	placeRes  execution.OrderResult
	placeErr  error
}

func (f *fakeBracketExec) PlaceOrder(_ context.Context, req execution.OrderRequest) (execution.OrderResult, error) {
	if f.placeErr != nil {
		return execution.OrderResult{}, f.placeErr
	}
	f.placed = append(f.placed, req)
	return f.placeRes, nil
}

func (f *fakeBracketExec) CancelOrder(_ context.Context, id string) error {
	if err, ok := f.cancelErr[id]; ok {
		return err
	}
	f.cancelled = append(f.cancelled, id)
	return nil
}

func (f *fakeBracketExec) GetOrderStatus(_ context.Context, id string) (execution.OrderResult, error) {
	if err, ok := f.statusErr[id]; ok {
		return execution.OrderResult{}, err
	}
	if res, ok := f.statuses[id]; ok {
		return res, nil
	}
	return execution.OrderResult{}, execution.ErrOrderNotFound
}

func (f *fakeBracketExec) GetPositions(_ context.Context) ([]execution.Position, error) {
	return nil, nil
}

func (f *fakeBracketExec) GetAccount(_ context.Context) (execution.AccountInfo, error) {
	return execution.AccountInfo{}, nil
}

func newBracketTestEngine(exec execution.Executor) *Engine {
	return &Engine{
		cfg:         &config.Config{},
		executor:    exec,
		portfolio:   portfolio.NewTracker(time.Hour),
		bus:         events.New(),
		costTracker: cost.NewTracker(),
	}
}

// insertBracketFixture creates an open trade with two OCO legs and returns
// (tradeID, tpLegBrokerID, slLegBrokerID).
func insertBracketFixture(t *testing.T, ticker string, qty float64) (string, string, string) {
	t.Helper()
	ctx := context.Background()
	tradeID, err := store.InsertTrade(ctx, store.Trade{
		Ticker: ticker, Direction: "long", Strategy: "momentum",
		EntryPrice: 100, Quantity: qty, StopLoss: 95, TakeProfit: 110,
		Status: "open",
	})
	if err != nil {
		t.Fatalf("InsertTrade: %v", err)
	}
	tpID := "tp-" + tradeID[:8]
	slID := "sl-" + tradeID[:8]
	for _, leg := range []store.Order{
		{TradeID: tradeID, BrokerOrderID: tpID, Ticker: ticker, Side: "sell",
			OrderType: "limit", Quantity: qty, LimitPrice: 110, Status: "pending",
			IsTakeProfit: true, ParentOrderID: "parent-" + tradeID[:8]},
		{TradeID: tradeID, BrokerOrderID: slID, Ticker: ticker, Side: "sell",
			OrderType: "stop", Quantity: qty, StopPrice: 95, Status: "pending",
			IsStopLoss: true, ParentOrderID: "parent-" + tradeID[:8]},
	} {
		if _, err := store.InsertOrder(ctx, leg); err != nil {
			t.Fatalf("InsertOrder: %v", err)
		}
	}
	return tradeID, tpID, slID
}

func getTrade(t *testing.T, id string) store.Trade {
	t.Helper()
	trades, err := store.GetOpenTrades(context.Background())
	if err != nil {
		t.Fatalf("GetOpenTrades: %v", err)
	}
	for _, tr := range trades {
		if tr.ID == id {
			return tr
		}
	}
	// Not open — fetch as closed via orders join is overkill; signal absence.
	return store.Trade{}
}

// TestReconcileBracket_StandaloneStopIsNotBrokerManaged pins the Codex P2:
// a trade whose only working exit order is a STANDALONE trailing stop
// (ParentOrderID empty) must stay app-managed, or software take-profit
// checks would silently stop for every trailing trade.
func TestReconcileBracket_StandaloneStopIsNotBrokerManaged(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("DATABASE_URL not set; skipping DB-backed engine test")
	}
	ctx := context.Background()
	tradeID, err := store.InsertTrade(ctx, store.Trade{
		Ticker: "STDA", Direction: "long", Strategy: "momentum",
		EntryPrice: 100, Quantity: 5, StopLoss: 95, TakeProfit: 110, Status: "open",
	})
	if err != nil {
		t.Fatalf("InsertTrade: %v", err)
	}
	if _, err := store.InsertOrder(ctx, store.Order{
		TradeID: tradeID, BrokerOrderID: "trail-1", Ticker: "STDA", Side: "sell",
		OrderType: "stop", Quantity: 5, StopPrice: 97, Status: "pending",
		IsStopLoss: true, // ParentOrderID deliberately empty: trailing manager's stop
	}); err != nil {
		t.Fatalf("InsertOrder: %v", err)
	}

	e := newBracketTestEngine(&fakeBracketExec{})
	trade := getTrade(t, tradeID)
	if handled := e.reconcileBracketTrade(ctx, trade); handled {
		t.Fatal("standalone trailing stop must not mark the trade broker-managed")
	}
}

func TestMonitorSoftwareTakeProfitCancelsStandaloneStopBeforeSell(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("DATABASE_URL not set; skipping DB-backed engine test")
	}
	ctx := context.Background()
	tradeID, err := store.InsertTrade(ctx, store.Trade{
		Ticker: "CAPA", Direction: "long", Strategy: "momentum",
		EntryPrice: 100, Quantity: 5, StopLoss: 95, TakeProfit: 110,
		Status: "open",
	})
	if err != nil {
		t.Fatalf("InsertTrade: %v", err)
	}
	if _, err := store.InsertOrder(ctx, store.Order{
		TradeID: tradeID, BrokerOrderID: "standalone-stop", Ticker: "CAPA",
		Side: "sell", OrderType: "stop", Quantity: 5, StopPrice: 97,
		Status: "pending", IsStopLoss: true,
	}); err != nil {
		t.Fatalf("InsertOrder: %v", err)
	}
	if err := store.UpsertCandles(ctx, []data.Candle{{
		Ticker: "CAPA", Timeframe: data.Timeframe1Hour,
		Timestamp: time.Now(), Open: 110, High: 112, Low: 109, Close: 111,
	}}); err != nil {
		t.Fatalf("UpsertCandles: %v", err)
	}

	exec := &fakeBracketExec{
		statuses: map[string]execution.OrderResult{
			"standalone-stop": {Status: "cancelled"},
		},
		placeRes: execution.OrderResult{
			BrokerOrderID: "tp-market", Status: "filled",
			FilledPrice: 111, FilledQty: 5,
		},
	}
	e := newBracketTestEngine(exec)
	e.lastExitReconcile = time.Now()
	e.monitorPositions(ctx)

	if len(exec.cancelled) != 1 || exec.cancelled[0] != "standalone-stop" {
		t.Fatalf("standalone stop was not cancelled first: %+v", exec.cancelled)
	}
	if len(exec.placed) != 1 || exec.placed[0].Type != execution.OrderTypeMarket {
		t.Fatalf("market TP exit = %+v, want one market order", exec.placed)
	}
	if tr := getTrade(t, tradeID); tr.ID != "" {
		t.Fatal("trade remained open after confirmed software TP fill")
	}
}

func TestReconcileBracket_UncappedStandaloneStopIsBrokerManaged(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("DATABASE_URL not set; skipping DB-backed engine test")
	}
	ctx := context.Background()
	tradeID, err := store.InsertTrade(ctx, store.Trade{
		Ticker: "USTP", Direction: "long", Strategy: "momentum",
		EntryPrice: 100, Quantity: 5, StopLoss: 95, TakeProfit: 0, Status: "open",
	})
	if err != nil {
		t.Fatalf("InsertTrade: %v", err)
	}
	if _, err := store.InsertOrder(ctx, store.Order{
		TradeID: tradeID, BrokerOrderID: "uncapped-stop", Ticker: "USTP", Side: "sell",
		OrderType: "stop", Quantity: 5, StopPrice: 95, Status: "pending",
		IsStopLoss: true,
	}); err != nil {
		t.Fatalf("InsertOrder: %v", err)
	}

	exec := &fakeBracketExec{statuses: map[string]execution.OrderResult{
		"uncapped-stop": {Status: "accepted"},
	}}
	e := newBracketTestEngine(exec)
	if handled := e.reconcileBracketTrade(ctx, getTrade(t, tradeID)); !handled {
		t.Fatal("uncapped standalone stop must suppress the software stop path")
	}
	if tr := getTrade(t, tradeID); tr.ID == "" {
		t.Fatal("working standalone stop must leave the trade open")
	}
}

func TestReconcileBracket_UncappedStandaloneStopFillClosesTrade(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("DATABASE_URL not set; skipping DB-backed engine test")
	}
	ctx := context.Background()
	tradeID, err := store.InsertTrade(ctx, store.Trade{
		Ticker: "USFL", Direction: "long", Strategy: "momentum",
		EntryPrice: 100, Quantity: 5, StopLoss: 95, TakeProfit: 0, Status: "open",
	})
	if err != nil {
		t.Fatalf("InsertTrade: %v", err)
	}
	if _, err := store.InsertOrder(ctx, store.Order{
		TradeID: tradeID, BrokerOrderID: "uncapped-stop-fill", Ticker: "USFL", Side: "sell",
		OrderType: "stop", Quantity: 5, StopPrice: 95, Status: "pending",
		IsStopLoss: true,
	}); err != nil {
		t.Fatalf("InsertOrder: %v", err)
	}

	exec := &fakeBracketExec{statuses: map[string]execution.OrderResult{
		"uncapped-stop-fill": {Status: "filled", FilledPrice: 94.75, FilledQty: 5},
	}}
	e := newBracketTestEngine(exec)
	if handled := e.reconcileBracketTrade(ctx, getTrade(t, tradeID)); !handled {
		t.Fatal("filled uncapped standalone stop must be reconciled")
	}
	if tr := getTrade(t, tradeID); tr.ID != "" {
		t.Fatal("standalone broker stop fill must close the DB trade")
	}
	if e.portfolio.CurrentState().LastLossAt.IsZero() {
		t.Fatal("standalone stop loss must record loss cooldown")
	}
}

// TestReconcileBracket_WorkingLegsSkipSoftwareExit: pending OCO legs →
// handled=true, trade stays open, nothing cancelled.
func TestReconcileBracket_WorkingLegsSkipSoftwareExit(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("DATABASE_URL not set; skipping DB-backed engine test")
	}
	ctx := context.Background()
	tradeID, tpID, slID := insertBracketFixture(t, "WRKG", 3)
	exec := &fakeBracketExec{statuses: map[string]execution.OrderResult{
		tpID: {Status: "accepted"},
		slID: {Status: "accepted"},
	}}
	e := newBracketTestEngine(exec)
	if handled := e.reconcileBracketTrade(ctx, getTrade(t, tradeID)); !handled {
		t.Fatal("working OCO legs must mark the trade broker-managed")
	}
	if tr := getTrade(t, tradeID); tr.ID == "" {
		t.Fatal("trade must remain open while legs work")
	}
}

// TestReconcileBracket_StopLegFillClosesTrade: SL fills at the broker →
// trade closed at the real fill, TP sibling marked cancelled, loss cooldown
// recorded (parity with the software exit path).
func TestReconcileBracket_StopLegFillClosesTrade(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("DATABASE_URL not set; skipping DB-backed engine test")
	}
	ctx := context.Background()
	tradeID, tpID, slID := insertBracketFixture(t, "SLFL", 3)
	exec := &fakeBracketExec{statuses: map[string]execution.OrderResult{
		tpID: {Status: "canceled"},
		slID: {Status: "filled", FilledPrice: 94.8, FilledQty: 3},
	}}
	e := newBracketTestEngine(exec)
	if handled := e.reconcileBracketTrade(ctx, getTrade(t, tradeID)); !handled {
		t.Fatal("a filled leg must be handled by the bracket path")
	}
	if tr := getTrade(t, tradeID); tr.ID != "" {
		t.Fatal("trade must be closed after the stop leg filled")
	}
	legs, err := store.GetOrdersByTrade(ctx, tradeID)
	if err != nil {
		t.Fatalf("GetOrdersByTrade: %v", err)
	}
	for _, o := range legs {
		switch o.BrokerOrderID {
		case slID:
			if o.Status != "filled" || o.FilledPrice != 94.8 {
				t.Fatalf("SL leg = %s @ %.2f, want filled @ 94.80", o.Status, o.FilledPrice)
			}
		case tpID:
			if o.Status != "cancelled" {
				t.Fatalf("TP sibling = %s, want cancelled", o.Status)
			}
		}
	}
	if e.portfolio.CurrentState().LastLossAt.IsZero() {
		t.Fatal("losing bracket stop-out must record the loss cooldown (RecordLoss)")
	}
}

// TestReconcileBracket_PartialTPPlusStopWeightedExit: TP partially fills
// 2/3 shares, OCO cancels it, the ratcheted stop fills the remaining 1 —
// the close must be the fill-weighted aggregate, not the stop price alone.
func TestReconcileBracket_PartialTPPlusStopWeightedExit(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("DATABASE_URL not set; skipping DB-backed engine test")
	}
	ctx := context.Background()
	tradeID, tpID, slID := insertBracketFixture(t, "PRTL", 3)
	exec := &fakeBracketExec{statuses: map[string]execution.OrderResult{
		tpID: {Status: "canceled", FilledPrice: 110, FilledQty: 2},
		slID: {Status: "filled", FilledPrice: 95, FilledQty: 1},
	}}
	e := newBracketTestEngine(exec)
	if handled := e.reconcileBracketTrade(ctx, getTrade(t, tradeID)); !handled {
		t.Fatal("filled leg must be handled")
	}
	// Weighted exit: (2×110 + 1×95) / 3 = 105. Entry 100, qty 3 → pnl +15.
	var exitPrice, pnl float64
	err := store.GetDB().QueryRow(ctx,
		`SELECT exit_price, pnl FROM trades WHERE id=$1 AND status='closed'`, tradeID,
	).Scan(&exitPrice, &pnl)
	if err != nil {
		t.Fatalf("closed trade lookup: %v", err)
	}
	if exitPrice < 104.99 || exitPrice > 105.01 {
		t.Fatalf("exit_price = %.4f, want fill-weighted 105", exitPrice)
	}
	if pnl < 14.9 || pnl > 15.1 {
		t.Fatalf("pnl = %.4f, want ~+15 (partial TP proceeds included)", pnl)
	}
}

// TestReconcileBracket_LegsDeadWithPartial_ReducesQuantity: both legs
// terminal, one carried a partial fill, remainder unsold — the tracked
// quantity must shrink before reverting to app-managed monitoring, or the
// software exit would over-sell shares the broker no longer holds.
func TestReconcileBracket_LegsDeadWithPartial_ReducesQuantity(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("DATABASE_URL not set; skipping DB-backed engine test")
	}
	ctx := context.Background()
	tradeID, tpID, slID := insertBracketFixture(t, "DEAD", 3)
	exec := &fakeBracketExec{statuses: map[string]execution.OrderResult{
		tpID: {Status: "canceled", FilledPrice: 110, FilledQty: 1},
		slID: {Status: "canceled"},
	}}
	e := newBracketTestEngine(exec)
	if handled := e.reconcileBracketTrade(ctx, getTrade(t, tradeID)); handled {
		t.Fatal("dead legs must revert the trade to app-managed monitoring")
	}
	tr := getTrade(t, tradeID)
	if tr.ID == "" {
		t.Fatal("trade must remain open (only 1 of 3 shares sold)")
	}
	if tr.Quantity != 2 {
		t.Fatalf("Quantity = %.2f, want 2 after the 1-share partial exit", tr.Quantity)
	}
}

// TestCancelBracketLegs_AllClearSemantics: a failed cancel must report NOT
// clear (the SELL caller blocks), and the failed leg's row must stay pending
// so the monitor keeps treating the trade as broker-managed.
func TestCancelBracketLegs_AllClearSemantics(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("DATABASE_URL not set; skipping DB-backed engine test")
	}
	ctx := context.Background()
	tradeID, tpID, slID := insertBracketFixture(t, "CNCL", 3)

	exec := &fakeBracketExec{cancelErr: map[string]error{slID: fmt.Errorf("simulated cancel failure")}}
	e := newBracketTestEngine(exec)
	if clear := e.cancelBracketLegsForTicker(ctx, "CNCL"); clear {
		t.Fatal("failed leg cancel must report not-clear")
	}
	legs, err := store.OpenExitLegsForTrade(ctx, tradeID)
	if err != nil {
		t.Fatalf("OpenExitLegsForTrade: %v", err)
	}
	if len(legs) != 1 || legs[0].BrokerOrderID != slID {
		t.Fatalf("working legs = %+v, want only the un-cancelled SL leg %s", legs, slID)
	}

	// Second attempt succeeds → all clear, no working legs remain.
	exec.cancelErr = nil
	if clear := e.cancelBracketLegsForTicker(ctx, "CNCL"); !clear {
		t.Fatal("successful cancels must report all-clear")
	}
	legs, _ = store.OpenExitLegsForTrade(ctx, tradeID)
	if len(legs) != 0 {
		t.Fatalf("working legs = %+v, want none", legs)
	}
	_ = tpID
}
