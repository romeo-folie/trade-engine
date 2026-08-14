package analysis

import "testing"

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
