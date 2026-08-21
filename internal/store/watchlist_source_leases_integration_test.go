package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/store"
	"github.com/romeo-folie/trade-engine/internal/tradeprofile"
)

func TestWatchlistSourceLeaseRefreshIsEvidenceGatedAndPreservesAdmission(t *testing.T) {
	ctx := context.Background()
	const ticker = "LEASE_REFRESH_RT"
	t.Cleanup(func() { _, _ = store.GetDB().Exec(ctx, "DELETE FROM watchlist WHERE ticker=$1", ticker) })
	now := time.Now().UTC().Truncate(time.Microsecond)
	firstExpiry := now.Add(24 * time.Hour)
	item := store.WatchlistItem{
		Ticker: ticker, Platform: "stock", Source: "scanner", Active: true,
		Strategies: []string{"momentum"}, Timeframes: []string{"4H"}, MinConfidence: 0.81,
		ExpiresAt: &firstExpiry,
		SourceLeases: []store.WatchlistSourceLease{{
			Source: "mover", EvidenceFingerprint: "first", QualifiedAt: now,
			ObservedAt: now, EffectiveAt: now, ExpiresAt: firstExpiry,
			Evidence: json.RawMessage(`{"price":10}`),
		}},
	}
	if err := store.AddWatchlistItem(ctx, item); err != nil {
		t.Fatalf("add lease-backed watchlist item: %v", err)
	}
	before, err := store.GetWatchlistItem(ctx, ticker, "stock")
	if err != nil || before == nil {
		t.Fatalf("read initial item: %v (%+v)", err, before)
	}

	// A retry offers a later expiry but the same fingerprint; it must be inert.
	retry := item
	retry.SourceLeases = append([]store.WatchlistSourceLease(nil), item.SourceLeases...)
	retry.SourceLeases[0].ExpiresAt = firstExpiry.Add(24 * time.Hour)
	changed, err := store.RefreshActiveScannerWatchlistItem(ctx, retry, nil)
	if err != nil || changed {
		t.Fatalf("identical refresh = changed=%v err=%v, want false/nil", changed, err)
	}
	afterRetry, err := store.GetWatchlistItem(ctx, ticker, "stock")
	if err != nil || afterRetry == nil || afterRetry.ExpiresAt == nil || !afterRetry.ExpiresAt.Equal(firstExpiry) {
		t.Fatalf("identical refresh renewed expiry: item=%+v err=%v", afterRetry, err)
	}

	updated := retry
	updated.SourceLeases[0].EvidenceFingerprint = "second"
	updated.SourceLeases[0].Evidence = json.RawMessage(`{"price":11}`)
	updated.SourceLeases[0].ExpiresAt = firstExpiry.Add(48 * time.Hour)
	changed, err = store.RefreshActiveScannerWatchlistItem(ctx, updated, nil)
	if err != nil || !changed {
		t.Fatalf("meaningful refresh = changed=%v err=%v, want true/nil", changed, err)
	}
	after, err := store.GetWatchlistItem(ctx, ticker, "stock")
	if err != nil || after == nil {
		t.Fatalf("read refreshed item: %v (%+v)", err, after)
	}
	if !after.CreatedAt.Equal(before.CreatedAt) || after.MinConfidence != before.MinConfidence ||
		len(after.Strategies) != 1 || after.Strategies[0] != "momentum" ||
		after.ExpiresAt == nil || !after.ExpiresAt.Equal(updated.SourceLeases[0].ExpiresAt) {
		t.Fatalf("refresh changed admission contract or did not project expiry: before=%+v after=%+v", before, after)
	}

	if err := store.RemoveWatchlistItem(ctx, ticker, "stock"); err != nil {
		t.Fatalf("remove active item: %v", err)
	}
	if _, err := store.RefreshActiveScannerWatchlistItem(ctx, updated, nil); !errors.Is(err, store.ErrWatchlistRefreshNotActive) {
		t.Fatalf("inactive refresh error = %v, want ErrWatchlistRefreshNotActive", err)
	}
}

