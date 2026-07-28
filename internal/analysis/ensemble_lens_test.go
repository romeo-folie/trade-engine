package analysis

import (
	"context"
	"fmt"
	"testing"
)

func lensResult(action Action, conf float64, selected, regime string) AnalystResult {
	return AnalystResult{
		Provider: "p", Model: "m",
		Response: AnalysisResponse{
			Action: action, Confidence: conf, SizePct: 2, StopLoss: 90, TakeProfit: 110,
			SelectedStrategy: selected, Regime: regime,
		},
	}
}

func lensResultWithRejections(action Action, conf float64, selected, regime string, rejected map[string]string) AnalystResult {
	r := lensResult(action, conf, selected, regime)
	r.Response.RejectedStrategies = rejected
	return r
}

// TestConsensusLens_UnanimousAmongWinners: aggregation rebuilds the response
// field by field, so anything not copied is lost. SelectedStrategy drives the
// engine's entry-gate keying, so losing it through the ensemble would leave the
// news-tape and anti-chase gates armed against the configured primary — exactly
// the bypass the candidate-lens change closes.
func TestConsensusLens_UnanimousAmongWinners(t *testing.T) {
	results := []AnalystResult{
		lensResult(ActionBuy, 0.72, "news_sentiment", "trending"),
		lensResult(ActionBuy, 0.70, "news_sentiment", "trending"),
	}
	selected, regime := consensusLens(results, ActionBuy)
	if selected != "news_sentiment" || regime != "trending" {
		t.Errorf("consensusLens = (%q, %q), want (news_sentiment, trending)", selected, regime)
	}
}

// A split selection has no single lens to attribute the decision to. Empty is
// the honest answer — inventing one would corrupt both the gate and the
// divergence measurement.
func TestConsensusLens_SplitYieldsEmpty(t *testing.T) {
	results := []AnalystResult{
		lensResult(ActionBuy, 0.72, "news_sentiment", "trending"),
		lensResult(ActionBuy, 0.70, "momentum", "ranging"),
	}
	if selected, regime := consensusLens(results, ActionBuy); selected != "" || regime != "" {
		t.Errorf("split selection must yield empty, got (%q, %q)", selected, regime)
	}
}

// Only analysts who voted for the winning action are consulted.
func TestConsensusLens_IgnoresLosingVoters(t *testing.T) {
	results := []AnalystResult{
		lensResult(ActionBuy, 0.72, "momentum", "trending"),
		lensResult(ActionHold, 0.40, "mean_reversion", "ranging"),
	}
	if selected, _ := consensusLens(results, ActionBuy); selected != "momentum" {
		t.Errorf("selected = %q, want momentum (HOLD voter must not dilute it)", selected)
	}
}

// TestTryFastPath_CarriesLens: the fast path is the unanimous-action route, so
// a unanimous lens must survive it.
func TestTryFastPath_CarriesLens(t *testing.T) {
	results := []AnalystResult{
		lensResultWithRejections(ActionBuy, 0.80, "news_sentiment", "trending", map[string]string{"momentum": "no volume"}),
		lensResultWithRejections(ActionBuy, 0.78, "news_sentiment", "trending", map[string]string{"mean_reversion": "no band touch", "news_sentiment": "malformed"}),
	}
	resp, ok := TryFastPath(results, 0.70, 0.20)
	if !ok {
		t.Fatal("fast path should have engaged")
	}
	if resp.SelectedStrategy != "news_sentiment" {
		t.Errorf("fast path dropped SelectedStrategy: %q", resp.SelectedStrategy)
	}
	if resp.Regime != "trending" {
		t.Errorf("fast path dropped Regime: %q", resp.Regime)
	}
	if len(resp.RejectedStrategies) != 2 {
		t.Errorf("fast path rejected strategies = %#v, want the two non-selected lenses", resp.RejectedStrategies)
	}
	if _, ok := resp.RejectedStrategies["news_sentiment"]; ok {
		t.Error("fast path must not report the selected lens as rejected")
	}
}

// TestMajorityVote_CarriesLens: same invariant on the fallback route.
func TestMajorityVote_CarriesLens(t *testing.T) {
	results := []AnalystResult{
		lensResultWithRejections(ActionBuy, 0.72, "momentum", "trending", map[string]string{"news_sentiment": "no catalyst"}),
		lensResultWithRejections(ActionBuy, 0.68, "momentum", "trending", map[string]string{"mean_reversion": "trend intact"}),
		lensResult(ActionHold, 0.50, "mean_reversion", "ranging"),
	}
	resp, ok := majorityVote(results)
	if !ok {
		t.Fatal("majority vote should have resolved")
	}
	if resp.Action != ActionBuy {
		t.Fatalf("winner = %q, want BUY", resp.Action)
	}
	if resp.SelectedStrategy != "momentum" {
		t.Errorf("majority vote dropped SelectedStrategy: %q", resp.SelectedStrategy)
	}
	if len(resp.RejectedStrategies) != 2 {
		t.Errorf("majority vote rejected strategies = %#v, want both winning-voter rejections", resp.RejectedStrategies)
	}
}

