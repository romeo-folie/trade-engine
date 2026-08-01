package views

import (
	"strings"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/execution"
	"github.com/romeo-folie/trade-engine/internal/store"
)

func bp(sym string, qty float64, side string) execution.BrokerPosition {
	return execution.BrokerPosition{Symbol: sym, Quantity: qty, Side: side}
}
func tr(ticker, dir string, qty float64) store.Trade {
	return store.Trade{Ticker: ticker, Direction: dir, EntryPrice: 10, Quantity: qty}
}

func kindFor(r BrokerReconResult, ticker string) string {
	for _, d := range r.Divergences {
		if d.Ticker == ticker {
			return d.Kind
		}
	}
	return ""
}

func TestReconcilePositions_InSync(t *testing.T) {
	broker := []execution.BrokerPosition{bp("AAPL", 100, "long"), bp("TSLA", 5, "short")}
	trades := []store.Trade{tr("AAPL", "long", 60), tr("AAPL", "long", 40), tr("TSLA", "short", 5)}
	r := ReconcilePositions(broker, trades, 0.02)
	if !r.InSync || len(r.Divergences) != 0 {
		t.Fatalf("expected in sync, got %+v", r.Divergences)
	}
	if !r.Available || r.BrokerCount != 2 || r.DBCount != 2 {
		t.Errorf("available=%v brokerCount=%d dbCount=%d", r.Available, r.BrokerCount, r.DBCount)
	}
}

func TestReconcilePositions_BrokerOnly(t *testing.T) {
	r := ReconcilePositions([]execution.BrokerPosition{bp("NVDA", 10, "long")}, nil, 0.02)
	if r.InSync || kindFor(r, "NVDA") != "broker_only" {
		t.Fatalf("expected broker_only, got %+v", r.Divergences)
	}
}

func TestReconcilePositions_DBOnly(t *testing.T) {
	r := ReconcilePositions(nil, []store.Trade{tr("META", "long", 20)}, 0.02)
	if r.InSync || kindFor(r, "META") != "db_only" {
		t.Fatalf("expected db_only, got %+v", r.Divergences)
	}
}

func TestReconcilePositions_QtyMismatch(t *testing.T) {
	// broker 100 vs db 90 → 10% gap, above 2% tolerance.
	r := ReconcilePositions([]execution.BrokerPosition{bp("AMD", 100, "long")},
		[]store.Trade{tr("AMD", "long", 90)}, 0.02)
	if kindFor(r, "AMD") != "qty_mismatch" {
		t.Fatalf("expected qty_mismatch, got %+v", r.Divergences)
	}
}

func TestReconcilePositions_SideMismatch(t *testing.T) {
	// same magnitude, opposite direction.
	r := ReconcilePositions([]execution.BrokerPosition{bp("AMC", 100, "long")},
		[]store.Trade{tr("AMC", "short", 100)}, 0.02)
	if kindFor(r, "AMC") != "side_mismatch" {
		t.Fatalf("expected side_mismatch, got %+v", r.Divergences)
	}
}

func TestReconcilePositions_WithinTolerance(t *testing.T) {
	// broker 100 vs db 101 → 1% gap, under 2% → in sync.
	r := ReconcilePositions([]execution.BrokerPosition{bp("SPY", 100, "long")},
		[]store.Trade{tr("SPY", "long", 101)}, 0.02)
	if !r.InSync {
		t.Fatalf("expected within tolerance to be in sync, got %+v", r.Divergences)
	}
}

func TestReconcilePositions_MalformedLotsIgnored(t *testing.T) {
	// The zero-qty DB lot must not count toward the DB position.
	broker := []execution.BrokerPosition{bp("INTC", 50, "long")}
	trades := []store.Trade{tr("INTC", "long", 50), {Ticker: "INTC", Direction: "long", EntryPrice: 0, Quantity: 999}}
	r := ReconcilePositions(broker, trades, 0.02)
	if !r.InSync {
		t.Fatalf("malformed lot should be ignored, got %+v", r.Divergences)
	}
}

func TestRenderBrokerReconLine(t *testing.T) {
	// Unavailable, no error → empty (clean dashboard).
	if s := renderBrokerReconLine(BrokerReconResult{}); s != "" {
		t.Errorf("unconfigured broker should render nothing, got %q", s)
	}
	// Error → failure note.
	if s := renderBrokerReconLine(BrokerReconResult{Err: "timeout"}); !strings.Contains(s, "broker check failed") {
		t.Errorf("expected failure note, got %q", s)
	}
	// In sync.
	sync := renderBrokerReconLine(BrokerReconResult{Available: true, InSync: true, BrokerCount: 26, CheckedAt: time.Now()})
	if !strings.Contains(sync, "in sync") {
		t.Errorf("expected in-sync line, got %q", sync)
	}
	// Diverging.
	div := renderBrokerReconLine(BrokerReconResult{
		Available:   true,
		Divergences: []PositionDivergence{{Ticker: "NVDA", BrokerQty: 10, Kind: "broker_only"}},
	})
	if !strings.Contains(div, "divergence") || !strings.Contains(div, "NVDA") {
		t.Errorf("expected divergence line, got %q", div)
	}
	// A persisted exit claim is transitional, not a red broker divergence.
	pending := renderBrokerReconLine(BrokerReconResult{
		Available:    true,
		PendingExits: []PositionDivergence{{Ticker: "PSN", Kind: "exit_pending"}, {Ticker: "MPLT", Kind: "exit_pending"}},
	})
	if !strings.Contains(pending, "exits finalizing") || !strings.Contains(pending, "PSN") || strings.Contains(pending, "divergence") {
		t.Errorf("expected pending-exit line, got %q", pending)
	}
	mixed := renderBrokerReconLine(BrokerReconResult{
		Available:    true,
		Divergences:  []PositionDivergence{{Ticker: "NVDA", Kind: "broker_only", BrokerQty: 2}},
		PendingExits: []PositionDivergence{{Ticker: "PSN", Kind: "exit_pending"}},
	})
	if !strings.Contains(mixed, "divergence") || !strings.Contains(mixed, "exit finalizing") {
		t.Errorf("expected mixed red divergence with pending count, got %q", mixed)
	}
}
