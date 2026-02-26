package views

import (
	"strings"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/store"
)

func TestRenderPositionDetailStatic(t *testing.T) {
	p := PositionRow{
		Ticker:       "AAPL",
		EntryPrice:   180,
		CurrentPrice: 185,
		Quantity:     10,
		PnL:          50,
		PnLPct:       2.78,
		StopLoss:     175,
		TakeProfit:   195,
		Strategy:     "default",
	}

	out := RenderPositionDetailStatic(p, 120, 40)

	checks := []string{"AAPL", "180.00", "185.00", "175.00", "195.00", "default", "esc"}
	for _, c := range checks {
		if !strings.Contains(out, c) {
			t.Errorf("expected position detail to contain %q", c)
		}
	}
}

func TestRenderPositionDetail_Full(t *testing.T) {
	d := PositionDetail{
		Trade: store.Trade{
			ID:         "trade-1",
			SignalID:   "sig-1",
			Ticker:     "TSLA",
			Direction:  "buy",
			Strategy:   "momentum",
			EntryPrice: 250,
			Quantity:   5,
			StopLoss:   240,
			TakeProfit: 270,
			Status:     "open",
		},
		CurrentPrice:      260,
		PnL:               50,
		PnLPct:            4.0,
		AnalysisReasoning: "Strong momentum with RSI oversold bounce and MACD crossover",
		AnalysisAction:    "BUY",
		AnalysisModel:     "claude-sonnet-4-20250514",
		RelatedSignals: []SignalRow{
			{ID: "sig-1", Ticker: "TSLA", Action: "BUY", Confidence: 0.85, Status: "executed", CreatedAt: time.Now()},
		},
	}

	out := RenderPositionDetail(d, 120, 40)

	checks := []string{"TSLA", "BUY", "250.00", "260.00", "momentum", "Analysis Reasoning", "MACD crossover", "Related Signals"}
	for _, c := range checks {
		if !strings.Contains(out, c) {
			t.Errorf("expected detail to contain %q", c)
		}
	}
}

func TestWrapText(t *testing.T) {
	text := "this is a test of the word wrapping function that should wrap at width"
	wrapped := wrapText(text, 20)
	lines := strings.Split(wrapped, "\n")
	for _, line := range lines {
		if len(line) > 20 {
			t.Errorf("line too long: %q (len %d)", line, len(line))
		}
	}
}

func TestWrapText_Empty(t *testing.T) {
	if wrapText("", 20) != "" {
		t.Error("expected empty string")
	}
}

func TestWrapText_ZeroWidth(t *testing.T) {
	text := "hello world"
	if wrapText(text, 0) != text {
		t.Error("zero width should return original")
	}
}
