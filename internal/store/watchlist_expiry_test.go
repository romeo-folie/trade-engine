package store

import (
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/tradeprofile"
)

func TestClearExpiredWatchlistContractKeepsOnlyIndependentContextRouting(t *testing.T) {
	expired := time.Now().Add(-time.Second)
	item := WatchlistItem{
		ExpiresAt:         &expired,
		Source:            "scanner",
		Strategies:        []string{"scanner_only"},
		Timeframes:        []string{"5m"},
		MinConfidence:     0.9,
		ScannerRunID:      "scanner-run",
		DiscoveryContext:  &tradeprofile.DiscoveryContext{Version: tradeprofile.DiscoveryContextVersion},
		ExecutionProfile:  &tradeprofile.ExecutionProfile{Version: tradeprofile.ExecutionProfileVersion},
		DiscoveryContexts: []WatchlistDiscoveryContext{{Key: "earnings_scheduled:2026-08-24"}},
	}
	clearExpiredWatchlistContract(&item)
	if item.DiscoveryContext != nil || item.ExecutionProfile != nil {
		t.Fatalf("expired legacy contract remained: %+v", item)
	}
	if len(item.DiscoveryContexts) != 1 {
		t.Fatalf("independent contexts were changed: %+v", item.DiscoveryContexts)
	}
	if item.Source != "earnings_watchlist" || item.ScannerRunID != "" || item.MinConfidence != 0 ||
		len(item.Strategies) != 3 || item.Strategies[0] != "trend_following" ||
		len(item.Timeframes) != 3 || item.Timeframes[0] != "1D" {
		t.Fatalf("expired scanner routing remained: %+v", item)
	}
}
