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
		SizePct: 2, StopLoss: 90, TakeProfit: 110,
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
	assertNonDirectional(t, resp)
}

func TestValidateFinalLensSelectionDistinguishesNoneAndNoConsensus(t *testing.T) {
	none := AnalysisResponse{Action: ActionBuy, SelectedStrategy: "none", SizePct: 2, StopLoss: 90, TakeProfit: 110}
	if got := ValidateFinalLensSelection(&none, AnalysisRequest{ExecutionProfile: profileForLensAudit()}, false); got.SelectionStatus != LensSelectionNone {
		t.Fatalf("none status = %q", got.SelectionStatus)
	}
	if none.Action != ActionHold {
		t.Fatalf("profile-bound none action = %q, want HOLD", none.Action)
	}
	assertNonDirectional(t, none)
	aggregate := AnalysisResponse{Action: ActionBuy, SizePct: 2, StopLoss: 90, TakeProfit: 110}
	if got := ValidateFinalLensSelection(&aggregate, AnalysisRequest{ExecutionProfile: profileForLensAudit()}, true); got.SelectionStatus != LensSelectionNoConsensus {
		t.Fatalf("aggregate empty status = %q", got.SelectionStatus)
	}
	if aggregate.Action != ActionHold {
		t.Fatalf("profile-bound no-consensus action = %q, want HOLD", aggregate.Action)
	}
	assertNonDirectional(t, aggregate)
}

func TestValidateFinalLensSelectionProfileBoundAbsentSelectionSuppressesBuy(t *testing.T) {
	resp := AnalysisResponse{Action: ActionBuy, SizePct: 2, StopLoss: 90, TakeProfit: 110}
	audit := ValidateFinalLensSelection(&resp, AnalysisRequest{ExecutionProfile: profileForLensAudit()}, false)
	if audit.SelectionStatus != LensSelectionAbsent {
		t.Fatalf("status = %q, want absent", audit.SelectionStatus)
	}
	if resp.Action != ActionHold {
		t.Fatalf("profile-bound absent selection action = %q, want HOLD", resp.Action)
	}
	assertNonDirectional(t, resp)
}

func TestValidateFinalLensSelectionRequiresCompleteProfileRejectionsForBuy(t *testing.T) {
	tests := []struct {
		name     string
		rejected map[string]string
	}{
		{name: "missing admissible lens", rejected: nil},
		{name: "selected lens included", rejected: map[string]string{
			"momentum": "still selected", "pullback_strength": "not enough confirmation",
		}},
		{name: "disallowed lens included", rejected: map[string]string{
			"pullback_strength": "not enough confirmation", "news_sentiment": "no catalyst",
		}},
		{name: "duplicate normalized lens", rejected: map[string]string{
			"pullback_strength": "not enough confirmation", " PULLBACK_STRENGTH ": "duplicate",
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := AnalysisResponse{
				Action: ActionBuy, SelectedStrategy: "momentum",
				SizePct: 2, StopLoss: 90, TakeProfit: 110,
				RejectedStrategies: tt.rejected,
			}
			audit := ValidateFinalLensSelection(&resp, AnalysisRequest{ExecutionProfile: profileForLensAudit()}, false)
			if audit.SelectionStatus != LensSelectionInvalid {
				t.Fatalf("status = %q, want invalid", audit.SelectionStatus)
			}
			if audit.SelectedLens != "momentum" {
				t.Fatalf("audit selected lens = %q, want original selection retained", audit.SelectedLens)
			}
			if resp.SelectedStrategy != "" {
				t.Fatalf("live selected lens = %q, want cleared", resp.SelectedStrategy)
			}
			assertNonDirectional(t, resp)
		})
	}
}

func TestValidateFinalLensSelectionAcceptsCompleteProfileRejectionsForBuy(t *testing.T) {
	resp := AnalysisResponse{
		Action: ActionBuy, SelectedStrategy: "momentum",
		SizePct: 2, StopLoss: 90, TakeProfit: 110,
		RejectedStrategies: map[string]string{"pullback_strength": "not enough confirmation"},
	}
	audit := ValidateFinalLensSelection(&resp, AnalysisRequest{ExecutionProfile: profileForLensAudit()}, false)
	if audit.SelectionStatus != LensSelectionSelected {
		t.Fatalf("status = %q, want selected", audit.SelectionStatus)
	}
	if resp.Action != ActionBuy || resp.SizePct != 2 || resp.StopLoss != 90 || resp.TakeProfit != 110 {
		t.Fatalf("complete profile BUY was altered: %#v", resp)
	}
}

// A scanner-owned execution profile authorises entries, not exits. An absent,
// invalid, or explicit no-fit lens must suppress a BUY so it cannot fall back
// to a legacy strategy, but it must never discard a protective SELL.
func TestValidateFinalLensSelectionPreservesProfileBoundSells(t *testing.T) {
	tests := []struct {
		name     string
		selected string
		status   LensSelectionStatus
	}{
		{name: "admissible", selected: "momentum", status: LensSelectionSelected},
		{name: "invalid", selected: "news_sentiment", status: LensSelectionInvalid},
		{name: "none", selected: "none", status: LensSelectionNone},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := AnalysisResponse{Action: ActionSell, SelectedStrategy: tt.selected}
			audit := ValidateFinalLensSelection(&resp, AnalysisRequest{ExecutionProfile: profileForLensAudit()}, false)
			if audit.SelectionStatus != tt.status {
				t.Fatalf("status = %q, want %q", audit.SelectionStatus, tt.status)
			}
			if resp.Action != ActionSell {
				t.Fatalf("SELL action = %q, want SELL", resp.Action)
			}
		})
	}
}

func assertNonDirectional(t *testing.T, resp AnalysisResponse) {
	t.Helper()
	if resp.SizePct != 0 || resp.StopLoss != 0 || resp.TakeProfit != 0 {
		t.Fatalf("HOLD must clear entry fields: size=%v stop=%v target=%v", resp.SizePct, resp.StopLoss, resp.TakeProfit)
	}
}
