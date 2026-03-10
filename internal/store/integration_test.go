package store_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/data"
	"github.com/romeo-folie/trade-engine/internal/store"
)

// TestMain connects to the database before any tests run.
// If DATABASE_URL is not set, the suite exits cleanly (no failure).
func TestMain(m *testing.M) {
	if os.Getenv("DATABASE_URL") == "" {
		fmt.Fprintln(os.Stderr, "DATABASE_URL not set; skipping store integration tests")
		os.Exit(0)
	}
	ctx := context.Background()
	if err := store.Connect(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "store.Connect: %v\n", err)
		os.Exit(1)
	}
	defer store.Close()
	os.Exit(m.Run())
}

// TestWatchlist_AddGetRemove verifies the full lifecycle of a watchlist entry.
func TestWatchlist_AddGetRemove(t *testing.T) {
	ctx := context.Background()
	const ticker = "TEST_INTG_AAPL"

	t.Cleanup(func() {
		// Best-effort soft-delete; ignore error in cleanup.
		_ = store.RemoveWatchlistItem(ctx, ticker, "stock")
	})

	item := store.WatchlistItem{
		Ticker:        ticker,
		Platform:      "stock",
		Strategies:    []string{"momentum"},
		Timeframes:    []string{"1d"},
		MinConfidence: 0.65,
		Source:        "manual",
	}

	if err := store.AddWatchlistItem(ctx, item); err != nil {
		t.Fatalf("AddWatchlistItem: %v", err)
	}

	// Should appear in active list.
	items, err := store.GetWatchlistItems(ctx, true)
	if err != nil {
		t.Fatalf("GetWatchlistItems: %v", err)
	}
	found := false
	for _, w := range items {
		if w.Ticker == ticker {
			found = true
			if !w.Active {
				t.Errorf("expected item active=true, got false")
			}
			break
		}
	}
	if !found {
		t.Errorf("expected ticker %q in active watchlist, not found", ticker)
	}

	// GetWatchlistItem by ticker.
	got, err := store.GetWatchlistItem(ctx, ticker, "stock")
	if err != nil {
		t.Fatalf("GetWatchlistItem: %v", err)
	}
	if got == nil {
		t.Fatal("GetWatchlistItem returned nil, want item")
	}
	if got.Ticker != ticker {
		t.Errorf("got Ticker=%q, want %q", got.Ticker, ticker)
	}

	// Soft-delete.
	if err := store.RemoveWatchlistItem(ctx, ticker, "stock"); err != nil {
		t.Fatalf("RemoveWatchlistItem: %v", err)
	}

	// Should no longer appear in active list.
	after, err := store.GetWatchlistItem(ctx, ticker, "stock")
	if err != nil {
		t.Fatalf("GetWatchlistItem after remove: %v", err)
	}
	if after != nil && after.Active {
		t.Errorf("expected item inactive after removal, got active=true")
	}
}

// TestSignal_InsertGetUpdate verifies signal insertion, retrieval, and status update.
func TestSignal_InsertGetUpdate(t *testing.T) {
	ctx := context.Background()

	sig := store.Signal{
		Ticker:            "TEST_INTG_MSFT",
		Action:            "BUY",
		Strategy:          "momentum",
		SignalPrice:       400.0,
		StopLoss:          392.0,
		TakeProfit:        416.0,
		Confidence:        0.82,
		SizePct:           5.0,
		Status:            "pending",
		ExpiresAt:         time.Now().Add(24 * time.Hour),
		StalePriceMovePct: 1.5,
	}

	id, err := store.InsertSignal(ctx, sig)
	if err != nil {
		t.Fatalf("InsertSignal: %v", err)
	}
	if id == "" {
		t.Fatal("InsertSignal returned empty ID")
	}

	// GetSignalByID round-trip.
	fetched, err := store.GetSignalByID(ctx, id)
	if err != nil {
		t.Fatalf("GetSignalByID: %v", err)
	}
	if fetched == nil {
		t.Fatal("GetSignalByID returned nil")
	}
	if fetched.Ticker != sig.Ticker {
		t.Errorf("Ticker: got %q, want %q", fetched.Ticker, sig.Ticker)
	}
	if fetched.Status != "pending" {
		t.Errorf("Status: got %q, want %q", fetched.Status, "pending")
	}

	// Update status to approved.
	if err := store.UpdateSignalStatus(ctx, id, "approved", ""); err != nil {
		t.Fatalf("UpdateSignalStatus: %v", err)
	}

	updated, err := store.GetSignalByID(ctx, id)
	if err != nil {
		t.Fatalf("GetSignalByID after update: %v", err)
	}
	if updated.Status != "approved" {
		t.Errorf("Status after update: got %q, want approved", updated.Status)
	}
}

// TestSignal_PendingQuery verifies GetPendingSignals returns inserted pending signals.
func TestSignal_PendingQuery(t *testing.T) {
	ctx := context.Background()

	id, err := store.InsertSignal(ctx, store.Signal{
		Ticker:     "TEST_INTG_GOOG",
		Action:     "SELL",
		Strategy:   "mean_reversion",
		SignalPrice: 175.0,
		Confidence: 0.75,
		SizePct:    3.0,
		Status:     "pending",
		ExpiresAt:  time.Now().Add(24 * time.Hour),
	})
	if err != nil {
		t.Fatalf("InsertSignal: %v", err)
	}

	pending, err := store.GetPendingSignals(ctx)
	if err != nil {
		t.Fatalf("GetPendingSignals: %v", err)
	}
	found := false
	for _, s := range pending {
		if s.ID == id {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("inserted pending signal %q not found in GetPendingSignals", id)
	}
}

// TestCandle_UpsertAndGet verifies candle storage and retrieval.
func TestCandle_UpsertAndGet(t *testing.T) {
	ctx := context.Background()
	const ticker = "TEST_INTG_SPY"
	now := time.Now().UTC().Truncate(24 * time.Hour)

	candles := []data.Candle{
		{
			Ticker:    ticker,
			Timeframe: data.Timeframe1Day,
			Timestamp: now.Add(-2 * 24 * time.Hour),
			Open:      500.0, High: 505.0, Low: 498.0, Close: 503.0, Volume: 1_000_000,
		},
		{
			Ticker:    ticker,
			Timeframe: data.Timeframe1Day,
			Timestamp: now.Add(-1 * 24 * time.Hour),
			Open:      503.0, High: 510.0, Low: 501.0, Close: 508.0, Volume: 1_200_000,
		},
	}

	if err := store.UpsertCandles(ctx, candles); err != nil {
		t.Fatalf("UpsertCandles: %v", err)
	}

	from := now.Add(-3 * 24 * time.Hour)
	to := now

	got, err := store.GetCandles(ctx, ticker, data.Timeframe1Day, from, to)
	if err != nil {
		t.Fatalf("GetCandles: %v", err)
	}
	if len(got) < 2 {
		t.Errorf("GetCandles: got %d candles, want >= 2", len(got))
	}

	// Latest candle lookup.
	latest, err := store.GetLatestCandle(ctx, ticker, data.Timeframe1Day)
	if err != nil {
		t.Fatalf("GetLatestCandle: %v", err)
	}
	if latest == nil {
		t.Fatal("GetLatestCandle returned nil")
	}
	if latest.Close != 508.0 {
		t.Errorf("GetLatestCandle.Close: got %v, want 508.0", latest.Close)
	}
}
