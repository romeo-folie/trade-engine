package engine

import (
	"context"
	"encoding/json"
	"testing"
	"time"
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

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
