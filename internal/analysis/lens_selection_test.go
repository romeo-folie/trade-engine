package analysis

import "testing"

// TestExtractAnalysisJSON_LensSelectionFields parses the lens-selection block
// the candidate-lens contract asks for (issue #509).
func TestExtractAnalysisJSON_LensSelectionFields(t *testing.T) {
	raw := `{
	  "reasoning": "ADX 15.1 with a 2y uptrend; no band touch",
	  "regime": "trending",
	  "selected_strategy": "trend_following",
	  "rejected_strategies": {"mean_reversion": "no band touch and RSI mid-range"},
	  "action": "BUY",
	  "confidence": 0.72,
	  "size_pct": 2.0,
	  "stop_loss": 84.10,
	  "take_profit": 95.00
	}`
	got, err := extractAnalysisJSON(raw)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if got.Regime != "trending" {
		t.Errorf("regime = %q, want trending", got.Regime)
	}
	if got.SelectedStrategy != "trend_following" {
		t.Errorf("selected_strategy = %q, want trend_following", got.SelectedStrategy)
	}
	if got.RejectedStrategies["mean_reversion"] == "" {
		t.Errorf("rejected_strategies not parsed: %#v", got.RejectedStrategies)
	}
	if got.Action != ActionBuy || got.Confidence != 0.72 {
		t.Errorf("existing fields regressed: action=%q conf=%v", got.Action, got.Confidence)
	}
}

// TestExtractAnalysisJSON_NormalizesLensCase: the watchlist stores lower-case
// labels, so a model echoing "Mean_Reversion" must still compare equal to the
// configured strategy — otherwise the mislabelling detector reports false
// divergence on nothing but capitalisation.
func TestExtractAnalysisJSON_NormalizesLensCase(t *testing.T) {
	raw := `{"reasoning":"r","regime":" Ranging ","selected_strategy":" Mean_Reversion ",
	  "rejected_strategies":{" Trend_Following ":" no trend "},
	  "action":"HOLD","confidence":0.4}`
	got, err := extractAnalysisJSON(raw)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if got.Regime != "ranging" {
		t.Errorf("regime = %q, want ranging", got.Regime)
	}
	if got.SelectedStrategy != "mean_reversion" {
		t.Errorf("selected_strategy = %q, want mean_reversion", got.SelectedStrategy)
	}
	if _, ok := got.RejectedStrategies["trend_following"]; !ok {
		t.Errorf("rejected_strategies key not normalized: %#v", got.RejectedStrategies)
	}
}

// TestExtractAnalysisJSON_RegimeMismatchHold is the escape hatch arriving over
// the wire: no admissible lens fit, so the analyst returns HOLD with
// selected_strategy "none". It must parse, and the non-directional invariant
// must still zero the sizing fields.
func TestExtractAnalysisJSON_RegimeMismatchHold(t *testing.T) {
	raw := `{"reasoning":"regime mismatch: ADX 15.1 contradicts mean_reversion",
	  "regime":"trending","selected_strategy":"none",
	  "rejected_strategies":{"mean_reversion":"price trending, not mean-reverting"},
	  "action":"HOLD","confidence":0.35,"size_pct":2.0,"stop_loss":80,"take_profit":90}`
	got, err := extractAnalysisJSON(raw)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if got.SelectedStrategy != "none" {
		t.Errorf("selected_strategy = %q, want none", got.SelectedStrategy)
	}
	if got.SizePct != 0 || got.StopLoss != 0 || got.TakeProfit != 0 {
		t.Errorf("HOLD must zero sizing: size=%v sl=%v tp=%v", got.SizePct, got.StopLoss, got.TakeProfit)
	}
}

