package orb

import (
	"context"
	"testing"

	"github.com/romeo-folie/trade-engine/internal/execution"
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
