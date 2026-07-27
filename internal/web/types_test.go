package web

import (
	"encoding/json"
	"testing"

	"github.com/romeo-folie/trade-engine/internal/analysis"
)

func TestMapAnalystResults_nilInput(t *testing.T) {
	analysts, ensemble := mapAnalystResults(nil)
	if analysts != nil {
		t.Fatal("expected nil analysts for nil input")
	}
	if ensemble != nil {
		t.Fatal("expected nil ensemble for nil input")
	}
}

func TestMapAnalystResults_emptyInput(t *testing.T) {
	analysts, ensemble := mapAnalystResults([]byte{})
	if analysts != nil {
		t.Fatal("expected nil analysts for empty input")
	}
	if ensemble != nil {
		t.Fatal("expected nil ensemble for empty input")
	}
}

func TestMapAnalystResults_validResults(t *testing.T) {
	results := []analysis.AnalystResult{
		{
			Provider: "anthropic",
			Model:    "claude-sonnet-4-5",
			Response: analysis.AnalysisResponse{
				Action:     analysis.ActionBuy,
				Confidence: 0.87,
				Reasoning:  "RSI 68, MACD crossover confirmed",
			},
			Meta: analysis.ModelCallMeta{Model: "claude-sonnet-4-5"},
		},
		{
			Provider: "anthropic",
			Model:    "claude-haiku-3-5",
			Response: analysis.AnalysisResponse{
				Action:     analysis.ActionBuy,
				Confidence: 0.92,
				Reasoning:  "Volume spike 2.1x, price breakout",
			},
			Meta: analysis.ModelCallMeta{Model: "claude-haiku-3-5"},
		},
		{
			Provider: "openai",
			Model:    "gpt-4o",
			Response: analysis.AnalysisResponse{
				Action:     analysis.ActionHold,
				Confidence: 0.55,
				Reasoning:  "Mixed signals, no clear bias",
			},
			Meta: analysis.ModelCallMeta{Model: "gpt-4o"},
		},
	}

	raw, err := json.Marshal(results)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	analysts, ensemble := mapAnalystResults(raw)
	if len(analysts) != 3 {
		t.Fatalf("expected 3 analysts, got %d", len(analysts))
	}

	// Check names
	if analysts[0].Name != "Standard Analysis" {
		t.Errorf("expected Standard Analysis, got %s", analysts[0].Name)
	}
	if analysts[1].Name != "High-Depth Analysis" {
		t.Errorf("expected High-Depth Analysis, got %s", analysts[1].Name)
	}
	if analysts[2].Name != "Deep-Reasoning Analysis" {
		t.Errorf("expected Deep-Reasoning Analysis, got %s", analysts[2].Name)
	}

	// Check verdict mapping
	if analysts[0].Verdict != "LONG" {
		t.Errorf("expected LONG, got %s", analysts[0].Verdict)
	}
	// HOLD stays HOLD — it is a decision to drop the ticker, not an absence of
	// one, and must not be flattened into NEUTRAL alongside WAIT.
	if analysts[2].Verdict != "HOLD" {
		t.Errorf("expected HOLD, got %s", analysts[2].Verdict)
	}

	// Check all active
	for i, a := range analysts {
		if !a.Active {
			t.Errorf("analyst %d should be active", i)
		}
	}

	// Check weight
	expectedWeight := 1.0 / 3.0
	for i, a := range analysts {
		if a.Weight < expectedWeight-0.01 || a.Weight > expectedWeight+0.01 {
			t.Errorf("analyst %d weight: want ~%.3f, got %.3f", i, expectedWeight, a.Weight)
		}
	}

	// Check ensemble
	if ensemble == nil {
		t.Fatal("expected non-nil ensemble")
	}
	if ensemble.Verdict != "LONG" {
		t.Errorf("expected ensemble LONG, got %s", ensemble.Verdict)
	}
	if ensemble.Confidence < 0.5 || ensemble.Confidence > 1.0 {
		t.Errorf("unexpected ensemble confidence: %.3f", ensemble.Confidence)
	}
}

