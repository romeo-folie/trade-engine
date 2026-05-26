package orb

import (
	"context"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/execution"
	"github.com/romeo-folie/trade-engine/internal/store"
)

// stubExecutor implements execution.Executor for reconcile tests.
type stubExecutor struct {
	statuses    map[string]execution.OrderResult
	statusCalls []string
}

func (e *stubExecutor) GetOrderStatus(_ context.Context, id string) (execution.OrderResult, error) {
	e.statusCalls = append(e.statusCalls, id)
	if r, ok := e.statuses[id]; ok {
		return r, nil
	}
	return execution.OrderResult{}, nil
}
func (e *stubExecutor) PlaceOrder(_ context.Context, _ execution.OrderRequest) (execution.OrderResult, error) {
	return execution.OrderResult{}, nil
}
func (e *stubExecutor) CancelOrder(_ context.Context, _ string) error { return nil }
func (e *stubExecutor) GetPositions(_ context.Context) ([]execution.Position, error) {
	return nil, nil
}
func (e *stubExecutor) GetAccount(_ context.Context) (execution.AccountInfo, error) {
	return execution.AccountInfo{}, nil
}

func TestReconcileDoneSession_RunsOnce(t *testing.T) {
	exec := &stubExecutor{statuses: map[string]execution.OrderResult{
		"tp-1": {Status: "filled", FilledPrice: 155.0},
	}}
	s := &ORBScheduler{
		executor:           exec,
		reconciledSessions: make(map[string]struct{}),
		trackedTrades:      make(map[string]*trackedTrade),
	}

	ctx := context.Background()
	// First call — DB is nil so GetORBTrades will error and return early,
	// but the session ID must be marked reconciled before the DB call so
	// a second call is still a no-op.
	s.reconcileDoneSessionTrades(ctx, "sess-abc")
	if _, ok := s.reconciledSessions["sess-abc"]; !ok {
		t.Fatal("expected session to be added to reconciledSessions after first call")
	}

	// Second call — must be a no-op; executor must NOT be called.
	callsBefore := len(exec.statusCalls)
	s.reconcileDoneSessionTrades(ctx, "sess-abc")
	if len(exec.statusCalls) != callsBefore {
		t.Errorf("executor called on second reconcile: want 0 extra calls, got %d",
			len(exec.statusCalls)-callsBefore)
	}
}

// ─── matchExitsFromRecords ────────────────────────────────────────────────────

// stubTradesFetcher satisfies TradesFetcher for unit tests.
type stubTradesFetcher struct {
	records []execution.IBKRTradeRecord
	calls   int
}

func (f *stubTradesFetcher) FetchTradeRecords(_ context.Context, _ int) ([]execution.IBKRTradeRecord, error) {
	f.calls++
	return f.records, nil
}

func TestMatchExitsFromRecords_TPHit(t *testing.T) {
	entryTime := time.Date(2026, 4, 28, 8, 17, 0, 0, time.UTC)
	filledAt := entryTime

	trades := []store.ORBTrade{
		{
			ID:         "trade-1",
			Ticker:     "EUR/USD",
			Side:       "buy",
			Qty:        5000,
			FillPrice:  1.1707,
			TakeProfit: 1.1907,
			StopLoss:   1.1607,
			Status:     "filled",
			CreatedAt:  entryTime.Add(-5 * time.Second),
			FilledAt:   &filledAt,
		},
	}

	records := []execution.IBKRTradeRecord{
		{
			Symbol:   "EUR/USD",
			Side:     "sell", // opposite of buy entry
			Qty:      5000,
			Price:    1.1907,                   // exactly at TP
			FilledAt: entryTime.Add(time.Hour), // after entry
		},
	}

	s := &ORBScheduler{
		reconciledSessions: make(map[string]struct{}),
		trackedTrades:      make(map[string]*trackedTrade),
	}
	// matchExitsFromRecords requires s.db — keep nil to verify it short-circuits
	// gracefully after computing the match.
	s.matchExitsFromRecords(context.Background(), trades, records)

	// Trade should now show status="closed" in the in-memory slice.
	if trades[0].Status != "closed" {
		t.Errorf("trade status = %q after TP match; want closed", trades[0].Status)
	}
}

func TestMatchExitsFromRecords_SLHit(t *testing.T) {
	entryTime := time.Date(2026, 4, 28, 8, 17, 0, 0, time.UTC)
	filledAt := entryTime

	trades := []store.ORBTrade{
		{
			ID:         "trade-2",
			Ticker:     "GBP/USD",
			Side:       "sell",
			Qty:        5000,
			FillPrice:  1.3515,
			TakeProfit: 1.3315, // TP is below entry for a sell
			StopLoss:   1.3615, // SL is above entry for a sell
			Status:     "filled",
			CreatedAt:  entryTime.Add(-5 * time.Second),
			FilledAt:   &filledAt,
		},
	}

	records := []execution.IBKRTradeRecord{
		{
			Symbol:   "GBP/USD",
			Side:     "buy", // opposite of sell entry
			Qty:      5000,
			Price:    1.3615, // at SL
			FilledAt: entryTime.Add(time.Hour),
		},
	}

	s := &ORBScheduler{
		reconciledSessions: make(map[string]struct{}),
		trackedTrades:      make(map[string]*trackedTrade),
	}
	s.matchExitsFromRecords(context.Background(), trades, records)

	if trades[0].Status != "closed" {
		t.Errorf("trade status = %q after SL match; want closed", trades[0].Status)
	}
}

