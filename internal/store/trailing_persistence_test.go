package store_test

import (
	"context"
	"math"
	"testing"

	"github.com/romeo-folie/trade-engine/internal/store"
)

// TestUpdateTradeTrailing_PreservesFractionalPrices pins the parameter typing
// of UpdateTradeTrailing. With a bare `NULLIF($1, 0)` Postgres infers the
// placeholders as `integer` from the literal 0, pgx encodes the float64 into an
// int4, and every trailing write silently loses its cents — stop_loss 8.50
// persisted as 8, high_water_mark 9.13 as 9. That both understated the stop
// against the broker's real resting leg and stopped the high-water mark from
// ever accumulating, so the ATR trail could not ratchet.
//
// The assertion is deliberately made through GetOpenTrades rather than a raw
// column read: the manager's next tick reads the trade back through exactly
// that path, and it is the read-back value it compares and ratchets from.
func TestUpdateTradeTrailing_PreservesFractionalPrices(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("DATABASE_URL not set; skipping store integration test")
	}
	ctx := context.Background()

	tradeID, err := store.InsertTrade(ctx, store.Trade{
		Ticker: "TRLP", Direction: "long", Strategy: "momentum",
		EntryPrice: 8.74, Quantity: 10, StopLoss: 8.00,
		Status: "open",
	})
	if err != nil {
		t.Fatalf("InsertTrade: %v", err)
	}

	const (
		wantStop = 8.5037
		wantHWM  = 9.1284
	)
	if err := store.UpdateTradeTrailing(ctx, tradeID, wantStop, wantHWM); err != nil {
		t.Fatalf("UpdateTradeTrailing: %v", err)
	}

	got := openTradeByID(ctx, t, tradeID)
	if math.Abs(got.StopLoss-wantStop) > 1e-9 {
		t.Errorf("stop_loss = %v, want %v (cents dropped: parameter typed as integer?)", got.StopLoss, wantStop)
	}
	if math.Abs(got.HighWaterMark-wantHWM) > 1e-9 {
		t.Errorf("high_water_mark = %v, want %v (cents dropped: parameter typed as integer?)", got.HighWaterMark, wantHWM)
	}

	// A zero argument still means "leave this column alone" — the sentinel the
	// manager relies on when it persists an HWM without moving the stop.
	if err := store.UpdateTradeTrailing(ctx, tradeID, 0, 9.4501); err != nil {
		t.Fatalf("UpdateTradeTrailing (hwm only): %v", err)
	}
	got = openTradeByID(ctx, t, tradeID)
	if math.Abs(got.StopLoss-wantStop) > 1e-9 {
		t.Errorf("stop_loss = %v after hwm-only update, want %v unchanged", got.StopLoss, wantStop)
	}
	if math.Abs(got.HighWaterMark-9.4501) > 1e-9 {
		t.Errorf("high_water_mark = %v, want 9.4501", got.HighWaterMark)
	}
}

// openTradeByID reads a trade back through the same query the trailing manager
// uses to enumerate its work.
func openTradeByID(ctx context.Context, t *testing.T, id string) store.Trade {
	t.Helper()
	trades, err := store.GetOpenTrades(ctx)
	if err != nil {
		t.Fatalf("GetOpenTrades: %v", err)
	}
	for _, tr := range trades {
		if tr.ID == id {
			return tr
		}
	}
	t.Fatalf("trade %s not found among open trades", id)
	return store.Trade{}
}
