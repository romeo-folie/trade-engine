package engine

import (
	"context"
	"errors"
	"math"
	"sync/atomic"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/execution"
	"github.com/romeo-folie/trade-engine/internal/store"
)

// fakeExitExecutor serves canned GetOrderStatus responses per broker order id.
type fakeExitExecutor struct {
	results map[string]execution.OrderResult
	errs    map[string]error
	calls   []string
}

func TestExitReconcileTickerRunsWithoutOverlapAndStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var calls, active, maxActive atomic.Int32
	done := make(chan struct{})
	go func() {
		runExitReconcileTicker(ctx, 2*time.Millisecond, func(context.Context) {
			current := active.Add(1)
			for {
				previous := maxActive.Load()
				if current <= previous || maxActive.CompareAndSwap(previous, current) {
					break
				}
			}
			calls.Add(1)
			time.Sleep(6 * time.Millisecond)
			active.Add(-1)
		})
		close(done)
	}()

	deadline := time.After(250 * time.Millisecond)
	for calls.Load() < 2 {
		select {
		case <-deadline:
			t.Fatal("independent reconcile ticker did not run twice")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("reconcile ticker did not stop after cancellation")
	}
	if maxActive.Load() != 1 {
		t.Fatalf("max concurrent sweeps = %d, want 1", maxActive.Load())
	}
}

func TestExitReconcilePhasesHaveIndependentDeadlines(t *testing.T) {
	claimedRan := false
	stats := runExitReconcilePhases(
		context.Background(),
		5*time.Millisecond,
		func(ctx context.Context) int {
			<-ctx.Done()
			return 1
		},
		func(ctx context.Context) ExitReconcileStats {
			claimedRan = true
			if err := ctx.Err(); err != nil {
				t.Fatalf("claimed phase inherited expired leg context: %v", err)
			}
			return ExitReconcileStats{Scanned: 1}
		},
	)
	if !claimedRan || stats.Scanned != 1 || stats.Errors != 1 {
		t.Fatalf("independent phases = called %v stats %+v", claimedRan, stats)
	}
}

func TestStartupExitReconcilePreventsFalseBrokerDrift(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("DATABASE_URL not set; skipping DB-backed engine test")
	}
	ctx := context.Background()
	tradeID, err := store.InsertTrade(ctx, store.Trade{
		Ticker: "TEST_BOOT_EXIT_DRIFT", Direction: "long", Strategy: "test",
		EntryPrice: 100, Quantity: 5,
	})
	if err != nil {
		t.Fatalf("InsertTrade: %v", err)
	}
	if claimed, err := store.ClaimTradeExitWithIntent(ctx, tradeID, store.ExitIntent{
		Reason: store.ExitReasonEMA9Close, Evidence: []byte(`{"daily_close":90}`),
	}); err != nil || !claimed {
		t.Fatalf("ClaimTradeExitWithIntent = %v, %v", claimed, err)
	}
	if _, err := store.GetDB().Exec(ctx, `UPDATE trades SET exit_claimed_at=NOW()-INTERVAL '30 minutes' WHERE id=$1`, tradeID); err != nil {
		t.Fatalf("age exit claim: %v", err)
	}
	if _, err := store.InsertOrder(ctx, store.Order{
		TradeID: tradeID, BrokerOrderID: "boot-exit-fill", Ticker: "TEST_BOOT_EXIT_DRIFT",
		Side: "sell", OrderType: "market", Quantity: 5, Status: "pending",
	}); err != nil {
		t.Fatalf("InsertOrder: %v", err)
	}
	exec := &fakeBracketExec{statuses: map[string]execution.OrderResult{
		"boot-exit-fill": {Status: "filled", FilledPrice: 90, FilledQty: 5},
	}}
	eng := newBracketTestEngine(exec)

	eng.ReconcileExitsNow(ctx)
	drifts, ok := ReconcileBrokerVsDBWithStoreStatus(ctx, &fakeBrokerView{}, 0)
	if !ok {
		t.Fatal("post-exit startup reconciliation did not complete")
	}
	for _, drift := range drifts {
		if drift.Ticker == "TEST_BOOT_EXIT_DRIFT" {
			t.Fatalf("resolved startup exit still drifted: %+v", drift)
		}
	}
	trade, err := store.GetTradeByID(ctx, tradeID)
	if err != nil || trade == nil || trade.Status != "closed" {
		t.Fatalf("startup exit trade = %+v err=%v, want closed", trade, err)
	}
}

