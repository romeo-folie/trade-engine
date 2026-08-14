package store_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/store"
	"github.com/romeo-folie/trade-engine/internal/tradeprofile"
)

func TestWatchlistTradeProfileRoundTrip(t *testing.T) {
	ctx := context.Background()
	const ticker = "PROFILE_RT"
	t.Cleanup(func() { _, _ = store.GetDB().Exec(ctx, "DELETE FROM watchlist WHERE ticker=$1", ticker) })
	runID := beginWatchlistTradeProfileRun(t, ctx)

	expiresAt := time.Now().Add(time.Hour)
	err := store.AddWatchlistItem(ctx, store.WatchlistItem{
		Ticker: ticker, Platform: "stock", Source: "scanner", ExpiresAt: &expiresAt, ScannerRunID: runID,
		Strategies: []string{"momentum"}, Timeframes: []string{"1D", "4H"},
		DiscoveryContext: &tradeprofile.DiscoveryContext{
			Version: tradeprofile.DiscoveryContextVersion,
			Sources: []string{"weekly_leader"}, Thesis: "Relative leader with sustained participation.",
			EvidenceBySource: map[string]json.RawMessage{
				"weekly_leader": json.RawMessage(`{"rank":1,"return_6m":72.4}`),
			},
		},
		ExecutionProfile: &tradeprofile.ExecutionProfile{
			Version:         tradeprofile.ExecutionProfileVersion,
			RecommendedLens: "momentum", AdmissibleLenses: []string{"momentum", "trend_following"},
			PrimaryTimeframe: "1D", EntryTimingTimeframe: "4H", ScorerRationale: "Leadership supports a daily thesis and timed entry.",
		},
	})
	if err != nil {
		t.Fatalf("AddWatchlistItem: %v", err)
	}

	item, err := store.GetWatchlistItem(ctx, ticker, "stock")
	if err != nil || item == nil {
		t.Fatalf("GetWatchlistItem = %#v, %v", item, err)
	}
	if item.DiscoveryContext == nil || item.DiscoveryContext.Thesis == "" ||
		item.ExecutionProfile == nil || item.ExecutionProfile.RecommendedLens != "momentum" {
		t.Fatalf("watchlist trade profile = %+v", item)
	}

	items, err := store.GetWatchlistItems(ctx, true)
	if err != nil {
		t.Fatalf("GetWatchlistItems: %v", err)
	}
	for _, listed := range items {
		if listed.Ticker == ticker && (listed.DiscoveryContext == nil || listed.ExecutionProfile == nil) {
			t.Fatalf("list path lost trade profile: %+v", listed)
		}
	}
}

func TestWatchlistTradeProfileRejectsInvalidContract(t *testing.T) {
	err := store.AddWatchlistItem(context.Background(), store.WatchlistItem{
		Ticker: "PROFILE_INVALID", Platform: "stock", Strategies: []string{"momentum"},
		DiscoveryContext: &tradeprofile.DiscoveryContext{Version: tradeprofile.DiscoveryContextVersion},
	})
	if err == nil {
		t.Fatal("AddWatchlistItem accepted an invalid discovery context")
	}
}

