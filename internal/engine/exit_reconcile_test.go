package engine

import (
	"context"
	"errors"
	"math"
	"testing"

	"github.com/romeo-folie/trade-engine/internal/execution"
	"github.com/romeo-folie/trade-engine/internal/store"
)

// fakeExitExecutor serves canned GetOrderStatus responses per broker order id.
type fakeExitExecutor struct {
	results map[string]execution.OrderResult
	errs    map[string]error
	calls   []string
}

func (f *fakeExitExecutor) GetOrderStatus(_ context.Context, brokerOrderID string) (execution.OrderResult, error) {
	f.calls = append(f.calls, brokerOrderID)
	if err, ok := f.errs[brokerOrderID]; ok {
		return execution.OrderResult{}, err
	}
	return f.results[brokerOrderID], nil
}

// fakeExitStore records reconciler mutations in memory.
type fakeExitStore struct {
	pending      []store.PendingExitOrder
	pendingErr   error
	tradeExitErr error                 // injected UpdateTradeExit failure
	orderFills   map[string][2]float64 // broker_order_id → {price, qty}
	tradeExits   map[string][3]float64 // trade_id → {exit, pnl, pnlPct}
	orderStatus  map[string]string     // broker_order_id → status
	tradeQty     map[string]float64
}

func newFakeExitStore(pending ...store.PendingExitOrder) *fakeExitStore {
	return &fakeExitStore{
		pending:     pending,
		orderFills:  map[string][2]float64{},
		tradeExits:  map[string][3]float64{},
		orderStatus: map[string]string{},
		tradeQty:    map[string]float64{},
	}
}

func (f *fakeExitStore) GetPendingExitOrders(_ context.Context, _ int) ([]store.PendingExitOrder, error) {
	return f.pending, f.pendingErr
}
func (f *fakeExitStore) UpdateOrderFill(_ context.Context, id string, price, qty float64, status string) error {
	f.orderFills[id] = [2]float64{price, qty}
	f.orderStatus[id] = status
	return nil
}
func (f *fakeExitStore) UpdateTradeExit(_ context.Context, id string, exit, pnl, pnlPct float64) error {
	if f.tradeExitErr != nil {
		return f.tradeExitErr
	}
	f.tradeExits[id] = [3]float64{exit, pnl, pnlPct}
	return nil
}
func (f *fakeExitStore) UpdateExitingTradeQuantity(_ context.Context, id string, quantity float64) error {
	f.tradeQty[id] = quantity
	return nil
}
func (f *fakeExitStore) MarkOrderStatus(_ context.Context, id, status string) error {
	f.orderStatus[id] = status
	return nil
}
func (f *fakeExitStore) ReleaseTradeExitClaim(_ context.Context, _ string) error {
	return nil
}

// lacExit mirrors the real 2026-07-02 LAC trade: recorded exit 5.50 (stale
// candle trigger price), broker actually filled at 3.82.
func lacExit() store.PendingExitOrder {
	return store.PendingExitOrder{
		OrderID:       "o1",
		BrokerOrderID: "b1",
		Ticker:        "LAC",
		TradeID:       "t1",
		Direction:     "long",
		EntryPrice:    3.86,
		Quantity:      480.122977,
		ExitPrice:     5.50,
	}
}

func TestExitReconcileCorrectsToBrokerFill(t *testing.T) {
	db := newFakeExitStore(lacExit())
	ex := &fakeExitExecutor{results: map[string]execution.OrderResult{
		"b1": {BrokerOrderID: "b1", Status: "filled", FilledPrice: 3.82, FilledQty: 480.122977},
	}}

	stats := ReconcileExitOrderFills(context.Background(), ex, db, 0)

	if stats.Corrected != 1 || stats.Scanned != 1 {
		t.Fatalf("stats = %+v, want Scanned=1 Corrected=1", stats)
	}
	fill, ok := db.orderFills["o1"]
	if !ok || fill[0] != 3.82 {
		t.Fatalf("order fill = %v, want price 3.82 backfilled", fill)
	}
	got, ok := db.tradeExits["t1"]
	if !ok {
		t.Fatal("trade exit not rewritten")
	}
	wantPnL := (3.82 - 3.86) * 480.122977
	if got[0] != 3.82 || math.Abs(got[1]-wantPnL) > 1e-9 {
		t.Fatalf("trade exit = %v, want exit=3.82 pnl=%.6f", got, wantPnL)
	}
	if got[1] >= 0 {
		t.Fatalf("pnl = %.4f, want a loss — the recorded +42%% gain was phantom", got[1])
	}
}