func TestMapAnalystResults_inactiveAnalyst(t *testing.T) {
	results := []analysis.AnalystResult{
		{
			Provider: "anthropic",
			Model:    "claude-sonnet-4-5",
			Response: analysis.AnalysisResponse{
				Action:     analysis.ActionSell,
				Confidence: 0.80,
				Reasoning:  "Bearish divergence",
			},
			Meta: analysis.ModelCallMeta{Model: "claude-sonnet-4-5"},
		},
		{
			// Empty response signals a failed/bypassed analyst
			Provider: "openai",
			Model:    "gpt-4o",
			Response: analysis.AnalysisResponse{},
			Meta:     analysis.ModelCallMeta{Model: "gpt-4o"},
		},
	}

	raw, _ := json.Marshal(results)
	analysts, ensemble := mapAnalystResults(raw)

	if len(analysts) != 2 {
		t.Fatalf("expected 2 analysts, got %d", len(analysts))
	}
	if !analysts[0].Active {
		t.Error("analyst 0 should be active")
	}
	if analysts[1].Active {
		t.Error("analyst 1 should be inactive (empty action)")
	}

	if ensemble.Verdict != "SHORT" {
		t.Errorf("expected ensemble SHORT, got %s", ensemble.Verdict)
	}
}

func TestMapAnalystResults_preservesSpecialistRoles(t *testing.T) {
	results := []analysis.AnalystResult{
		{
			Role: "fundamental",
			Response: analysis.AnalysisResponse{
				Action: analysis.ActionBuy, Confidence: 0.82, Reasoning: "Strong earnings",
			},
			Meta: analysis.ModelCallMeta{Model: "model-a"},
		},
		{
			Role: "risk_manager",
			Response: analysis.AnalysisResponse{
				Action: analysis.ActionHold, Confidence: 0.71, Reasoning: "Stop is too wide",
			},
			Meta: analysis.ModelCallMeta{Model: "model-b"},
		},
	}

	raw, _ := json.Marshal(results)
	analysts, _ := mapAnalystResults(raw)

	if len(analysts) != 2 {
		t.Fatalf("expected 2 analysts, got %d", len(analysts))
	}
	if analysts[0].Role != "fundamental" || analysts[0].Name != "Fundamental" {
		t.Fatalf("fundamental analyst = %+v", analysts[0])
	}
	if analysts[1].Role != "risk_manager" || analysts[1].Name != "Risk Manager" {
		t.Fatalf("risk manager analyst = %+v", analysts[1])
	}
}

func TestMapAnalystResults_tieBreakLongShort(t *testing.T) {
	// When LONG and SHORT have equal votes, ensemble should be NEUTRAL (not LONG).
	results := []analysis.AnalystResult{
		{
			Response: analysis.AnalysisResponse{Action: analysis.ActionBuy, Confidence: 0.80, Reasoning: "Bullish"},
			Meta:     analysis.ModelCallMeta{Model: "model-a"},
		},
		{
			Response: analysis.AnalysisResponse{Action: analysis.ActionSell, Confidence: 0.75, Reasoning: "Bearish"},
			Meta:     analysis.ModelCallMeta{Model: "model-b"},
		},
	}
	raw, _ := json.Marshal(results)
	_, ensemble := mapAnalystResults(raw)

	if ensemble == nil {
		t.Fatal("expected non-nil ensemble")
	}
	// An opposed BUY-vs-SELL tie resolves to HOLD, matching the engine's own
	// rule in internal/analysis/ensemble.go ("no directional bias, fall to
	// HOLD"). It previously reported NEUTRAL, which no engine action produces.
	if ensemble.Verdict != "HOLD" {
		t.Errorf("expected tie-break verdict HOLD, got %s", ensemble.Verdict)
	}
}