func TestWatchlistTradeProfileNilUpsertPreservesAndCompleteUpsertReplaces(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("DATABASE_URL not set; skipping store integration test")
	}
	ctx := context.Background()
	const ticker = "PROFILE_PRESERVE_RT"
	t.Cleanup(func() { _, _ = store.GetDB().Exec(ctx, "DELETE FROM watchlist WHERE ticker=$1", ticker) })
	initialRunID := beginWatchlistTradeProfileRun(t, ctx)

	firstExpiry := time.Now().Add(time.Hour).UTC().Truncate(time.Microsecond)
	initial := store.WatchlistItem{
		Ticker: ticker, Platform: "stock", Source: "scanner", GraphContext: "Original graph context", ExpiresAt: &firstExpiry, ScannerRunID: initialRunID,
		Strategies: []string{"momentum"}, Timeframes: []string{"1D"}, MinConfidence: 0.82,
		DiscoveryContext: &tradeprofile.DiscoveryContext{
			Version: tradeprofile.DiscoveryContextVersion, Sources: []string{"weekly_leader"},
			Thesis: "Original thesis", EvidenceBySource: map[string]json.RawMessage{
				"weekly_leader": json.RawMessage(`{"rank":1}`),
			},
		},
		ExecutionProfile: &tradeprofile.ExecutionProfile{
			Version: tradeprofile.ExecutionProfileVersion, RecommendedLens: "momentum",
			AdmissibleLenses: []string{"momentum", "trend_following"}, PrimaryTimeframe: "1D",
			ScorerRationale: "Original scanner recommendation.",
		},
	}
	if err := store.AddWatchlistItem(ctx, initial); err != nil {
		t.Fatalf("insert profile: %v", err)
	}

	legacyRefresh := initial
	legacyRefresh.Source = "manual"
	legacyRefresh.GraphContext = "manual refresh must not replace scanner context"
	legacyRefresh.Strategies = []string{"mean_reversion"}
	legacyRefresh.MinConfidence = 0.41
	legacyRefresh.DiscoveryContext = nil
	legacyRefresh.ExecutionProfile = nil
	legacyRefresh.ScannerRunID = ""
	// This models a normal manual/config refresh rather than a copied scanner
	// item: it has no scanner TTL to replace. The existing profile-backed
	// provenance envelope must therefore retain its original expiry too.
	legacyRefresh.ExpiresAt = nil
	if err := store.AddWatchlistItem(ctx, legacyRefresh); err != nil {
		t.Fatalf("legacy refresh: %v", err)
	}
	stored, err := store.GetWatchlistItem(ctx, ticker, "stock")
	if err != nil || stored == nil {
		t.Fatalf("read preserved profile = %#v, %v", stored, err)
	}
	if stored.DiscoveryContext == nil || stored.DiscoveryContext.Thesis != "Original thesis" ||
		stored.ExecutionProfile == nil || stored.ExecutionProfile.RecommendedLens != "momentum" ||
		stored.ScannerRunID != initialRunID || stored.Source != "scanner" ||
		stored.GraphContext != "Original graph context" || stored.ExpiresAt == nil ||
		!stored.ExpiresAt.Equal(firstExpiry) || stored.MinConfidence != initial.MinConfidence {
		t.Fatalf("legacy refresh erased scanner contracts: %+v", stored)
	}

	replacementExpiry := time.Now().Add(2 * time.Hour).UTC().Truncate(time.Microsecond)
	replacement := initial
	replacement.ScannerRunID = beginWatchlistTradeProfileRun(t, ctx)
	replacement.ExpiresAt = &replacementExpiry
	replacement.DiscoveryContext = &tradeprofile.DiscoveryContext{
		Version: tradeprofile.DiscoveryContextVersion, Sources: []string{"premarket_episodic"},
		Thesis: "Fresh catalyst", EvidenceBySource: map[string]json.RawMessage{
			"premarket_episodic": json.RawMessage(`{"gap_pct":8.2}`),
		},
	}
	replacement.ExecutionProfile = &tradeprofile.ExecutionProfile{
		Version: tradeprofile.ExecutionProfileVersion, RecommendedLens: "news_sentiment",
		AdmissibleLenses: []string{"news_sentiment", "momentum"}, PrimaryTimeframe: "1H",
		ScorerRationale: "Fresh event requires intraday confirmation.",
	}
	if err := store.AddWatchlistItem(ctx, replacement); err != nil {
		t.Fatalf("replace profile: %v", err)
	}
	stored, err = store.GetWatchlistItem(ctx, ticker, "stock")
	if err != nil || stored == nil {
		t.Fatalf("read replacement profile = %#v, %v", stored, err)
	}
	if stored.DiscoveryContext == nil || stored.DiscoveryContext.Thesis != "Fresh catalyst" ||
		stored.ExecutionProfile == nil || stored.ExecutionProfile.RecommendedLens != "news_sentiment" ||
		stored.ScannerRunID != replacement.ScannerRunID || !stored.Active ||
		stored.ExpiresAt == nil || !stored.ExpiresAt.Equal(replacementExpiry) {
		t.Fatalf("complete scanner refresh did not replace contracts atomically: %+v", stored)
	}
}

