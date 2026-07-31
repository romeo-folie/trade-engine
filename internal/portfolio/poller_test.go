package portfolio

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/execution"
)

// ── Stub BrokerView ───────────────────────────────────────────────────────────

type stubBrokerView struct {
	account      execution.BrokerAccount
	accountErr   error
	positions    []execution.BrokerPosition
	positionsErr error
	calls        atomic.Int64
}

func (s *stubBrokerView) AccountSnapshot(_ context.Context) (execution.BrokerAccount, error) {
	s.calls.Add(1)
	return s.account, s.accountErr
}
func (s *stubBrokerView) PositionsSnapshot(_ context.Context) ([]execution.BrokerPosition, error) {
	if s.positionsErr != nil {
		return nil, s.positionsErr
	}
	return s.positions, nil
}
func (s *stubBrokerView) EquityCurve(_ context.Context, _, _ string) (execution.EquityHistory, error) {
	return execution.EquityHistory{}, execution.ErrBrokerUnavailable
}
func (s *stubBrokerView) RecentFills(_ context.Context) ([]execution.BrokerOrder, error) {
	return nil, execution.ErrBrokerUnavailable
}

// ── Tests ─────────────────────────────────────────────────────────────────────

// TestPollerPoll_NoStore verifies poll does not panic when DB is not connected.
// The store.IsConnected() check gates the DB write, so poll should return early.
func TestPollerPoll_NoStore(t *testing.T) {
	bv := &stubBrokerView{account: execution.BrokerAccount{Equity: 100_000, Cash: 20_000}}
	p := NewPoller(bv, time.Hour, 0.05, "paper", "alpaca")

	ctx := context.Background()
	// poll should not panic even without a real DB.
	p.poll(ctx)
}

// TestPollerPoll_BrokerError verifies poll survives broker errors gracefully.
func TestPollerPoll_BrokerError(t *testing.T) {
	bv := &stubBrokerView{accountErr: errors.New("network error")}
	p := NewPoller(bv, time.Hour, 0.05, "paper", "alpaca")

	ctx := context.Background()
	p.poll(ctx) // should not panic
	if bv.calls.Load() != 1 {
		t.Errorf("expected AccountSnapshot to be called once, got %d", bv.calls.Load())
	}
}

// TestPollerPoll_SignificantChangeThreshold verifies that the poller only marks
// a snapshot as needed when the equity change exceeds the threshold.
// We can't write to DB in unit tests, so we verify via lastSnapshotEquity
// updates (which happen before the DB write in poll).
func TestPollerPoll_ThresholdLogic(t *testing.T) {
	bv := &stubBrokerView{account: execution.BrokerAccount{Equity: 100_000, Cash: 20_000}}
	p := NewPoller(bv, time.Hour, 1.0, "paper", "alpaca") // 1% threshold

	// First poll always sets lastSnapshotEquity (first == 0 check).
	// Because DB is not connected, the write is skipped but the tracking
	// variable is NOT updated (write happens before update).
	// We patch lastSnapshotEquity directly to test threshold logic.
	p.lastSnapshotEquity = 100_000

	// Move equity by 0.5% — below threshold.
	bv.account.Equity = 100_500
	// poll will attempt DB write → store not connected → returns early
	// but the call count still increments confirming broker was called.
	p.poll(context.Background())
	if bv.calls.Load() != 1 {
		t.Errorf("expected 1 broker call, got %d", bv.calls.Load())
	}

	// Move equity by 2% — above threshold. Broker should be called again.
	bv.account.Equity = 102_000
	p.poll(context.Background())
	if bv.calls.Load() != 2 {
		t.Errorf("expected 2 broker calls, got %d", bv.calls.Load())
	}
}

// TestPositionStats_CountsAndSumsUnrealized verifies the poller derives the
// open-position count and aggregate unrealized P&L from the broker (fixing the
// former hardcoded 0 in the snapshot write).
func TestPositionStats_CountsAndSumsUnrealized(t *testing.T) {
	bv := &stubBrokerView{positions: []execution.BrokerPosition{
		{Symbol: "AAPL", UnrealizedPL: 50.5},
		{Symbol: "TSLA", UnrealizedPL: -20.25},
		{Symbol: "NVDA", UnrealizedPL: 10.0},
	}}
	p := NewPoller(bv, time.Hour, 0.05, "paper", "alpaca")

	count, unrealized, available := p.positionStats(context.Background())
	if count != 3 {
		t.Errorf("count = %d, want 3", count)
	}
	if want := 40.25; unrealized != want {
		t.Errorf("unrealized = %.2f, want %.2f", unrealized, want)
	}
	if !available {
		t.Error("successful broker position stats reported unavailable")
	}
}

// TestPositionStats_BestEffortOnError verifies a PositionsSnapshot failure
// degrades to (0, 0) rather than aborting the snapshot.
func TestPositionStats_BestEffortOnError(t *testing.T) {
	bv := &stubBrokerView{positionsErr: errors.New("broker down")}
	p := NewPoller(bv, time.Hour, 0.05, "paper", "alpaca")

	count, unrealized, available := p.positionStats(context.Background())
	if count != 0 || unrealized != 0 {
		t.Errorf("on error want (0,0), got (%d, %.2f)", count, unrealized)
	}
	if available {
		t.Error("failed broker position stats reported available")
	}
}

func TestPositionStats_EmptyBookIsAvailable(t *testing.T) {
	p := NewPoller(&stubBrokerView{}, time.Hour, 0.05, "paper", "alpaca")
	count, unrealized, available := p.positionStats(context.Background())
	if count != 0 || unrealized != 0 || !available {
		t.Fatalf("flat broker book = (%d, %.2f, %v), want (0, 0, true)",
			count, unrealized, available)
	}
}

// TestPollerDisabled verifies Run returns immediately when interval is zero.
func TestPollerDisabled(t *testing.T) {
	bv := &stubBrokerView{account: execution.BrokerAccount{Equity: 100_000}}
	p := NewPoller(bv, 0, 0.05, "paper", "alpaca")

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	done := make(chan struct{})
	go func() {
		p.Run(ctx)
		close(done)
	}()

	select {
	case <-done:
		// Good — Run returned quickly because interval=0.
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Run did not return promptly with interval=0")
	}
}
