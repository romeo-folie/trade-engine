package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/store"
)

// TestEarnings_NextOnOrAfterAndSeedPrecedence covers migration 045: upsert + the
// "next earnings on or after" query the gate uses, and that a 'seed' row is never
// overwritten by a 'finnhub' refresh.
func TestEarnings_NextOnOrAfterAndSeedPrecedence(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("DATABASE_URL not set; skipping store integration test")
	}
	ctx := context.Background()
	db := store.GetDB()
	const sym = "NVDAEARNTEST"
	mkDate := func(s string) time.Time {
		d, _ := time.Parse("2006-01-02", s)
		return d
	}

	// Two seeded historical dates.
	for _, d := range []string{"2024-08-28", "2024-11-20"} {
		if err := store.UpsertEarnings(ctx, db, sym, mkDate(d), "amc", "seed"); err != nil {
			t.Fatalf("seed upsert %s: %v", d, err)
		}
	}

	// A signal 2 days before the Aug report → next earnings is 2024-08-28.
	asOf := time.Date(2024, 8, 26, 13, 45, 0, 0, time.UTC)
	when, ok := store.NextEarningsOnOrAfter(ctx, db, sym, asOf)
	if !ok || when.Format("2006-01-02") != "2024-08-28" {
		t.Fatalf("next on/after %s = %v ok=%v; want 2024-08-28", asOf.Format("2006-01-02"), when, ok)
	}

	// A signal AFTER the Aug report → next is the Nov report.
	after := time.Date(2024, 9, 1, 13, 45, 0, 0, time.UTC)
	when, ok = store.NextEarningsOnOrAfter(ctx, db, sym, after)
	if !ok || when.Format("2006-01-02") != "2024-11-20" {
		t.Fatalf("next after Aug = %v ok=%v; want 2024-11-20", when, ok)
	}

	// A signal past all dates → none scheduled (gate treats as "no imminent earnings").
	if _, ok := store.NextEarningsOnOrAfter(ctx, db, sym, mkDate("2030-01-01")); ok {
		t.Errorf("expected no earnings on/after 2030")
	}

	// A 'finnhub' refresh must NOT overwrite the authoritative 'seed' row.
	if err := store.UpsertEarnings(ctx, db, sym, mkDate("2024-08-28"), "bmo", "finnhub"); err != nil {
		t.Fatalf("finnhub upsert: %v", err)
	}
	events, err := store.GetEarnings(ctx, db, sym, mkDate("2024-08-01"), mkDate("2024-09-01"))
	if err != nil || len(events) != 1 {
		t.Fatalf("GetEarnings = %+v err=%v; want 1 row", events, err)
	}
	if events[0].Source != "seed" || events[0].SessionHour != "amc" {
		t.Errorf("seed row was clobbered by finnhub refresh: %+v", events[0])
	}
}