func TestManualUpsertReplacesExpiredWatchlistTradeProfile(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("DATABASE_URL not set; skipping store integration test")
	}
	ctx := context.Background()
	const ticker = "PROFILE_EXPIRED_REPLACE_RT"
	t.Cleanup(func() { _, _ = store.GetDB().Exec(ctx, "DELETE FROM watchlist WHERE ticker=$1", ticker) })
	runID := beginWatchlistTradeProfileRun(t, ctx)
	expiredAt := time.Now().Add(-time.Hour).UTC().Truncate(time.Microsecond)
	if err := store.AddWatchlistItem(ctx, store.WatchlistItem{
		Ticker: ticker, Platform: "stock", Source: "scanner", GraphContext: "expired scanner origin",
		ScannerRunID: runID, ExpiresAt: &expiredAt, Strategies: []string{"momentum"}, Timeframes: []string{"1D"}, MinConfidence: 0.88,
		DiscoveryContext: &tradeprofile.DiscoveryContext{
			Version: tradeprofile.DiscoveryContextVersion, Sources: []string{"weekly_leader"}, Thesis: "Expired scanner thesis",
			EvidenceBySource: map[string]json.RawMessage{"weekly_leader": json.RawMessage(`{"rank":1}`)},
		},
		ExecutionProfile: &tradeprofile.ExecutionProfile{
			Version: tradeprofile.ExecutionProfileVersion, RecommendedLens: "momentum", AdmissibleLenses: []string{"momentum"},
			PrimaryTimeframe: "1D", ScorerRationale: "Expired scanner recommendation.",
		},
	}); err != nil {
		t.Fatalf("insert expired scanner profile: %v", err)
	}

	if err := store.AddWatchlistItem(ctx, store.WatchlistItem{
		Ticker: ticker, Platform: "stock", Source: "manual", Strategies: []string{"mean_reversion"},
		Timeframes: []string{"4H"}, MinConfidence: 0.37,
	}); err != nil {
		t.Fatalf("manual replacement of expired profile: %v", err)
	}
	stored, err := store.GetWatchlistItem(ctx, ticker, "stock")
	if err != nil || stored == nil {
		t.Fatalf("read manual replacement = %#v, %v", stored, err)
	}
	if !stored.Active || stored.Source != "manual" || stored.GraphContext != "" || stored.ScannerRunID != "" ||
		stored.ExpiresAt != nil || stored.DiscoveryContext != nil || stored.ExecutionProfile != nil ||
		stored.MinConfidence != 0.37 {
		t.Fatalf("manual upsert retained expired scanner contract: %+v", stored)
	}
}

