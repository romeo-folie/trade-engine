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

type recoveryExitExecutor struct {
	*fakeExitExecutor
	positions []execution.Position
	working   []execution.WorkingOrder
}

func (f *recoveryExitExecutor) GetPositions(context.Context) ([]execution.Position, error) {
	return f.positions, nil
}

func (f *recoveryExitExecutor) GetWorkingOrders(context.Context) ([]execution.WorkingOrder, error) {
	return f.working, nil
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
	appliedFill  map[string]float64
}

type recoveryExitStore struct {
	*fakeExitStore
	orderless        []store.Trade
	owned            []store.Trade
	durableOwnership []store.DurableExitOrderOwnership
	inserted         []store.Order
	released         []string
}

func (f *recoveryExitStore) GetOrderlessExitingTrades(context.Context, int) ([]store.Trade, error) {
	return f.orderless, nil
}

func (f *recoveryExitStore) GetOwnedPositionTrades(context.Context) ([]store.Trade, error) {
	return f.owned, nil
}

func (f *recoveryExitStore) GetDurableExitOrderOwnership(context.Context) ([]store.DurableExitOrderOwnership, error) {
	return f.durableOwnership, nil
}

func (f *recoveryExitStore) InsertOrderWithRetry(_ context.Context, o store.Order) (string, error) {
	f.inserted = append(f.inserted, o)
	return o.BrokerOrderID, nil
}

func (f *recoveryExitStore) ApplyRecoveredExit(
	_ context.Context,
	id string,
	exitPrice, soldQty float64,
) (store.TradeRealization, error) {
	var trade store.Trade
	for _, candidate := range f.orderless {
		if candidate.ID == id {
			trade = candidate
			break
		}
	}
	remaining := math.Max(0, trade.Quantity-soldQty)
	pnl, pnlPct := tradePnL(
		trade.Direction, trade.EntryPrice, soldQty, exitPrice,
	)
	result := store.TradeRealization{
		ExitPrice: exitPrice,
		PnL:       pnl,
		PnLPct:    pnlPct,
		ExitQty:   soldQty,
		Remaining: remaining,
		Status:    "open",
	}
	if remaining > 0 {
		f.tradeQty[id] = remaining
		f.released = append(f.released, id)
		return result, nil
	}
	result.Status = "closed"
	f.tradeExits[id] = [3]float64{exitPrice, pnl, pnlPct}
	return result, nil
}

func (f *recoveryExitStore) ReleaseTradeExitClaim(_ context.Context, id string) error {
	f.released = append(f.released, id)
	return nil
}

