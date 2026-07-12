package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/store"
)

// TestOptionIV_RoundTrip verifies migration 040 applies and the upsert/read of
// daily ATM IV round-trips, including the ON CONFLICT overwrite.
func TestOptionIV_RoundTrip(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("DATABASE_URL not set; skipping store integration test")
	}
	ctx := context.Background()
	db := store.GetDB()
	const u = "SPYIVTEST"
	d1 := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	d2 := time.Date(2026, 6, 2, 0, 0, 0, 0, time.UTC)

	if err := store.UpsertOptionIV(ctx, db, u, d1, 0.18, "derived"); err != nil {
		t.Fatalf("upsert d1: %v", err)
	}
	if err := store.UpsertOptionIV(ctx, db, u, d2, 0.20, "live"); err != nil {
		t.Fatalf("upsert d2: %v", err)
	}
	// Overwrite d1 (ON CONFLICT).
	if err := store.UpsertOptionIV(ctx, db, u, d1, 0.19, "live"); err != nil {
		t.Fatalf("upsert d1 again: %v", err)
	}

	hist, err := store.GetOptionIVHistory(ctx, db, u, d1.AddDate(0, 0, -1), d2)
	if err != nil {
		t.Fatalf("GetOptionIVHistory: %v", err)
	}
	if len(hist) != 2 {
		t.Fatalf("len = %d; want 2", len(hist))
	}
	// Oldest first; d1 overwritten to 0.19.
	if hist[0].ATMIV != 0.19 || hist[0].Source != "live" {
		t.Errorf("d1 row = %+v; want 0.19/live", hist[0])
	}
	if hist[1].ATMIV != 0.20 {
		t.Errorf("d2 IV = %v; want 0.20", hist[1].ATMIV)
	}

	// A 'derived' backfill re-run must NOT overwrite the 'live' d1 row.
	if err := store.UpsertOptionIV(ctx, db, u, d1, 0.15, "derived"); err != nil {
		t.Fatalf("upsert d1 derived: %v", err)
	}
	hist, err = store.GetOptionIVHistory(ctx, db, u, d1.AddDate(0, 0, -1), d2)
	if err != nil {
		t.Fatalf("GetOptionIVHistory 2: %v", err)
	}
	if hist[0].ATMIV != 0.19 || hist[0].Source != "live" {
		t.Errorf("d1 after derived re-run = %+v; want unchanged 0.19/live", hist[0])
	}
}

// TestOptionIV_AsOfInstantFiltering verifies migration 042: the read filters on the
// precise sample INSTANT (as_of), so a 09:45 sample is excluded from a read ending at
// 09:40 (no lookahead for a pre-sample breakout) but included when the read reaches
// 09:50.
func TestOptionIV_AsOfInstantFiltering(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("DATABASE_URL not set; skipping store integration test")
	}
	ctx := context.Background()
	db := store.GetDB()
	et, _ := time.LoadLocation("America/New_York")
	const u = "SPYIVASOF"

	asOf := time.Date(2026, 6, 3, 9, 45, 0, 0, et) // sampled 09:45 ET
	if err := store.UpsertOptionIV(ctx, db, u, asOf, 0.25, "derived"); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	from := time.Date(2026, 6, 2, 0, 0, 0, 0, et)

	// Read ending 09:40 (before the sample) → excluded.
	before, _ := store.GetOptionIVHistory(ctx, db, u, from, time.Date(2026, 6, 3, 9, 40, 0, 0, et))
	if len(before) != 0 {
		t.Errorf("a 09:45 sample must be excluded from a read ending 09:40 (lookahead), got %d rows", len(before))
	}
	// Read ending 09:50 (after the sample) → included.
	after, _ := store.GetOptionIVHistory(ctx, db, u, from, time.Date(2026, 6, 3, 9, 50, 0, 0, et))
	if len(after) != 1 {
		t.Fatalf("a 09:45 sample must be included in a read ending 09:50, got %d rows", len(after))
	}
	if after[0].ATMIV != 0.25 {
		t.Errorf("IV = %v; want 0.25", after[0].ATMIV)
	}
}