func TestArbitrate_CarriesLensRejections(t *testing.T) {
	results := []AnalystResult{
		lensResultWithRejections(ActionBuy, 0.72, "momentum", "trending", map[string]string{"news_sentiment": "no catalyst"}),
		lensResultWithRejections(ActionBuy, 0.70, "momentum", "trending", map[string]string{"mean_reversion": "trend intact"}),
	}
	cap := &captureArbiter{response: AnalysisResponse{Action: ActionBuy, Confidence: 0.8, Reasoning: "agreement"}}
	result, err := Arbitrate(context.Background(), cap, AnalysisRequest{Ticker: "TEST"}, results)
	if err != nil {
		t.Fatalf("Arbitrate returned error: %v", err)
	}
	if len(result.Final.RejectedStrategies) != 2 {
		t.Errorf("arbiter rejected strategies = %#v, want both winning-voter rejections", result.Final.RejectedStrategies)
	}
	if _, ok := result.Final.RejectedStrategies["momentum"]; ok {
		t.Error("arbiter must not report the selected lens as rejected")
	}
}

// TestSpecialistConsensusLens excludes the risk manager: it reviews the
// directional findings rather than voting, and is never asked to select a lens.
// Without this backfill, specialist mode returns the risk manager's response
// verbatim — with empty lens fields — and the engine's selected-lens entry
// gates silently revert to the configured primary.
func TestSpecialistConsensusLens(t *testing.T) {
	results := []specialistResult{
		{Role: "fundamental", Response: AnalysisResponse{Action: ActionBuy, SelectedStrategy: "news_sentiment", Regime: "trending"}},
		{Role: "technical", Response: AnalysisResponse{Action: ActionBuy, SelectedStrategy: "news_sentiment", Regime: "trending"}},
		// The risk manager carries no selection and must not blank the result.
		{Role: "risk_manager", Response: AnalysisResponse{Action: ActionBuy}},
	}
	selected, regime := specialistConsensusLens(results, ActionBuy)
	if selected != "news_sentiment" {
		t.Errorf("selected = %q, want news_sentiment (risk manager must be excluded)", selected)
	}
	if regime != "trending" {
		t.Errorf("regime = %q, want trending", regime)
	}
}

// Failed specialists must not contribute, and a genuine split still yields empty.
func TestSpecialistConsensusLens_SplitAndErrors(t *testing.T) {
	split := []specialistResult{
		{Role: "fundamental", Response: AnalysisResponse{Action: ActionBuy, SelectedStrategy: "momentum"}},
		{Role: "technical", Response: AnalysisResponse{Action: ActionBuy, SelectedStrategy: "trend_following"}},
	}
	if selected, _ := specialistConsensusLens(split, ActionBuy); selected != "" {
		t.Errorf("split must yield empty, got %q", selected)
	}
	withErr := []specialistResult{
		{Role: "fundamental", Response: AnalysisResponse{Action: ActionBuy, SelectedStrategy: "momentum"}},
		{Role: "technical", Err: errTest, Response: AnalysisResponse{Action: ActionBuy, SelectedStrategy: "trend_following"}},
	}
	if selected, _ := specialistConsensusLens(withErr, ActionBuy); selected != "momentum" {
		t.Errorf("failed specialist must not dilute consensus, got %q", selected)
	}
}

var errTest = fmt.Errorf("specialist failed")

// TestSpecialistConsensusRejections: the risk manager runs the lens contract
// too, so its own rejection map must not survive next to a consensus-derived
// selection — otherwise the aggregate can list its selected lens as rejected.
func TestSpecialistConsensusRejections(t *testing.T) {
	results := []specialistResult{
		{Role: "fundamental", Response: AnalysisResponse{
			Action: ActionBuy, SelectedStrategy: "momentum",
			RejectedStrategies: map[string]string{"mean_reversion": "trending"},
		}},
		{Role: "technical", Response: AnalysisResponse{
			Action: ActionBuy, SelectedStrategy: "momentum",
			RejectedStrategies: map[string]string{"news_sentiment": "no catalyst"},
		}},
		// Risk manager picked differently; its rejections must not leak through.
		{Role: "risk_manager", Response: AnalysisResponse{
			Action: ActionBuy, SelectedStrategy: "mean_reversion",
			RejectedStrategies: map[string]string{"momentum": "chasing"},
		}},
	}
	got := specialistConsensusRejections(results, ActionBuy, "momentum")
	if _, bad := got["momentum"]; bad {
		t.Error("selected lens appears in its own rejection map")
	}
	for _, want := range []string{"mean_reversion", "news_sentiment"} {
		if _, ok := got[want]; !ok {
			t.Errorf("missing rejection for %q: %#v", want, got)
		}
	}
	if got := specialistConsensusRejections(results, ActionBuy, ""); got != nil {
		t.Errorf("no consensus selection must yield nil rejections, got %#v", got)
	}
}
