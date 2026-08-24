package engine

import (
	"reflect"
	"testing"

	"github.com/romeo-folie/trade-engine/internal/analysis"
	"github.com/romeo-folie/trade-engine/internal/data"
	"github.com/romeo-folie/trade-engine/internal/store"
	"github.com/romeo-folie/trade-engine/internal/tradeprofile"
)

func TestResolveTimeframeWorkflow(t *testing.T) {
	validProfile := func(primary, entry string) *tradeprofile.ExecutionProfile {
		return &tradeprofile.ExecutionProfile{
			Version:              tradeprofile.ExecutionProfileVersion,
			RecommendedLens:      "momentum",
			AdmissibleLenses:     []string{"momentum"},
			PrimaryTimeframe:     primary,
			EntryTimingTimeframe: entry,
			ScorerRationale:      "source-owned timeframe roles",
		}
	}

	tests := []struct {
		name        string
		legacy      []string
		profile     *tradeprofile.ExecutionProfile
		wantPrimary data.Timeframe
		wantEntry   data.Timeframe
		wantFetch   []string
		wantProfile bool
	}{
		{
			name:   "valid profile owns primary and entry timing over legacy list",
			legacy: []string{"1h", "4h"}, profile: validProfile(" 1d ", " 4H "),
			wantPrimary: data.Timeframe1Day, wantEntry: data.Timeframe4Hour,
			wantFetch: []string{"1d", "4h"}, wantProfile: true,
		},
		{
			name:   "valid profile without entry timing has one thesis timeframe",
			legacy: []string{"1d", "4h"}, profile: validProfile("4H", ""),
			wantPrimary: data.Timeframe4Hour, wantFetch: []string{"4h"}, wantProfile: true,
		},
		{
			name:        "legacy keeps first non daily timeframe as primary",
			legacy:      []string{"1H", "4H"},
			wantPrimary: data.Timeframe1Hour, wantFetch: []string{"1h", "4h"},
		},
		{
			name:        "legacy keeps daily preference even when daily is later",
			legacy:      []string{"1h", "1D"},
			wantPrimary: data.Timeframe1Day, wantFetch: []string{"1h", "1d"},
		},
		{
			name:   "invalid profile falls back to legacy behaviour",
			legacy: []string{"1h", "4h"}, profile: validProfile("1D", "1D"),
			wantPrimary: data.Timeframe1Hour, wantFetch: []string{"1h", "4h"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveTimeframeWorkflow(tt.legacy, tt.profile)
			if got.PrimaryTimeframe != tt.wantPrimary ||
				got.EntryTimingTimeframe != tt.wantEntry ||
				got.ProfileOwned != tt.wantProfile ||
				!reflect.DeepEqual(got.FetchTimeframes, tt.wantFetch) {
				t.Fatalf("resolveTimeframeWorkflow() = %+v, want primary=%q entry=%q fetch=%v profileOwned=%t",
					got, tt.wantPrimary, tt.wantEntry, tt.wantFetch, tt.wantProfile)
			}
		})
	}
}

func TestMergeProfileConfirmation(t *testing.T) {
	primary := analysis.AnalysisResponse{Action: analysis.ActionBuy, Confidence: .84, SizePct: 3, StopLoss: 90, TakeProfit: 120, SelectedStrategy: "momentum", Reasoning: "daily thesis"}
	audit := analysis.LensSelectionAudit{SelectedLens: "momentum", SelectionStatus: analysis.LensSelectionSelected}
	confirmation := analysis.AnalysisResponse{Action: analysis.ActionBuy, Confidence: .76, SizePct: 2, StopLoss: 96, TakeProfit: 115, SelectedStrategy: "momentum", Reasoning: "timing confirmed"}
	confirmed := mergeProfileConfirmation(primary, confirmation, audit, audit, []string{"momentum"}, true)
	if confirmed.Action != analysis.ActionBuy || confirmed.Confidence != .76 || confirmed.SizePct != 2 || confirmed.StopLoss != 96 || confirmed.TakeProfit != 115 {
		t.Fatalf("confirmed merge = %+v", confirmed)
	}

	rejected := confirmation
	rejected.Action = analysis.ActionSell
	got := mergeProfileConfirmation(primary, rejected, audit, audit, []string{"momentum"}, true)
	if got.Action != analysis.ActionWait || got.SizePct != 0 || got.StopLoss != 0 || got.TakeProfit != 0 {
		t.Fatalf("rejected confirmation must fail closed: %+v", got)
	}

	driftAudit := analysis.LensSelectionAudit{SelectedLens: "trend_following", SelectionStatus: analysis.LensSelectionSelected}
	got = mergeProfileConfirmation(primary, confirmation, audit, driftAudit, []string{"momentum"}, true)
	if got.Action != analysis.ActionWait {
		t.Fatalf("lens drift must fail closed: %+v", got)
	}
}

