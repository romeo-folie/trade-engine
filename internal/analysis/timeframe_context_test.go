package analysis

import (
	"strings"
	"testing"
	"time"
)

func TestAnalysisRequestExposesPromptSafeTimeframeWorkflow(t *testing.T) {
	req := AnalysisRequest{
		TimeframeRole: TimeframeRoleEntryConfirmation,
		PrimaryThesisContext: &PrimaryThesisContext{
			PrimaryTimeframe: "1D", EntryTimingTimeframe: "4H", SelectedLens: "momentum",
		},
	}
	if req.TimeframeRole != TimeframeRoleEntryConfirmation ||
		req.PrimaryThesisContext.PrimaryTimeframe != "1D" ||
		req.PrimaryThesisContext.SelectedLens != "momentum" {
		t.Fatalf("AnalysisRequest did not retain timeframe workflow: %+v", req)
	}
}

func TestRenderTimeframeContextsMakesSupplementalEvidencePartOfOneDecision(t *testing.T) {
	text := RenderTimeframeContexts([]TimeframeContext{{
		Timeframe: "4h", Role: "entry_timing",
		RecentCandles: []CandleSnapshot{{Timestamp: time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC), Open: 100, High: 103, Low: 99, Close: 102, Volume: 42}},
		Indicators:    IndicatorSnapshot{EMA20: 100, RSI: 61, ADX: 24, ATR: 2.5},
	}})
	for _, want := range []string{"ONE UNIFIED DECISION", "4h (entry_timing evidence)", "Latest close: 102.00", "RSI 61.00", "Do not treat a supplemental timeframe as a separate vote", "use its latest close as the entry reference"} {
		if !strings.Contains(text, want) {
			t.Fatalf("rendered context missing %q:\n%s", want, text)
		}
	}
}
