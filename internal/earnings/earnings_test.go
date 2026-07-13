package earnings

import (
	"context"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/data"
	"github.com/romeo-folie/trade-engine/internal/store"
)

// TestHistoricalSeed_ValidAndChronological is a pure guard on the curated table: every
// date parses, and each symbol's dates are strictly increasing (no dupes/typos).
func TestHistoricalSeed_ValidAndChronological(t *testing.T) {
	if len(HistoricalSeed) == 0 {
		t.Fatal("HistoricalSeed is empty")
	}
	for sym, dates := range HistoricalSeed {
		if len(dates) < 4 {
			t.Errorf("%s has only %d seeded dates; expected ~8 (2 years quarterly)", sym, len(dates))
		}
		var prev time.Time
		for _, ds := range dates {
			d, err := time.Parse("2006-01-02", ds)
			if err != nil {
				t.Errorf("%s: unparseable date %q: %v", sym, ds, err)
				continue
			}
			if !prev.IsZero() && !d.After(prev) {
				t.Errorf("%s: date %s is not after previous %s (dupe/out-of-order)", sym, ds, prev.Format("2006-01-02"))
			}
			prev = d
		}
	}
}

type fakeFetcher struct {
	rel []data.EarningsRelease
	err error
}

func (f fakeFetcher) FetchEarningsCalendar(_ context.Context, _ string, _, _ time.Time) ([]data.EarningsRelease, error) {
	return f.rel, f.err
}

// TestSync_UpsertsFromFetcher covers Sync end-to-end against the store (integration).
func TestSync_UpsertsFromFetcher(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("DATABASE_URL not set; skipping store integration test")
	}
	ctx := context.Background()
	db := store.GetDB()
	const sym = "SYNCEARNTEST"
	mk := func(s string) time.Time { d, _ := time.Parse("2006-01-02", s); return d }

	f := fakeFetcher{rel: []data.EarningsRelease{
		{Symbol: sym, Date: mk("2026-02-25"), Hour: "amc"},
		{Symbol: sym, Date: mk("2026-05-27"), Hour: "amc"},
	}}
	n, err := Sync(ctx, db, f, []string{sym}, mk("2026-01-01"), mk("2026-12-31"))
	if err != nil || n != 2 {
		t.Fatalf("Sync = %d err=%v; want 2", n, err)
	}
	when, ok := store.NextEarningsOnOrAfter(ctx, db, sym, mk("2026-04-01"))
	if !ok || when.Format("2006-01-02") != "2026-05-27" {
		t.Fatalf("next after Apr = %v ok=%v; want 2026-05-27", when, ok)
	}
}

// TestStoreProvider_UsesContextAsOf: the provider evaluates "next earnings" at the asOf its
// closure returns — the backtest hook that keeps it replay-instant, not wall-clock.
func TestStoreProvider_UsesContextAsOf(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("DATABASE_URL not set; skipping store integration test")
	}
	ctx := context.Background()
	db := store.GetDB()
	const sym = "PROVEARNTEST"
	mk := func(s string) time.Time { d, _ := time.Parse("2006-01-02", s); return d }
	if err := store.UpsertEarnings(ctx, db, sym, mk("2024-08-28"), "amc", "seed"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// asOf before the date → returns it.
	p := NewStoreProvider(db, func(context.Context) time.Time { return mk("2024-08-20") })
	if when, ok := p.NextEarnings(ctx, sym); !ok || when.Format("2006-01-02") != "2024-08-28" {
		t.Fatalf("pre-date NextEarnings = %v ok=%v; want 2024-08-28", when, ok)
	}
	// asOf after the date → none.
	p2 := NewStoreProvider(db, func(context.Context) time.Time { return mk("2024-09-01") })
	if _, ok := p2.NextEarnings(ctx, sym); ok {
		t.Errorf("post-date NextEarnings should be false")
	}
}
