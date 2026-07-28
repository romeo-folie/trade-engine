package prompts

import (
	"strings"
	"testing"
	"time"
)

// TestLenses_FiltersToAnalysisLenses: the watchlist strategies column mixes
// analytical lenses with entry-gate tags (the scanner appends
// "pullback_strength"). Only lenses may be offered to an analyst as a choice.
func TestLenses_FiltersToAnalysisLenses(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{"drops entry-gate tag", []string{"momentum", "pullback_strength"}, []string{"momentum"}},
		{"drops unknown label", []string{"mean_reversion", "swing"}, []string{"mean_reversion"}},
		{"preserves order", []string{"news_sentiment", "momentum"}, []string{"news_sentiment", "momentum"}},
		{"dedupes", []string{"momentum", "momentum"}, []string{"momentum"}},
		{"normalizes case and space", []string{" Mean_Reversion "}, []string{"mean_reversion"}},
		{"empty falls back", nil, []string{defaultLens}},
		{"all-unusable falls back", []string{"pullback_strength", "swing"}, []string{defaultLens}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Lenses(tc.in)
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("Lenses(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// TestCandidateLensBlock_ListsEveryAdmissibleLens is the core of the multi-lens
// change: an analyst must see every candidate, not just the configured primary.
func TestCandidateLensBlock_ListsEveryAdmissibleLens(t *testing.T) {
	sys := buildSystem(Lenses([]string{"mean_reversion", "trend_following"}))
	if !strings.Contains(sys, "CANDIDATE LENSES: mean_reversion, trend_following") {
		t.Errorf("system prompt does not list both admissible lenses:\n%s", sys)
	}
	// The identity framing is what created advocacy pressure — it must be gone.
	if strings.Contains(sys, `running the "`) {
		t.Error("system prompt still injects the strategy as an identity")
	}
}

// TestCandidateLensBlock_EscapeHatch guards the answer that did not previously
// exist: no admissible lens fits, so HOLD on regime grounds is correct.
func TestCandidateLensBlock_EscapeHatch(t *testing.T) {
	for _, sys := range []string{
		buildSystem(Lenses([]string{"mean_reversion"})),
		buildMomentumSystem(Lenses([]string{"momentum"})),
	} {
		if !strings.Contains(sys, "regime mismatch:") {
			t.Error("prompt is missing the regime-mismatch escape hatch")
		}
		if !strings.Contains(sys, `set "selected_strategy" to "none"`) {
			t.Error("prompt does not tell the analyst how to signal no-fit")
		}
		if !strings.Contains(sys, "not a failure to find a trade") {
			t.Error("prompt does not legitimise the no-fit answer")
		}
	}
}

// TestCandidateLensBlock_LensShoppingGuard: presenting a menu of lenses is also
// presenting a menu of justifications. The explicit-rejection requirement is
// the only thing standing between those two readings — without it the change
// is strictly more permissive than the single-assignment framing it replaced.
func TestCandidateLensBlock_LensShoppingGuard(t *testing.T) {
	sys := buildSystem(Lenses([]string{"momentum", "trend_following"}))
	for _, want := range []string{
		"why every REJECTED lens does not fit",
		"Selecting a lens because it yields a tradeable setup",
		"is the specific failure this structure exists",
	} {
		if !strings.Contains(sys, want) {
			t.Errorf("lens-shopping guard missing %q", want)
		}
	}
}

// TestOutputSchema_ReasoningPrecedesAction: models generate left-to-right, so
// key order decides whether the reasoning produces the decision or merely
// rationalises one already emitted.
func TestOutputSchema_ReasoningPrecedesAction(t *testing.T) {
	reasoning := strings.Index(OutputSchema, `"reasoning"`)
	action := strings.Index(OutputSchema, `"action"`)
	if reasoning < 0 || action < 0 {
		t.Fatalf("schema missing reasoning (%d) or action (%d)", reasoning, action)
	}
	if reasoning > action {
		t.Error("\"reasoning\" must precede \"action\" in the schema, else it is post-hoc")
	}
	for _, want := range []string{`"selected_strategy"`, `"rejected_strategies"`, `"regime"`} {
		if !strings.Contains(OutputSchema, want) {
			t.Errorf("schema missing %s", want)
		}
	}
}

// TestLensGuidance_AttachedWhenCandidate: guidance used to key off the single
// configured strategy. Now that any admissible lens may be selected, the
// news_sentiment anti-chase block must ride along whenever it is a CANDIDATE —
// including when it is not first.
func TestLensGuidance_AttachedWhenCandidate(t *testing.T) {
	const marker = "STRATEGY GUIDANCE — news_sentiment"
	sys := buildSystem(Lenses([]string{"momentum", "news_sentiment"}))
	if !strings.Contains(sys, marker) {
		t.Error("news_sentiment guidance missing when it is a non-primary candidate")
	}
	if sys := buildSystem(Lenses([]string{"momentum", "trend_following"})); strings.Contains(sys, marker) {
		t.Error("news_sentiment guidance leaked into a set that does not admit it")
	}
}

// TestPrompts_NoLeakedFormatVerbs_MultiLens re-runs the %%-escaping guard over
// the new multi-lens rendering path, where a fresh Sprintf argument was added.
func TestPrompts_NoLeakedFormatVerbs_MultiLens(t *testing.T) {
	lenses := Lenses([]string{"momentum", "news_sentiment", "trend_following"})
	rendered := map[string]string{
		"generalist": buildSystem(lenses),
		"momentum":   buildMomentumSystem(lenses),
	}
	for _, role := range []string{"fundamental", "technical", "sentiment", "risk_manager"} {
		sys, _ := BuildRolePrompt(role, "AAPL", lenses, "1h", nil, Indicators{}, nil, Portfolio{}, "", nil, "")
		rendered["role:"+role] = sys
	}
	for name, text := range rendered {
		if strings.Contains(text, "%%") {
			t.Errorf("%s prompt leaks a double-escaped percent", name)
		}
		if strings.Contains(text, "%!") {
			t.Errorf("%s prompt has a Sprintf arity error (%%! verb)", name)
		}
		if !strings.Contains(text, "CANDIDATE LENSES:") {
			t.Errorf("%s prompt is missing the candidate-lens contract", name)
		}
	}
}

// TestBuildUser_RendersEveryProvidedCandle is the regression guard for the
// hardcoded 10-candle cap that used to sit inside buildUser. It silently
// overrode both analysis.max_candles_per_prompt and
// history_window_by_strategy, which made the widened candidate-lens window a
// no-op: the engine could pass 20 candles and the analyst would still see 10.
// The cap now lives in exactly one config-driven place (the engine).
func TestBuildUser_RendersEveryProvidedCandle(t *testing.T) {
	const n = 20
	candles := make([]Candle, n)
	base := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	for i := range candles {
		candles[i] = Candle{
			Timestamp: base.Add(-time.Duration(i) * time.Hour),
			Open:      1, High: 2, Low: 0.5, Close: 1.5, Volume: 100, VWAP: 1.2,
		}
	}
	user := buildUser("KO", "1d", candles, Indicators{}, nil, Portfolio{}, "", nil, "")
	// One row per candle, each stamped with its own timestamp.
	for i := range candles {
		stamp := candles[i].Timestamp.Format("2006-01-02 15:04")
		if !strings.Contains(user, stamp) {
			t.Errorf("candle %d (%s) did not reach the prompt", i, stamp)
		}
	}
}

// TestExtractionContract_NestedRejectedMap documents why the fenced-JSON regex
// tolerates the nested rejected_strategies object: the lazy quantifier in
// jsonFenceRe must still satisfy the trailing fence, so it expands past the
// nested closing brace to the outer one. Asserted here because a future
// "simplification" of that regex would silently drop the field.
func TestExtractionContract_NestedRejectedMap(t *testing.T) {
	lenses := Lenses([]string{"mean_reversion", "trend_following"})
	sys := buildSystem(lenses)
	if !strings.Contains(sys, `"rejected_strategies": {"<lens>": "<one clause on why it does not fit>"}`) {
		t.Error("schema no longer asks for a nested rejected_strategies object")
	}
}
