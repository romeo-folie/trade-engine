package tui

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/events"
	"github.com/romeo-folie/trade-engine/internal/execution"
	"github.com/romeo-folie/trade-engine/internal/tui/views"
)

// stubBrokerView records whether the broker was queried; all methods are no-ops.
type stubBrokerView struct {
	positionsCalled bool
	accountErr      error
}

func (s *stubBrokerView) AccountSnapshot(context.Context) (execution.BrokerAccount, error) {
	return execution.BrokerAccount{Equity: 100000, Cash: 90000}, s.accountErr
}

func TestFetchBrokerRecon_AccountFailureRejectsPartialSnapshot(t *testing.T) {
	stub := &stubBrokerView{accountErr: errors.New("account unavailable")}
	msg := fetchBrokerRecon(stub, false, 0)().(brokerReconMsg)
	if msg.snapshot != nil {
		t.Fatal("positions-only response must not become an authoritative snapshot")
	}
	if msg.result.Err == "" {
		t.Fatal("account failure must be surfaced")
	}
}

func TestBrokerReconFailureClearsPriorSnapshot(t *testing.T) {
	prior := &views.BrokerDashboardSnapshot{
		Account: &execution.BrokerAccount{Equity: 100000},
	}
	m := Model{mode: "paper", brokerSnapshot: prior}
	next, _ := m.Update(brokerReconMsg{
		result: views.BrokerReconResult{Err: "broker unavailable"},
	})
	updated := next.(Model)
	if updated.brokerSnapshot != nil {
		t.Fatal("failed refresh must clear the prior authoritative broker snapshot")
	}
}

func TestBrokerReconSnapshotRetainedUntilExposureEvent(t *testing.T) {
	account := execution.BrokerAccount{Equity: 100000, Cash: 90000}
	m := Model{mode: "paper", brokerView: &stubBrokerView{}}
	next, _ := m.Update(brokerReconMsg{
		result: views.BrokerReconResult{Available: true, InSync: true},
		snapshot: &views.BrokerDashboardSnapshot{
			Account: &account, FetchedAt: time.Now(),
		},
	})
	updated := next.(Model)
	if updated.brokerSnapshot == nil {
		t.Fatal("broker snapshot not retained across routine polls")
	}
	if !updated.dashData.BrokerAuthoritative || updated.dashData.TotalValue != account.Equity {
		t.Fatalf("broker snapshot was not applied to reconciliation refresh: %+v", updated.dashData)
	}

	next, cmd := updated.Update(eventMsg(events.Event{Type: events.TradeClosed}))
	updated = next.(Model)
	if updated.brokerSnapshot != nil {
		t.Fatal("exposure-changing event did not invalidate broker snapshot")
	}
	if cmd == nil {
		t.Fatal("exposure-changing event did not request a replacement broker snapshot")
	}
}

func TestSignalEventDoesNotInvalidateBrokerSnapshot(t *testing.T) {
	account := execution.BrokerAccount{Equity: 100000, Cash: 90000}
	snapshot := &views.BrokerDashboardSnapshot{Account: &account, FetchedAt: time.Now()}
	m := Model{mode: "paper", brokerSnapshot: snapshot}

	next, _ := m.Update(eventMsg(events.Event{Type: events.SignalGenerated}))
	if updated := next.(Model); updated.brokerSnapshot != snapshot {
		t.Fatal("signal-only event invalidated broker snapshot")
	}
}

func TestSupersededBrokerResponseIsIgnored(t *testing.T) {
	account := execution.BrokerAccount{Equity: 101000, Cash: 90000}
	current := &views.BrokerDashboardSnapshot{Account: &account, FetchedAt: time.Now()}
	m := Model{
		mode: "paper", brokerSnapshot: current, brokerReconGeneration: 2,
	}

	next, _ := m.Update(brokerReconMsg{
		result: views.BrokerReconResult{Err: "older request failed"},
		// An older failure must not clear the newer successful book.
		generation: 1,
	})
	updated := next.(Model)
	if updated.brokerSnapshot != current {
		t.Fatal("superseded broker response replaced current snapshot")
	}
}

