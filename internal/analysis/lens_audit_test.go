package analysis

import (
	"testing"

	"github.com/romeo-folie/trade-engine/internal/tradeprofile"
)

func profileForLensAudit() *tradeprofile.ExecutionProfile {
	return &tradeprofile.ExecutionProfile{
		Version: tradeprofile.ExecutionProfileVersion, RecommendedLens: "momentum",
		AdmissibleLenses: []string{"momentum", "pullback_strength"},
		PrimaryTimeframe: "1D", ScorerRationale: "test profile",
	}
}

func TestAnalysisRequestLensSetPrefersExecutionProfile(t *testing.T) {
	req := AnalysisRequest{
		Strategies:       []string{"mean_reversion"},
		ExecutionProfile: profileForLensAudit(),
	}
	got := req.LensSet()
	if len(got) != 2 || got[0] != "momentum" || got[1] != "pullback_strength" {
		t.Fatalf("LensSet() = %v, want profile lenses", got)
	}
}

func TestValidateFinalLensSelectionRejectsProfileExternalLens(t *testing.T) {
	resp := AnalysisResponse{
		SelectedStrategy: "news_sentiment", Regime: "trending",
		RejectedStrategies: map[string]string{"momentum": "no follow-through"},
	}
	audit := ValidateFinalLensSelection(&resp, AnalysisRequest{ExecutionProfile: profileForLensAudit()}, true)
	if audit.SelectionStatus != LensSelectionInvalid || audit.SelectedLens != "news_sentiment" {
		t.Fatalf("audit = %#v, want invalid recorded selection", audit)
	}
	if resp.SelectedStrategy != "" {
		t.Fatalf("invalid response selection = %q, want cleared before downstream use", resp.SelectedStrategy)
	}
}

func TestValidateFinalLensSelectionDistinguishesNoneAndNoConsensus(t *testing.T) {
	none := AnalysisResponse{SelectedStrategy: "none"}
	if got := ValidateFinalLensSelection(&none, AnalysisRequest{ExecutionProfile: profileForLensAudit()}, false); got.SelectionStatus != LensSelectionNone {
		t.Fatalf("none status = %q", got.SelectionStatus)
	}
	aggregate := AnalysisResponse{}
	if got := ValidateFinalLensSelection(&aggregate, AnalysisRequest{ExecutionProfile: profileForLensAudit()}, true); got.SelectionStatus != LensSelectionNoConsensus {
		t.Fatalf("aggregate empty status = %q", got.SelectionStatus)
	}
}
