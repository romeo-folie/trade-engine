package engine

import (
	"errors"
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
			Version: tradeprofile.ExecutionProfileVersion, RecommendedLens: "momentum",
			AdmissibleLenses: []string{"momentum"}, PrimaryTimeframe: primary,
			EntryTimingTimeframe: entry, ScorerRationale: "source-owned timeframe roles",
		}
	}
	tests := []struct {
		name                   string
		legacy                 []string
		profile                *tradeprofile.ExecutionProfile
		wantPrimary, wantEntry data.Timeframe
		wantFetch              []string
		wantProfile            bool
	}{
		{"profile uses thesis and entry evidence in one workflow", []string{"1h", "4h"}, validProfile(" 1d ", " 4H "), data.Timeframe1Day, data.Timeframe4Hour, []string{"1d", "4h"}, true},
		{"legacy retains every configured timeframe as evidence", []string{"1H", "4H"}, nil, data.Timeframe1Hour, "", []string{"1h", "4h"}, false},
		{"legacy keeps daily primary but preserves intraday context", []string{"1h", "1D"}, nil, data.Timeframe1Day, "", []string{"1h", "1d"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveTimeframeWorkflow(tt.legacy, tt.profile)
			if got.PrimaryTimeframe != tt.wantPrimary || got.EntryTimingTimeframe != tt.wantEntry || got.ProfileOwned != tt.wantProfile || !reflect.DeepEqual(got.FetchTimeframes, tt.wantFetch) {
				t.Fatalf("resolveTimeframeWorkflow() = %+v", got)
			}
		})
	}
}

func TestTimeframeAuditContextsRecordUnifiedEvidence(t *testing.T) {
	workflow := timeframeWorkflow{PrimaryTimeframe: data.Timeframe1Day, EntryTimingTimeframe: data.Timeframe4Hour, FetchTimeframes: []string{"1d", "4h", "1h"}}
	contexts := []analysis.TimeframeContext{{Timeframe: "4h", Role: "entry_timing"}, {Timeframe: "1h", Role: "supporting"}}
	got := timeframeAuditContexts("1d", workflow, contexts, nil)
	want := []store.TimeframePassAudit{
		{Timeframe: "1d", Role: "primary_thesis", Status: "included"},
		{Timeframe: "4h", Role: "entry_timing", Status: "included"},
		{Timeframe: "1h", Role: "supporting", Status: "included"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("contexts = %+v, want %+v", got, want)
	}
}

func TestDecisiveReasonUsesModelFieldThenReasoningFallback(t *testing.T) {
	if got := decisiveReason(analysis.AnalysisResponse{DecisiveReason: "4H volume failed to confirm."}); got != "4H volume failed to confirm." {
		t.Fatalf("got %q", got)
	}
	if got := decisiveReason(analysis.AnalysisResponse{Reasoning: "Price is above resistance.\nMore detail."}); got != "Price is above resistance." {
		t.Fatalf("got %q", got)
	}
}

func TestFinalLensSuppressionReasonMatchesFinalOutcome(t *testing.T) {
	if got := finalLensSuppressionReason(analysis.LensSelectionAudit{SelectionStatus: analysis.LensSelectionInvalid}); got != "Entry suppressed because the selected lens is not valid for this scanner profile." {
		t.Fatalf("invalid suppression reason = %q", got)
	}
	if got := finalLensSuppressionReason(analysis.LensSelectionAudit{SelectionStatus: analysis.LensSelectionNone}); got != "Entry suppressed because no configured lens fits the current regime." {
		t.Fatalf("no-fit suppression reason = %q", got)
	}
}

func TestTimeframeRefreshErrorRejectsStaleSupplementalEvidence(t *testing.T) {
	refreshErr := errors.New("provider unavailable")
	if got := timeframeRefreshError(data.Timeframe4Hour, map[data.Timeframe]error{data.Timeframe4Hour: refreshErr}); !errors.Is(got, refreshErr) {
		t.Fatalf("refresh error = %v, want %v", got, refreshErr)
	}
	if got := timeframeRefreshError(data.Timeframe4Hour, nil); got != nil {
		t.Fatalf("nil refresh map error = %v", got)
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
}

func TestPopulatePrimaryWorkflowAuditIncludesFinalDecision(t *testing.T) {
	workflow := store.TimeframeWorkflowAudit{RoutingSource: "legacy", Primary: store.TimeframePassAudit{Timeframe: "4h"}}
	resp := analysis.AnalysisResponse{Action: analysis.ActionWait, Confidence: .63}
	audit := analysis.LensSelectionAudit{SelectedLens: "momentum"}
	populatePrimaryWorkflowAudit(&workflow, resp, audit)
	if workflow.Primary.Action != "WAIT" || workflow.Primary.Confidence != .63 || workflow.Primary.SelectedLens != "momentum" {
		t.Fatalf("audit = %+v", workflow.Primary)
	}
}
