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
		Action: ActionBuy, SelectedStrategy: "news_sentiment", Regime: "trending",
		RejectedStrategies: map[string]string{"momentum": "no follow-through"},
	}
	audit := ValidateFinalLensSelection(&resp, AnalysisRequest{ExecutionProfile: profileForLensAudit()}, true)
	if audit.SelectionStatus != LensSelectionInvalid || audit.SelectedLens != "news_sentiment" {
		t.Fatalf("audit = %#v, want invalid recorded selection", audit)
	}
	if resp.SelectedStrategy != "" {
		t.Fatalf("invalid response selection = %q, want cleared before downstream use", resp.SelectedStrategy)
	}
	if resp.Action != ActionHold {
		t.Fatalf("invalid response action = %q, want non-actionable HOLD", resp.Action)
	}
}

func TestValidateFinalLensSelectionDistinguishesNoneAndNoConsensus(t *testing.T) {
	none := AnalysisResponse{Action: ActionBuy, SelectedStrategy: "none"}
	if got := ValidateFinalLensSelection(&none, AnalysisRequest{ExecutionProfile: profileForLensAudit()}, false); got.SelectionStatus != LensSelectionNone {
		t.Fatalf("none status = %q", got.SelectionStatus)
	}
	if none.Action != ActionHold {
		t.Fatalf("profile-bound none action = %q, want HOLD", none.Action)
	}
	aggregate := AnalysisResponse{Action: ActionBuy}
	if got := ValidateFinalLensSelection(&aggregate, AnalysisRequest{ExecutionProfile: profileForLensAudit()}, true); got.SelectionStatus != LensSelectionNoConsensus {
		t.Fatalf("aggregate empty status = %q", got.SelectionStatus)
	}
	if aggregate.Action != ActionHold {
		t.Fatalf("profile-bound no-consensus action = %q, want HOLD", aggregate.Action)
	}
}

func TestValidateFinalLensSelectionProfileBoundAbsentSelectionSuppressesBuy(t *testing.T) {
	resp := AnalysisResponse{Action: ActionBuy}
	audit := ValidateFinalLensSelection(&resp, AnalysisRequest{ExecutionProfile: profileForLensAudit()}, false)
	if audit.SelectionStatus != LensSelectionAbsent {
		t.Fatalf("status = %q, want absent", audit.SelectionStatus)
	}
	if resp.Action != ActionHold {
		t.Fatalf("profile-bound absent selection action = %q, want HOLD", resp.Action)
	}
}