func TestLegacyWatchlistUpsertReplacesLineageWithoutContract(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("DATABASE_URL not set; skipping store integration test")
	}
	ctx := context.Background()
	const ticker = "PROFILE_LEGACY_REPLACE_RT"
	t.Cleanup(func() { _, _ = store.GetDB().Exec(ctx, "DELETE FROM watchlist WHERE ticker=$1", ticker) })
	runID := beginWatchlistTradeProfileRun(t, ctx)
	expiresAt := time.Now().Add(time.Hour).UTC().Truncate(time.Microsecond)
	if err := store.AddWatchlistItem(ctx, store.WatchlistItem{
		Ticker: ticker, Platform: "stock", Source: "scanner", GraphContext: "legacy scanner origin",
		ScannerRunID: runID, ExpiresAt: &expiresAt,
		Strategies: []string{"momentum"}, Timeframes: []string{"1D"},
	}); err != nil {
		t.Fatalf("insert legacy scanner row: %v", err)
	}

	if err := store.AddWatchlistItem(ctx, store.WatchlistItem{
		Ticker: ticker, Platform: "stock", Source: "manual",
		Strategies: []string{"mean_reversion"}, Timeframes: []string{"4H"},
	}); err != nil {
		t.Fatalf("manual legacy refresh: %v", err)
	}
	stored, err := store.GetWatchlistItem(ctx, ticker, "stock")
	if err != nil || stored == nil {
		t.Fatalf("read legacy refresh = %#v, %v", stored, err)
	}
	if stored.Source != "manual" || stored.GraphContext != "" || stored.ScannerRunID != "" ||
		stored.ExpiresAt != nil || stored.DiscoveryContext != nil || stored.ExecutionProfile != nil {
		t.Fatalf("legacy refresh retained scanner lineage: %+v", stored)
	}
}

func TestInvalidPersistedWatchlistContractDegradesOnlyThatRow(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("DATABASE_URL not set; skipping store integration test")
	}
	ctx := context.Background()
	const invalidTicker = "PROFILE_INVALID_DEGRADE_RT"
	const legacyTicker = "PROFILE_LIST_SURVIVES_RT"
	t.Cleanup(func() {
		_, _ = store.GetDB().Exec(ctx, "DELETE FROM watchlist WHERE ticker IN ($1, $2)", invalidTicker, legacyTicker)
	})
	runID := beginWatchlistTradeProfileRun(t, ctx)
	expiresAt := time.Now().Add(time.Hour).UTC().Truncate(time.Microsecond)
	if err := store.AddWatchlistItem(ctx, store.WatchlistItem{
		Ticker: invalidTicker, Platform: "stock", Source: "scanner", ScannerRunID: runID, ExpiresAt: &expiresAt,
		Strategies: []string{"momentum"}, Timeframes: []string{"1D"},
		DiscoveryContext: &tradeprofile.DiscoveryContext{
			Version: tradeprofile.DiscoveryContextVersion, Sources: []string{"weekly_leader"}, Thesis: "Valid before corruption",
			EvidenceBySource: map[string]json.RawMessage{"weekly_leader": json.RawMessage(`{"rank":1}`)},
		},
		ExecutionProfile: &tradeprofile.ExecutionProfile{
			Version: tradeprofile.ExecutionProfileVersion, RecommendedLens: "momentum", AdmissibleLenses: []string{"momentum"},
			PrimaryTimeframe: "1D", ScorerRationale: "Valid before corruption.",
		},
	}); err != nil {
		t.Fatalf("insert profile row: %v", err)
	}
	if err := store.AddWatchlistItem(ctx, store.WatchlistItem{
		Ticker: legacyTicker, Platform: "stock", Source: "manual",
		Strategies: []string{"mean_reversion"}, Timeframes: []string{"1D"},
	}); err != nil {
		t.Fatalf("insert unaffected row: %v", err)
	}
	if _, err := store.GetDB().Exec(ctx,
		"UPDATE watchlist SET discovery_context=$2::jsonb WHERE ticker=$1", invalidTicker, `{"version":999}`,
	); err != nil {
		t.Fatalf("corrupt persisted discovery context: %v", err)
	}

	items, err := store.GetWatchlistItems(ctx, false)
	if err != nil {
		t.Fatalf("GetWatchlistItems failed for one invalid contract: %v", err)
	}
	var invalid, unaffected *store.WatchlistItem
	for i := range items {
		switch items[i].Ticker {
		case invalidTicker:
			invalid = &items[i]
		case legacyTicker:
			unaffected = &items[i]
		}
	}
	if invalid == nil || invalid.DiscoveryContext != nil || invalid.ExecutionProfile != nil {
		t.Fatalf("invalid row did not degrade to legacy: %+v", invalid)
	}
	if unaffected == nil || unaffected.Source != "manual" {
		t.Fatalf("valid row was not returned intact: %+v", unaffected)
	}
}

