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

// TestGetORBStats_FoldsScaleOutPair verifies a split-bracket trade (a partial + its runner)
// counts as ONE logical trade, with win/loss judged on the COMBINED PnL — not two separate
// rows. The partial wins (+60) and the runner loses (−40); the trade nets +20 → one win.
func TestGetORBStats_FoldsScaleOutPair(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("DATABASE_URL not set; skipping store integration test")
	}
	ctx := context.Background()
	db := store.GetDB()
	const ticker = "TESTSO/USD"

	sess, err := store.CreateORBSession(ctx, db, time.Now(), "newyork")
	if err != nil {
		t.Fatalf("CreateORBSession: %v", err)
	}

	runnerID, err := store.CreateORBTrade(ctx, db, store.ORBTrade{
		SessionID: sess.ID, Ticker: ticker, Side: "buy",
		EntryStop: 1.10, StopLoss: 1.09, TakeProfit: 1.12, Qty: 6000,
		Status: "filled", ScaleOutRole: "runner",
	})
	if err != nil {
		t.Fatalf("create runner: %v", err)
	}
	partialID, err := store.CreateORBTrade(ctx, db, store.ORBTrade{
		SessionID: sess.ID, Ticker: ticker, Side: "buy",
		EntryStop: 1.10, StopLoss: 1.09, TakeProfit: 1.11, Qty: 4000,
		Status: "filled", ScaleOutRole: "partial", ScaleOutParentID: &runnerID,
	})
	if err != nil {
		t.Fatalf("create partial: %v", err)
	}

	// Partial hits its (nearer) TP for +60; runner stops out for −40. Combined: +20.
	if err := store.CloseORBTrade(ctx, db, partialID, "tp", "broker", 1.11, 60.0); err != nil {
		t.Fatalf("close partial: %v", err)
	}

	// Before the runner closes, the logical trade is still OPEN (one leg working).
	stats, _ := store.GetORBStats(ctx, db)
	if row := findStat(stats, ticker); row == nil || row.OpenFilled != 1 || row.Closed != 0 {
		t.Fatalf("mid scale-out: want open_filled=1 closed=0; got %+v", row)
	}

	if err := store.CloseORBTrade(ctx, db, runnerID, "stop", "broker", 1.09, -40.0); err != nil {
		t.Fatalf("close runner: %v", err)
	}

	stats, _ = store.GetORBStats(ctx, db)
	row := findStat(stats, ticker)
	if row == nil {
		t.Fatalf("no stats row for %s", ticker)
	}
	if row.Closed != 1 {
		t.Errorf("Closed = %d; want 1 logical trade (not 2 rows)", row.Closed)
	}
	if row.Wins != 1 || row.Losses != 0 {
		t.Errorf("Wins/Losses = %d/%d; want 1/0 (combined +20 is a win)", row.Wins, row.Losses)
	}
	if row.TotalPnL != 20.0 {
		t.Errorf("TotalPnL = %v; want 20 (60 partial − 40 runner)", row.TotalPnL)
	}
	if row.StopExits != 1 || row.TPExits != 0 {
		t.Errorf("exit-type = stop %d / tp %d; want 1/0 (the runner leg is the trade's exit)", row.StopExits, row.TPExits)
	}
	if row.OpenFilled != 0 {
		t.Errorf("OpenFilled = %d; want 0 (both legs closed)", row.OpenFilled)
	}
}

// TestGetORBStats_PartialCancelledStillCounts verifies a scale-out group where the runner
// closed but the partial never filled (cancelled at cleanup) still counts as one completed
// trade with the runner's realized PnL — it must not vanish because a sibling is 'cancelled'
// rather than 'closed'.
func TestGetORBStats_PartialCancelledStillCounts(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("DATABASE_URL not set; skipping store integration test")
	}
	ctx := context.Background()
	db := store.GetDB()
	const ticker = "TESTPC/USD"

	sess, err := store.CreateORBSession(ctx, db, time.Now(), "tokyo")
	if err != nil {
		t.Fatalf("CreateORBSession: %v", err)
	}

	runnerID, err := store.CreateORBTrade(ctx, db, store.ORBTrade{
		SessionID: sess.ID, Ticker: ticker, Side: "buy",
		EntryStop: 1.10, StopLoss: 1.09, TakeProfit: 1.12, Qty: 6000,
		Status: "filled", ScaleOutRole: "runner",
	})
	if err != nil {
		t.Fatalf("create runner: %v", err)
	}
	partialID, err := store.CreateORBTrade(ctx, db, store.ORBTrade{
		SessionID: sess.ID, Ticker: ticker, Side: "buy",
		EntryStop: 1.10, StopLoss: 1.09, TakeProfit: 1.11, Qty: 4000,
		Status: "pending", ScaleOutRole: "partial", ScaleOutParentID: &runnerID,
	})
	if err != nil {
		t.Fatalf("create partial: %v", err)
	}

	// Partial never filled → cancelled; runner takes profit for +90.
	if _, err := db.Exec(ctx, `UPDATE orb_trades SET status='cancelled' WHERE id=$1`, partialID); err != nil {
		t.Fatalf("cancel partial: %v", err)
	}
	if err := store.CloseORBTrade(ctx, db, runnerID, "tp", "broker", 1.12, 90.0); err != nil {
		t.Fatalf("close runner: %v", err)
	}

	stats, _ := store.GetORBStats(ctx, db)
	row := findStat(stats, ticker)
	if row == nil {
		t.Fatalf("trade vanished from stats — cancelled partial sibling hid the realized runner")
	}
	if row.Closed != 1 || row.Wins != 1 || row.TotalPnL != 90.0 {
		t.Errorf("got closed=%d wins=%d pnl=%v; want 1/1/90 (runner realized, partial cancelled)", row.Closed, row.Wins, row.TotalPnL)
	}
	if row.OpenFilled != 0 {
		t.Errorf("OpenFilled = %d; want 0 (group is fully resolved)", row.OpenFilled)
	}
}

func findStat(stats []store.ORBStats, ticker string) *store.ORBStats {
	for i := range stats {
		if stats[i].Ticker == ticker {
			return &stats[i]
		}
	}
	return nil
}