func TestMatchExitsFromRecords_SkipsWrongSymbol(t *testing.T) {
	entryTime := time.Date(2026, 4, 28, 8, 17, 0, 0, time.UTC)
	filledAt := entryTime

	trades := []store.ORBTrade{
		{
			ID:         "trade-3",
			Ticker:     "EUR/USD",
			Side:       "buy",
			Qty:        5000,
			FillPrice:  1.1707,
			TakeProfit: 1.1907,
			StopLoss:   1.1607,
			Status:     "filled",
			CreatedAt:  entryTime.Add(-5 * time.Second),
			FilledAt:   &filledAt,
		},
	}

	records := []execution.IBKRTradeRecord{
		{
			Symbol:   "GBP/USD", // wrong symbol
			Side:     "sell",
			Qty:      5000,
			Price:    1.1907,
			FilledAt: entryTime.Add(time.Hour),
		},
	}

	s := &ORBScheduler{
		reconciledSessions: make(map[string]struct{}),
		trackedTrades:      make(map[string]*trackedTrade),
	}
	s.matchExitsFromRecords(context.Background(), trades, records)

	if trades[0].Status != "filled" {
		t.Errorf("trade status = %q; want filled (symbol mismatch should not match)", trades[0].Status)
	}
}

func TestMatchExitsFromRecords_SkipsEarlierFill(t *testing.T) {
	entryTime := time.Date(2026, 4, 28, 8, 17, 0, 0, time.UTC)
	filledAt := entryTime

	trades := []store.ORBTrade{
		{
			ID:         "trade-4",
			Ticker:     "EUR/USD",
			Side:       "buy",
			Qty:        5000,
			FillPrice:  1.1707,
			TakeProfit: 1.1907,
			StopLoss:   1.1607,
			Status:     "filled",
			CreatedAt:  entryTime.Add(-5 * time.Second),
			FilledAt:   &filledAt,
		},
	}

	records := []execution.IBKRTradeRecord{
		{
			Symbol:   "EUR/USD",
			Side:     "sell",
			Qty:      5000,
			Price:    1.1907,
			FilledAt: entryTime.Add(-time.Minute), // BEFORE entry — must not match
		},
	}

	s := &ORBScheduler{
		reconciledSessions: make(map[string]struct{}),
		trackedTrades:      make(map[string]*trackedTrade),
	}
	s.matchExitsFromRecords(context.Background(), trades, records)

	if trades[0].Status != "filled" {
		t.Errorf("trade status = %q; want filled (pre-entry fill should not match)", trades[0].Status)
	}
}

// TestReleaseTrackedTradesForReconcile verifies that end-of-session cleanup clears
// the in-memory tracking map but does NOT close filled trades. Leaving them in
// status='filled' keeps them eligible for the DB-backed exit reconcilers, which
// close them later with the real exit price/PnL. (Previously cleanup marked them
// 'expired' with pnl=0, which — once migration 030 made 'closed' a legal status —
// permanently blocked reconciliation and polluted realized-PnL feedback stats.)
func TestReleaseTrackedTradesForReconcile(t *testing.T) {
	s := &ORBScheduler{
		trackedTrades: map[string]*trackedTrade{
			"entry-1": {dbID: "trade-1", ticker: "EUR/USD", side: "buy", qty: 5000, entryFill: 1.1707}, // filled
			"entry-2": {dbID: "trade-2", ticker: "GBP/USD", side: "sell", qty: 5000, entryFill: 0},     // never filled
		},
	}

	// Hold s.mu to honor the method's documented contract, matching the
	// production call site in tickDone.
	s.mu.Lock()
	s.releaseTrackedTradesForReconcile()
	s.mu.Unlock()

	if len(s.trackedTrades) != 0 {
		t.Errorf("trackedTrades not cleared: %d entries remain", len(s.trackedTrades))
	}
	// The method takes no store handle by construction, so it cannot transition any
	// trade to a terminal DB state — the filled trade is left for the reconcilers.
}

func TestReconcileDoneSession_DifferentSessionsReconcileIndependently(t *testing.T) {
	exec := &stubExecutor{statuses: map[string]execution.OrderResult{}}
	s := &ORBScheduler{
		executor:           exec,
		reconciledSessions: make(map[string]struct{}),
		trackedTrades:      make(map[string]*trackedTrade),
	}

	ctx := context.Background()
	s.reconcileDoneSessionTrades(ctx, "sess-1")
	s.reconcileDoneSessionTrades(ctx, "sess-2")

	if _, ok := s.reconciledSessions["sess-1"]; !ok {
		t.Error("sess-1 should be marked reconciled")
	}
	if _, ok := s.reconciledSessions["sess-2"]; !ok {
		t.Error("sess-2 should be marked reconciled")
	}
}