func TestWatchlistRemainsLiveUntilItsFinalSourceLeaseExpires(t *testing.T) {
	ctx := context.Background()
	const ticker = "LEASE_MULTI_RT"
	t.Cleanup(func() { _, _ = store.GetDB().Exec(ctx, "DELETE FROM watchlist WHERE ticker=$1", ticker) })
	now := time.Now().UTC().Truncate(time.Microsecond)
	firstExpiry := now.Add(-time.Hour)
	secondExpiry := now.Add(time.Hour)
	item := store.WatchlistItem{
		Ticker: ticker, Platform: "stock", Source: "scanner", Strategies: []string{"momentum"},
		ExpiresAt: &secondExpiry,
		SourceLeases: []store.WatchlistSourceLease{
			{Source: "mover", EvidenceFingerprint: "mover-1", QualifiedAt: now.Add(-2 * time.Hour), ObservedAt: now.Add(-2 * time.Hour), EffectiveAt: now.Add(-2 * time.Hour), ExpiresAt: firstExpiry, Evidence: json.RawMessage(`{"price":10}`)},
			{Source: "weekly_leader", EvidenceFingerprint: "weekly-1", QualifiedAt: now, ObservedAt: now, EffectiveAt: now, ExpiresAt: secondExpiry, Evidence: json.RawMessage(`{"rank":1}`)},
		},
	}
	if err := store.AddWatchlistItem(ctx, item); err != nil {
		t.Fatalf("add multi-lease item: %v", err)
	}
	count, err := store.CountActiveScannerItems(ctx)
	if err != nil || count < 1 {
		t.Fatalf("active scanner count with one remaining lease = %d, %v", count, err)
	}
	active, err := store.GetWatchlistItems(ctx, true)
	if err != nil {
		t.Fatalf("list active watchlist: %v", err)
	}
	if !containsWatchlistTicker(active, ticker) {
		t.Fatalf("ticker disappeared after only one source lease expired: %+v", active)
	}

	if _, err := store.GetDB().Exec(ctx, `
		UPDATE watchlist_source_leases SET expires_at=NOW()-INTERVAL '1 second'
		WHERE watchlist_id=(SELECT id FROM watchlist WHERE ticker=$1)`, ticker); err != nil {
		t.Fatalf("expire final source lease: %v", err)
	}
	expired, err := store.ExpireScannedItems(ctx)
	if err != nil || !containsString(expired, ticker) {
		t.Fatalf("expire final lease = %v, %v", expired, err)
	}
	active, err = store.GetWatchlistItems(ctx, true)
	if err != nil {
		t.Fatalf("list expired watchlist: %v", err)
	}
	if containsWatchlistTicker(active, ticker) {
		t.Fatalf("ticker remained live after final source lease expiry: %+v", active)
	}
}

func TestRefreshDropsEvidenceWhoseSourceLeaseExpired(t *testing.T) {
	ctx := context.Background()
	const ticker = "LEASE_EVIDENCE_EXPIRY_RT"
	t.Cleanup(func() { _, _ = store.GetDB().Exec(ctx, "DELETE FROM watchlist WHERE ticker=$1", ticker) })
	now := time.Now().UTC().Truncate(time.Microsecond)
	runID, err := store.BeginScannerRun(ctx, store.ScannerRun{StartedAt: now})
	if err != nil {
		t.Fatalf("begin scanner run: %v", err)
	}
	liveExpiry := now.Add(24 * time.Hour)
	expiredExpiry := now.Add(-time.Hour)
	profile := &tradeprofile.ExecutionProfile{
		Version: tradeprofile.ExecutionProfileVersion, RecommendedLens: "momentum",
		AdmissibleLenses: []string{"momentum"}, PrimaryTimeframe: "1D", ScorerRationale: "Test evidence expiry.",
	}
	item := store.WatchlistItem{
		Ticker: ticker, Platform: "stock", Source: "scanner", ScannerRunID: runID, Strategies: []string{"momentum"}, Timeframes: []string{"1D"}, ExpiresAt: &liveExpiry,
		DiscoveryContext: &tradeprofile.DiscoveryContext{
			Version: tradeprofile.DiscoveryContextVersion, Sources: []string{"mover", "political_disclosure"}, Thesis: "Initial provenance",
			EvidenceBySource: map[string]json.RawMessage{"mover": json.RawMessage(`{"price":10}`), "political_disclosure": json.RawMessage(`{"report_count":2}`)},
		},
		ExecutionProfile: profile,
		SourceLeases: []store.WatchlistSourceLease{
			{Source: "mover", EvidenceFingerprint: "mover-old", QualifiedAt: now, ObservedAt: now, EffectiveAt: now, ExpiresAt: liveExpiry, Evidence: json.RawMessage(`{"price":10}`)},
			{Source: "political_disclosure", EvidenceFingerprint: "filing-old", QualifiedAt: now.Add(-48 * time.Hour), ObservedAt: now.Add(-48 * time.Hour), EffectiveAt: now.Add(-48 * time.Hour), ExpiresAt: expiredExpiry, Evidence: json.RawMessage(`{"report_count":2}`)},
		},
	}
	if err := store.AddWatchlistItem(ctx, item); err != nil {
		t.Fatalf("add item: %v", err)
	}
	refresh := item
	refresh.DiscoveryContext = &tradeprofile.DiscoveryContext{
		Version: tradeprofile.DiscoveryContextVersion, Sources: []string{"mover"}, Thesis: "Fresh mover provenance",
		EvidenceBySource: map[string]json.RawMessage{"mover": json.RawMessage(`{"price":11}`)},
	}
	refresh.SourceLeases = []store.WatchlistSourceLease{{
		Source: "mover", EvidenceFingerprint: "mover-new", QualifiedAt: now, ObservedAt: now, EffectiveAt: now, ExpiresAt: liveExpiry, Evidence: json.RawMessage(`{"price":11}`),
	}}
	if changed, err := store.RefreshActiveScannerWatchlistItem(ctx, refresh, nil); err != nil || !changed {
		t.Fatalf("refresh = changed=%v err=%v, want true/nil", changed, err)
	}
	stored, err := store.GetWatchlistItem(ctx, ticker, "stock")
	if err != nil || stored == nil || stored.DiscoveryContext == nil {
		t.Fatalf("read refreshed item: %+v, %v", stored, err)
	}
	if _, found := stored.DiscoveryContext.EvidenceBySource["political_disclosure"]; found {
		t.Fatalf("expired political evidence remained: %+v", stored.DiscoveryContext)
	}
	if _, found := stored.DiscoveryContext.EvidenceBySource["mover"]; !found {
		t.Fatalf("fresh mover evidence missing: %+v", stored.DiscoveryContext)
	}
}

