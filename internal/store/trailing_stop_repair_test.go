package store_test

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/romeo-folie/trade-engine/internal/store"
)

// TestMigration076_RepairsTruncatedStopsOnly exercises migration 076's repair
// predicate against seeded rows. The migration itself runs once on a clean
// schema where no corrupt rows exist, so its SQL is re-executed here against
// deliberately corrupted fixtures — the statement is a plain idempotent UPDATE,
// so replaying it is safe and is the only way to prove it selects the right
// rows.
//
// Two corruption signatures must be repaired and three healthy shapes must be
// left untouched. Over-reaching matters as much as under-reaching: this
// rewrites live protective stops, so a predicate that also caught legitimately
// diverged rows would silently move a stop the operator or the manager set.
func TestMigration076_RepairsTruncatedStopsOnly(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("DATABASE_URL not set; skipping store integration test")
	}
	ctx := context.Background()

	type fixture struct {
		name       string
		ticker     string
		dbStop     float64
		legStop    float64
		legStatus  string
		tradeState string
		want       float64
	}
	fixtures := []fixture{
		{
			name: "cents dropped by int4 encoding", ticker: "RPR1",
			dbStop: 8, legStop: 8.5, legStatus: "pending", tradeState: "open", want: 8.5,
		},
		{
			name: "sub-dollar write swallowed by the 0 sentinel", ticker: "RPR2",
			dbStop: 0.45, legStop: 0.5, legStatus: "pending", tradeState: "open", want: 0.5,
		},
		{
			// Stop genuinely at a whole dollar: nothing was lost.
			name: "already consistent", ticker: "RPR3",
			dbStop: 38, legStop: 38, legStatus: "pending", tradeState: "open", want: 38,
		},
		{
			// Diverged, but not by truncation — the manager may have moved the
			// DB stop ahead of a leg that has yet to be replaced. Not ours.
			name: "diverged but not a truncation", ticker: "RPR4",
			dbStop: 12.25, legStop: 13.75, legStatus: "pending", tradeState: "open", want: 12.25,
		},
		{
			// A terminal leg is not the live protective stop.
			name: "truncated but leg is cancelled", ticker: "RPR5",
			dbStop: 20, legStop: 20.6, legStatus: "cancelled", tradeState: "open", want: 20,
		},
		{
			// Closed trades keep their historical record intact.
			name: "truncated but trade closed", ticker: "RPR6",
			dbStop: 30, legStop: 30.4, legStatus: "pending", tradeState: "closed", want: 30,
		},
		{
			// Sub-$1 but the leg is BELOW the stored stop: neither corruption
			// mode can lose value upward, so this is some other divergence and
			// repairing it would loosen live protection.
			name: "sub-dollar leg below stored stop must not lower it", ticker: "RPR7",
			dbStop: 0.8, legStop: 0.75, legStatus: "pending", tradeState: "open", want: 0.8,
		},
	}

	ids := make(map[string]string, len(fixtures))
	for _, f := range fixtures {
		id, err := store.InsertTrade(ctx, store.Trade{
			Ticker: f.ticker, Direction: "long", Strategy: "momentum",
			EntryPrice: f.legStop * 1.1, Quantity: 5, StopLoss: f.dbStop,
			Status: "open",
		})
		if err != nil {
			t.Fatalf("%s: InsertTrade: %v", f.name, err)
		}
		ids[f.ticker] = id
		if _, err := store.InsertOrder(ctx, store.Order{
			TradeID: id, BrokerOrderID: "leg-" + f.ticker, Ticker: f.ticker,
			Side: "sell", OrderType: "stop", Quantity: 5, StopPrice: f.legStop,
			Status: f.legStatus, IsStopLoss: true,
		}); err != nil {
			t.Fatalf("%s: InsertOrder: %v", f.name, err)
		}
		if f.tradeState != "open" {
			if _, err := store.GetDB().Exec(ctx,
				`UPDATE trades SET status=$1 WHERE id=$2`, f.tradeState, id); err != nil {
				t.Fatalf("%s: set status: %v", f.name, err)
			}
		}
	}

	sql, err := os.ReadFile(filepath.Join(
		findMigrationsDir(), "076_repair_truncated_trailing_stops.sql"))
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	if _, err := store.GetDB().Exec(ctx, string(sql)); err != nil {
		t.Fatalf("run repair migration: %v", err)
	}

	for _, f := range fixtures {
		var got float64
		if err := store.GetDB().QueryRow(ctx,
			`SELECT stop_loss FROM trades WHERE id=$1`, ids[f.ticker]).Scan(&got); err != nil {
			t.Fatalf("%s: read back: %v", f.name, err)
		}
		if math.Abs(got-f.want) > 1e-9 {
			t.Errorf("%s (%s): stop_loss = %v, want %v", f.name, f.ticker, got, f.want)
		}
	}
}
