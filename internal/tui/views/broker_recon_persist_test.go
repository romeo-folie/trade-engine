package views

import (
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/engine"
)

func TestParsePersistedRecon_InSync(t *testing.T) {
	r := ParsePersistedRecon(`{"checked_at":"2026-07-09T12:00:00Z","drifts":[]}`)
	if !r.Available || !r.InSync || len(r.Divergences) != 0 {
		t.Fatalf("expected available+in-sync, got %+v", r)
	}
	if r.CheckedAt.IsZero() {
		t.Error("expected CheckedAt to be parsed")
	}
}

func TestParsePersistedRecon_Diverging(t *testing.T) {
	r := ParsePersistedRecon(`{"checked_at":"2026-07-09T12:00:00Z","drifts":[{"ticker":"NVDA","broker_qty":10,"db_qty":0,"source":"broker_only"}]}`)
	if r.InSync || len(r.Divergences) != 1 {
		t.Fatalf("expected 1 divergence, got %+v", r.Divergences)
	}
	d := r.Divergences[0]
	if d.Ticker != "NVDA" || d.Kind != "broker_only" || d.BrokerQty != 10 {
		t.Errorf("divergence parsed wrong: %+v", d)
	}
}

func TestParsePersistedRecon_Invalid(t *testing.T) {
	r := ParsePersistedRecon(`not json`)
	if r.Available || r.Err == "" {
		t.Errorf("expected unavailable result with error, got %+v", r)
	}
}

// The engine writes the blob and the dashboard reads it — this guards that the
// two packages agree on the wire format across the process boundary.
func TestPersistedReconContract_EngineToDashboard(t *testing.T) {
	blob, err := engine.MarshalReconciliation([]engine.BrokerDrift{
		{Ticker: "AMC", BrokerQty: -5, DBQty: 5, Source: "side_mismatch"},
	}, time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	r := ParsePersistedRecon(blob)
	if !r.Available || r.InSync || len(r.Divergences) != 1 {
		t.Fatalf("engine blob did not parse into a divergence: %+v", r)
	}
	d := r.Divergences[0]
	if d.Ticker != "AMC" || d.Kind != "side_mismatch" || d.BrokerQty != -5 || d.DBQty != 5 {
		t.Errorf("contract mismatch: %+v", d)
	}
}