// TestExtractAnalysisJSON_LensFieldsSurviveTruncation: reasoning-first means a
// truncated response now loses "action" LAST rather than first. The regex
// recovery path must still pick up the lens fields, which precede it.
func TestExtractAnalysisJSON_LensFieldsSurviveTruncation(t *testing.T) {
	truncated := `{"reasoning":"long analysis text that never terminates properly",
	  "regime":"ranging","selected_strategy":"mean_reversion",
	  "action":"BUY","confidence":0.71,"size_pct":2.0,"stop_loss":10.5,"take_profit":`
	got, err := extractAnalysisJSON(truncated)
	if err != nil {
		t.Fatalf("recovery failed on truncated output: %v", err)
	}
	if got.Action != ActionBuy {
		t.Errorf("action = %q, want BUY", got.Action)
	}
	if got.SelectedStrategy != "mean_reversion" {
		t.Errorf("selected_strategy lost in recovery: %q", got.SelectedStrategy)
	}
	if got.Regime != "ranging" {
		t.Errorf("regime lost in recovery: %q", got.Regime)
	}
}

// TestExtractAnalysisJSON_OmittedLensFieldsDoNotFail: the fields are
// observation-only. A model that ignores them must keep its vote rather than
// being dropped from the ensemble.
func TestExtractAnalysisJSON_OmittedLensFieldsDoNotFail(t *testing.T) {
	got, err := extractAnalysisJSON(`{"action":"BUY","confidence":0.7,"reasoning":"r"}`)
	if err != nil {
		t.Fatalf("legacy-shaped response must still parse: %v", err)
	}
	if got.Action != ActionBuy {
		t.Errorf("action = %q, want BUY", got.Action)
	}
	if got.SelectedStrategy != "" || got.Regime != "" {
		t.Errorf("absent fields should stay empty, got %q / %q", got.SelectedStrategy, got.Regime)
	}
}

// TestLensSet_FallsBackToPrimary: callers that never learned about Strategies
// (tests, one-off tooling) must keep single-lens behaviour instead of silently
// dropping to the package default.
func TestLensSet_FallsBackToPrimary(t *testing.T) {
	if got := (AnalysisRequest{Strategy: "momentum"}).LensSet(); len(got) != 1 || got[0] != "momentum" {
		t.Errorf("LensSet() = %v, want [momentum]", got)
	}
	full := AnalysisRequest{Strategy: "momentum", Strategies: []string{"momentum", "trend_following"}}
	if got := full.LensSet(); len(got) != 2 {
		t.Errorf("LensSet() = %v, want both lenses", got)
	}
	if got := (AnalysisRequest{}).LensSet(); got != nil {
		t.Errorf("LensSet() = %v, want nil for an empty request", got)
	}
}

// TestExtractAnalysisJSON_FencedNestedRejectedMap: models commonly wrap output
// in a ```json fence, and rejected_strategies is the first NESTED object the
// schema has ever asked for. jsonFenceRe uses a lazy quantifier, so the concern
// is that it stops at the nested closing brace — it does not, because the match
// must still satisfy the trailing fence. Asserted so a future edit to that
// regex cannot silently drop the field.
func TestExtractAnalysisJSON_FencedNestedRejectedMap(t *testing.T) {
	raw := "```json\n" + `{
  "reasoning": "trend intact",
  "regime": "trending",
  "selected_strategy": "trend_following",
  "rejected_strategies": {"mean_reversion": "no band touch", "momentum": "volume flat"},
  "action": "BUY",
  "confidence": 0.72,
  "size_pct": 2.0,
  "stop_loss": 84.1,
  "take_profit": 95.0
}` + "\n```"
	got, err := extractAnalysisJSON(raw)
	if err != nil {
		t.Fatalf("fenced response failed to parse: %v", err)
	}
	if len(got.RejectedStrategies) != 2 {
		t.Fatalf("rejected_strategies lost through the fence path: %#v", got.RejectedStrategies)
	}
	if got.RejectedStrategies["mean_reversion"] != "no band touch" {
		t.Errorf("rejection reason mangled: %#v", got.RejectedStrategies)
	}
	if got.SelectedStrategy != "trend_following" || got.Action != ActionBuy {
		t.Errorf("selected=%q action=%q", got.SelectedStrategy, got.Action)
	}
	if got.TakeProfit != 95.0 {
		t.Errorf("trailing fields after the nested map were truncated: tp=%v", got.TakeProfit)
	}
}