func TestStartupExitReconcileBooksProtectiveStopBeforeBrokerDrift(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("DATABASE_URL not set; skipping DB-backed engine test")
	}
	ctx := context.Background()
	tradeID, err := store.InsertTrade(ctx, store.Trade{
		Ticker: "TEST_BOOT_STOP_DRIFT", Direction: "long", Strategy: "test",
		EntryPrice: 100, Quantity: 5, StopLoss: 95, TakeProfit: 0, Status: "open",
	})
	if err != nil {
		t.Fatalf("InsertTrade: %v", err)
	}
	if _, err := store.InsertOrder(ctx, store.Order{
		TradeID: tradeID, BrokerOrderID: "boot-protective-stop", Ticker: "TEST_BOOT_STOP_DRIFT",
		Side: "sell", OrderType: "stop", Quantity: 5, StopPrice: 95,
		Status: "pending", IsStopLoss: true,
	}); err != nil {
		t.Fatalf("InsertOrder: %v", err)
	}
	filledAt := time.Date(2026, 8, 10, 19, 56, 27, 0, time.UTC)
	exec := &fakeBracketExec{statuses: map[string]execution.OrderResult{
		"boot-protective-stop": {
			Status: "filled", FilledPrice: 94.75, FilledQty: 5, FilledAt: filledAt,
		},
	}}
	eng := newBracketTestEngine(exec)

	// This call is deliberately independent of market hours. cmd/te/run.go
	// invokes it synchronously before its broker-vs-DB comparison.
	eng.ReconcileExitsNow(ctx)
	drifts, ok := ReconcileBrokerVsDBWithStoreStatus(ctx, &fakeBrokerView{}, 0)
	if !ok {
		t.Fatal("post-stop startup reconciliation did not complete")
	}
	for _, drift := range drifts {
		if drift.Ticker == "TEST_BOOT_STOP_DRIFT" {
			t.Fatalf("broker-filled protective stop still drifted: %+v", drift)
		}
	}
	trade, err := store.GetTradeByID(ctx, tradeID)
	if err != nil || trade == nil || trade.Status != "closed" {
		t.Fatalf("protective-stop trade = %+v err=%v, want closed", trade, err)
	}
	orders, err := store.GetOrdersByTrade(ctx, tradeID)
	if err != nil || len(orders) != 1 {
		t.Fatalf("GetOrdersByTrade = %+v, %v", orders, err)
	}
	if orders[0].Status != "filled" || orders[0].FilledAt == nil || !orders[0].FilledAt.Equal(filledAt) {
		t.Fatalf("protective stop = status %q filled_at %s, want filled at %s",
			orders[0].Status, orders[0].FilledAt, filledAt)
	}
}

