package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/store"
)

// GetFreshORBRiskMultiplier returns the latest stored value within the freshness bound for
// the (ticker, window), and reports absence/staleness (so the caller falls back to 1.0).
func TestGetFreshORBRiskMultiplier(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("DATABASE_URL not set; skipping store integration test")
	}
	ctx := context.Background()
	const ticker = "TESTAT/USD"
	old := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC) // before any seeded row

	// No insight yet → not found.
	if _, ok, err := store.GetFreshORBRiskMultiplier(ctx, ticker, "30d", old); err != nil || ok {
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

	// notBefore before both rows → latest (day2) wins.
	v, ok, err := store.GetFreshORBRiskMultiplier(ctx, ticker, "30d", old)
	if err != nil || !ok {
		t.Fatalf("expected found; got ok=%v err=%v", ok, err)
	}
	if v != 1.3 {
		t.Errorf("value = %v; want 1.3 (latest date)", v)
	}

	// Freshness bound AFTER the latest row → treated as stale, not found.
	if _, ok, _ := store.GetFreshORBRiskMultiplier(ctx, ticker, "30d", day2.AddDate(0, 0, 1)); ok {
		t.Error("a multiplier older than notBefore must be treated as stale (not found)")
	}

	// A notBefore between the two rows → only day2 qualifies (still 1.3).
	if v, ok, _ := store.GetFreshORBRiskMultiplier(ctx, ticker, "30d", day2); !ok || v != 1.3 {
		t.Errorf("boundary notBefore=day2: got v=%v ok=%v; want 1.3/true", v, ok)
	}

	// Production pattern: notBefore carries a time-of-day. The DATE column must still match a
	// row written on the SAME calendar day (guards the DATE-vs-TIMESTAMPTZ boundary) — the
	// scheduler truncates notBefore to midnight, but the store query must be robust regardless.
	if v, ok, _ := store.GetFreshORBRiskMultiplier(ctx, ticker, "30d", day2.Add(14*time.Hour)); !ok || v != 1.3 {
		t.Errorf("notBefore with time-of-day on the boundary day: got v=%v ok=%v; want 1.3/true", v, ok)
	}

	// A different window has no value.
	if _, ok, _ := store.GetFreshORBRiskMultiplier(ctx, ticker, "7d", old); ok {
		t.Error("expected no value for the 7d window")
	}
}
