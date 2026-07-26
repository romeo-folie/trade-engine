package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/data"
	"github.com/romeo-folie/trade-engine/internal/store"
)

// UpsertNewsItems must return external_id → UUID for NEW and ALREADY-STORED
// rows alike: the enricher uses these IDs to record relationship mentions (the
// corroboration count gating graph-neighbor scanner eligibility), and the
// engine re-fetches the same 48h of articles every tick — so duplicate rows
// are the common case, not the exception.
func TestUpsertNewsItems_ReturnsIDsForDuplicates(t *testing.T) {
	ctx := context.Background()
	ext := "sympathy-ids-" + time.Now().Format("150405.000000")
	items := []data.NewsItem{{
		ExternalID:  ext,
		Headline:    "ids test",
		URL:         "https://example.com/" + ext,
		Source:      "test",
		PublishedAt: time.Now(),
	}}

	first, err := store.UpsertNewsItems(ctx, items)
	if err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	id1, ok := first[ext]
	if !ok || id1 == "" {
		t.Fatalf("first upsert returned no ID: %v", first)
	}

	second, err := store.UpsertNewsItems(ctx, items)
	if err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	id2, ok := second[ext]
	if !ok || id2 == "" {
		t.Fatalf("duplicate upsert returned no ID (mention recording would silently stop): %v", second)
	}
	if id1 != id2 {
		t.Fatalf("duplicate upsert returned a different ID: %s vs %s", id1, id2)
	}
}

func TestWatchlistPullbackRequalificationQuarantine(t *testing.T) {
	ctx := context.Background()
	const ticker = "PULLBACK_REQUAL_RT"
	t.Cleanup(func() {
		_, _ = store.GetDB().Exec(ctx, "DELETE FROM watchlist WHERE ticker=$1", ticker)
	})

	exp := time.Now().Add(72 * time.Hour)
	item := store.WatchlistItem{
		Ticker: ticker, Platform: "stock", Strategies: []string{"momentum"},
		Timeframes: []string{"1H"}, Source: "scanner", ExpiresAt: &exp,
	}
	if err := store.AddWatchlistItem(ctx, item); err != nil {
		t.Fatalf("seed watchlist item: %v", err)
	}
	if _, err := store.GetDB().Exec(ctx, `
		UPDATE watchlist
		SET active = false, pullback_requalification_required = true
		WHERE ticker = $1
	`, ticker); err != nil {
		t.Fatalf("quarantine watchlist item: %v", err)
	}

	if err := store.AddWatchlistItem(ctx, item); !errors.Is(err, store.ErrWatchlistRequalificationRequired) {
		t.Fatalf("unqualified scanner reactivation error = %v", err)
	}
	got, err := store.GetWatchlistItem(ctx, ticker, "stock")
	if err != nil || got == nil {
		t.Fatalf("read quarantined item: %v (item=%v)", err, got)
	}
	if got.Active || !got.PullbackRequalificationRequired {
		t.Fatalf("quarantine was cleared without evidence: %+v", got)
	}

	item.PullbackRequalificationComplete = true
	if err := store.AddWatchlistItem(ctx, item); err != nil {
		t.Fatalf("definitive requalification: %v", err)
	}
	got, err = store.GetWatchlistItem(ctx, ticker, "stock")
	if err != nil || got == nil {
		t.Fatalf("read requalified item: %v (item=%v)", err, got)
	}
	if !got.Active || got.PullbackRequalificationRequired {
		t.Fatalf("definitive requalification did not reactivate row: %+v", got)
	}
}

// Watchlist GraphContext (migration 038) round-trips through add + both read
// paths, so the engine can carry a scanner candidate's origin thesis into the
// deep-analysis prompt.
func TestWatchlistGraphContext_RoundTrip(t *testing.T) {
	ctx := context.Background()
	const ticker = "SYMPATHY_RT"
	const origin = `SYMPATHY TRIGGER — NVDA news 2h ago (POSITIVE 0.82): "beat". Relationship: NVDA (on watchlist) supplies to this company.`
	t.Cleanup(func() {
		_, _ = store.GetDB().Exec(ctx, "DELETE FROM watchlist WHERE ticker=$1", ticker)
	})

	exp := time.Now().Add(72 * time.Hour)
	if err := store.AddWatchlistItem(ctx, store.WatchlistItem{
		Ticker: ticker, Platform: "stock", Strategies: []string{"momentum"},
		Timeframes: []string{"1H"}, MinConfidence: 0.7, Source: "scanner",
		GraphContext: origin, ExpiresAt: &exp,
	}); err != nil {
		t.Fatalf("AddWatchlistItem: %v", err)
	}

	item, err := store.GetWatchlistItem(ctx, ticker, "stock")
	if err != nil || item == nil {
		t.Fatalf("GetWatchlistItem: %v (item=%v)", err, item)
	}
	if item.GraphContext != origin {
		t.Errorf("GetWatchlistItem GraphContext = %q", item.GraphContext)
	}

	items, err := store.GetWatchlistItems(ctx, true)
	if err != nil {
		t.Fatalf("GetWatchlistItems: %v", err)
	}
	found := false
	for _, it := range items {
		if it.Ticker == ticker {
			found = true
			if it.GraphContext != origin {
				t.Errorf("GetWatchlistItems GraphContext = %q", it.GraphContext)
			}
		}
	}
	if !found {
		t.Error("added item not returned by GetWatchlistItems")
	}

	// Re-add without context (e.g. later plain screener re-add) must clear it —
	// the origin thesis belongs to the sourcing event, not the ticker forever.
	if err := store.AddWatchlistItem(ctx, store.WatchlistItem{
		Ticker: ticker, Platform: "stock", Strategies: []string{"momentum"},
		Timeframes: []string{"1H"}, Source: "scanner", ExpiresAt: &exp,
	}); err != nil {
		t.Fatalf("re-add: %v", err)
	}
	item, _ = store.GetWatchlistItem(ctx, ticker, "stock")
	if item.GraphContext != "" {
		t.Errorf("re-add without context must clear GraphContext, got %q", item.GraphContext)
	}
}