func TestBrokerFailureUsesReconciliationBackoff(t *testing.T) {
	before := time.Now()
	m := Model{mode: "paper", brokerReconGeneration: 3, brokerReconInFlight: true}
	next, _ := m.Update(brokerReconMsg{
		result:     views.BrokerReconResult{Err: "broker unavailable"},
		generation: 3,
	})
	updated := next.(Model)
	if updated.brokerReconInFlight {
		t.Fatal("failed broker request remained in flight")
	}
	if updated.brokerRetryAfter.Before(before.Add(brokerReconInterval - time.Second)) {
		t.Fatalf("retry deadline = %v, want roughly one reconciliation interval", updated.brokerRetryAfter)
	}
}

func TestRefreshDashboardRetainsBrokerObservedDrawdownPeak(t *testing.T) {
	account := execution.BrokerAccount{Equity: 120000, Cash: 100000}
	snapshot := &views.BrokerDashboardSnapshot{Account: &account, FetchedAt: time.Now()}
	m := Model{mode: "paper", brokerSnapshot: snapshot}
	cfg := displayConfig{}

	m.refreshDashboard(context.Background(), cfg)
	if m.brokerDrawdownPeak != 120000 {
		t.Fatalf("observed peak = %v, want 120000", m.brokerDrawdownPeak)
	}

	account.Equity = 100000
	m.refreshDashboard(context.Background(), cfg)
	if !m.dashData.DrawdownAvailable || m.dashData.DrawdownPct < 16.66 || m.dashData.DrawdownPct > 16.67 {
		t.Fatalf("drawdown after broker decline = available %v pct %v, want about 16.67%%",
			m.dashData.DrawdownAvailable, m.dashData.DrawdownPct)
	}
}

func TestBrokerSnapshotMatchesDBExposure(t *testing.T) {
	snapshot := &views.BrokerDashboardSnapshot{
		DBExposureKnown: true, DBExposureSignature: "book-a",
	}
	if !brokerSnapshotMatchesDB(snapshot, views.DashboardData{
		DBExposureKnown: true, DBExposureSignature: "book-a",
	}) {
		t.Fatal("matching DB exposure invalidated broker snapshot")
	}
	if brokerSnapshotMatchesDB(snapshot, views.DashboardData{
		DBExposureKnown: true, DBExposureSignature: "book-b",
	}) {
		t.Fatal("changed DB exposure retained stale broker snapshot")
	}
}

func TestDisplayPortfolioScope(t *testing.T) {
	for _, tt := range []struct {
		name                 string
		enabled, paper       bool
		apiKey, mode, broker string
	}{
		{"paper alpaca", true, true, "key", "paper", "alpaca"},
		{"live alpaca", true, false, "key", "live", "alpaca"},
		{"no credentials", true, true, "", "*", "*"},
		{"stocks disabled", false, false, "key", "*", "*"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			mode, broker := displayPortfolioScope(tt.enabled, tt.paper, tt.apiKey)
			if mode != tt.mode || broker != tt.broker {
				t.Fatalf("scope = %q/%q, want %q/%q", mode, broker, tt.mode, tt.broker)
			}
		})
	}
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

// Without a live DB connection, fetchBrokerRecon still returns authoritative
// broker economics; only DB reconciliation/metadata are unavailable.
func TestFetchBrokerRecon_NoDBReturnsBrokerSnapshot(t *testing.T) {
	stub := &stubBrokerView{}
	msg := fetchBrokerRecon(stub, false, 0)() // store is not connected in unit tests
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
	if !stub.positionsCalled {
		t.Error("broker should still be queried when the DB is unavailable")
	}
	if recon.snapshot == nil || recon.snapshot.Account == nil {
		t.Error("expected a complete broker snapshot when DB reconciliation is unavailable")
	}
}
