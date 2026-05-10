package engine

import (
	"errors"
	"testing"

	"github.com/romeo-folie/trade-engine/internal/analysis"
	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/cost"
)

func borderlineCfg() config.AnalysisConfig {
	return config.AnalysisConfig{
		MultiTimeframeBorderlineEnabled: true,
		MultiTimeframeAltMap: map[string]string{
			"1h": "4h",
			"4h": "1h",
			"1d": "1h",
		},
		MultiTimeframeBorderlineConfMin: 0.55,
		MultiTimeframeBorderlineConfMax: 0.70,
	}
}

func TestShouldRunSecondPass_BorderlineWAITTriggers(t *testing.T) {
	cfg := borderlineCfg()
	resp := analysis.AnalysisResponse{Action: analysis.ActionWait, Confidence: 0.62}
	alt, ok := shouldRunSecondPass(cfg, resp, "1h", false)
	if !ok {
		t.Fatalf("expected second pass to trigger for borderline WAIT, got ok=false")
	}
	if alt != "4h" {
		t.Fatalf("expected alt=4h, got %q", alt)
	}
}

func TestShouldRunSecondPass_BoundsAreInclusive(t *testing.T) {
	cfg := borderlineCfg()
	for _, conf := range []float64{0.55, 0.70} {
		resp := analysis.AnalysisResponse{Action: analysis.ActionWait, Confidence: conf}
		if _, ok := shouldRunSecondPass(cfg, resp, "1d", false); !ok {
			t.Fatalf("expected second pass at conf=%.2f (inclusive bound)", conf)
		}
	}
}

func TestShouldRunSecondPass_OutsideBandSkipped(t *testing.T) {
	cfg := borderlineCfg()
	cases := []float64{0.10, 0.50, 0.71, 0.90}
	for _, conf := range cases {
		resp := analysis.AnalysisResponse{Action: analysis.ActionWait, Confidence: conf}
		if _, ok := shouldRunSecondPass(cfg, resp, "1h", false); ok {
			t.Fatalf("expected skip for conf=%.2f outside band", conf)
		}
	}
}

func TestShouldRunSecondPass_NonWAITSkipped(t *testing.T) {
	cfg := borderlineCfg()
	for _, action := range []analysis.Action{analysis.ActionBuy, analysis.ActionSell, analysis.ActionHold} {
		resp := analysis.AnalysisResponse{Action: action, Confidence: 0.62}
		if _, ok := shouldRunSecondPass(cfg, resp, "1h", false); ok {
			t.Fatalf("expected skip for non-WAIT action %s", action)
		}
	}
}

func TestShouldRunSecondPass_RecursionGuard(t *testing.T) {
	cfg := borderlineCfg()
	resp := analysis.AnalysisResponse{Action: analysis.ActionWait, Confidence: 0.62}
	// isSecondPass=true must always block, regardless of other state.
	if _, ok := shouldRunSecondPass(cfg, resp, "1h", true); ok {
		t.Fatalf("expected recursion guard to block second pass when isSecondPass=true")
	}
}

func TestShouldRunSecondPass_DisabledFlag(t *testing.T) {
	cfg := borderlineCfg()
	cfg.MultiTimeframeBorderlineEnabled = false
	resp := analysis.AnalysisResponse{Action: analysis.ActionWait, Confidence: 0.62}
	if _, ok := shouldRunSecondPass(cfg, resp, "1h", false); ok {
		t.Fatalf("expected disabled flag to block second pass")
	}
}

func TestShouldRunSecondPass_UnknownTimeframeFallsBack(t *testing.T) {
	cfg := borderlineCfg()
	resp := analysis.AnalysisResponse{Action: analysis.ActionWait, Confidence: 0.62}
	// "5m" is not in the alt-map — must skip without crashing.
	if alt, ok := shouldRunSecondPass(cfg, resp, "5m", false); ok || alt != "" {
		t.Fatalf("expected skip for unmapped timeframe, got alt=%q ok=%v", alt, ok)
	}
}

func TestShouldRunSecondPass_NilAltMap(t *testing.T) {
	cfg := borderlineCfg()
	cfg.MultiTimeframeAltMap = nil
	resp := analysis.AnalysisResponse{Action: analysis.ActionWait, Confidence: 0.62}
	if _, ok := shouldRunSecondPass(cfg, resp, "1h", false); ok {
		t.Fatalf("expected skip when alt map is nil")
	}
}

func TestShouldRunSecondPass_TimeframeCaseInsensitive(t *testing.T) {
	cfg := borderlineCfg()
	resp := analysis.AnalysisResponse{Action: analysis.ActionWait, Confidence: 0.62}
	if alt, ok := shouldRunSecondPass(cfg, resp, "1H", false); !ok || alt != "4h" {
		t.Fatalf("expected case-insensitive lookup, got alt=%q ok=%v", alt, ok)
	}
}

// TestShouldRunSecondPass_AltValueNormalised: when config has an uppercase or
// space-padded alt value (e.g. `1h: " 4H "`), the returned alt must be
// lowercase + trimmed so downstream candle lookups (which use lowercase
// timeframe strings) hit the right rows. Issue #321 PR #336 (Copilot).
func TestShouldRunSecondPass_AltValueNormalised(t *testing.T) {
	cfg := borderlineCfg()
	cfg.MultiTimeframeAltMap = map[string]string{
		"1h": " 4H ",
		"4h": "1D",
	}
	resp := analysis.AnalysisResponse{Action: analysis.ActionWait, Confidence: 0.62}
	if alt, ok := shouldRunSecondPass(cfg, resp, "1h", false); !ok || alt != "4h" {
		t.Fatalf("expected normalised alt=4h, got alt=%q ok=%v", alt, ok)
	}
	if alt, ok := shouldRunSecondPass(cfg, resp, "4h", false); !ok || alt != "1d" {
		t.Fatalf("expected normalised alt=1d, got alt=%q ok=%v", alt, ok)
	}
}

