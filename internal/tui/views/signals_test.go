package views

import (
	"strings"
	"testing"
	"time"
)

func TestRenderSignalFeed_Empty(t *testing.T) {
	out := RenderSignalFeed(SignalFeedData{LastUpdate: time.Now()}, 120, 0)
	if !strings.Contains(out, "No signals yet") {
		t.Fatal("expected empty state")
	}
}

func TestRenderSignalFeed_MixedStatuses(t *testing.T) {
	data := SignalFeedData{
		Signals: []SignalFeedRow{
			{Ticker: "AAPL", Action: "BUY", Status: "pending", Confidence: 0.80, SignalPrice: 150, CreatedAt: time.Now(), ExpiresAt: time.Now().Add(30 * time.Minute)},
			{Ticker: "TSLA", Action: "SELL", Status: "blocked", Confidence: 0.50, SignalPrice: 200, BlockReason: "confidence 0.50 < threshold 0.65", CreatedAt: time.Now().Add(-10 * time.Minute)},
			{Ticker: "GOOG", Action: "BUY", Status: "executed", Confidence: 0.90, SignalPrice: 170, StopLoss: 165, TakeProfit: 180, CreatedAt: time.Now().Add(-1 * time.Hour)},
			{Ticker: "MSFT", Action: "BUY", Status: "expired", Confidence: 0.70, SignalPrice: 400, CreatedAt: time.Now().Add(-2 * time.Hour)},
		},
		LastUpdate: time.Now(),
	}

	out := RenderSignalFeed(data, 120, 0)
	if !strings.Contains(out, "AAPL") {
		t.Fatal("expected AAPL")
	}
	if !strings.Contains(out, "blocked") {
		t.Fatal("expected blocked status")
	}
	if !strings.Contains(out, "confidence") {
		t.Fatal("expected block reason detail")
	}
	if !strings.Contains(out, "4 signals") {
		t.Fatal("expected signal count")
	}
}

func TestRenderSignalFeed_SelectionMarker(t *testing.T) {
	data := SignalFeedData{
		Signals: []SignalFeedRow{
			{Ticker: "A", Action: "BUY", Status: "pending", CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Minute)},
			{Ticker: "B", Action: "SELL", Status: "executed", CreatedAt: time.Now()},
		},
		Selected:   1,
		LastUpdate: time.Now(),
	}
	out := RenderSignalFeed(data, 120, 0)
	lines := strings.Split(out, "\n")
	found := false
	for _, line := range lines {
		if strings.Contains(line, "▸") && strings.Contains(line, "B") {
			found = true
		}
	}
	if !found {
		t.Fatal("expected selection marker on second signal")
	}
}

func TestCountStatuses(t *testing.T) {
	signals := []SignalFeedRow{
		{Status: "pending"},
		{Status: "pending"},
		{Status: "blocked"},
		{Status: "executed"},
	}
	result := countStatuses(signals)
	if !strings.Contains(result, "2 pending") {
		t.Fatalf("expected '2 pending' in %q", result)
	}
	if !strings.Contains(result, "1 blocked") {
		t.Fatalf("expected '1 blocked' in %q", result)
	}
}

func TestFormatDuration(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{30 * time.Second, "30s"},
		{5 * time.Minute, "5m"},
		{2*time.Hour + 15*time.Minute, "2h15m"},
		{48 * time.Hour, "2d"},
	}
	for _, tt := range tests {
		got := formatDuration(tt.d)
		if got != tt.want {
			t.Errorf("formatDuration(%v) = %q, want %q", tt.d, got, tt.want)
		}
	}
}
