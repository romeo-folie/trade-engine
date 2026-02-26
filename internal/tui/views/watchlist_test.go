package views

import (
	"strings"
	"testing"
	"time"
)

func TestRenderWatchlist_Empty(t *testing.T) {
	out := RenderWatchlist(WatchlistData{LastUpdate: time.Now()}, 120)
	if !strings.Contains(out, "empty") {
		t.Fatal("expected empty state")
	}
}

func TestRenderWatchlist_MixedItems(t *testing.T) {
	data := WatchlistData{
		Items: []WatchlistRow{
			{Ticker: "AAPL", Platform: "stock", Active: true, Timeframes: []string{"1d", "1h"}, Strategies: []string{"momentum"}, AddedAt: time.Now()},
			{Ticker: "TSLA", Platform: "stock", Active: false, Timeframes: []string{"1d"}, AddedAt: time.Now()},
			{Ticker: "GOOG", Platform: "stock", Active: true, Timeframes: []string{"5m", "1d"}, Strategies: []string{"trend", "mean_reversion"}, AddedAt: time.Now()},
		},
		Selected:   1,
		LastUpdate: time.Now(),
	}
	out := RenderWatchlist(data, 120)
	if !strings.Contains(out, "AAPL") || !strings.Contains(out, "TSLA") || !strings.Contains(out, "GOOG") {
		t.Fatal("expected all tickers")
	}
	if !strings.Contains(out, "inactive") {
		t.Fatal("expected inactive status for TSLA")
	}
	if !strings.Contains(out, "3 items (2 active)") {
		t.Fatalf("expected count summary, got: %s", out)
	}
	if !strings.Contains(out, "▸") {
		t.Fatal("expected selection marker")
	}
}
