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
