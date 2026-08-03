package engine

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/execution"
	"github.com/romeo-folie/trade-engine/internal/store"
)

func TestMarshalReconciliation_RoundTrip(t *testing.T) {
	checked := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	drifts := []BrokerDrift{
		{Ticker: "NVDA", BrokerQty: 10, DBQty: 0, Source: "broker_only"},
		{Ticker: "META", BrokerQty: 0, DBQty: 20, Source: "db_only"},
	}
	blob, err := MarshalReconciliation(drifts, checked)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var pr PersistedReconciliation
	if err := json.Unmarshal([]byte(blob), &pr); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !pr.CheckedAt.Equal(checked) {
		t.Errorf("CheckedAt = %v, want %v", pr.CheckedAt, checked)
	}
	if len(pr.Drifts) != 2 || pr.Drifts[0].Ticker != "NVDA" || pr.Drifts[1].Source != "db_only" {
		t.Errorf("drifts round-tripped wrong: %+v", pr.Drifts)
	}
	// Lowercase wire tags are the cross-process contract.
	for _, want := range []string{`"ticker"`, `"broker_qty"`, `"db_qty"`, `"source"`, `"checked_at"`} {
		if !contains(blob, want) {
			t.Errorf("blob missing wire key %s: %s", want, blob)
		}
	}
}

func TestMarshalReconciliation_InSyncEmptyDrifts(t *testing.T) {
	blob, err := MarshalReconciliation(nil, time.Unix(0, 0).UTC())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var pr PersistedReconciliation
	if err := json.Unmarshal([]byte(blob), &pr); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(pr.Drifts) != 0 {
		t.Errorf("expected no drifts, got %+v", pr.Drifts)
	}
}

// TestReconcileStatus_FailureIsNotClean is the P2 guard: a failed reconcile
// (broker error, nil broker) must report ok=false so persisters don't overwrite
// known drift with a false "in sync"; a clean reconcile reports ok=true.
func TestReconcileStatus_FailureIsNotClean(t *testing.T) {
	// Clean: broker and DB agree → no drift, ok=true.
	drifts, ok := reconcileBrokerVsDB(context.Background(),
		&fakeBrokerView{positions: []execution.BrokerPosition{{Symbol: "AAPL", Quantity: 10, Side: "long"}}},
		&fakeStore{trades: []store.Trade{{Ticker: "AAPL", Direction: "long", Quantity: 10}}},
		0.01)
	if !ok || len(drifts) != 0 {
		t.Errorf("clean reconcile: drifts=%v ok=%v, want empty/true", drifts, ok)
	}

	// Broker fetch failure → ok=false (indistinguishable-from-clean bug fixed).
	if _, ok := reconcileBrokerVsDB(context.Background(),
		&fakeBrokerView{err: errors.New("api down")}, &fakeStore{}, 0.01); ok {
		t.Error("broker failure must report ok=false")
	}

	// nil broker → ok=false.
	if _, ok := reconcileBrokerVsDB(context.Background(), nil, &fakeStore{}, 0.01); ok {
		t.Error("nil broker must report ok=false")
	}
}

func TestRunReconcileLoop_DisabledReturns(t *testing.T) {
	// nil broker or interval <= 0 must return immediately (not block) — if the
	// guard were missing this test would hang.
	done := make(chan struct{})
	go func() {
		RunReconcileLoop(context.Background(), nil, time.Hour) // nil broker
		RunReconcileLoop(context.Background(), nil, 0)         // interval 0
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("RunReconcileLoop did not return with disabling args")
	}
}

func TestDriftAlertStateDeduplicatesChangesAndRearmsAfterResolution(t *testing.T) {
	initial := BrokerDrift{Ticker: "PSN", BrokerQty: 0, DBQty: 10, Source: "db_only"}
	state := newDriftAlertState([]BrokerDrift{initial})

	state, fresh := state.advance([]BrokerDrift{initial})
	if len(fresh) != 0 {
		t.Fatalf("unchanged boot drift alerted again: %+v", fresh)
	}
	changed := initial
	changed.DBQty = 9
	state, fresh = state.advance([]BrokerDrift{changed})
	if len(fresh) != 1 {
		t.Fatalf("changed drift alerts = %+v, want one", fresh)
	}
	state, fresh = state.advance([]BrokerDrift{{Ticker: "MPLT", DBQty: 5, Source: "exit_pending"}})
	if len(fresh) != 0 || len(state) != 0 {
		t.Fatalf("resolved/pending state = %v fresh=%v, want empty/no alert", state, fresh)
	}
	_, fresh = state.advance([]BrokerDrift{initial})
	if len(fresh) != 1 {
		t.Fatalf("reappearing drift alerts = %+v, want one", fresh)
	}
}

func TestDriftAlertStateIgnoresSubToleranceQuantityNoise(t *testing.T) {
	initial := BrokerDrift{Ticker: "PSN", Source: "mismatch", BrokerQty: 10, DBQty: 12}
	state := newDriftAlertState([]BrokerDrift{initial})

	next, fresh := state.advance([]BrokerDrift{{
		Ticker: "PSN", Source: "mismatch", BrokerQty: 10.005, DBQty: 11.995,
	}})
	if len(fresh) != 0 {
		t.Fatalf("sub-tolerance drift noise alerted: %+v", fresh)
	}
	_, fresh = next.advance([]BrokerDrift{{
		Ticker: "PSN", Source: "mismatch", BrokerQty: 10.02, DBQty: 12,
	}})
	if len(fresh) != 1 {
		t.Fatalf("material drift change alerts = %d, want 1", len(fresh))
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
