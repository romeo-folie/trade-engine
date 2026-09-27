package analysis

import (
	"fmt"
	"testing"
)

// TestToPromptPortfolio_PinSortCap: the analyzed ticker's own position must be
// pinned first (the cap may never drop the position SELL/pyramid decisions
// depend on), the remainder sorts alphabetically for cache-stable rendering,
// and positions beyond the cap are counted in OmittedPositions.
func TestToPromptPortfolio_PinSortCap(t *testing.T) {
	var ctxPos []PositionSummary
	for i := 0; i < maxPromptPositions+4; i++ { // 19 positions, ZZTOP last alphabetically
		ctxPos = append(ctxPos, PositionSummary{Ticker: fmt.Sprintf("T%02d", i), Quantity: 1})
	}
	ctxPos = append(ctxPos, PositionSummary{Ticker: "ZZTOP", Quantity: 5, EntryCount: 2, EntryCountKnown: true})

	got := toPromptPortfolio(PortfolioContext{CashAvailable: 100, PortfolioEquity: 1000, OpenPositions: ctxPos}, "ZZTOP")
	if got.OpenPositions[0].Ticker != "ZZTOP" {
		t.Fatalf("analyzed ticker not pinned first: got %q", got.OpenPositions[0].Ticker)
	}
	if got.PortfolioEquity != 1000 || got.OpenPositions[0].EntryCount != 2 || !got.OpenPositions[0].EntryCountKnown {
		t.Fatalf("equity or entry count lost in prompt conversion: %+v", got)
	}
	if len(got.OpenPositions) != maxPromptPositions {
		t.Fatalf("cap not applied: got %d positions", len(got.OpenPositions))
	}
	if got.OmittedPositions != 5 { // 20 total - 15 cap
		t.Fatalf("OmittedPositions = %d, want 5", got.OmittedPositions)
	}
	// Rest sorted alphabetically after the pin.
	for i := 2; i < len(got.OpenPositions); i++ {
		if got.OpenPositions[i-1].Ticker > got.OpenPositions[i].Ticker {
			t.Fatalf("positions not sorted after pin: %q > %q", got.OpenPositions[i-1].Ticker, got.OpenPositions[i].Ticker)
		}
	}
}
