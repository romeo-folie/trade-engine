package analysis

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"testing"
)

// captureArbiter is a minimal ModelProvider used by tests to capture the
// arbiter prompt produced by Arbitrate without making any real LLM call.
type captureArbiter struct {
	captured string
}

func (c *captureArbiter) Analyze(_ context.Context, req AnalysisRequest) (AnalysisResponse, ModelCallMeta, error) {
	c.captured = req.CustomUserMessage
	return AnalysisResponse{Action: ActionHold, Confidence: 0.5, Reasoning: "ok"}, ModelCallMeta{Provider: "test", Model: "arbiter"}, nil
}
func (c *captureArbiter) ProviderName() string { return "test" }
func (c *captureArbiter) ModelName() string    { return "arbiter" }
func (c *captureArbiter) IsAvailable() bool    { return true }

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

func TestMajorityVote_TieBuyVsHoldReturnsHold(t *testing.T) {
	// BUY and HOLD tied → HOLD wins (neutral on ambiguous signal).
	results := []AnalystResult{makeResult(ActionBuy, 0.80, nil), makeResult(ActionHold, 0.80, nil)}
	resp, ok := majorityVote(results)
	if !ok {
		t.Fatal("majorityVote should succeed")
	}
	if resp.Action != ActionHold {
		t.Fatalf("action = %q, want HOLD on BUY-HOLD tie (neutral on ambiguous signal)", resp.Action)
	}
}

func TestMajorityVote_TieBuyVsSellReturnsHold(t *testing.T) {
	// BUY and SELL tied → HOLD (neutral) rather than favouring BUY by iteration order.
	// Prevents directional bias when analysts are evenly split.
	results := []AnalystResult{makeResult(ActionBuy, 0.80, nil), makeResult(ActionSell, 0.80, nil)}
	resp, ok := majorityVote(results)
	if !ok {
		t.Fatal("majorityVote should succeed")
	}
	if resp.Action != ActionHold {
		t.Fatalf("action = %q, want HOLD on BUY-SELL tie (no directional bias)", resp.Action)
	}
}

func TestMajorityVote_AllFailed(t *testing.T) {
	results := []AnalystResult{makeResult(ActionBuy, 0.80, fmt.Errorf("err")), makeResult(ActionSell, 0.80, fmt.Errorf("err"))}
	_, ok := majorityVote(results)
	if ok {
		t.Fatal("majorityVote should fail when all analysts errored")
	}
}

func TestMajorityVote_WaitMajority(t *testing.T) {
	// 2 WAIT vs 1 BUY — WAIT wins outright.
	results := []AnalystResult{
		makeResult(ActionWait, 0.80, nil),
		makeResult(ActionWait, 0.75, nil),
		makeResult(ActionBuy, 0.90, nil),
	}
	resp, ok := majorityVote(results)
	if !ok {
		t.Fatal("majorityVote should succeed")
	}
	if resp.Action != ActionWait {
		t.Fatalf("action = %q, want WAIT (2 vs 1 BUY)", resp.Action)
	}
}

func TestMajorityVote_TieWaitVsBuyReturnsBuy(t *testing.T) {
	// WAIT and BUY tied → BUY wins. Production showed WAIT-wins-ties was the
	// dominant reason directional signals were silently downgraded.
	// Refusing to act on a 50/50 split is a worse default than acting on the
	// directional side analysts proposed.
	results := []AnalystResult{
		makeResult(ActionWait, 0.80, nil),
		makeResult(ActionBuy, 0.80, nil),
	}
	resp, ok := majorityVote(results)
	if !ok {
		t.Fatal("majorityVote should succeed")
	}
	if resp.Action != ActionBuy {
		t.Fatalf("action = %q, want BUY on WAIT-BUY tie (directional wins)", resp.Action)
	}
}

