package engine

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/analysis"
	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/cost"
	"github.com/romeo-folie/trade-engine/internal/data"
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

// TestShouldRunSecondPass_AltSelfLoopSkipped: when the alt-map points a
// timeframe at itself (e.g. mis-config `1h: "1h"`), the second pass would
// re-analyse the same timeframe — doubling token cost for zero new
// information. Skip rather than fire. Issue #321 PR #336 (Copilot review).
func TestShouldRunSecondPass_AltSelfLoopSkipped(t *testing.T) {
	cfg := borderlineCfg()
	cfg.MultiTimeframeAltMap = map[string]string{"1h": "1h"}
	resp := analysis.AnalysisResponse{Action: analysis.ActionWait, Confidence: 0.62}
	if alt, ok := shouldRunSecondPass(cfg, resp, "1h", false); ok {
		t.Fatalf("expected self-loop alt mapping to skip, got alt=%q ok=true", alt)
	}
	// Case-insensitive variant: "1H" → "1h" still considered self-loop.
	cfg.MultiTimeframeAltMap = map[string]string{"1h": " 1H "}
	if alt, ok := shouldRunSecondPass(cfg, resp, "1h", false); ok {
		t.Fatalf("expected normalised self-loop to skip, got alt=%q ok=true", alt)
	}
}

// TestLoadAltCandles_FetcherInvokedBeforeLoad asserts the on-demand fetch
// runs before the loader reads, so the alt-tf bars are fresh even when the
// watchlist item didn't list the alt timeframe. Issue #321 PR #336 (Copilot).
func TestLoadAltCandles_FetcherInvokedBeforeLoad(t *testing.T) {
	var order []string
	fetcher := func(_ context.Context, ticker string, tf data.Timeframe) {
		if ticker != "AAPL" || tf != data.Timeframe("4h") {
			t.Errorf("fetcher called with wrong args: ticker=%q tf=%q", ticker, tf)
		}
		order = append(order, "fetch")
	}
	loader := func(_ context.Context, _ string, _ data.Timeframe, _, _ time.Time) ([]data.Candle, error) {
		order = append(order, "load")
		// Return enough fresh candles to pass the warmup gate.
		out := make([]data.Candle, 25)
		now := time.Now().UTC()
		for i := range out {
			out[i] = data.Candle{Ticker: "AAPL", Timeframe: "4h", Timestamp: now.Add(-time.Duration(25-i) * 4 * time.Hour), Close: 100, Open: 100, High: 101, Low: 99, Volume: 1000}
		}
		return out, nil
	}
	candles, ok := loadAltCandles(context.Background(), "AAPL", "4h", fetcher, loader)
	if !ok {
		t.Fatalf("expected ok=true with fresh candles available")
	}
	if len(candles) != 25 {
		t.Fatalf("expected 25 candles, got %d", len(candles))
	}
	if len(order) != 2 || order[0] != "fetch" || order[1] != "load" {
		t.Fatalf("expected fetch-then-load ordering, got %v", order)
	}
}

// TestLoadAltCandles_FetchFreshBars: when the loader returns enough
// candles after the on-demand fetch, the helper proceeds. This is the
// "fresh after fetch" branch — proves the fetch closes the staleness gap.
func TestLoadAltCandles_FetchFreshBars(t *testing.T) {
	var fetchCalls int
	fetcher := func(_ context.Context, _ string, _ data.Timeframe) { fetchCalls++ }
	loader := func(_ context.Context, _ string, _ data.Timeframe, _, _ time.Time) ([]data.Candle, error) {
		// Simulate the fetch having just upserted fresh bars.
		out := make([]data.Candle, 30)
		for i := range out {
			out[i].Timestamp = time.Now().UTC().Add(-time.Duration(30-i) * time.Hour)
		}
		return out, nil
	}
	if _, ok := loadAltCandles(context.Background(), "AAPL", "1h", fetcher, loader); !ok {
		t.Fatalf("expected ok=true after fresh fetch")
	}
	if fetchCalls != 1 {
		t.Fatalf("expected exactly 1 fetch call, got %d", fetchCalls)
	}
}

// TestLoadAltCandles_StillStaleAfterFetchSkips: if the on-demand fetch fails
// upstream and the loader returns too few bars, we must not push outdated
// data into the arbiter — fall back to the original WAIT (return ok=false).
func TestLoadAltCandles_StillStaleAfterFetchSkips(t *testing.T) {
	fetcher := func(_ context.Context, _ string, _ data.Timeframe) {} // simulated no-op fetch
	loader := func(_ context.Context, _ string, _ data.Timeframe, _, _ time.Time) ([]data.Candle, error) {
		// Only 5 bars available — below the 20-bar warmup threshold.
		return make([]data.Candle, 5), nil
	}
	if _, ok := loadAltCandles(context.Background(), "AAPL", "1h", fetcher, loader); ok {
		t.Fatalf("expected ok=false when alt candles remain insufficient after fetch")
	}
}

// TestLoadAltCandles_LoaderErrorFallsBack: a transient DB error surfacing
// from the loader must short-circuit to fallback (no panic, ok=false).
func TestLoadAltCandles_LoaderErrorFallsBack(t *testing.T) {
	loader := func(_ context.Context, _ string, _ data.Timeframe, _, _ time.Time) ([]data.Candle, error) {
		return nil, errors.New("db down")
	}
	if _, ok := loadAltCandles(context.Background(), "AAPL", "1h", nil, loader); ok {
		t.Fatalf("expected ok=false on loader error")
	}
}

// TestLoadAltCandles_NilFetcherStillLoads: when no fetcher is wired (e.g.
// engines constructed in test fixtures), loadAltCandles must still attempt
// the load rather than silently no-op.
func TestLoadAltCandles_NilFetcherStillLoads(t *testing.T) {
	var loadCalls int
	loader := func(_ context.Context, _ string, _ data.Timeframe, _, _ time.Time) ([]data.Candle, error) {
		loadCalls++
		return make([]data.Candle, 25), nil
	}
	if _, ok := loadAltCandles(context.Background(), "AAPL", "1h", nil, loader); !ok {
		t.Fatalf("expected ok=true with nil fetcher and sufficient candles")
	}
	if loadCalls != 1 {
		t.Fatalf("expected exactly 1 load call, got %d", loadCalls)
	}
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
