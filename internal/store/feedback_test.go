package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/store"
)

// GetLatestORBRiskMultiplier returns the most recent stored value for the (ticker, window)
// and reports absence (so the caller can fall back to neutral 1.0).
func TestGetLatestORBRiskMultiplier(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("DATABASE_URL not set; skipping store integration test")
	}
	ctx := context.Background()
	const ticker = "TESTAT/USD"

	// No insight yet → not found.
	if _, ok, err := store.GetLatestORBRiskMultiplier(ctx, ticker, "30d"); err != nil || ok {
		t.Fatalf("expected not-found; got ok=%v err=%v", ok, err)
	}

	day1 := time.Date(2026, 6, 28, 0, 0, 0, 0, time.UTC)
	day2 := time.Date(2026, 6, 29, 0, 0, 0, 0, time.UTC)
	mk := func(date time.Time, val float64) {
		if err := store.UpsertFeedbackInsight(ctx, store.FeedbackInsight{
			Date: date, Scope: "orb_session", ScopeKey: ticker,
			Metric: "risk_multiplier", Value: val, Window: "30d", TradeCount: 40,
		}); err != nil {
			t.Fatalf("seed insight: %v", err)
		}
	}
	mk(day1, 0.7)
	mk(day2, 1.3) // newer → should win

	v, ok, err := store.GetLatestORBRiskMultiplier(ctx, ticker, "30d")
	if err != nil || !ok {
		t.Fatalf("expected found; got ok=%v err=%v", ok, err)
	}
	if v != 1.3 {
		t.Errorf("value = %v; want 1.3 (latest date)", v)
	}

	// A different window has no value.
	if _, ok, _ := store.GetLatestORBRiskMultiplier(ctx, ticker, "7d"); ok {
		t.Error("expected no value for the 7d window")
	}
}