func newFakeExitStore(pending ...store.PendingExitOrder) *fakeExitStore {
	return &fakeExitStore{
		pending:     pending,
		orderFills:  map[string][2]float64{},
		tradeExits:  map[string][3]float64{},
		orderStatus: map[string]string{},
		tradeQty:    map[string]float64{},
		appliedFill: map[string]float64{},
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
func (f *fakeExitStore) ApplyPartialExit(
	_ context.Context,
	tradeID, orderID string,
	price, cumulativeQty float64,
	status string,
) (float64, float64, error) {
	current, known := f.tradeQty[tradeID]
	if !known {
		for _, p := range f.pending {
			if p.TradeID == tradeID {
				current = p.Quantity
				break
			}
		}
	}
	applied := math.Max(0, cumulativeQty-f.appliedFill[orderID])
	f.appliedFill[orderID] += applied
	current = math.Max(0, current-applied)
	f.tradeQty[tradeID] = current
	f.orderFills[orderID] = [2]float64{price, cumulativeQty}
	f.orderStatus[orderID] = status
	return current, applied, nil
}
func (f *fakeExitStore) UpdateTradeExit(_ context.Context, id string, exit, pnl, pnlPct float64) error {
	if f.tradeExitErr != nil {
		return f.tradeExitErr
	}
	f.tradeExits[id] = [3]float64{exit, pnl, pnlPct}
	return nil
}
func (f *fakeExitStore) FinalizeTradeExit(
	_ context.Context,
	id string,
	exit, pnl, pnlPct float64,
) (store.TradeRealization, error) {
	if f.tradeExitErr != nil {
		return store.TradeRealization{}, f.tradeExitErr
	}
	f.tradeExits[id] = [3]float64{exit, pnl, pnlPct}
	return store.TradeRealization{
		ExitPrice: exit,
		PnL:       pnl,
		PnLPct:    pnlPct,
		Status:    "closed",
	}, nil
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

func TestExitReconcileCancelledStopFillFallsBackToStopPrice(t *testing.T) {
	p := lacExit()
	p.TradeStatus = "exiting"
	p.Quantity = 5
	p.OrderQuantity = 5
	p.StopPrice = 3.75
	db := newFakeExitStore(p)
	ex := &fakeExitExecutor{results: map[string]execution.OrderResult{
		"b1": {Status: "cancelled", FilledQty: 2},
	}}

	ReconcileExitOrderFills(context.Background(), ex, db, 0)

	if got := db.orderFills[p.OrderID]; got != [2]float64{3.75, 2} {
		t.Fatalf("terminal stop fill = %v, want stop-price fallback 3.75/2", got)
	}
	if got := db.tradeQty[p.TradeID]; got != 3 {
		t.Fatalf("remaining quantity = %v, want 3", got)
	}
}

func TestExitReconcileFilledStopFallsBackToRecordedPrice(t *testing.T) {
	p := lacExit()
	p.TradeStatus = "exiting"
	p.Quantity = 5
	p.OrderQuantity = 5
	p.RecordedPrice = 3.80
	p.StopPrice = 3.75
	db := newFakeExitStore(p)
	ex := &fakeExitExecutor{results: map[string]execution.OrderResult{
		"b1": {Status: "filled", FilledQty: 5},
	}}

	ReconcileExitOrderFills(context.Background(), ex, db, 0)

	if got := db.orderFills[p.OrderID]; got != [2]float64{3.80, 5} {
		t.Fatalf("filled stop = %v, want recorded-price fallback 3.80/5", got)
	}
	if got := db.tradeQty[p.TradeID]; got != 0 {
		t.Fatalf("remaining quantity = %v, want 0", got)
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

func TestExitReconcileAccumulatesMultipleTerminalRowsForOneTrade(t *testing.T) {
	p1 := lacExit()
	p1.OrderID, p1.BrokerOrderID = "o1", "b1"
	p1.TradeStatus, p1.Quantity, p1.OrderQuantity = "exiting", 10, 10
	p2 := p1
	p2.OrderID, p2.BrokerOrderID = "o2", "b2"
	db := newFakeExitStore(p1, p2)
	ex := &fakeExitExecutor{results: map[string]execution.OrderResult{
		"b1": {Status: "cancelled", FilledPrice: 9, FilledQty: 3},
		"b2": {Status: "cancelled", FilledPrice: 8, FilledQty: 2},
	}}

	ReconcileExitOrderFills(context.Background(), ex, db, 0)

	if got := db.tradeQty[p1.TradeID]; got != 5 {
		t.Fatalf("remaining quantity = %v, want both terminal fills deducted to 5", got)
	}
}

func TestExitReconcileFilledResizeStopLeavesRemainderOpen(t *testing.T) {
	p := lacExit()
	p.TradeStatus = "exiting"
	p.Quantity = 10
	p.OrderQuantity = 5
	db := newFakeExitStore(p)
	ex := &fakeExitExecutor{results: map[string]execution.OrderResult{
		"b1": {Status: "filled", FilledPrice: 9, FilledQty: 5},
	}}

	ReconcileExitOrderFills(context.Background(), ex, db, 0)

	if got := db.tradeQty[p.TradeID]; got != 5 {
		t.Fatalf("remaining quantity = %v, want 5 after older stop filled", got)
	}
	if _, closed := db.tradeExits[p.TradeID]; closed {
		t.Fatal("partial-size stop fill closed the entire larger trade")
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

func TestExitReconcileAppliesDurableFillAfterPurgedTerminalOrder(t *testing.T) {
	p := store.PendingExitOrder{
		OrderID: "purged-row", BrokerOrderID: "purged-stop", Ticker: "AAPL",
		TradeID: "purged-trade", Direction: "long", EntryPrice: 100,
		Quantity: 10, OrderQuantity: 10, RecordedFill: 4, RecordedPrice: 98,
		OrderStatus: "filled", TradeStatus: "exiting",
	}
	db := newFakeExitStore(p)
	ex := &fakeExitExecutor{errs: map[string]error{
		"purged-stop": execution.ErrOrderNotFound,
	}}

	stats := ReconcileExitOrderFills(context.Background(), ex, db, 0)

	if stats.NotFound != 1 || stats.DurableRecovered != 1 || stats.Errors != 0 {
		t.Fatalf("stats = %+v, want one not-found and one durable recovery", stats)
	}
	if got := db.tradeQty[p.TradeID]; got != 6 {
		t.Fatalf("remaining trade quantity = %.6f, want 6 after durable fill", got)
	}
	if got := db.appliedFill[p.OrderID]; got != 4 {
		t.Fatalf("applied durable fill = %.6f, want 4", got)
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

func TestOrderlessRecoveryDoesNotGuessBetweenMultipleWorkingOrders(t *testing.T) {
	t1 := store.Trade{
		ID: "t1", Ticker: "AAPL", Direction: "long", Status: "exiting",
		EntryPrice: 100, Quantity: 5,
	}
	t2 := t1
	t2.ID = "t2"
	db := &recoveryExitStore{
		fakeExitStore: newFakeExitStore(),
		orderless:     []store.Trade{t1, t2},
		owned:         []store.Trade{t1, t2},
	}
	ex := &recoveryExitExecutor{
		fakeExitExecutor: &fakeExitExecutor{},
		positions: []execution.Position{
			{Ticker: "AAPL", Quantity: 10, Side: "long"},
		},
		working: []execution.WorkingOrder{
			{OrderID: "w1", Ticker: "AAPL", Side: "sell", Status: "pending"},
			{OrderID: "w2", Ticker: "AAPL", Side: "sell", Status: "pending"},
		},
	}

	stats := ReconcileExitOrderFills(context.Background(), ex, db, 0)

	if len(db.inserted) != 0 || len(db.released) != 0 {
		t.Fatalf("ambiguous recovery mutated state: inserted=%v released=%v",
			db.inserted, db.released)
	}
	if stats.Recovered != 0 {
		t.Fatalf("stats = %+v, want no guessed recovery", stats)
	}
}

func TestOrderlessRecoveryExcludesDurableOrdersOutsideWorkBatch(t *testing.T) {
	orderless := store.Trade{
		ID: "orderless", Ticker: "AAPL", Direction: "long", Status: "exiting",
		EntryPrice: 100, Quantity: 5,
	}
	other := orderless
	other.ID = "durable"
	db := &recoveryExitStore{
		fakeExitStore: newFakeExitStore(), // current LIMIT batch is empty
		orderless:     []store.Trade{orderless},
		owned:         []store.Trade{orderless, other},
		durableOwnership: []store.DurableExitOrderOwnership{{
			BrokerOrderID: "known-outside-batch",
			Ticker:        "AAPL",
			Side:          "sell",
			TradeStatus:   "exiting",
			ClaimOwned:    true,
		}},
	}
	ex := &recoveryExitExecutor{
		fakeExitExecutor: &fakeExitExecutor{},
		positions: []execution.Position{{
			Ticker: "AAPL", Quantity: 5, Side: "long",
		}},
		working: []execution.WorkingOrder{{
			OrderID: "known-outside-batch", Ticker: "AAPL",
			Side: "sell", Status: "pending",
		}},
	}

	stats := ReconcileExitOrderFills(context.Background(), ex, db, 1)

	if len(db.inserted) != 0 || len(db.released) != 0 {
		t.Fatalf("out-of-batch durable order was misattributed: inserted=%v released=%v",
			db.inserted, db.released)
	}
	if stats.Recovered != 0 {
		t.Fatalf("stats = %+v, want no guessed recovery", stats)
	}
}

func TestOrderlessRecoveryReleasesClaimWithPreexistingProtectiveStop(t *testing.T) {
	trade := store.Trade{
		ID: "orderless", Ticker: "AAPL", Direction: "long", Status: "exiting",
		EntryPrice: 100, Quantity: 5,
	}
	db := &recoveryExitStore{
		fakeExitStore: newFakeExitStore(),
		orderless:     []store.Trade{trade},
		owned:         []store.Trade{trade},
		durableOwnership: []store.DurableExitOrderOwnership{{
			BrokerOrderID: "old-protective-stop",
			Ticker:        "AAPL",
			Side:          "sell",
			TradeStatus:   "exiting",
			ClaimOwned:    false,
		}},
	}
	ex := &recoveryExitExecutor{
		fakeExitExecutor: &fakeExitExecutor{},
		positions: []execution.Position{{
			Ticker: "AAPL", Quantity: 5, Side: "long",
		}},
		working: []execution.WorkingOrder{{
			OrderID: "old-protective-stop", Ticker: "AAPL",
			Side: "sell", Status: "pending",
		}},
	}

	stats := ReconcileExitOrderFills(context.Background(), ex, db, 1)

	if len(db.inserted) != 0 {
		t.Fatalf("pre-existing stop reconstructed as claim order: %+v", db.inserted)
	}
	if len(db.released) != 1 || db.released[0] != trade.ID {
		t.Fatalf("claim was not safely reopened: released=%v", db.released)
	}
	if stats.Recovered != 1 {
		t.Fatalf("stats = %+v, want one recovered claim", stats)
	}
}

func TestOrderlessRecoveryRetainsClaimForPreclaimPartialTerminalFill(t *testing.T) {
	trade := store.Trade{
		ID: "orderless-partial", Ticker: "AAPL", Direction: "long", Status: "exiting",
		EntryPrice: 100, Quantity: 10,
	}
	db := &recoveryExitStore{
		fakeExitStore: newFakeExitStore(store.PendingExitOrder{
			OrderID: "stop-row", BrokerOrderID: "old-partial-stop", Ticker: "AAPL",
			TradeID: trade.ID, Direction: "long", EntryPrice: 100, Quantity: 10,
			OrderQuantity: 10, RecordedFill: 4, RecordedPrice: 98,
			TradeStatus: "exiting",
		}),
		orderless: []store.Trade{trade},
		owned:     []store.Trade{trade},
		durableOwnership: []store.DurableExitOrderOwnership{{
			BrokerOrderID: "old-partial-stop", Ticker: "AAPL", Side: "sell",
			TradeStatus: "exiting", ClaimOwned: true,
		}},
	}
	ex := &recoveryExitExecutor{
		fakeExitExecutor: &fakeExitExecutor{errs: map[string]error{
			"old-partial-stop": execution.ErrOrderNotFound,
		}},
		positions: []execution.Position{{Ticker: "AAPL", Quantity: 6, Side: "long"}},
	}

	stats := ReconcileExitOrderFills(context.Background(), ex, db, 1)

	if len(db.released) != 0 || len(db.tradeExits) != 0 || len(db.inserted) != 0 {
		t.Fatalf("preclaim partial fill was recovered a second time: released=%v closed=%v inserted=%v",
			db.released, db.tradeExits, db.inserted)
	}
	if stats.NotFound != 1 || stats.Recovered != 0 {
		t.Fatalf("stats = %+v, want one not-found and no recovery", stats)
	}
}

func TestOrderlessRecoveryClosesFlatTradeAtConservativePrice(t *testing.T) {
	trade := store.Trade{
		ID: "t1", Ticker: "AAPL", Direction: "long", Status: "exiting",
		EntryPrice: 100, Quantity: 5,
	}
	db := &recoveryExitStore{
		fakeExitStore: newFakeExitStore(),
		orderless:     []store.Trade{trade},
		owned:         []store.Trade{trade},
	}
	ex := &recoveryExitExecutor{fakeExitExecutor: &fakeExitExecutor{}}

	stats := ReconcileExitOrderFills(context.Background(), ex, db, 0)

	exit, ok := db.tradeExits[trade.ID]
	if !ok {
		t.Fatal("flat orderless trade remained exiting")
	}
	if exit[0] != trade.EntryPrice || exit[1] != 0 {
		t.Fatalf("conservative exit = %v, want entry-price close with zero estimated pnl", exit)
	}
	if stats.Recovered != 1 {
		t.Fatalf("stats = %+v, want one recovery", stats)
	}
}
