package tui

import (
	"context"
	"testing"

	"github.com/romeo-folie/trade-engine/internal/execution"
)

// stubBrokerView records whether the broker was queried; all methods are no-ops.
type stubBrokerView struct{ positionsCalled bool }

func (s *stubBrokerView) AccountSnapshot(context.Context) (execution.BrokerAccount, error) {
	return execution.BrokerAccount{}, nil
}
func (s *stubBrokerView) PositionsSnapshot(context.Context) ([]execution.BrokerPosition, error) {
	s.positionsCalled = true
	return nil, nil
}
func (s *stubBrokerView) EquityCurve(context.Context, string, string) (execution.EquityHistory, error) {
	return execution.EquityHistory{}, nil
}
func (s *stubBrokerView) RecentFills(context.Context) ([]execution.BrokerOrder, error) {
	return nil, nil
}

// Without a live DB connection, fetchBrokerRecon must return an unavailable
// result rather than panicking in store.GetOpenTrades (regression for the
// crash Codex flagged on #401).
func TestFetchBrokerRecon_NoDBDoesNotPanic(t *testing.T) {
	stub := &stubBrokerView{}
	msg := fetchBrokerRecon(stub)() // store is not connected in unit tests
	recon, ok := msg.(brokerReconMsg)
	if !ok {
		t.Fatalf("expected brokerReconMsg, got %T", msg)
	}
	if recon.result.Available {
		t.Error("expected result to be unavailable when DB is not connected")
	}
	if recon.result.Err == "" {
		t.Error("expected an error message explaining why reconciliation was skipped")
	}
	if stub.positionsCalled {
		t.Error("broker should not be queried when the DB is unavailable")
	}
}