func TestMajorityVote_TieWaitVsSellReturnsSell(t *testing.T) {
	// Mirror of the WAIT-vs-BUY case for SELL.
	results := []AnalystResult{
		makeResult(ActionWait, 0.80, nil),
		makeResult(ActionSell, 0.80, nil),
	}
	resp, ok := majorityVote(results)
	if !ok {
		t.Fatal("majorityVote should succeed")
	}
	if resp.Action != ActionSell {
		t.Fatalf("action = %q, want SELL on WAIT-SELL tie (directional wins)", resp.Action)
	}
}

func TestMajorityVote_TieTradeVsSkipReturnsSkip(t *testing.T) {
	// ORB fallback: TRADE and SKIP tied → SKIP wins. The conservative ORB gate
	// must hold on the arbiter-fallback path — a split candidate must not become
	// executable just because the arbiter failed (PR #346 review). This is
	// deliberately the opposite of the stock WAIT-vs-directional flip.
	results := []AnalystResult{
		makeResult(ActionTrade, 0.80, nil),
		makeResult(ActionSkip, 0.80, nil),
	}
	resp, ok := majorityVote(results)
	if !ok {
		t.Fatal("majorityVote should succeed")
	}
	if resp.Action != ActionSkip {
		t.Fatalf("action = %q, want SKIP on TRADE-SKIP tie (ORB stays conservative)", resp.Action)
	}
}

func TestMajorityVote_TieWaitVsHoldReturnsHold(t *testing.T) {
	// WAIT and HOLD tied → HOLD wins (more conservative; no confirmed catalyst).
	results := []AnalystResult{
		makeResult(ActionWait, 0.75, nil),
		makeResult(ActionHold, 0.75, nil),
	}
	resp, ok := majorityVote(results)
	if !ok {
		t.Fatal("majorityVote should succeed")
	}
	if resp.Action != ActionHold {
		t.Fatalf("action = %q, want HOLD on WAIT-HOLD tie (most conservative)", resp.Action)
	}
}