func TestReconcileExitsNowWithoutDBDoesNotPanic(t *testing.T) {
	if store.IsConnected() {
		t.Skip("requires disconnected store")
	}
	eng := newBracketTestEngine(&fakeBracketExec{})
	eng.ReconcileExitsNow(context.Background())
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
	orderFillAt  map[string]time.Time
	tradeFillAt  map[string]time.Time
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
		TradeID: trade.ID, Ticker: trade.Ticker, Direction: trade.Direction,
		EntryPrice: trade.EntryPrice, ExitPrice: exitPrice,
		PnL: pnl, PnLPct: pnlPct, ExitQty: soldQty,
		Remaining: remaining, Status: "open",
		ExitReason: trade.ExitReason, ExitEvidence: trade.ExitEvidence,
		ExitFilledAt: trade.ExitFilledAt,
	}
	if remaining > 0 {
		f.tradeQty[id] = remaining
		f.released = append(f.released, id)
		return result, nil
	}
	result.Status = "closed"
	result.JustClosed = true
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
		orderFillAt: map[string]time.Time{},
		tradeFillAt: map[string]time.Time{},
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
func (f *fakeExitStore) UpdateOrderFillAt(ctx context.Context, id string, price, qty float64, status string, filledAt time.Time) error {
	if err := f.UpdateOrderFill(ctx, id, price, qty, status); err != nil {
		return err
	}
	if filledAt.After(f.orderFillAt[id]) {
		f.orderFillAt[id] = filledAt
	}
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
func (f *fakeExitStore) ApplyPartialExitAt(
	ctx context.Context, tradeID, orderID string, price, cumulativeQty float64,
	status string, filledAt time.Time,
) (float64, float64, error) {
	remaining, applied, err := f.ApplyPartialExit(ctx, tradeID, orderID, price, cumulativeQty, status)
	if err == nil && filledAt.After(f.orderFillAt[orderID]) {
		f.orderFillAt[orderID] = filledAt
	}
	if err == nil && filledAt.After(f.tradeFillAt[tradeID]) {
		f.tradeFillAt[tradeID] = filledAt
	}
	return remaining, applied, err
}
func (f *fakeExitStore) UpdateTradeExit(_ context.Context, id string, exit, pnl, pnlPct float64) error {
	if f.tradeExitErr != nil {
		return f.tradeExitErr
	}
	f.tradeExits[id] = [3]float64{exit, pnl, pnlPct}
	return nil
}
func (f *fakeExitStore) UpdateTradeExitAt(ctx context.Context, id string, exit, pnl, pnlPct float64, filledAt time.Time) error {
	if err := f.UpdateTradeExit(ctx, id, exit, pnl, pnlPct); err != nil {
		return err
	}
	if filledAt.After(f.tradeFillAt[id]) {
		f.tradeFillAt[id] = filledAt
	}
	return nil
}
func (f *fakeExitStore) UpdateTradeExitFilledAt(_ context.Context, id string, filledAt time.Time) error {
	if filledAt.After(f.tradeFillAt[id]) {
		f.tradeFillAt[id] = filledAt
	}
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
	realized := store.TradeRealization{
		TradeID:   id,
		ExitPrice: exit,
		PnL:       pnl,
		PnLPct:    pnlPct,
		Status:    "closed", JustClosed: true,
	}
	for _, p := range f.pending {
		if p.TradeID == id {
			realized.Ticker, realized.Direction, realized.EntryPrice = p.Ticker, p.Direction, p.EntryPrice
			realized.ExitReason, realized.ExitEvidence = p.ExitReason, p.ExitEvidence
			break
		}
	}
	if filledAt := f.tradeFillAt[id]; !filledAt.IsZero() {
		realized.ExitFilledAt = &filledAt
	}
	return realized, nil
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
	p.ExitReason = store.ExitReasonEMA9Close
	p.ExitEvidence = []byte(`{"daily_close":3.82,"ema9":4.10}`)
	db := newFakeExitStore(p)
	filledAt := time.Date(2026, 8, 1, 14, 5, 2, 0, time.UTC)
	ex := &fakeExitExecutor{results: map[string]execution.OrderResult{
		"b1": {Status: "filled", FilledPrice: 3.82, FilledQty: p.Quantity, FilledAt: filledAt},
	}}
	losses := 0

	stats := ReconcileExitOrderFills(
		context.Background(), ex, db, 0, func() { losses++ },
	)

	if stats.Corrected != 1 || losses != 1 {
		t.Fatalf("stats=%+v losses=%d, want finalized loss", stats, losses)
	}
	if len(stats.Closed) != 1 {
		t.Fatalf("closed events = %d, want one newly finalized trade", len(stats.Closed))
	}
	closed := stats.Closed[0]
	if closed.TradeID != p.TradeID || closed.Ticker != p.Ticker ||
		closed.ExitReason != store.ExitReasonEMA9Close || string(closed.ExitEvidence) != string(p.ExitEvidence) ||
		closed.ExitFilledAt == nil || !closed.ExitFilledAt.Equal(filledAt) || !closed.JustClosed {
		t.Fatalf("reconciled close metadata = %+v evidence=%s", closed, closed.ExitEvidence)
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
	filledAt := time.Date(2026, 8, 1, 14, 5, 2, 0, time.UTC)
	p := store.PendingExitOrder{
		OrderID: "purged-row", BrokerOrderID: "purged-stop", Ticker: "AAPL",
		TradeID: "purged-trade", Direction: "long", EntryPrice: 100,
		Quantity: 10, OrderQuantity: 10, RecordedFill: 4, RecordedPrice: 98,
		RecordedFilledAt: &filledAt, OrderStatus: "filled", TradeStatus: "exiting",
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
	if got := db.tradeFillAt[p.TradeID]; !got.Equal(filledAt) {
		t.Fatalf("durable fill timestamp = %v, want %v", got, filledAt)
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

func TestOrderlessEMARecoveryDoesNotMarkMarketExitAsStopLoss(t *testing.T) {
	trade := store.Trade{
		ID: "t1", Ticker: "AAPL", Direction: "long", Status: "exiting",
		EntryPrice: 100, Quantity: 5, ExitReason: store.ExitReasonEMA9Close,
	}
	db := &recoveryExitStore{
		fakeExitStore: newFakeExitStore(), orderless: []store.Trade{trade}, owned: []store.Trade{trade},
	}
	ex := &recoveryExitExecutor{
		fakeExitExecutor: &fakeExitExecutor{},
		positions:        []execution.Position{{Ticker: "AAPL", Quantity: 5, Side: "long"}},
		working:          []execution.WorkingOrder{{OrderID: "ema-exit", Ticker: "AAPL", Side: "sell", Status: "pending"}},
	}

	stats := ReconcileExitOrderFills(context.Background(), ex, db, 0)
	if stats.Recovered != 1 || len(db.inserted) != 1 {
		t.Fatalf("stats=%+v inserted=%+v, want reconstructed EMA order", stats, db.inserted)
	}
	if db.inserted[0].IsStopLoss || db.inserted[0].IsTakeProfit {
		t.Fatalf("EMA reconstructed order has bracket flags: %+v", db.inserted[0])
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
		EntryPrice: 100, Quantity: 5, ExitReason: store.ExitReasonSellSignal,
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
	if len(stats.Closed) != 1 || stats.Closed[0].ExitReason != store.ExitReasonSellSignal {
		t.Fatalf("recovered close events = %+v, want durable sell reason", stats.Closed)
	}
}
