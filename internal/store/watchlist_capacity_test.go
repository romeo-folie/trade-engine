package store

import (
	"context"
	"errors"
	"testing"
)

func TestAddWatchlistItemEnforcesConfiguredMainWatchlistCapacity(t *testing.T) {
	if !IsConnected() {
		t.Skip("DATABASE_URL not set")
	}
	ctx := context.Background()
	const firstTicker = "MAIN_WATCHLIST_CAP_A_RT"
	const secondTicker = "MAIN_WATCHLIST_CAP_B_RT"
	const expiredTicker = "MAIN_WATCHLIST_CAP_EXPIRED_EARNINGS_RT"
	t.Cleanup(func() {
		_, _ = GetDB().Exec(ctx, `DELETE FROM watchlist WHERE ticker IN ($1,$2,$3)`, firstTicker, secondTicker, expiredTicker)
	})

	var used int
	items, err := GetWatchlistItems(ctx, true)
	if err != nil {
		t.Fatalf("get live watchlist rows: %v", err)
	}
	used = len(items)
	if err := AddWatchlistItem(ctx, WatchlistItem{
		Ticker: expiredTicker, Platform: "stock", Source: "earnings_watchlist",
		Strategies: []string{"momentum"}, Timeframes: []string{"1D"},
	}); err != nil {
		t.Fatalf("add expired earnings fixture: %v", err)
	}
	limit := used + 1
	first := WatchlistItem{
		Ticker: firstTicker, Platform: "stock", Source: "manual",
		Strategies: []string{"momentum"}, Timeframes: []string{"1D"},
		MaxActiveWatchlist: limit,
	}
	if err := AddWatchlistItem(ctx, first); err != nil {
		t.Fatalf("add first entry under cap: %v", err)
	}
	second := first
	second.Ticker = secondTicker
	if err := AddWatchlistItem(ctx, second); !errors.Is(err, ErrWatchlistCapacity) {
		t.Fatalf("add second entry at cap: %v, want ErrWatchlistCapacity", err)
	}
	if err := AddWatchlistItem(ctx, first); err != nil {
		t.Fatalf("refresh existing active entry at cap: %v", err)
	}
	if err := AddWatchlistItem(ctx, WatchlistItem{
		Ticker: secondTicker, Platform: "stock", Source: "manual",
		Strategies: []string{"momentum"}, Timeframes: []string{"1D"},
	}); err != nil {
		t.Fatalf("create inactive reactivation fixture: %v", err)
	}
	if err := RemoveWatchlistItem(ctx, secondTicker, "stock"); err != nil {
		t.Fatalf("deactivate reactivation fixture: %v", err)
	}
	if err := ReactivateWatchlistItemWithLimit(ctx, secondTicker, "stock", limit); !errors.Is(err, ErrWatchlistCapacity) {
		t.Fatalf("reactivate at cap: %v, want ErrWatchlistCapacity", err)
	}
}
