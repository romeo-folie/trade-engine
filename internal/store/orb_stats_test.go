package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/store"
)

// TestGetORBStats_CountsTimeExits verifies the GetORBStats aggregate buckets
// exit_type='time_exit' into its own counter (and that those rows still count
// toward wins/losses by pnl sign and toward broker exit-source). Without the
// dedicated FILTER, time exits would silently vanish from the exit-type breakdown.
func TestGetORBStats_CountsTimeExits(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("DATABASE_URL not set; skipping store integration test")
	}
	ctx := context.Background()
	db := store.GetDB()
	const ticker = "TESTTE/USD" // unique ticker so we assert on exactly our rows

	sess, err := store.CreateORBSession(ctx, db, time.Now(), "london")
	if err != nil {
		t.Fatalf("CreateORBSession: %v", err)
	}

	// Three filled trades on the same ticker: one time-exit (loss), one tp (win),
	// one stop (loss). All recorded as broker-sourced.
	mk := func(side string) string {
		id, err := store.CreateORBTrade(ctx, db, store.ORBTrade{
			SessionID: sess.ID, Ticker: ticker, Side: side,
			EntryStop: 1.10, StopLoss: 1.09, TakeProfit: 1.12, Qty: 5000,
			Status: "filled",
		})
		if err != nil {
			t.Fatalf("CreateORBTrade: %v", err)
		}
		return id
	}
	timeID, tpID, stopID := mk("buy"), mk("buy"), mk("buy")

	if err := store.CloseORBTrade(ctx, db, timeID, "time_exit", "broker", 1.095, -25.0); err != nil {
		t.Fatalf("close time_exit: %v", err)
	}
	if err := store.CloseORBTrade(ctx, db, tpID, "tp", "broker", 1.12, 100.0); err != nil {
		t.Fatalf("close tp: %v", err)
	}
	if err := store.CloseORBTrade(ctx, db, stopID, "stop", "broker", 1.09, -50.0); err != nil {
		t.Fatalf("close stop: %v", err)
	}

	stats, err := store.GetORBStats(ctx, db)
	if err != nil {
		t.Fatalf("GetORBStats: %v", err)
	}
	var row *store.ORBStats
	for i := range stats {
		if stats[i].Ticker == ticker {
			row = &stats[i]
			break
		}
	}
	if row == nil {
		t.Fatalf("no stats row for %s", ticker)
	}

	if row.TimeExits != 1 {
		t.Errorf("TimeExits = %d; want 1", row.TimeExits)
	}
	if row.TPExits != 1 {
		t.Errorf("TPExits = %d; want 1", row.TPExits)
	}
	if row.StopExits != 1 {
		t.Errorf("StopExits = %d; want 1", row.StopExits)
	}
	if row.Wins != 1 {
		t.Errorf("Wins = %d; want 1 (the tp)", row.Wins)
	}
	if row.Losses != 2 {
		t.Errorf("Losses = %d; want 2 (time_exit + stop)", row.Losses)
	}
	if row.BrokerExits != 3 {
		t.Errorf("BrokerExits = %d; want 3", row.BrokerExits)
	}
}
