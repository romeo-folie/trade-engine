package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/data"
	"github.com/romeo-folie/trade-engine/internal/store"
)

// TestUpsertCandles_DropsCorruptBar proves the guard is actually wired into the
// persistence chokepoint (issue #519), not merely implemented next to it.
//
// The unit tests in internal/data cover the filter itself. This one covers the
// integration: a refactor that reorders or removes the data.SanitizeCandles
// call in UpsertCandles would still compile and still pass every data-package
// test, silently reintroducing the bug.
func TestUpsertCandles_DropsCorruptBar(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("DATABASE_URL not set; skipping store integration test")
	}
	ctx := context.Background()

	const ticker = "ZZSANITY"
	base := time.Date(2026, 8, 4, 0, 0, 0, 0, time.UTC)

	// 25 well-behaved hourly bars establish the median-range reference.
	batch := make([]data.Candle, 0, 27)
	for i := range 25 {
		batch = append(batch, data.Candle{
			Ticker: ticker, Timeframe: data.Timeframe1Hour,
			Timestamp: base.Add(time.Duration(i) * time.Hour),
			Open:      190.34, High: 191.34, Low: 189.34, Close: 190.34, Volume: 1000,
		})
	}
	// The real ANET-shaped corruption: a 34.78-wide bar on ~2.0 typical range.
	corrupt := data.Candle{
		Ticker: ticker, Timeframe: data.Timeframe1Hour,
		Timestamp: base.Add(25 * time.Hour),
		Open:      190.40, High: 224.98, Low: 190.20, Close: 211.47, Volume: 5000,
	}
	// A structurally impossible bar: high below low.
	inverted := data.Candle{
		Ticker: ticker, Timeframe: data.Timeframe1Hour,
		Timestamp: base.Add(26 * time.Hour),
		Open:      190.0, High: 188.0, Low: 192.0, Close: 190.0, Volume: 1000,
	}
	batch = append(batch, corrupt, inverted)

	t.Cleanup(func() {
		_, _ = store.GetDB().Exec(context.Background(),
			`DELETE FROM candles WHERE ticker = $1`, ticker)
	})

	if err := store.UpsertCandles(ctx, batch); err != nil {
		t.Fatalf("UpsertCandles: %v", err)
	}

	stored, err := store.GetCandles(ctx, ticker, data.Timeframe1Hour,
		base.Add(-time.Hour), base.Add(48*time.Hour))
	if err != nil {
		t.Fatalf("GetCandles: %v", err)
	}
	if len(stored) != 25 {
		t.Errorf("persisted %d candles, want 25 (both bad bars dropped)", len(stored))
	}
	for _, c := range stored {
		if c.High > 200 {
			t.Errorf("corrupt bar reached the database: high %.2f @ %s", c.High, c.Timestamp)
		}
		if c.High < c.Low {
			t.Errorf("inverted bar reached the database: H %.2f < L %.2f", c.High, c.Low)
		}
	}
}

// A batch consisting only of corrupt bars must not error — there is simply
// nothing left to write. Callers treat a non-nil error as a fetch failure and
// log it as such, so returning one here would turn provider corruption into a
// spurious persistence alarm.
func TestUpsertCandles_AllRejectedIsNotAnError(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("DATABASE_URL not set; skipping store integration test")
	}
	ctx := context.Background()
	bad := []data.Candle{{
		Ticker: "ZZSANITY2", Timeframe: data.Timeframe1Hour, Timestamp: time.Now().UTC(),
		Open: 10, High: 8, Low: 12, Close: 10, // high < low
	}}
	if err := store.UpsertCandles(ctx, bad); err != nil {
		t.Fatalf("all-rejected batch should be a no-op, got %v", err)
	}
}