func TestTruncate(t *testing.T) {
	short := "hello"
	if got := truncate(short, 120); got != short {
		t.Errorf("expected %q, got %q", short, got)
	}

	long := ""
	for i := 0; i < 200; i++ {
		long += "x"
	}
	got := truncate(long, 120)
	if len([]rune(got)) != 120 {
		t.Errorf("expected 120 runes, got %d", len([]rune(got)))
	}
}

// mapVerdict once returned NEUTRAL for every non-directional action, so the
// signal and analysis side panels could not tell HOLD ("drop this ticker") from
// WAIT ("re-evaluate next tick"), nor an ORB candidate that passed the gate
// (TRADE) from one that was filtered out (SKIP).
func TestMapVerdict(t *testing.T) {
	cases := []struct {
		in   analysis.Action
		want string
	}{
		{analysis.ActionBuy, "LONG"},
		{analysis.ActionSell, "SHORT"},
		{analysis.ActionHold, "HOLD"},
		{analysis.ActionWait, "WAIT"},
		{analysis.ActionTrade, "TRADE"},
		{analysis.ActionSkip, "SKIP"},
		{analysis.ActionAdd, "ADD"},
		// Only a genuinely unclassifiable action stays NEUTRAL. An analyst that
		// errored leaves the action empty.
		{"", "NEUTRAL"},
		{"SOMETHING_NEW", "NEUTRAL"},
	}
	for _, c := range cases {
		if got := mapVerdict(c.in); got != c.want {
			t.Errorf("mapVerdict(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// HOLD and WAIT must never map to the same string, or the panels regress to the
// ambiguity this change exists to remove.
func TestMapVerdictDistinguishesHoldFromWait(t *testing.T) {
	if mapVerdict(analysis.ActionHold) == mapVerdict(analysis.ActionWait) {
		t.Fatalf("HOLD and WAIT both render as %q", mapVerdict(analysis.ActionHold))
	}
	if mapVerdict(analysis.ActionTrade) == mapVerdict(analysis.ActionSkip) {
		t.Fatalf("TRADE and SKIP both render as %q", mapVerdict(analysis.ActionTrade))
	}
}

// Tie-breaks mirror internal/analysis/ensemble.go so the panel's summary agrees
// with the decision the engine actually reached.
func TestDeriveEnsembleVerdict(t *testing.T) {
	cases := []struct {
		name  string
		votes map[string]int
		want  string
	}{
		{"no active analysts", map[string]int{}, "NEUTRAL"},
		{"clear majority long", map[string]int{"LONG": 2, "HOLD": 1}, "LONG"},
		{"clear majority wait", map[string]int{"WAIT": 2, "LONG": 1}, "WAIT"},
		{"hold wins ties outright", map[string]int{"HOLD": 1, "LONG": 1}, "HOLD"},
		{"opposed directional tie falls to hold", map[string]int{"LONG": 2, "SHORT": 2}, "HOLD"},
		{"directional beats wait on a tie", map[string]int{"LONG": 1, "WAIT": 1}, "LONG"},
		{"short beats wait on a tie", map[string]int{"SHORT": 1, "WAIT": 1}, "SHORT"},
		{"skip beats trade on a tie", map[string]int{"SKIP": 1, "TRADE": 1}, "SKIP"},
		{"unanimous hold", map[string]int{"HOLD": 3}, "HOLD"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := deriveEnsembleVerdict(c.votes); got != c.want {
				t.Errorf("deriveEnsembleVerdict(%v) = %q, want %q", c.votes, got, c.want)
			}
		})
	}
}

// Go randomises map iteration order; the resolution must not depend on it.
func TestDeriveEnsembleVerdictIsDeterministic(t *testing.T) {
	votes := map[string]int{"LONG": 1, "SHORT": 1, "WAIT": 1, "TRADE": 1, "SKIP": 1}
	first := deriveEnsembleVerdict(votes)
	for i := range 200 {
		if got := deriveEnsembleVerdict(votes); got != first {
			t.Fatalf("iteration %d returned %q, want a stable %q", i, got, first)
		}
	}
}