// TestShouldRunSecondPass_WhitespaceOnlyAltSkipped: a whitespace-only alt
// value normalises to "" and must skip rather than fire a doomed candle
// lookup. Issue #321 PR #336.
func TestShouldRunSecondPass_WhitespaceOnlyAltSkipped(t *testing.T) {
	cfg := borderlineCfg()
	cfg.MultiTimeframeAltMap = map[string]string{"1h": "   "}
	resp := analysis.AnalysisResponse{Action: analysis.ActionWait, Confidence: 0.62}
	if alt, ok := shouldRunSecondPass(cfg, resp, "1h", false); ok || alt != "" {
		t.Fatalf("expected whitespace-only alt to skip, got alt=%q ok=%v", alt, ok)
	}
}

func TestCombineForArbiter_TagsReasoningWithTimeframe(t *testing.T) {
	primary := []analysis.AnalystResult{
		{Provider: "p1", Model: "m1", Response: analysis.AnalysisResponse{Action: analysis.ActionWait, Confidence: 0.6, Reasoning: "primary reasoning"}},
		{Provider: "p2", Model: "m2", Err: errors.New("failed"), Response: analysis.AnalysisResponse{Reasoning: "should-not-be-tagged"}},
	}
	alt := []analysis.AnalystResult{
		{Provider: "p3", Model: "m3", Response: analysis.AnalysisResponse{Action: analysis.ActionBuy, Confidence: 0.8, Reasoning: "alt reasoning"}},
	}
	combined := combineForArbiter("1h", "4h", primary, alt)
	if len(combined) != 3 {
		t.Fatalf("expected 3 combined results, got %d", len(combined))
	}
	if got := combined[0].Response.Reasoning; got != "[1h] primary reasoning" {
		t.Errorf("primary tag: got %q", got)
	}
	// Failed primary result's reasoning is preserved untouched (no tag) — the
	// arbiter only sees successful results anyway, so tagging is unnecessary.
	if got := combined[1].Response.Reasoning; got != "should-not-be-tagged" {
		t.Errorf("failed-primary should be untagged: got %q", got)
	}
	if got := combined[2].Response.Reasoning; got != "[4h] alt reasoning" {
		t.Errorf("alt tag: got %q", got)
	}
}

func TestRecordMultiTFCost_BucketsTokensWithSuffix(t *testing.T) {
	tracker := cost.NewTracker()
	e := &Engine{costTracker: tracker}

	result := analysis.AnalyzeResult{
		AnalystResults: []analysis.AnalystResult{
			{Meta: analysis.ModelCallMeta{Provider: "anthropic", Model: "claude", InputTokens: 100, OutputTokens: 50}},
			{Meta: analysis.ModelCallMeta{Provider: "openai", Model: "gpt-4", InputTokens: 80, OutputTokens: 40}},
		},
	}
	e.recordMultiTFCost(result)

	snap := tracker.TakeSnapshot()
	want := map[string]bool{
		"anthropic/claude" + multiTFCostSuffix: false,
		"openai/gpt-4" + multiTFCostSuffix:     false,
	}
	for _, b := range snap.ModelBreakdown {
		key := b.Provider + "/" + b.Model
		if _, ok := want[key]; ok {
			want[key] = true
		} else {
			t.Errorf("unexpected bucket %q (no #mtf2 suffix or wrong key)", key)
		}
	}
	for k, ok := range want {
		if !ok {
			t.Errorf("missing expected bucket %q", k)
		}
	}
}

func TestRecordMultiTFCost_SkipsZeroTokenEntries(t *testing.T) {
	tracker := cost.NewTracker()
	e := &Engine{costTracker: tracker}

	result := analysis.AnalyzeResult{
		AnalystResults: []analysis.AnalystResult{
			{Meta: analysis.ModelCallMeta{Provider: "anthropic", Model: "claude"}}, // 0 tokens
			{Meta: analysis.ModelCallMeta{Provider: "", Model: "no-provider", InputTokens: 1}},
		},
	}
	e.recordMultiTFCost(result)
	snap := tracker.TakeSnapshot()
	if len(snap.ModelBreakdown) != 0 {
		t.Errorf("expected no buckets recorded for zero/empty entries, got %+v", snap.ModelBreakdown)
	}
}

func TestRecordMultiTFCost_NilTrackerSafe(t *testing.T) {
	e := &Engine{}
	// Must not panic.
	e.recordMultiTFCost(analysis.AnalyzeResult{})
}

func TestShouldRunSecondPass_ZeroBandSkipped(t *testing.T) {
	// Defensive: a zero-zero band is treated as misconfiguration and must
	// skip rather than match every conf=0 WAIT.
	cfg := borderlineCfg()
	cfg.MultiTimeframeBorderlineConfMin = 0
	cfg.MultiTimeframeBorderlineConfMax = 0
	resp := analysis.AnalysisResponse{Action: analysis.ActionWait, Confidence: 0.0}
	if _, ok := shouldRunSecondPass(cfg, resp, "1h", false); ok {
		t.Fatalf("expected zero-band misconfig to skip")
	}
}
