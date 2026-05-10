package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/portfolio"
	"github.com/romeo-folie/trade-engine/internal/store"
)

// TestAggregateDailyCounters seeds a known mix of signals (executed, blocked
// with multiple reasons, pending) and closed trades (winning, losing,
// breakeven) for a single calendar day and asserts the rollup matches.
//
// Uses TRUNCATE on entry to start from a clean slate so it doesn't depend on
// preceding test order, and re-truncates on exit so the next test in the
// suite isn't surprised. Skipped automatically when DATABASE_URL is unset
// (TestMain bails before this runs in that case).
func TestAggregateDailyCounters(t *testing.T) {
	ctx := context.Background()

	// Clean slate for the two tables we touch. CASCADE clears trades that
	// reference any signals we're about to insert.
	if _, err := store.GetDB().Exec(ctx, "TRUNCATE signals, trades CASCADE"); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	t.Cleanup(func() {
		_, _ = store.GetDB().Exec(ctx, "TRUNCATE signals, trades CASCADE")
	})

	now := time.Now().UTC()
	today := now.Truncate(24 * time.Hour)

	// 5 signals total today: 1 executed, 3 blocked (2 confidence, 1 R:R), 1 pending.
	insertSignal := func(status, blockReason string) string {
		id, err := store.InsertSignal(ctx, store.Signal{
			Ticker:            "TEST",
			Action:            "BUY",
			Strategy:          "momentum",
			SignalPrice:       100,
			StopLoss:          95,
			TakeProfit:        110,
			Confidence:        0.7,
			SizePct:           2.0,
			Status:            status,
			BlockReason:       blockReason,
			ExpiresAt:         now.Add(15 * time.Minute),
			StalePriceMovePct: 2.0,
		})
		if err != nil {
			t.Fatalf("InsertSignal(%s): %v", status, err)
		}
		return id
	}

	executedSigID := insertSignal("executed", "")
	insertSignal("blocked", "confidence 0.55 < threshold 0.65")
	insertSignal("blocked", "confidence 0.50 < threshold 0.65")
	insertSignal("blocked", "R:R 1.20 < minimum 1.40")
	insertSignal("pending", "")

	// 3 closed trades today: 2 wins ($50, $100), 1 loss ($-25), 1 breakeven (excluded).
	insertClosedTrade := func(pnl float64) {
		id, err := store.InsertTrade(ctx, store.Trade{
			SignalID:   executedSigID,
			Ticker:     "TEST",
			Direction:  "long",
			Strategy:   "momentum",
			EntryPrice: 100,
			Quantity:   10,
			StopLoss:   95,
			TakeProfit: 110,
		})
		if err != nil {
			t.Fatalf("InsertTrade: %v", err)
		}
		// Close it with the requested PnL — opened_at + closed_at need to fall on
		// today for the rollup to count it. AggregateDailyCounters keys off
		// closed_at::date so we set that explicitly.
		if _, err := store.GetDB().Exec(ctx, `
			UPDATE trades SET status='closed', exit_price=$1, pnl=$2,
			       closed_at=$3 WHERE id=$4`,
			100.0+pnl/10.0, pnl, now, id); err != nil {
			t.Fatalf("close trade: %v", err)
		}
	}
	insertClosedTrade(50)
	insertClosedTrade(100)
	insertClosedTrade(-25)
	insertClosedTrade(0) // breakeven — should be ignored by the rollup

	dc, err := store.AggregateDailyCounters(ctx, today)
	if err != nil {
		t.Fatalf("AggregateDailyCounters: %v", err)
	}

	if dc.SignalsTotal != 5 {
		t.Errorf("SignalsTotal=%d want 5", dc.SignalsTotal)
	}
	if dc.SignalsBlocked != 3 {
		t.Errorf("SignalsBlocked=%d want 3", dc.SignalsBlocked)
	}
	if dc.SignalsExecuted != 1 {
		t.Errorf("SignalsExecuted=%d want 1", dc.SignalsExecuted)
	}
	if dc.ExecutedWinning != 2 {
		t.Errorf("ExecutedWinning=%d want 2", dc.ExecutedWinning)
	}
	if dc.ExecutedLosing != 1 {
		t.Errorf("ExecutedLosing=%d want 1", dc.ExecutedLosing)
	}
	if len(dc.BlockedByReason) != 3 {
		t.Errorf("BlockedByReason size=%d want 3, got %v", len(dc.BlockedByReason), dc.BlockedByReason)
	}
	if dc.BlockedByReason["R:R 1.20 < minimum 1.40"] != 1 {
		t.Errorf("R:R bucket missing or wrong: %v", dc.BlockedByReason)
	}
	if dc.BlockedByReason["confidence 0.55 < threshold 0.65"] != 1 {
		t.Errorf("confidence bucket missing: %v", dc.BlockedByReason)
	}
}

