package engine

import (
	"testing"

	"github.com/romeo-folie/trade-engine/internal/portfolio"
)

// TestBuildPortfolioContext maps tracker positions into the analysis request,
// skipping zero-quantity phantoms.
func TestBuildPortfolioContext(t *testing.T) {
	got := buildPortfolioContext(2500.0, []portfolio.ExecPosition{
		{Ticker: "HIMS", Side: "long", Quantity: 10, AvgCostBasis: 42.5, CurrentValue: 430, UnrealizedPL: 5},
		{Ticker: "GHOST", Quantity: 0}, // closed-out phantom — must be skipped
	})
	if got.CashAvailable != 2500.0 {
		t.Errorf("CashAvailable = %v, want 2500", got.CashAvailable)
	}
	if len(got.OpenPositions) != 1 || got.OpenPositions[0].Ticker != "HIMS" {
		t.Fatalf("expected only HIMS, got %+v", got.OpenPositions)
	}
	p := got.OpenPositions[0]
	if p.Side != "long" || p.Quantity != 10 || p.AvgCostBasis != 42.5 || p.UnrealizedPL != 5 {
		t.Errorf("field mapping wrong: %+v", p)
	}
}