func TestMajorityVote_AllWait(t *testing.T) {
	// Unanimous WAIT — fast path should also catch this but majorityVote handles it.
	results := []AnalystResult{
		makeResult(ActionWait, 0.85, nil),
		makeResult(ActionWait, 0.80, nil),
		makeResult(ActionWait, 0.78, nil),
	}
	resp, ok := majorityVote(results)
	if !ok {
		t.Fatal("majorityVote should succeed")
	}
	if resp.Action != ActionWait {
		t.Fatalf("action = %q, want WAIT", resp.Action)
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

// TestArbiterLabel_UniqueUpToReasonableSize asserts that arbiterLabel produces
// distinct labels across the full Excel-column rollover range. The bound is
// 1000 to cover the 702→AAA transition where the round-2 fixup silently
// collided with i=26 (both → "Model-AA"). Issue #321 PR #336.
func TestArbiterLabel_UniqueUpToReasonableSize(t *testing.T) {
	const N = 1000
	seen := make(map[string]int, N)
	for i := 0; i < N; i++ {
		l := arbiterLabel(i)
		if prev, dup := seen[l]; dup {
			t.Fatalf("arbiterLabel(%d)=%q duplicates label from i=%d", i, l, prev)
		}
		seen[l] = i
	}
	// Sanity: first six labels are Model-A..Model-F.
	for i, want := range []string{"Model-A", "Model-B", "Model-C", "Model-D", "Model-E", "Model-F"} {
		if got := arbiterLabel(i); got != want {
			t.Errorf("arbiterLabel(%d)=%q, want %q", i, got, want)
		}
	}
}

// TestArbiterLabel_NoCollisionAtThreeLetterTransition explicitly guards the
// 702-index wrap that the round-2 fixup got wrong: bijective base-26 must
// emit "Model-AAA" at i=702, not "Model-AA" (which is i=26). Issue #321
// PR #336 (Copilot review).
func TestArbiterLabel_NoCollisionAtThreeLetterTransition(t *testing.T) {
	cases := []struct {
		i    int
		want string
	}{
		{0, "Model-A"},
		{25, "Model-Z"},
		{26, "Model-AA"},
		{27, "Model-AB"},
		{51, "Model-AZ"},
		{52, "Model-BA"},
		{701, "Model-ZZ"},
		{702, "Model-AAA"},
		{703, "Model-AAB"},
	}
	for _, c := range cases {
		if got := arbiterLabel(c.i); got != c.want {
			t.Errorf("arbiterLabel(%d)=%q, want %q", c.i, got, c.want)
		}
	}
	if arbiterLabel(701) == arbiterLabel(702) {
		t.Errorf("arbiterLabel(701) must differ from arbiterLabel(702): got %q", arbiterLabel(701))
	}
	if arbiterLabel(26) == arbiterLabel(702) {
		t.Errorf("arbiterLabel(26) must differ from arbiterLabel(702): got %q", arbiterLabel(26))
	}
}

// countingAnalyst counts Analyze invocations and returns a fixed BUY response.
type countingAnalyst struct {
	calls int
	name  string
}

func (c *countingAnalyst) Analyze(_ context.Context, _ AnalysisRequest) (AnalysisResponse, ModelCallMeta, error) {
	c.calls++
	return AnalysisResponse{Action: ActionBuy, Confidence: 0.7, Reasoning: "vote"}, ModelCallMeta{Provider: c.name, Model: "m"}, nil
}
func (c *countingAnalyst) ProviderName() string { return c.name }
func (c *countingAnalyst) ModelName() string    { return "m" }
func (c *countingAnalyst) IsAvailable() bool    { return true }

// TestAnalyzeEnsembleWithSet_DoesNotInvokeArbiter is the regression guard for
// the Issue #321 PR #336 alt-pass fix: the multi-timeframe alt pass must use
// AnalyzeEnsembleWithSet (analyst fan-out only) rather than AnalyzeFull, so
// the arbiter is not consulted twice for every borderline WAIT. This test
// fans out to 3 analysts and verifies that the arbiter — wired on the
// Analyzer — is never asked for a vote.
func TestAnalyzeEnsembleWithSet_DoesNotInvokeArbiter(t *testing.T) {
	cap := &captureArbiter{}
	a1 := &countingAnalyst{name: "a1"}
	a2 := &countingAnalyst{name: "a2"}
	a3 := &countingAnalyst{name: "a3"}
	an := &Analyzer{
		analysts: []ModelProvider{a1, a2, a3},
		arbiter:  cap,
	}
	results, err := an.AnalyzeEnsembleWithSet(context.Background(), AnalysisRequest{Ticker: "X"}, an.analysts)
	if err != nil {
		t.Fatalf("AnalyzeEnsembleWithSet returned error: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("expected 3 analyst results, got %d", len(results))
	}
	if a1.calls != 1 || a2.calls != 1 || a3.calls != 1 {
		t.Fatalf("expected each analyst to be called exactly once: a1=%d a2=%d a3=%d", a1.calls, a2.calls, a3.calls)
	}
	if cap.captured != "" {
		t.Fatalf("arbiter must NOT be invoked by AnalyzeEnsembleWithSet, got captured prompt: %q", cap.captured)
	}
}

// TestArbitrate_PromptHasUniqueLabelsForLargeSet asserts that every label in
// the arbiter prompt is unique even when the combined analyst set exceeds the
// old hardcoded slice of 5 (which previously made "Model-A" appear twice for
// 6 results). Issue #321 PR #336.
func TestArbitrate_PromptHasUniqueLabelsForLargeSet(t *testing.T) {
	results := make([]AnalystResult, 0, 8)
	for i := 0; i < 8; i++ {
		r := makeResult(ActionBuy, 0.7, nil)
		r.Provider = fmt.Sprintf("p%d", i)
		r.Model = fmt.Sprintf("m%d", i)
		results = append(results, r)
	}
	cap := &captureArbiter{}
	if _, err := Arbitrate(context.Background(), cap, AnalysisRequest{Ticker: "TEST"}, results); err != nil {
		t.Fatalf("Arbitrate returned error: %v", err)
	}
	if cap.captured == "" {
		t.Fatal("expected arbiter prompt to be captured")
	}
	// Extract all "Model-..." labels from the prompt.
	re := regexp.MustCompile(`Model-[A-Z]+`)
	matches := re.FindAllString(cap.captured, -1)
	if len(matches) < 8 {
		t.Fatalf("expected at least 8 Model-* labels in prompt, got %d (%v)", len(matches), matches)
	}
	seen := map[string]int{}
	for _, m := range matches {
		seen[m]++
	}
	for label, count := range seen {
		if count > 1 {
			t.Errorf("label %q appears %d times in arbiter prompt — labels must be unique", label, count)
		}
	}
	if len(seen) < 8 {
		t.Fatalf("expected ≥ 8 unique labels for 8 analysts, got %d (%v)", len(seen), seen)
	}
}

// TestArbiterLabel_TwoLetterRollover covers the >26 case to make sure the
// Excel-style rollover is correct and never collides with the single-letter
// labels.
func TestArbiterLabel_TwoLetterRollover(t *testing.T) {
	if got := arbiterLabel(26); got != "Model-AA" {
		t.Errorf("arbiterLabel(26)=%q, want Model-AA", got)
	}
	if got := arbiterLabel(27); got != "Model-AB" {
		t.Errorf("arbiterLabel(27)=%q, want Model-AB", got)
	}
	if got := arbiterLabel(51); got != "Model-AZ" {
		t.Errorf("arbiterLabel(51)=%q, want Model-AZ", got)
	}
	if got := arbiterLabel(52); got != "Model-BA" {
		t.Errorf("arbiterLabel(52)=%q, want Model-BA", got)
	}
}

// TestArbiterSystemPrompt_Calibration guards the prompt-calibration bundle on
// the arbiter: its confidence is what the entry gate reads on every arbitrated
// signal, so it must share the analysts' anchors and R:R filter, cap size_pct
// at the analyst/risk limit (5.0, was 10.0), and not leak raw format verbs
// (the old plain-string prompt showed literal "%.2f" and "20%%+").
func TestArbiterSystemPrompt_Calibration(t *testing.T) {
	sys := arbiterSystemPrompt()
	if !strings.Contains(sys, "0.70 = clear edge") {
		t.Error("arbiter prompt is missing the confidence anchors")
	}
	if !strings.Contains(sys, "R:R SELF-FILTER (MANDATORY)") {
		t.Error("arbiter prompt is missing the R:R self-filter")
	}
	if !strings.Contains(sys, `"size_pct": <0.0–5.0>`) {
		t.Error("arbiter size_pct cap must be 5.0 to match the analyst schema and risk.max_position_pct")
	}
	if strings.Contains(sys, "%.2f") || strings.Contains(sys, "%%") {
		t.Error("arbiter prompt leaks raw format verbs")
	}
}

// TestBuildArbiterUserMessage_SignalPrice asserts the arbiter user message
// defines signal_price for the R:R self-filter (Codex #391 P2): with candles it
// names the latest close; without, it explicitly says the price is unavailable
// rather than leaving the filter to compute against a guess.
func TestBuildArbiterUserMessage_SignalPrice(t *testing.T) {
	withPrice := buildArbiterUserMessage("AAPL", 187.42, "(analyses)")
	if !strings.Contains(withPrice, "Current price (latest close): 187.42") {
		t.Error("arbiter user message missing the signal_price definition line")
	}
	noPrice := buildArbiterUserMessage("AAPL", 0, "(analyses)")
	if !strings.Contains(noPrice, "Current price: unavailable") {
		t.Error("arbiter user message must state when the current price is unavailable")
	}
}