// TestDailySummary_BlockedByReasonRoundTrip writes a daily_summaries row
// through the production code path (portfolio.MaybeDailySummary, which is
// what the engine calls every minute) carrying a non-empty
// blocked_by_reason map, then reads it back and checks the JSONB column
// round-trips correctly.
//
// The previous implementation passed `[]byte` as the bind value without an
// explicit ::jsonb cast — pgx encodes []byte as bytea, which Postgres rejects
// when the column is jsonb. This test would catch any regression to that
// path so the fix doesn't silently break again.
func TestDailySummary_BlockedByReasonRoundTrip(t *testing.T) {
	ctx := context.Background()

	// Don't TRUNCATE daily_summaries — other tests may depend on existing
	// rows. Use today's date and rely on the upsert to overwrite.
	today := time.Now().UTC().Truncate(24 * time.Hour)

	tr := portfolio.NewTracker(0) // 0 cooldown so MaybeDailySummary runs immediately
	tr.Update(nil, 100_000, 100_000, 0, 0)

	counters := store.DailyCounters{
		Date:            today,
		SignalsTotal:    7,
		SignalsBlocked:  4,
		SignalsExecuted: 3,
		ExecutedWinning: 2,
		ExecutedLosing:  1,
		BlockedByReason: map[string]int{
			"confidence 0.55 < threshold 0.65": 2,
			"R:R 1.20 < minimum 1.40":          1,
			"max_open_positions reached":       1,
		},
	}

	tr.MaybeDailySummary(ctx,
		100_000.0,
		1, 1, 0, 0,
		counters.SignalsTotal, counters.SignalsBlocked,
		0, 0,
		counters,
	)

	got, err := store.GetDailySummaryByDate(ctx, today)
	if err != nil {
		t.Fatalf("GetDailySummaryByDate: %v", err)
	}
	if got == nil {
		t.Fatal("GetDailySummaryByDate: returned nil — write likely failed; check logs for jsonb cast error")
	}
	if got.BlockedByReason == nil {
		t.Fatalf("blocked_by_reason round-tripped as nil; want non-empty map")
	}
	if got.BlockedByReason["confidence 0.55 < threshold 0.65"] != 2 {
		t.Errorf("confidence bucket = %d, want 2; full map=%v",
			got.BlockedByReason["confidence 0.55 < threshold 0.65"], got.BlockedByReason)
	}
	if got.BlockedByReason["R:R 1.20 < minimum 1.40"] != 1 {
		t.Errorf("R:R bucket = %d, want 1", got.BlockedByReason["R:R 1.20 < minimum 1.40"])
	}
	if got.SignalsTotal != 7 || got.ExecutedWinning != 2 || got.ExecutedLosing != 1 {
		t.Errorf("scalar counters mismatch: total=%d winning=%d losing=%d",
			got.SignalsTotal, got.ExecutedWinning, got.ExecutedLosing)
	}
}

// TestAggregateDailyCounters_LiveBetweenRollups simulates the dashboard's
// 5-second poll cadence: the daily-summaries row is written once with N
// signals, the engine then inserts another signal between rollups, and the
// next AggregateDailyCounters call must reflect the new count. Without the
// handler's live-overlay (Issue #325 review fix), the dashboard would stay
// pinned to the persisted row's stale value until the next rollup tick.
func TestAggregateDailyCounters_LiveBetweenRollups(t *testing.T) {
	ctx := context.Background()

	if _, err := store.GetDB().Exec(ctx, "TRUNCATE signals, trades CASCADE"); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	t.Cleanup(func() {
		_, _ = store.GetDB().Exec(ctx, "TRUNCATE signals, trades CASCADE")
	})

	now := time.Now().UTC()
	today := now.Truncate(24 * time.Hour)

	insert := func() {
		if _, err := store.InsertSignal(ctx, store.Signal{
			Ticker: "TEST", Action: "BUY", Strategy: "momentum",
			SignalPrice: 100, StopLoss: 95, TakeProfit: 110, Confidence: 0.7,
			SizePct: 1.0, Status: "executed",
			ExpiresAt: now.Add(15 * time.Minute), StalePriceMovePct: 2.0,
		}); err != nil {
			t.Fatalf("InsertSignal: %v", err)
		}
	}

	// First poll (T=12:01 in the spec): 2 signals.
	insert()
	insert()
	first, err := store.AggregateDailyCounters(ctx, today)
	if err != nil {
		t.Fatalf("first AggregateDailyCounters: %v", err)
	}
	if first.SignalsTotal != 2 {
		t.Fatalf("first poll: SignalsTotal=%d want 2", first.SignalsTotal)
	}

	// Engine produces a third signal between rollup ticks.
	insert()

	// Second poll (T=12:02): the dashboard re-queries and must see 3.
	second, err := store.AggregateDailyCounters(ctx, today)
	if err != nil {
		t.Fatalf("second AggregateDailyCounters: %v", err)
	}
	if second.SignalsTotal != 3 {
		t.Errorf("second poll: SignalsTotal=%d want 3 (live overlay broken?)",
			second.SignalsTotal)
	}
}
