package analysis

import (
	"fmt"
	"strings"
	"testing"
)

func makeResult(action Action, conf float64, err error) AnalystResult {
	return AnalystResult{
		Provider: "test", Model: "test-model",
		Response: AnalysisResponse{Action: action, Confidence: conf, SizePct: 2.0, StopLoss: 100.0, TakeProfit: 110.0, Reasoning: "test"},
		Err:      err,
	}
}

func TestTryFastPath_UnanimousAboveThreshold(t *testing.T) {
	results := []AnalystResult{makeResult(ActionBuy, 0.88, nil), makeResult(ActionBuy, 0.85, nil), makeResult(ActionBuy, 0.82, nil), makeResult(ActionBuy, 0.84, nil)}
	resp, ok := TryFastPath(results, 0.80, 0.15)
	if !ok {
		t.Fatal("expected fast-path to hit on unanimous BUY")
	}
	if resp.Action != ActionBuy {
		t.Fatalf("action = %q, want BUY", resp.Action)
	}
	if resp.Confidence < 0.84 || resp.Confidence > 0.86 {
		t.Fatalf("avg confidence = %.3f, want ~0.848", resp.Confidence)
	}
}

func TestTryFastPath_DisagreementBlocks(t *testing.T) {
	results := []AnalystResult{makeResult(ActionBuy, 0.88, nil), makeResult(ActionSell, 0.85, nil), makeResult(ActionBuy, 0.82, nil)}
	_, ok := TryFastPath(results, 0.80, 0.15)
	if ok {
		t.Fatal("fast-path should be blocked when analysts disagree")
	}
}

func TestTryFastPath_ConfidenceBelowThreshold(t *testing.T) {
	results := []AnalystResult{makeResult(ActionBuy, 0.75, nil), makeResult(ActionBuy, 0.72, nil), makeResult(ActionBuy, 0.74, nil)}
	_, ok := TryFastPath(results, 0.80, 0.15)
	if ok {
		t.Fatal("fast-path blocked when max confidence < 0.80")
	}
}

func TestTryFastPath_SpreadTooWide(t *testing.T) {
	results := []AnalystResult{makeResult(ActionBuy, 0.92, nil), makeResult(ActionBuy, 0.75, nil)}
	_, ok := TryFastPath(results, 0.80, 0.15)
	if ok {
		t.Fatal("fast-path blocked when spread > 0.15")
	}
}

func TestTryFastPath_FailedAnalystsIgnored(t *testing.T) {
	results := []AnalystResult{makeResult(ActionBuy, 0.88, nil), makeResult(ActionBuy, 0.85, nil), makeResult(ActionBuy, 0.00, fmt.Errorf("timeout"))}
	resp, ok := TryFastPath(results, 0.80, 0.15)
	if !ok {
		t.Fatal("fast-path should hit with 2+ successful unanimous results")
	}
	if resp.Action != ActionBuy {
		t.Fatalf("action = %q, want BUY", resp.Action)
	}
}

func TestTryFastPath_FewerThanTwoSuccesses(t *testing.T) {
	results := []AnalystResult{makeResult(ActionBuy, 0.90, nil), makeResult(ActionBuy, 0.00, fmt.Errorf("err")), makeResult(ActionBuy, 0.00, fmt.Errorf("err"))}
	_, ok := TryFastPath(results, 0.80, 0.15)
	if ok {
		t.Fatal("fast-path requires at least 2 successful results")
	}
}

func TestTryFastPath_HoldAction(t *testing.T) {
	results := []AnalystResult{makeResult(ActionHold, 0.85, nil), makeResult(ActionHold, 0.83, nil)}
	resp, ok := TryFastPath(results, 0.80, 0.15)
	if !ok {
		t.Fatal("fast-path should hit on unanimous HOLD")
	}
	if resp.Action != ActionHold {
		t.Fatalf("action = %q, want HOLD", resp.Action)
	}
}

func TestMajorityVote_ClearMajority(t *testing.T) {
	results := []AnalystResult{makeResult(ActionBuy, 0.80, nil), makeResult(ActionBuy, 0.75, nil), makeResult(ActionSell, 0.90, nil)}
	resp, ok := majorityVote(results)
	if !ok {
		t.Fatal("majorityVote should succeed")
	}
	if resp.Action != ActionBuy {
		t.Fatalf("action = %q, want BUY (2 vs 1)", resp.Action)
	}
}

func TestMajorityVote_TieFavorsHold(t *testing.T) {
	results := []AnalystResult{makeResult(ActionBuy, 0.80, nil), makeResult(ActionHold, 0.80, nil)}
	resp, ok := majorityVote(results)
	if !ok {
		t.Fatal("majorityVote should succeed")
	}
	if resp.Action != ActionHold {
		t.Fatalf("action = %q, want HOLD on tie", resp.Action)
	}
}

func TestMajorityVote_AllFailed(t *testing.T) {
	results := []AnalystResult{makeResult(ActionBuy, 0.80, fmt.Errorf("err")), makeResult(ActionSell, 0.80, fmt.Errorf("err"))}
	_, ok := majorityVote(results)
	if ok {
		t.Fatal("majorityVote should fail when all analysts errored")
	}
}

func TestExtractDissent_Present(t *testing.T) {
	got := extractDissent("Analysis complete. DISSENT: Model-A said SELL while others said BUY.")
	if got == "" {
		t.Fatal("expected non-empty dissent")
	}
	if !strings.Contains(got, "SELL") {
		t.Fatalf("dissent should mention SELL, got: %q", got)
	}
}

func TestExtractDissent_Absent(t *testing.T) {
	got := extractDissent("All models agreed on BUY with strong signals.")
	if got != "" {
		t.Fatalf("expected empty dissent, got: %q", got)
	}
}

func TestExtractDissent_CaseInsensitive(t *testing.T) {
	got := extractDissent("Models diverged. dissent: minor stop-loss disagreement.")
	if got == "" {
		t.Fatal("extractDissent should be case-insensitive")
	}
}