func TestScannerFallbackDoesNotAttachLeaseToActiveManualItem(t *testing.T) {
	ctx := context.Background()
	const ticker = "LEASE_MANUAL_RACE_RT"
	t.Cleanup(func() { _, _ = store.GetDB().Exec(ctx, "DELETE FROM watchlist WHERE ticker=$1", ticker) })
	if err := store.AddWatchlistItem(ctx, store.WatchlistItem{
		Ticker: ticker, Platform: "stock", Source: "manual", Strategies: []string{"mean_reversion"}, Timeframes: []string{"1D"},
	}); err != nil {
		t.Fatalf("add manual item: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	expiresAt := now.Add(24 * time.Hour)
	err := store.AddWatchlistItemUnlessActive(ctx, store.WatchlistItem{
		Ticker: ticker, Platform: "stock", Source: "scanner", Strategies: []string{"momentum"}, Timeframes: []string{"1D"}, ExpiresAt: &expiresAt,
		SourceLeases: []store.WatchlistSourceLease{{
			Source: "mover", EvidenceFingerprint: "manual-race", QualifiedAt: now, ObservedAt: now, EffectiveAt: now, ExpiresAt: expiresAt, Evidence: json.RawMessage(`{"price":11}`),
		}},
	})
	if !errors.Is(err, store.ErrWatchlistActiveItemExists) {
		t.Fatalf("scanner fallback error = %v, want ErrWatchlistActiveItemExists", err)
	}
	item, err := store.GetWatchlistItem(ctx, ticker, "stock")
	if err != nil || item == nil || item.Source != "manual" || item.ExpiresAt != nil {
		t.Fatalf("manual item gained scanner lifecycle: %+v, %v", item, err)
	}
	var leaseCount int
	if err := store.GetDB().QueryRow(ctx, `SELECT count(*) FROM watchlist_source_leases l JOIN watchlist w ON w.id=l.watchlist_id WHERE w.ticker=$1`, ticker).Scan(&leaseCount); err != nil || leaseCount != 0 {
		t.Fatalf("manual item lease count = %d, %v; want 0", leaseCount, err)
	}
}

func TestExpiredLeaseWithIdenticalEvidenceDoesNotReactivateItem(t *testing.T) {
	ctx := context.Background()
	const ticker = "LEASE_IDENTICAL_EXPIRED_RT"
	t.Cleanup(func() { _, _ = store.GetDB().Exec(ctx, "DELETE FROM watchlist WHERE ticker=$1", ticker) })
	now := time.Now().UTC().Truncate(time.Microsecond)
	liveExpiry := now.Add(time.Hour)
	item := store.WatchlistItem{
		Ticker: ticker, Platform: "stock", Source: "scanner", Strategies: []string{"momentum"}, Timeframes: []string{"1D"}, ExpiresAt: &liveExpiry,
		SourceLeases: []store.WatchlistSourceLease{{
			Source: "mover", EvidenceFingerprint: "unchanged", QualifiedAt: now, ObservedAt: now, EffectiveAt: now, ExpiresAt: liveExpiry, Evidence: json.RawMessage(`{"price":10}`),
		}},
	}
	if err := store.AddWatchlistItem(ctx, item); err != nil {
		t.Fatalf("add initial item: %v", err)
	}
	if _, err := store.GetDB().Exec(ctx, `
		UPDATE watchlist_source_leases SET expires_at=NOW()-INTERVAL '1 second'
		WHERE watchlist_id=(SELECT id FROM watchlist WHERE ticker=$1)`, ticker); err != nil {
		t.Fatalf("expire lease: %v", err)
	}
	if _, err := store.GetDB().Exec(ctx, `UPDATE watchlist SET expires_at=NOW()-INTERVAL '1 second' WHERE ticker=$1`, ticker); err != nil {
		t.Fatalf("expire watchlist projection: %v", err)
	}
	expiredAt := now.Add(-time.Hour)
	item.ExpiresAt = &expiredAt
	if err := store.AddWatchlistItem(ctx, item); !errors.Is(err, store.ErrWatchlistRequalificationRequired) {
		t.Fatalf("identical expired re-admission error = %v, want ErrWatchlistRequalificationRequired", err)
	}
	active, err := store.GetWatchlistItems(ctx, true)
	if err != nil {
		t.Fatalf("list active items: %v", err)
	}
	if containsWatchlistTicker(active, ticker) {
		t.Fatalf("identical expired evidence reactivated ticker: %+v", active)
	}
}

func containsWatchlistTicker(items []store.WatchlistItem, ticker string) bool {
	for _, item := range items {
		if item.Ticker == ticker {
			return true
		}
	}
	return false
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
