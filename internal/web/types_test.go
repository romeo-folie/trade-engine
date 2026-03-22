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
	if analysts[0].Name != "Technical" {
		t.Errorf("expected Technical, got %s", analysts[0].Name)
	}
	if analysts[1].Name != "Momentum" {
		t.Errorf("expected Momentum, got %s", analysts[1].Name)
	}
	if analysts[2].Name != "Sentiment" {
		t.Errorf("expected Sentiment, got %s", analysts[2].Name)
	}

	// Check verdict mapping
	if analysts[0].Verdict != "LONG" {
		t.Errorf("expected LONG, got %s", analysts[0].Verdict)
	}
	if analysts[2].Verdict != "NEUTRAL" {
		t.Errorf("expected NEUTRAL, got %s", analysts[2].Verdict)
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

func TestMapVerdict(t *testing.T) {
	cases := []struct {
		in   analysis.Action
		want string
	}{
		{analysis.ActionBuy, "LONG"},
		{analysis.ActionSell, "SHORT"},
		{analysis.ActionHold, "NEUTRAL"},
		{"", "NEUTRAL"},
	}
	for _, c := range cases {
		if got := mapVerdict(c.in); got != c.want {
			t.Errorf("mapVerdict(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
