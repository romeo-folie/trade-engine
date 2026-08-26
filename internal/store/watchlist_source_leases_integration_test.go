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
	changedLeases, err := store.RefreshActiveScannerWatchlistItemWithChangedLeases(ctx, retry, nil)
	if err != nil || len(changedLeases) != 0 {
		t.Fatalf("identical refresh = changed=%#v err=%v, want no changes/nil", changedLeases, err)
	}
	afterRetry, err := store.GetWatchlistItem(ctx, ticker, "stock")
	if err != nil || afterRetry == nil || afterRetry.ExpiresAt == nil || !afterRetry.ExpiresAt.Equal(firstExpiry) {
		t.Fatalf("identical refresh renewed expiry: item=%+v err=%v", afterRetry, err)
	}

	updated := retry
	updated.SourceLeases[0].EvidenceFingerprint = "second"
	updated.SourceLeases[0].Evidence = json.RawMessage(`{"price":11}`)
	updated.SourceLeases[0].ExpiresAt = firstExpiry.Add(48 * time.Hour)
	changedLeases, err = store.RefreshActiveScannerWatchlistItemWithChangedLeases(ctx, updated, nil)
	if err != nil || len(changedLeases) != 1 || changedLeases[0].Source != "mover" ||
		changedLeases[0].EvidenceFingerprint != "second" {
		t.Fatalf("meaningful refresh = changed=%#v err=%v, want changed mover/nil", changedLeases, err)
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

func TestActiveWatchlistRaceReclassifiesResearchQueueMembership(t *testing.T) {
	ctx := context.Background()
	const ticker = "LEASE_QUEUE_RECLASSIFY_RT"
	t.Cleanup(func() { _, _ = store.GetDB().Exec(ctx, "DELETE FROM watchlist WHERE ticker=$1", ticker) })
	now := time.Now().UTC().Truncate(time.Microsecond)
	expiresAt := now.Add(24 * time.Hour)
	item := store.WatchlistItem{
		Ticker: ticker, Platform: "stock", Source: "scanner", Active: true,
		Strategies: []string{"momentum"}, Timeframes: []string{"4H"}, ExpiresAt: &expiresAt,
		SourceLeases: []store.WatchlistSourceLease{{
			Source: "mover", EvidenceFingerprint: "market", QualifiedAt: now, ObservedAt: now,
			EffectiveAt: now, ExpiresAt: expiresAt, Evidence: json.RawMessage(`{"price":10}`),
		}},
	}
	if err := store.AddWatchlistItem(ctx, item); err != nil {
		t.Fatalf("add scanner item: %v", err)
	}
	if _, err := store.GetDB().Exec(ctx, `
		INSERT INTO research_queue_memberships (watchlist_id,bucket,allocation,priority)
		SELECT id,'market_activity','reserved','{}'::jsonb FROM watchlist
		WHERE ticker=$1 AND platform='stock'`, ticker); err != nil {
		t.Fatalf("seed research queue membership: %v", err)
	}

	political := item
	political.SourceLeases = []store.WatchlistSourceLease{{
		Source: "political_disclosure", EvidenceFingerprint: "political", QualifiedAt: now, ObservedAt: now,
		EffectiveAt: now, ExpiresAt: expiresAt, Evidence: json.RawMessage(`{"report_ids":["r1"]}`),
	}}
	if changed, err := store.AddWatchlistItemUnlessActiveWithChangedLeases(ctx, political); !errors.Is(err, store.ErrWatchlistActiveItemExists) || len(changed) != 1 {
		t.Fatalf("active-row race refresh = changed=%+v err=%v", changed, err)
	}
	var bucket string
	if err := store.GetDB().QueryRow(ctx, `
		SELECT q.bucket FROM research_queue_memberships q
		JOIN watchlist w ON w.id=q.watchlist_id WHERE w.ticker=$1`, ticker).Scan(&bucket); err != nil {
		t.Fatalf("read research queue membership: %v", err)
	}
	if bucket != "political_disclosure" {
		t.Fatalf("membership bucket=%q, want political_disclosure", bucket)
	}
}

func TestExpireDiscoveryContextsReleasesSharedScannerQueueMembership(t *testing.T) {
	ctx := context.Background()
	const ticker = "EXPIRED_SHARED_CONTEXT_RT"
	t.Cleanup(func() { _, _ = store.GetDB().Exec(ctx, "DELETE FROM watchlist WHERE ticker=$1", ticker) })
	now := time.Now().UTC().Truncate(time.Microsecond)
	expiredAt := now.Add(-time.Hour)
	item := store.WatchlistItem{
		Ticker: ticker, Platform: "stock", Source: "scanner", Active: true,
		Strategies: []string{"momentum"}, Timeframes: []string{"4H"}, ExpiresAt: &expiredAt,
		SourceLeases: []store.WatchlistSourceLease{{
			Source: "mover", EvidenceFingerprint: "expired-market", QualifiedAt: expiredAt.Add(-time.Hour),
			ObservedAt: expiredAt.Add(-time.Hour), EffectiveAt: expiredAt.Add(-time.Hour), ExpiresAt: expiredAt,
			Evidence: json.RawMessage(`{"price":10}`),
		}},
	}
	if err := store.AddWatchlistItem(ctx, item); err != nil {
		t.Fatalf("add expired scanner item: %v", err)
	}
	context := tradeprofile.DiscoveryContext{
		Version: tradeprofile.DiscoveryContextVersion, Sources: []string{"earnings_scheduled"},
		Thesis:           "Expired scheduled earnings context.",
		EvidenceBySource: map[string]json.RawMessage{"earnings_scheduled": json.RawMessage(`{"event":"earnings"}`)},
	}
	if err := store.UpsertWatchlistDiscoveryContext(ctx, ticker, "stock", "earnings_scheduled:expired", context, &expiredAt); err != nil {
		t.Fatalf("upsert expired earnings context: %v", err)
	}
	if _, err := store.GetDB().Exec(ctx, `
		INSERT INTO research_queue_memberships (watchlist_id,bucket,allocation,priority)
		SELECT id,'market_activity','reserved','{}'::jsonb FROM watchlist
		WHERE ticker=$1 AND platform='stock'`, ticker); err != nil {
		t.Fatalf("seed research queue membership: %v", err)
	}

	if err := store.ExpireWatchlistDiscoveryContexts(ctx); err != nil {
		t.Fatalf("expire discovery contexts: %v", err)
	}
	var active bool
	if err := store.GetDB().QueryRow(ctx, `SELECT active FROM watchlist WHERE ticker=$1 AND platform='stock'`, ticker).Scan(&active); err != nil {
		t.Fatalf("read watchlist state: %v", err)
	}
	if active {
		t.Fatal("shared scanner row remained active after its final lease and context expired")
	}
	var memberships int
	if err := store.GetDB().QueryRow(ctx, `
		SELECT COUNT(*) FROM research_queue_memberships q
		JOIN watchlist w ON w.id=q.watchlist_id WHERE w.ticker=$1`, ticker).Scan(&memberships); err != nil {
		t.Fatalf("count queue memberships: %v", err)
	}
	if memberships != 0 {
		t.Fatalf("expired shared row retained %d research queue memberships", memberships)
	}
}

func TestExpireDiscoveryContextsReleasesManualEarningsQueueMembership(t *testing.T) {
	ctx := context.Background()
	const ticker = "EXPIRED_MANUAL_CONTEXT_RT"
	t.Cleanup(func() { _, _ = store.GetDB().Exec(ctx, "DELETE FROM watchlist WHERE ticker=$1", ticker) })
	if err := store.AddWatchlistItem(ctx, store.WatchlistItem{
		Ticker: ticker, Platform: "stock", Source: "manual", Active: true,
		Strategies: []string{"momentum"}, Timeframes: []string{"4H"},
	}); err != nil {
		t.Fatalf("add manual item: %v", err)
	}
	expiredAt := time.Now().UTC().Add(-time.Hour)
	context := tradeprofile.DiscoveryContext{
		Version: tradeprofile.DiscoveryContextVersion, Sources: []string{"earnings_scheduled"},
		Thesis:           "Expired scheduled earnings context.",
		EvidenceBySource: map[string]json.RawMessage{"earnings_scheduled": json.RawMessage(`{"event":"earnings"}`)},
	}
	if err := store.UpsertWatchlistDiscoveryContext(ctx, ticker, "stock", "earnings_scheduled:expired", context, &expiredAt); err != nil {
		t.Fatalf("upsert expired earnings context: %v", err)
	}
	if _, err := store.GetDB().Exec(ctx, `
		INSERT INTO research_queue_memberships (watchlist_id,bucket,allocation,priority)
		SELECT id,'earnings_scheduled','reserved','{}'::jsonb FROM watchlist
		WHERE ticker=$1 AND platform='stock'`, ticker); err != nil {
		t.Fatalf("seed research queue membership: %v", err)
	}
	if err := store.ExpireWatchlistDiscoveryContexts(ctx); err != nil {
		t.Fatalf("expire discovery contexts: %v", err)
	}
	var active bool
	if err := store.GetDB().QueryRow(ctx, `SELECT active FROM watchlist WHERE ticker=$1 AND platform='stock'`, ticker).Scan(&active); err != nil {
		t.Fatalf("read manual watchlist state: %v", err)
	}
	if !active {
		t.Fatal("expired earnings context deactivated its manual owner")
	}
	var memberships int
	if err := store.GetDB().QueryRow(ctx, `
		SELECT COUNT(*) FROM research_queue_memberships q
		JOIN watchlist w ON w.id=q.watchlist_id WHERE w.ticker=$1`, ticker).Scan(&memberships); err != nil {
		t.Fatalf("count queue memberships: %v", err)
	}
	if memberships != 0 {
		t.Fatalf("manual row retained %d expired earnings queue memberships", memberships)
	}
}

func TestExpireDiscoveryContextsKeepsLegacyScannerWithoutExpiry(t *testing.T) {
	ctx := context.Background()
	const ticker = "LEGACY_SCANNER_NO_EXPIRY_RT"
	t.Cleanup(func() { _, _ = store.GetDB().Exec(ctx, "DELETE FROM watchlist WHERE ticker=$1", ticker) })
	if err := store.AddWatchlistItem(ctx, store.WatchlistItem{
		Ticker: ticker, Platform: "stock", Source: "scanner", Active: true,
		Strategies: []string{"momentum"}, Timeframes: []string{"4H"},
	}); err != nil {
		t.Fatalf("add legacy scanner item: %v", err)
	}
	if err := store.ExpireWatchlistDiscoveryContexts(ctx); err != nil {
		t.Fatalf("expire discovery contexts: %v", err)
	}
	var active bool
	if err := store.GetDB().QueryRow(ctx, `SELECT active FROM watchlist WHERE ticker=$1 AND platform='stock'`, ticker).Scan(&active); err != nil {
		t.Fatalf("read legacy scanner state: %v", err)
	}
	if !active {
		t.Fatal("after-close context cleanup deactivated a legacy scanner row with no expiry")
	}
}

func TestRemoveWatchlistItemReleasesResearchQueueMembership(t *testing.T) {
	ctx := context.Background()
	const ticker = "REMOVE_QUEUE_MEMBERSHIP_RT"
	t.Cleanup(func() { _, _ = store.GetDB().Exec(ctx, "DELETE FROM watchlist WHERE ticker=$1", ticker) })
	if err := store.AddWatchlistItem(ctx, store.WatchlistItem{
		Ticker: ticker, Platform: "stock", Source: "manual", Active: true,
		Strategies: []string{"momentum"}, Timeframes: []string{"4H"},
	}); err != nil {
		t.Fatalf("add manual item: %v", err)
	}
	if _, err := store.GetDB().Exec(ctx, `
		INSERT INTO research_queue_memberships (watchlist_id,bucket,allocation,priority)
		SELECT id,'earnings_scheduled','reserved','{}'::jsonb FROM watchlist
		WHERE ticker=$1 AND platform='stock'`, ticker); err != nil {
		t.Fatalf("seed research queue membership: %v", err)
	}
	if err := store.RemoveWatchlistItem(ctx, ticker, "stock"); err != nil {
		t.Fatalf("remove watchlist item: %v", err)
	}
	var memberships int
	if err := store.GetDB().QueryRow(ctx, `
		SELECT COUNT(*) FROM research_queue_memberships q
		JOIN watchlist w ON w.id=q.watchlist_id WHERE w.ticker=$1`, ticker).Scan(&memberships); err != nil {
		t.Fatalf("count queue memberships: %v", err)
	}
	if memberships != 0 {
		t.Fatalf("removed row retained %d research queue memberships", memberships)
	}
}

func TestReactivateWatchlistItemRequiresFreshResearchAdmission(t *testing.T) {
	ctx := context.Background()
	const scannerTicker = "REACTIVATE_RESEARCH_RT"
	const manualTicker = "REACTIVATE_MANUAL_RT"
	t.Cleanup(func() {
		_, _ = store.GetDB().Exec(ctx, "DELETE FROM watchlist WHERE ticker IN ($1,$2)", scannerTicker, manualTicker)
	})
	now := time.Now().UTC()
	expires := now.Add(time.Hour)
	if err := store.AddWatchlistItem(ctx, store.WatchlistItem{
		Ticker: scannerTicker, Platform: "stock", Source: "scanner",
		Strategies: []string{"momentum"}, Timeframes: []string{"4H"},
		SourceLeases: []store.WatchlistSourceLease{{
			Source: "mover", EvidenceFingerprint: "research", QualifiedAt: now,
			ObservedAt: now, EffectiveAt: now, ExpiresAt: expires, Evidence: json.RawMessage(`{}`),
		}},
	}); err != nil {
		t.Fatalf("add scanner item: %v", err)
	}
	if _, err := store.GetDB().Exec(ctx, `
		INSERT INTO research_queue_memberships (watchlist_id,bucket,allocation,priority)
		SELECT id,'market_activity','reserved','{}'::jsonb FROM watchlist WHERE ticker=$1`, scannerTicker); err != nil {
		t.Fatalf("seed research membership: %v", err)
	}
	if err := store.RemoveWatchlistItem(ctx, scannerTicker, "stock"); err != nil {
		t.Fatalf("disable scanner item: %v", err)
	}
	if err := store.ReactivateWatchlistItem(ctx, scannerTicker, "stock"); !errors.Is(err, store.ErrWatchlistResearchReactivationRequiresAdmission) {
		t.Fatalf("reactivate scanner item error=%v, want research admission requirement", err)
	}
	var active bool
	var memberships int
	if err := store.GetDB().QueryRow(ctx, `
		SELECT w.active,COUNT(q.watchlist_id) FROM watchlist w
		LEFT JOIN research_queue_memberships q ON q.watchlist_id=w.id
		WHERE w.ticker=$1 GROUP BY w.active`, scannerTicker).Scan(&active, &memberships); err != nil {
		t.Fatalf("read disabled scanner state: %v", err)
	}
	if active || memberships != 0 {
		t.Fatalf("research reactivation changed active=%v memberships=%d", active, memberships)
	}

	if err := store.AddWatchlistItem(ctx, store.WatchlistItem{
		Ticker: manualTicker, Platform: "stock", Source: "manual",
		Strategies: []string{"momentum"}, Timeframes: []string{"4H"},
	}); err != nil {
		t.Fatalf("add manual item: %v", err)
	}
	if err := store.RemoveWatchlistItem(ctx, manualTicker, "stock"); err != nil {
		t.Fatalf("disable manual item: %v", err)
	}
	if err := store.ReactivateWatchlistItem(ctx, manualTicker, "stock"); err != nil {
		t.Fatalf("reactivate manual item: %v", err)
	}
	if err := store.GetDB().QueryRow(ctx, `SELECT active FROM watchlist WHERE ticker=$1`, manualTicker).Scan(&active); err != nil {
		t.Fatalf("read reactivated manual state: %v", err)
	}
	if !active {
		t.Fatal("manual item did not reactivate")
	}
}

func TestStaleEarningsCapacitySuppressionRollsBackCohort(t *testing.T) {
	ctx := context.Background()
	var earningsRunsTable *string
	if err := store.GetDB().QueryRow(ctx, `SELECT to_regclass('public.earnings_watchlist_runs')::text`).Scan(&earningsRunsTable); err != nil {
		t.Fatalf("check earnings watchlist schema: %v", err)
	}
	if earningsRunsTable == nil {
		t.Skip("earnings watchlist schema is unavailable in this integration database")
	}
	target := time.Date(2035, time.January, 8, 0, 0, 0, 0, time.UTC)
	const slotTicker = "EARNINGS_STALE_SLOT_RT"
	const candidateTicker = "EARNINGS_STALE_CANDIDATE_RT"
	t.Cleanup(func() {
		_, _ = store.GetDB().Exec(context.Background(), `DELETE FROM earnings_watchlist_runs WHERE target_session_date=$1::date`, target)
		_, _ = store.GetDB().Exec(context.Background(), "DELETE FROM watchlist WHERE ticker IN ($1,$2)", slotTicker, candidateTicker)
	})
	now := time.Now().UTC().Truncate(time.Microsecond)
	expiresAt := now.Add(24 * time.Hour)
	if err := store.AddWatchlistItem(ctx, store.WatchlistItem{
		Ticker: slotTicker, Platform: "stock", Source: "scanner", Active: true,
		Strategies: []string{"momentum"}, Timeframes: []string{"4H"}, ExpiresAt: &expiresAt,
		SourceLeases: []store.WatchlistSourceLease{{
			Source: "mover", EvidenceFingerprint: "slot", QualifiedAt: now, ObservedAt: now,
			EffectiveAt: now, ExpiresAt: expiresAt, Evidence: json.RawMessage(`{"price":10}`),
		}},
	}); err != nil {
		t.Fatalf("add capacity fixture: %v", err)
	}
	if _, err := store.GetDB().Exec(ctx, `
		INSERT INTO research_queue_memberships (watchlist_id,bucket,allocation,priority)
		SELECT id,'market_activity','reserved','{}'::jsonb FROM watchlist WHERE ticker=$1`, slotTicker); err != nil {
		t.Fatalf("seed occupied queue slot: %v", err)
	}
	staleAttempt, claimed, err := store.ClaimEarningsWatchlistRun(ctx, target, now)
	if err != nil || !claimed {
		t.Fatalf("claim stale attempt=%q claimed=%v err=%v", staleAttempt, claimed, err)
	}
	if _, err := store.GetDB().Exec(ctx, `UPDATE earnings_watchlist_runs SET lease_until=$2 WHERE target_session_date=$1::date`, target, now.Add(-time.Minute)); err != nil {
		t.Fatalf("expire stale lease: %v", err)
	}
	if freshAttempt, claimed, err := store.ClaimEarningsWatchlistRun(ctx, target, now); err != nil || !claimed || freshAttempt == staleAttempt {
		t.Fatalf("reclaim attempt=%q claimed=%v err=%v", freshAttempt, claimed, err)
	}
	context := tradeprofile.DiscoveryContext{
		Version: tradeprofile.DiscoveryContextVersion, Sources: []string{"earnings_scheduled"}, Thesis: "Scheduled earnings.",
		EvidenceBySource: map[string]json.RawMessage{"earnings_scheduled": json.RawMessage(`{"event":"earnings"}`)},
	}
	err = store.PublishEarningsWatchlistRun(ctx, target, staleAttempt, []store.EarningsWatchlistCandidate{{
		Ticker: candidateTicker, ContextKey: "earnings_scheduled:stale", Context: context, ExpiresAt: expiresAt,
		ResearchQueue: &store.ResearchQueueAdmission{Capacity: 1, Bucket: "earnings_scheduled", Reservation: 1, Overflow: 0},
	}})
	if err == nil {
		t.Fatal("stale capacity-suppressed publish succeeded")
	}
	var contexts, decisions int
	if err := store.GetDB().QueryRow(ctx, `SELECT COUNT(*) FROM watchlist_discovery_contexts c JOIN watchlist w ON w.id=c.watchlist_id WHERE w.ticker=$1`, candidateTicker).Scan(&contexts); err != nil {
		t.Fatalf("count stale contexts: %v", err)
	}
	if err := store.GetDB().QueryRow(ctx, `SELECT COUNT(*) FROM research_queue_decisions WHERE ticker=$1`, candidateTicker).Scan(&decisions); err != nil {
		t.Fatalf("count stale decisions: %v", err)
	}
	if contexts != 0 || decisions != 0 {
		t.Fatalf("stale publish committed contexts=%d decisions=%d", contexts, decisions)
	}
}

func TestEarningsCapacitySuppressionPreservesLiquidityOrderAndRollsBackCohort(t *testing.T) {
	ctx := context.Background()
	var earningsRunsTable *string
	if err := store.GetDB().QueryRow(ctx, `SELECT to_regclass('public.earnings_watchlist_runs')::text`).Scan(&earningsRunsTable); err != nil {
		t.Fatalf("check earnings watchlist schema: %v", err)
	}
	if earningsRunsTable == nil {
		t.Skip("earnings watchlist schema is unavailable in this integration database")
	}
	target := time.Date(2035, time.January, 9, 0, 0, 0, 0, time.UTC)
	const firstTicker = "ZZZ_EARNINGS_LIQUIDITY_FIRST_RT"
	const suppressedTicker = "AAA_EARNINGS_LIQUIDITY_SUPPRESSED_RT"
	t.Cleanup(func() {
		_, _ = store.GetDB().Exec(context.Background(), `DELETE FROM research_queue_decisions WHERE ticker IN ($1,$2)`, firstTicker, suppressedTicker)
		_, _ = store.GetDB().Exec(context.Background(), `DELETE FROM earnings_watchlist_runs WHERE target_session_date=$1::date`, target)
		_, _ = store.GetDB().Exec(context.Background(), "DELETE FROM watchlist WHERE ticker IN ($1,$2)", firstTicker, suppressedTicker)
	})
	var baselineMemberships int
	if err := store.GetDB().QueryRow(ctx, `SELECT COUNT(*) FROM research_queue_memberships`).Scan(&baselineMemberships); err != nil {
		t.Fatalf("count baseline queue memberships: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	attemptID, claimed, err := store.ClaimEarningsWatchlistRun(ctx, target, now)
	if err != nil || !claimed {
		t.Fatalf("claim attempt=%q claimed=%v err=%v", attemptID, claimed, err)
	}
	context := tradeprofile.DiscoveryContext{
		Version: tradeprofile.DiscoveryContextVersion, Sources: []string{"earnings_scheduled"}, Thesis: "Scheduled earnings.",
		EvidenceBySource: map[string]json.RawMessage{"earnings_scheduled": json.RawMessage(`{"event":"earnings"}`)},
	}
	queue := func(rank int) *store.ResearchQueueAdmission {
		priority, marshalErr := json.Marshal(map[string]any{"liquidity_rank": rank})
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		return &store.ResearchQueueAdmission{
			Capacity: baselineMemberships + 1, Bucket: "earnings_scheduled",
			Reservation: baselineMemberships + 1, Overflow: 0, Priority: priority,
		}
	}
	err = store.PublishEarningsWatchlistRun(ctx, target, attemptID, []store.EarningsWatchlistCandidate{
		// The feeder supplies liquidity order. Canonical lock order must not
		// change it, so this first Z ticker claims the final slot and A is the
		// durable capacity suppression.
		{Ticker: firstTicker, ContextKey: "earnings_scheduled:first", Context: context, ExpiresAt: now.Add(24 * time.Hour), LiquidityRank: 1, ResearchQueue: queue(1)},
		{Ticker: suppressedTicker, ContextKey: "earnings_scheduled:suppressed", Context: context, ExpiresAt: now.Add(24 * time.Hour), LiquidityRank: 2, ResearchQueue: queue(2)},
	})
	if !errors.Is(err, store.ErrResearchQueueCapacity) {
		t.Fatalf("publish error=%v, want research queue capacity", err)
	}
	var contexts, memberships, audits, firstDecisions, suppressedDecisions int
	if err := store.GetDB().QueryRow(ctx, `
		SELECT
			(SELECT COUNT(*) FROM watchlist_discovery_contexts c JOIN watchlist w ON w.id=c.watchlist_id WHERE w.ticker IN ($1,$2)),
			(SELECT COUNT(*) FROM research_queue_memberships q JOIN watchlist w ON w.id=q.watchlist_id WHERE w.ticker IN ($1,$2)),
			(SELECT COUNT(*) FROM earnings_watchlist_candidates WHERE target_session_date=$3::date),
			(SELECT COUNT(*) FROM research_queue_decisions WHERE ticker=$1),
			(SELECT COUNT(*) FROM research_queue_decisions WHERE ticker=$2)`,
		firstTicker, suppressedTicker, target,
	).Scan(&contexts, &memberships, &audits, &firstDecisions, &suppressedDecisions); err != nil {
		t.Fatalf("read rollback state: %v", err)
	}
	if contexts != 0 || memberships != 0 || audits != 0 || firstDecisions != 0 || suppressedDecisions != 1 {
		t.Fatalf("capacity rollback contexts=%d memberships=%d audits=%d first_decisions=%d suppressed_decisions=%d", contexts, memberships, audits, firstDecisions, suppressedDecisions)
	}
	var status string
	if err := store.GetDB().QueryRow(ctx, `SELECT status FROM earnings_watchlist_runs WHERE target_session_date=$1::date`, target).Scan(&status); err != nil {
		t.Fatalf("read failed run: %v", err)
	}
	if status != "failed" {
		t.Fatalf("run status=%q, want failed for retry", status)
	}
}

func TestResearchQueueReclassificationSerializesWithAdmissionLock(t *testing.T) {
	ctx := context.Background()
	var queueTable *string
	if err := store.GetDB().QueryRow(ctx, `SELECT to_regclass('public.research_queue_memberships')::text`).Scan(&queueTable); err != nil {
		t.Fatalf("check research queue schema: %v", err)
	}
	if queueTable == nil {
		t.Skip("research queue schema is unavailable in this integration database")
	}
	holder, err := store.GetDB().Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback(ctx) //nolint:errcheck
	if _, err := holder.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('research-queue:v1', 0))`); err != nil {
		t.Fatal(err)
	}
	reclassifier, err := store.GetDB().Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		err := store.ReclassifyResearchQueueMemberships(ctx, reclassifier)
		if err == nil {
			err = reclassifier.Commit(ctx)
		} else {
			_ = reclassifier.Rollback(ctx)
		}
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("reclassification bypassed queue lock: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if err := holder.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("reclassification after queue release: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("reclassification remained blocked after queue release")
	}
}

func TestRemoveWatchlistItemTakesTickerBeforeQueueLock(t *testing.T) {
	ctx := context.Background()
	const ticker = "REMOVE_QUEUE_LOCK_ORDER_RT"
	t.Cleanup(func() { _, _ = store.GetDB().Exec(ctx, "DELETE FROM watchlist WHERE ticker=$1", ticker) })
	if err := store.AddWatchlistItem(ctx, store.WatchlistItem{Ticker: ticker, Platform: "stock", Source: "scanner", Strategies: []string{"momentum"}, Timeframes: []string{"4H"}}); err != nil {
		t.Fatal(err)
	}
	holder, err := store.GetDB().Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback(ctx) //nolint:errcheck
	var backendPID int
	if err := holder.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&backendPID); err != nil {
		t.Fatal(err)
	}
	if _, err := holder.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "scanner-watchlist:stock:"+ticker); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- store.RemoveWatchlistItem(ctx, ticker, "stock") }()
	blocked := false
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if err := store.GetDB().QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_stat_activity a WHERE $1 = ANY(pg_blocking_pids(a.pid)) AND a.wait_event_type='Lock' AND a.query LIKE '%pg_advisory_xact_lock%')`, backendPID).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			break
		}
	}
	if !blocked {
		t.Fatal("removal did not block on held ticker lock")
	}
	queueTx, err := store.GetDB().Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var acquired bool
	if err := queueTx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock(hashtextextended('research-queue:v1',0))`).Scan(&acquired); err != nil {
		t.Fatal(err)
	}
	if !acquired {
		t.Fatal("removal acquired queue before its ticker lock")
	}
	if err := queueTx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := holder.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("removal remained blocked")
	}
}

func TestLegacyScannerLeaseRefreshInstallsCompleteTradeProfile(t *testing.T) {
	ctx := context.Background()
	const ticker = "LEASE_LEGACY_PROFILE_RT"
	t.Cleanup(func() { _, _ = store.GetDB().Exec(ctx, "DELETE FROM watchlist WHERE ticker=$1", ticker) })
	now := time.Now().UTC().Truncate(time.Microsecond)
	expiresAt := now.Add(24 * time.Hour)
	legacy := store.WatchlistItem{
		Ticker: ticker, Platform: "stock", Source: "scanner", Strategies: []string{"momentum"},
		Timeframes: []string{"1D"}, ExpiresAt: &expiresAt,
		SourceLeases: []store.WatchlistSourceLease{{
			Source: "mover", EvidenceFingerprint: "legacy", QualifiedAt: now, ObservedAt: now,
			EffectiveAt: now, ExpiresAt: expiresAt, Evidence: json.RawMessage(`{"price":10}`),
		}},
	}
	if err := store.AddWatchlistItem(ctx, legacy); err != nil {
		t.Fatalf("add legacy scanner item: %v", err)
	}
	refresh := legacy
	refresh.DiscoveryContext = &tradeprofile.DiscoveryContext{
		Version: tradeprofile.DiscoveryContextVersion, Sources: []string{"mover"}, Thesis: "Fresh mover evidence",
		EvidenceBySource: map[string]json.RawMessage{"mover": json.RawMessage(`{"price":11}`)},
	}
	refresh.ExecutionProfile = &tradeprofile.ExecutionProfile{
		Version: tradeprofile.ExecutionProfileVersion, RecommendedLens: "momentum",
		AdmissibleLenses: []string{"momentum"}, PrimaryTimeframe: "1D", ScorerRationale: "Fresh scanner refresh.",
	}
	refresh.SourceLeases = append([]store.WatchlistSourceLease(nil), legacy.SourceLeases...)
	refresh.SourceLeases[0].EvidenceFingerprint = "fresh"
	refresh.SourceLeases[0].Evidence = json.RawMessage(`{"price":11}`)
	if changed, err := store.RefreshActiveScannerWatchlistItem(ctx, refresh, nil); err != nil || !changed {
		t.Fatalf("refresh legacy scanner item = changed=%v err=%v, want true/nil", changed, err)
	}
	stored, err := store.GetWatchlistItem(ctx, ticker, "stock")
	if err != nil || stored == nil || stored.DiscoveryContext == nil || stored.ExecutionProfile == nil {
		t.Fatalf("legacy refresh did not install complete trade profile: %+v, %v", stored, err)
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
		Ticker: ticker, Platform: "stock", Source: "scanner", ScannerRunID: runID, Strategies: []string{"momentum"}, Timeframes: []string{"1D"}, GraphContext: "stale admission thesis", ExpiresAt: &liveExpiry,
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
	beforeRefresh, err := store.GetWatchlistItem(ctx, ticker, "stock")
	if err != nil || beforeRefresh == nil || beforeRefresh.EffectiveDiscoveryContext() == nil {
		t.Fatalf("read lease-backed item before refresh: %+v, %v", beforeRefresh, err)
	}
	if _, found := beforeRefresh.EffectiveDiscoveryContext().EvidenceBySource["political_disclosure"]; found {
		t.Fatalf("expired political evidence reached effective context without a refresh: %+v", beforeRefresh.EffectiveDiscoveryContext())
	}
	if beforeRefresh.EffectiveDiscoveryContext().Thesis == "" {
		t.Fatalf("live source context lost its usable thesis: %+v", beforeRefresh.EffectiveDiscoveryContext())
	}
	refresh := item
	refresh.GraphContext = "fresh refreshed thesis"
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
	if stored.GraphContext != "fresh refreshed thesis" {
		t.Fatalf("refreshed graph context = %q, want current provenance", stored.GraphContext)
	}
	if _, err := store.GetDB().Exec(ctx, `
		UPDATE watchlist_source_leases SET expires_at=NOW()-INTERVAL '1 second'
		WHERE watchlist_id=(SELECT id FROM watchlist WHERE ticker=$1)`, ticker); err != nil {
		t.Fatalf("expire final source lease: %v", err)
	}
	expiredItem, err := store.GetWatchlistItem(ctx, ticker, "stock")
	if err != nil || expiredItem == nil {
		t.Fatalf("read item after final source expiry: %+v, %v", expiredItem, err)
	}
	if expiredItem.DiscoveryContext != nil || expiredItem.ExecutionProfile != nil {
		t.Fatalf("expired final source returned half contract: %+v", expiredItem)
	}
}

func TestActiveScannerRaceCommitsRefreshedLease(t *testing.T) {
	ctx := context.Background()
	const ticker = "LEASE_RACE_COMMIT_RT"
	t.Cleanup(func() { _, _ = store.GetDB().Exec(ctx, "DELETE FROM watchlist WHERE ticker=$1", ticker) })
	now := time.Now().UTC().Truncate(time.Microsecond)
	expiresAt := now.Add(24 * time.Hour)
	initial := store.WatchlistItem{Ticker: ticker, Platform: "stock", Source: "scanner", Strategies: []string{"momentum"}, Timeframes: []string{"1D"}, ExpiresAt: &expiresAt,
		SourceLeases: []store.WatchlistSourceLease{{Source: "mover", EvidenceFingerprint: "mover", QualifiedAt: now, ObservedAt: now, EffectiveAt: now, ExpiresAt: expiresAt, Evidence: json.RawMessage(`{"price":10}`)}},
	}
	if err := store.AddWatchlistItem(ctx, initial); err != nil {
		t.Fatalf("add initial: %v", err)
	}
	race := initial
	race.SourceLeases = []store.WatchlistSourceLease{{Source: "graph_neighbor", EvidenceFingerprint: "graph", QualifiedAt: now, ObservedAt: now, EffectiveAt: now, ExpiresAt: expiresAt, Evidence: json.RawMessage(`{"relationship":"supplies"}`)}}
	changedLeases, err := store.AddWatchlistItemWithChangedLeases(ctx, race)
	if !errors.Is(err, store.ErrWatchlistActiveItemExists) {
		t.Fatalf("race error = %v, want ErrWatchlistActiveItemExists", err)
	}
	if len(changedLeases) != 1 || changedLeases[0].Source != "graph_neighbor" {
		t.Fatalf("changed leases = %+v, want committed graph-neighbor lease", changedLeases)
	}
	var count int
	if err := store.GetDB().QueryRow(ctx, `SELECT count(*) FROM watchlist_source_leases l JOIN watchlist w ON w.id=l.watchlist_id WHERE w.ticker=$1 AND l.source='graph_neighbor'`, ticker).Scan(&count); err != nil || count != 1 {
		t.Fatalf("committed graph lease = %d, %v; want 1", count, err)
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

func TestScannerReadmissionReplacesExpiredScannerLeaseTakenOverByEarnings(t *testing.T) {
	ctx := context.Background()
	const ticker = "LEASE_EARNINGS_READMISSION_RT"
	t.Cleanup(func() { _, _ = store.GetDB().Exec(ctx, "DELETE FROM watchlist WHERE ticker=$1", ticker) })
	now := time.Now().UTC().Truncate(time.Microsecond)
	expired := now.Add(-time.Hour)
	if err := store.AddWatchlistItem(ctx, store.WatchlistItem{
		Ticker: ticker, Platform: "stock", Source: "scanner", Strategies: []string{"momentum"}, Timeframes: []string{"1D"}, ExpiresAt: &expired,
		SourceLeases: []store.WatchlistSourceLease{{
			Source: "mover", EvidenceFingerprint: "expired-mover", QualifiedAt: now.Add(-48 * time.Hour), ObservedAt: now.Add(-48 * time.Hour), EffectiveAt: now.Add(-48 * time.Hour), ExpiresAt: expired, Evidence: json.RawMessage(`{"price":10}`),
		}},
	}); err != nil {
		t.Fatalf("add expired scanner row: %v", err)
	}
	// Earnings publication preserves this row and its historical scanner lease
	// while making its own additive context the active lifecycle owner.
	if err := store.AddWatchlistItem(ctx, store.WatchlistItem{
		Ticker: ticker, Platform: "stock", Source: "earnings_watchlist",
		Strategies: []string{"trend_following", "momentum", "news_sentiment"}, Timeframes: []string{"1D", "4H", "1H"},
		GraphContext: "Scheduled earnings watchlist candidate.",
	}); err != nil {
		t.Fatalf("take over expired scanner row for earnings: %v", err)
	}
	newExpiry := now.Add(24 * time.Hour)
	err := store.AddWatchlistItemUnlessActive(ctx, store.WatchlistItem{
		Ticker: ticker, Platform: "stock", Source: "scanner", Strategies: []string{"momentum"}, Timeframes: []string{"4H"}, ExpiresAt: &newExpiry,
		SourceLeases: []store.WatchlistSourceLease{{
			Source: "mover", EvidenceFingerprint: "fresh-mover", QualifiedAt: now, ObservedAt: now, EffectiveAt: now, ExpiresAt: newExpiry, Evidence: json.RawMessage(`{"price":11}`),
		}},
	})
	if err != nil {
		t.Fatalf("readmit fresh scanner evidence over earnings takeover: %v", err)
	}
	item, err := store.GetWatchlistItem(ctx, ticker, "stock")
	if err != nil || item == nil || item.Source != "scanner" || item.ExpiresAt == nil || !item.ExpiresAt.Equal(newExpiry) {
		t.Fatalf("scanner readmission = %+v, %v", item, err)
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