func TestExitReconcileFinalizesExitingTradeAndRecordsConfirmedLoss(t *testing.T) {
	p := lacExit()
	p.TradeStatus = "exiting"
	p.ExitPrice = 0
	db := newFakeExitStore(p)
	ex := &fakeExitExecutor{results: map[string]execution.OrderResult{
		"b1": {Status: "filled", FilledPrice: 3.82, FilledQty: p.Quantity},
	}}
	losses := 0

	stats := ReconcileExitOrderFills(
		context.Background(), ex, db, 0, func() { losses++ },
	)

	if stats.Corrected != 1 || losses != 1 {
		t.Fatalf("stats=%+v losses=%d, want finalized loss", stats, losses)
	}
	if _, ok := db.tradeExits[p.TradeID]; !ok {
		t.Fatal("exiting trade was not finalized at broker fill")
	}
}

func TestExitReconcileCancelledPartialReopensOnlyRemainder(t *testing.T) {
	p := lacExit()
	p.TradeStatus = "exiting"
	p.Quantity = 10
	db := newFakeExitStore(p)
	ex := &fakeExitExecutor{results: map[string]execution.OrderResult{
		"b1": {Status: "cancelled", FilledPrice: 3.80, FilledQty: 4},
	}}

	stats := ReconcileExitOrderFills(context.Background(), ex, db, 0)

	if stats.Terminal != 1 || db.tradeQty[p.TradeID] != 6 {
		t.Fatalf("stats=%+v remaining=%v, want reopened remainder 6", stats, db.tradeQty[p.TradeID])
	}
	if fill := db.orderFills[p.OrderID]; fill != [2]float64{3.80, 4} {
		t.Fatalf("partial order fill = %v, want price 3.80 qty 4", fill)
	}
}

func TestExitReconcileTerminalFillReplayUsesDurableAllocation(t *testing.T) {
	p := lacExit()
	p.TradeStatus = "exiting"
	p.Quantity = 5
	p.OrderQuantity = 5
	p.RecordedFill = 2
	p.AggregateFill = 2
	db := newFakeExitStore(p)
	ex := &fakeExitExecutor{results: map[string]execution.OrderResult{
		"b1": {Status: "cancelled", FilledPrice: 3.80, FilledQty: 2},
	}}

	ReconcileExitOrderFills(context.Background(), ex, db, 0)

	if got := db.tradeQty[p.TradeID]; got != 3 {
		t.Fatalf("remaining quantity = %v, want 3 after replaying already-recorded fill", got)
	}
	if got := db.orderFills[p.OrderID][1]; got != 2 {
		t.Fatalf("recorded fill = %v, want cumulative 2", got)
	}
}

func TestExitReconcileTerminalFillSubtractsAllocationsOutsideBatch(t *testing.T) {
	p := lacExit()
	p.TradeStatus = "exiting"
	p.Quantity = 5
	p.OrderQuantity = 5
	p.AggregateFill = 2 // another lot, omitted by LIMIT, already owns 2 shares
	db := newFakeExitStore(p)
	ex := &fakeExitExecutor{results: map[string]execution.OrderResult{
		"b1": {Status: "cancelled", FilledPrice: 3.80, FilledQty: 4},
	}}

	ReconcileExitOrderFills(context.Background(), ex, db, 0)

	if got := db.orderFills[p.OrderID][1]; got != 2 {
		t.Fatalf("new lot allocation = %v, want only unallocated aggregate remainder 2", got)
	}
	if got := db.tradeQty[p.TradeID]; got != 3 {
		t.Fatalf("remaining quantity = %v, want 3", got)
	}
}

func TestExitReconcileShortDirectionPnL(t *testing.T) {
	p := lacExit()
	p.Direction = "short"
	p.EntryPrice = 10.0
	p.Quantity = 5
	db := newFakeExitStore(p)
	ex := &fakeExitExecutor{results: map[string]execution.OrderResult{
		"b1": {Status: "filled", FilledPrice: 8.0, FilledQty: 5},
	}}

	ReconcileExitOrderFills(context.Background(), ex, db, 0)

	got := db.tradeExits["t1"]
	if got[1] != 10.0 { // (10-8)*5
		t.Fatalf("short pnl = %.4f, want 10.0", got[1])
	}
	if got[2] != 20.0 { // 10 / 50 * 100
		t.Fatalf("short pnl_pct = %.4f, want 20.0", got[2])
	}
}

func TestExitReconcileMatchingFillOnlyBackfillsOrder(t *testing.T) {
	p := lacExit()
	p.ExitPrice = 3.82 // recorded exit already equals broker fill
	db := newFakeExitStore(p)
	ex := &fakeExitExecutor{results: map[string]execution.OrderResult{
		"b1": {Status: "filled", FilledPrice: 3.82, FilledQty: 480.122977},
	}}

	stats := ReconcileExitOrderFills(context.Background(), ex, db, 0)

	if stats.Confirmed != 1 || stats.Corrected != 0 {
		t.Fatalf("stats = %+v, want Confirmed=1 Corrected=0", stats)
	}
	if _, touched := db.tradeExits["t1"]; touched {
		t.Fatal("trade rewritten despite matching fill")
	}
	if _, ok := db.orderFills["o1"]; !ok {
		t.Fatal("order fill columns not backfilled")
	}
}