func TestDecisionPolicyAfterConfirmationPrefersConfirmationAudit(t *testing.T) {
	primary := &analysis.DecisionPolicyAudit{Mode: "primary", Applied: false}
	confirmation := &analysis.DecisionPolicyAudit{Mode: "confirmation", Applied: true}
	if got := decisionPolicyAfterConfirmation(primary, confirmation); got != confirmation {
		t.Fatalf("final policy = %+v, want confirmation %+v", got, confirmation)
	}
	if got := decisionPolicyAfterConfirmation(primary, nil); got != primary {
		t.Fatalf("missing confirmation policy = %+v, want primary %+v", got, primary)
	}
}

func TestDecisionPolicyAllowsReArbitration(t *testing.T) {
	if !decisionPolicyAllowsReArbitration(nil) || !decisionPolicyAllowsReArbitration(&analysis.DecisionPolicyAudit{}) {
		t.Fatal("unapplied policy should allow re-arbitration")
	}
	if decisionPolicyAllowsReArbitration(&analysis.DecisionPolicyAudit{Applied: true}) {
		t.Fatal("applied specialist authority policy must prevent re-arbitration")
	}
}

func TestQualifiedWaitRequiresCompleteRejections(t *testing.T) {
	audit := analysis.LensSelectionAudit{SelectedLens: "momentum", SelectionStatus: analysis.LensSelectionSelected}
	wait := analysis.AnalysisResponse{Action: analysis.ActionWait, Confidence: .75, SelectedStrategy: "momentum", RejectedStrategies: map[string]string{"trend_following": "range not established"}}
	if !profileConfirmationEligible(wait, audit, []string{"momentum", "trend_following"}) {
		t.Fatal("qualified WAIT should request confirmation")
	}
	wait.RejectedStrategies = map[string]string{}
	if profileConfirmationEligible(wait, audit, []string{"momentum", "trend_following"}) {
		t.Fatal("WAIT without complete rejection coverage must not request confirmation")
	}
}

func TestProfileMomentumPlayUsesTimeframeRole(t *testing.T) {
	profile := &tradeprofile.ExecutionProfile{Version: tradeprofile.ExecutionProfileVersion, RecommendedLens: "momentum", AdmissibleLenses: []string{"momentum"}, PrimaryTimeframe: "1D", ScorerRationale: "swing"}
	if profileMomentumPlay("1d", "scanner", profile) {
		t.Fatal("daily profile must use swing prompt")
	}
	profile.PrimaryTimeframe = "4H"
	if !profileMomentumPlay("4h", "scanner", profile) {
		t.Fatal("event profile must use momentum prompt")
	}
	if !profileMomentumPlay("1d", "scanner", nil) {
		t.Fatal("legacy scanner behavior must remain unchanged")
	}
}

func TestEntryGateAuditStatus(t *testing.T) {
	tests := []struct {
		name      string
		evaluated bool
		reason    string
		want      string
	}{
		{name: "unavailable evidence fails open", want: "unavailable_fail_open"},
		{name: "evaluated and blocked", evaluated: true, reason: "too extended", want: "blocked"},
		{name: "evaluated and passed", evaluated: true, want: "passed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := entryGateAuditStatus(tt.evaluated, tt.reason); got != tt.want {
				t.Fatalf("entryGateAuditStatus(%t, %q) = %q, want %q", tt.evaluated, tt.reason, got, tt.want)
			}
		})
	}
}

func TestPopulatePrimaryWorkflowAuditIncludesLegacyDecision(t *testing.T) {
	workflow := store.TimeframeWorkflowAudit{RoutingSource: "legacy", Primary: store.TimeframePassAudit{Timeframe: "4h"}}
	resp := analysis.AnalysisResponse{Action: analysis.ActionWait, Confidence: .63}
	audit := analysis.LensSelectionAudit{SelectedLens: "momentum"}
	populatePrimaryWorkflowAudit(&workflow, resp, audit)
	if workflow.Primary.Action != "WAIT" || workflow.Primary.Confidence != .63 || workflow.Primary.SelectedLens != "momentum" {
		t.Fatalf("legacy primary audit omitted decision fields: %+v", workflow.Primary)
	}
}

func TestSignalEvidenceAfterConfirmationKeepsCandlesAndTimeframePaired(t *testing.T) {
	primary := []data.Candle{{Close: 100}}
	confirmation := []data.Candle{{Close: 105}}
	got, timeframe := signalEvidenceAfterConfirmation(primary, "1d", confirmation, "4h", "confirmed", analysis.ActionBuy)
	if got[len(got)-1].Close != 105 || timeframe != "4h" {
		t.Fatalf("confirmed BUY evidence = close %.2f timeframe %q, want fresh 4h confirmation", got[len(got)-1].Close, timeframe)
	}
	got, timeframe = signalEvidenceAfterConfirmation(primary, "1d", confirmation, "4h", "rejected", analysis.ActionWait)
	if got[len(got)-1].Close != 100 || timeframe != "1d" {
		t.Fatalf("rejected confirmation must retain primary evidence, got close %.2f timeframe %q", got[len(got)-1].Close, timeframe)
	}
}
