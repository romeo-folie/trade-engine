package engine

import (
	"testing"

	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/portfolio"
	"github.com/romeo-folie/trade-engine/internal/store"
)

func TestBuildRiskInputPendingEntryUsesReferenceCostBasis(t *testing.T) {
	in := buildRiskInput(config.RiskConfig{}, "BUY", "HIMS", 0.9, 1, 90, 130, 120,
		portfolio.State{TotalValue: 10_000}, []store.Trade{
			{Ticker: "HIMS", Direction: "long", Quantity: 10, EntryPrice: 100, Status: "open"},
			// A working durable entry is not yet a database trade, but it must
			// contribute its immutable reference price to pyramid gain checks.
			{Ticker: "HIMS", Direction: "long", Quantity: 10, EntryPrice: 200, Status: "open"},
		})
	if in.LongAvgEntryPrice != 150 {
		t.Fatalf("pending-entry weighted cost basis = %v, want 150", in.LongAvgEntryPrice)
	}
}

// TestBuildPortfolioContext maps tracker positions into the analysis request,
// skipping zero-quantity phantoms; a populated tracker never falls back.
func TestBuildPortfolioContext(t *testing.T) {
	state := portfolio.State{Cash: 2500, TotalValue: 10000}
	got := buildPortfolioContext(state, []portfolio.ExecPosition{
		{Ticker: "HIMS", Side: "long", Quantity: 10, AvgCostBasis: 42.5, CurrentValue: 430, UnrealizedPL: 5},
		{Ticker: "GHOST", Quantity: 0}, // closed-out phantom — must be skipped
	}, []store.Trade{{Ticker: "SHOULD_NOT_APPEAR", Direction: "long", Quantity: 1, EntryPrice: 1}})
	if got.CashAvailable != 2500 || !got.CashKnown {
		t.Errorf("cash mapping wrong: %+v", got)
	}
	if len(got.OpenPositions) != 1 || got.OpenPositions[0].Ticker != "HIMS" {
		t.Fatalf("expected only HIMS (no fallback when tracker populated), got %+v", got.OpenPositions)
	}
	p := got.OpenPositions[0]
	if p.Side != "long" || p.Quantity != 10 || p.AvgCostBasis != 42.5 || p.UnrealizedPL != 5 {
		t.Errorf("field mapping wrong: %+v", p)
	}
}

// TestBuildPortfolioContext_StoreFallback: with an empty tracker (paper mode —
// PaperExecutor.GetPositions returns nil, Codex #392 P1) open trades aggregate
// per (ticker, direction) with a weighted-average entry, and carry no
// mark-to-market (CurrentValue/UnrealizedPL zero → renderer omits the bucket).
func TestBuildPortfolioContext_StoreFallback(t *testing.T) {
	got := buildPortfolioContext(portfolio.State{Cash: 0, TotalValue: 9500}, nil, []store.Trade{
		{Ticker: "HIMS", Direction: "long", Quantity: 10, EntryPrice: 40},
		{Ticker: "HIMS", Direction: "long", Quantity: 10, EntryPrice: 50}, // pyramid row
		{Ticker: "MU", Direction: "long", Quantity: 5, EntryPrice: 88},
	})
	if !got.CashKnown {
		t.Error("equity > 0 must mark cash as known (fully deployed $0)")
	}
	if len(got.OpenPositions) != 2 {
		t.Fatalf("expected 2 aggregated positions, got %+v", got.OpenPositions)
	}
	hims := got.OpenPositions[0]
	if hims.Ticker != "HIMS" || hims.Quantity != 20 || hims.AvgCostBasis != 45 {
		t.Errorf("pyramid aggregation wrong: %+v", hims)
	}
	if hims.CurrentValue != 0 || hims.UnrealizedPL != 0 {
		t.Errorf("fallback positions must not fabricate mark-to-market: %+v", hims)
	}
}

// TestBuildPortfolioContext_CashUnknown: an uninitialized tracker (no cash, no
// equity) must NOT claim a known $0 balance.
func TestBuildPortfolioContext_CashUnknown(t *testing.T) {
	got := buildPortfolioContext(portfolio.State{}, nil, nil)
	if got.CashKnown {
		t.Error("zeroed tracker must render cash as unknown, not $0")
	}
}