func TestExitReconcileCancelledMarksOrderNotTrade(t *testing.T) {
	db := newFakeExitStore(lacExit())
	ex := &fakeExitExecutor{results: map[string]execution.OrderResult{
		"b1": {Status: "canceled"},
	}}

	stats := ReconcileExitOrderFills(context.Background(), ex, db, 0)

	if stats.Terminal != 1 {
		t.Fatalf("stats = %+v, want Terminal=1", stats)
	}
	if db.orderStatus["o1"] != "cancelled" {
		t.Fatalf("order status = %q, want cancelled", db.orderStatus["o1"])
	}
	if len(db.tradeExits) != 0 {
		t.Fatal("trade must not be rewritten from a cancelled exit — no fill exists")
	}
}

func TestExitReconcileLeavesWorkingAndUnknownOrders(t *testing.T) {
	p2 := lacExit()
	p2.OrderID, p2.BrokerOrderID, p2.TradeID = "o2", "b2", "t2"
	p3 := lacExit()
	p3.OrderID, p3.BrokerOrderID, p3.TradeID = "o3", "b3", "t3"
	db := newFakeExitStore(lacExit(), p2, p3)
	ex := &fakeExitExecutor{
		results: map[string]execution.OrderResult{
			"b1": {Status: "accepted"},                                          // still working
			"b2": {Status: "partially_filled", FilledPrice: 3.9, FilledQty: 10}, // partial — #363/#369 own the qty model
		},
		errs: map[string]error{
			"b3": execution.ErrOrderNotFound,
		},
	}

	stats := ReconcileExitOrderFills(context.Background(), ex, db, 0)

	if stats.StillPending != 2 || stats.NotFound != 1 {
		t.Fatalf("stats = %+v, want StillPending=2 NotFound=1", stats)
	}
	if len(db.orderFills) != 0 || len(db.tradeExits) != 0 || len(db.orderStatus) != 0 {
		t.Fatalf("no mutations expected; got fills=%v exits=%v status=%v",
			db.orderFills, db.tradeExits, db.orderStatus)
	}
}

func TestExitReconcileFilledWithoutPriceCountsError(t *testing.T) {
	db := newFakeExitStore(lacExit())
	ex := &fakeExitExecutor{results: map[string]execution.OrderResult{
		"b1": {Status: "filled", FilledPrice: 0},
	}}

	stats := ReconcileExitOrderFills(context.Background(), ex, db, 0)

	if stats.Errors != 1 {
		t.Fatalf("stats = %+v, want Errors=1", stats)
	}
	if len(db.tradeExits) != 0 {
		t.Fatal("trade must not be rewritten without a real fill price")
	}
}

func TestExitReconcileTradeUpdateFailureKeepsOrderSweepable(t *testing.T) {
	// If the trade correction fails, the order row must NOT be flipped to
	// 'filled' — the sweep only selects unfilled orders, so writing the order
	// first would make the bad exit price permanent (Codex review on #383).
	db := newFakeExitStore(lacExit())
	db.tradeExitErr = errors.New("transient db error")
	ex := &fakeExitExecutor{results: map[string]execution.OrderResult{
		"b1": {Status: "filled", FilledPrice: 3.82, FilledQty: 480.122977},
	}}

	stats := ReconcileExitOrderFills(context.Background(), ex, db, 0)

	if stats.Errors != 1 || stats.Corrected != 0 {
		t.Fatalf("stats = %+v, want Errors=1 Corrected=0", stats)
	}
	if len(db.orderFills) != 0 {
		t.Fatal("order flipped to filled despite failed trade update — row no longer retryable")
	}
}

func TestExitReconcileNilExecutorNoop(t *testing.T) {
	db := newFakeExitStore(lacExit())
	stats := ReconcileExitOrderFills(context.Background(), nil, db, 0)
	if stats.Scanned != 0 {
		t.Fatalf("stats = %+v, want untouched zero value", stats)
	}
}

func TestExitReconcileStoreFetchError(t *testing.T) {
	db := newFakeExitStore()
	db.pendingErr = errors.New("db down")
	ex := &fakeExitExecutor{}
	stats := ReconcileExitOrderFills(context.Background(), ex, db, 0)
	if stats.Errors != 1 || len(ex.calls) != 0 {
		t.Fatalf("stats = %+v calls=%v, want Errors=1 and no broker calls", stats, ex.calls)
	}
}