func TestWatchlistTradeProfileRejectsPartialUpsertsWithoutMutation(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("DATABASE_URL not set; skipping store integration test")
	}
	ctx := context.Background()
	const ticker = "PROFILE_PARTIAL_RT"
	t.Cleanup(func() { _, _ = store.GetDB().Exec(ctx, "DELETE FROM watchlist WHERE ticker=$1", ticker) })
	runID := beginWatchlistTradeProfileRun(t, ctx)

	expiresAt := time.Now().Add(time.Hour).UTC().Truncate(time.Microsecond)
	initial := store.WatchlistItem{
		Ticker: ticker, Platform: "stock", Source: "scanner", ExpiresAt: &expiresAt, ScannerRunID: runID,
		Strategies: []string{"momentum"}, Timeframes: []string{"1D"},
		DiscoveryContext: &tradeprofile.DiscoveryContext{
			Version: tradeprofile.DiscoveryContextVersion, Sources: []string{"weekly_leader"},
			Thesis: "Original thesis", EvidenceBySource: map[string]json.RawMessage{
				"weekly_leader": json.RawMessage(`{"rank":1}`),
			},
		},
		ExecutionProfile: &tradeprofile.ExecutionProfile{
			Version: tradeprofile.ExecutionProfileVersion, RecommendedLens: "momentum",
			AdmissibleLenses: []string{"momentum", "trend_following"}, PrimaryTimeframe: "1D",
			ScorerRationale: "Original scanner recommendation.",
		},
	}
	if err := store.AddWatchlistItem(ctx, initial); err != nil {
		t.Fatalf("insert profile: %v", err)
	}

	partialDiscovery := initial
	partialDiscovery.Strategies = []string{"mean_reversion"}
	partialDiscovery.ExecutionProfile = nil
	if err := store.AddWatchlistItem(ctx, partialDiscovery); err == nil {
		t.Fatal("partial discovery context upsert succeeded")
	}
	assertWatchlistTradeProfileUnchanged(t, ctx, ticker, runID)

	partialProfile := initial
	partialProfile.Strategies = []string{"trend_following"}
	partialProfile.DiscoveryContext = nil
	if err := store.AddWatchlistItem(ctx, partialProfile); err == nil {
		t.Fatal("partial execution profile upsert succeeded")
	}
	assertWatchlistTradeProfileUnchanged(t, ctx, ticker, runID)
}

func beginWatchlistTradeProfileRun(t *testing.T, ctx context.Context) string {
	t.Helper()
	runID, err := store.BeginScannerRun(ctx, store.ScannerRun{StartedAt: time.Now().UTC()})
	if err != nil {
		t.Fatalf("BeginScannerRun: %v", err)
	}
	t.Cleanup(func() { _, _ = store.GetDB().Exec(ctx, "DELETE FROM scanner_runs WHERE id=$1", runID) })
	return runID
}

func assertWatchlistTradeProfileUnchanged(t *testing.T, ctx context.Context, ticker, runID string) {
	t.Helper()
	stored, err := store.GetWatchlistItem(ctx, ticker, "stock")
	if err != nil || stored == nil {
		t.Fatalf("read unchanged profile = %#v, %v", stored, err)
	}
	if stored.DiscoveryContext == nil || stored.DiscoveryContext.Thesis != "Original thesis" ||
		stored.ExecutionProfile == nil || stored.ExecutionProfile.RecommendedLens != "momentum" ||
		stored.ScannerRunID != runID || len(stored.Strategies) != 1 || stored.Strategies[0] != "momentum" {
		t.Fatalf("partial upsert mutated stored watchlist item: %+v", stored)
	}
}
