package store

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/tradeprofile"
)

func TestResearchQueueAdmissionNormalizesStaleBucketBeforeCounting(t *testing.T) {
	if !IsConnected() {
		t.Skip("DATABASE_URL not set")
	}
	ctx := context.Background()
	const staleTicker = "QUEUE_STALE_BUCKET_RT"
	const candidateTicker = "QUEUE_NORMALIZED_ADMISSION_RT"
	t.Cleanup(func() {
		_, _ = GetDB().Exec(ctx, "DELETE FROM watchlist WHERE ticker IN ($1,$2)", staleTicker, candidateTicker)
	})
	now := time.Now().UTC()
	expires := now.Add(time.Hour)
	if err := AddWatchlistItem(ctx, WatchlistItem{Ticker: staleTicker, Platform: "stock", Source: "scanner", Strategies: []string{"momentum"}, Timeframes: []string{"4H"}, SourceLeases: []WatchlistSourceLease{{Source: "political_disclosure", EvidenceFingerprint: "stale", QualifiedAt: now, ObservedAt: now, EffectiveAt: now, ExpiresAt: expires, Evidence: json.RawMessage(`{}`)}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := GetDB().Exec(ctx, `INSERT INTO research_queue_memberships (watchlist_id,bucket,allocation,priority) SELECT id,'market_activity','reserved','{}'::jsonb FROM watchlist WHERE ticker=$1`, staleTicker); err != nil {
		t.Fatal(err)
	}
	var membershipCount, marketReserved int
	if err := GetDB().QueryRow(ctx, `SELECT COUNT(*), COUNT(*) FILTER (WHERE bucket='market_activity' AND allocation='reserved') FROM research_queue_memberships`).Scan(&membershipCount, &marketReserved); err != nil {
		t.Fatal(err)
	}
	if err := AddWatchlistItem(ctx, WatchlistItem{Ticker: candidateTicker, Platform: "stock", Source: "scanner", Strategies: []string{"momentum"}, Timeframes: []string{"4H"}, SourceLeases: []WatchlistSourceLease{{Source: "mover", EvidenceFingerprint: "candidate", QualifiedAt: now, ObservedAt: now, EffectiveAt: now, ExpiresAt: expires, Evidence: json.RawMessage(`{}`)}}, ResearchQueue: &ResearchQueueAdmission{Capacity: membershipCount + 1, Bucket: "market_activity", Reservation: marketReserved, Overflow: 0}}); err != nil {
		t.Fatalf("admission did not normalize stale bucket: %v", err)
	}
	var staleBucket, candidateBucket string
	if err := GetDB().QueryRow(ctx, `SELECT q.bucket FROM research_queue_memberships q JOIN watchlist w ON w.id=q.watchlist_id WHERE w.ticker=$1`, staleTicker).Scan(&staleBucket); err != nil {
		t.Fatal(err)
	}
	if err := GetDB().QueryRow(ctx, `SELECT q.bucket FROM research_queue_memberships q JOIN watchlist w ON w.id=q.watchlist_id WHERE w.ticker=$1`, candidateTicker).Scan(&candidateBucket); err != nil {
		t.Fatal(err)
	}
	if staleBucket != "political_disclosure" || candidateBucket != "market_activity" {
		t.Fatalf("buckets stale=%s candidate=%s", staleBucket, candidateBucket)
	}
}

func TestExistingResearchQueueMembershipRefreshesOnlyItsSelectedBucketPriority(t *testing.T) {
	if !IsConnected() {
		t.Skip("DATABASE_URL not set")
	}
	ctx := context.Background()
	const ticker = "QUEUE_EXISTING_PRIORITY_RT"
	t.Cleanup(func() { _, _ = GetDB().Exec(ctx, "DELETE FROM watchlist WHERE ticker=$1", ticker) })
	now := time.Now().UTC()
	expires := now.Add(time.Hour)
	if err := AddWatchlistItem(ctx, WatchlistItem{
		Ticker: ticker, Platform: "stock", Source: "scanner",
		Strategies: []string{"momentum"}, Timeframes: []string{"4H"},
		SourceLeases: []WatchlistSourceLease{{
			Source: "mover", EvidenceFingerprint: "market", QualifiedAt: now,
			ObservedAt: now, EffectiveAt: now, ExpiresAt: expires, Evidence: json.RawMessage(`{}`),
		}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := GetDB().Exec(ctx, `
		INSERT INTO research_queue_memberships (watchlist_id,bucket,allocation,priority)
		SELECT id,'market_activity','reserved','{"score":1}'::jsonb FROM watchlist WHERE ticker=$1`, ticker); err != nil {
		t.Fatal(err)
	}
	if _, err := GetDB().Exec(ctx, `
		INSERT INTO watchlist_source_leases
			(watchlist_id,source,evidence_fingerprint,qualified_at,observed_at,effective_at,expires_at,evidence)
		SELECT id,'political_disclosure','political',$2,$2,$2,$3,'{}'::jsonb FROM watchlist WHERE ticker=$1`, ticker, now, expires); err != nil {
		t.Fatal(err)
	}
	politicalPriority := json.RawMessage(`{"score":99}`)
	tx, err := GetDB().Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := recordResearchQueueAdmissionTx(ctx, tx, WatchlistItem{Ticker: ticker, Platform: "stock"}, ResearchQueueAdmission{
		Bucket: "political_disclosure", Priority: politicalPriority,
	}, researchQueueReservation{existing: true}); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var bucket string
	var score int
	if err := GetDB().QueryRow(ctx, `
		SELECT bucket,(priority->>'score')::int FROM research_queue_memberships q
		JOIN watchlist w ON w.id=q.watchlist_id WHERE w.ticker=$1`, ticker).Scan(&bucket, &score); err != nil {
		t.Fatal(err)
	}
	if bucket != "political_disclosure" || score != 99 {
		t.Fatalf("political reclassification bucket=%q score=%d, want political_disclosure/99", bucket, score)
	}

	marketPriority := json.RawMessage(`{"score":777}`)
	tx, err = GetDB().Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := recordResearchQueueAdmissionTx(ctx, tx, WatchlistItem{Ticker: ticker, Platform: "stock"}, ResearchQueueAdmission{
		Bucket: "market_activity", Priority: marketPriority,
	}, researchQueueReservation{existing: true}); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := GetDB().QueryRow(ctx, `
		SELECT bucket,(priority->>'score')::int FROM research_queue_memberships q
		JOIN watchlist w ON w.id=q.watchlist_id WHERE w.ticker=$1`, ticker).Scan(&bucket, &score); err != nil {
		t.Fatal(err)
	}
	if bucket != "political_disclosure" || score != 99 {
		t.Fatalf("lower-precedence admission overwrote bucket=%q score=%d", bucket, score)
	}
}

func TestExpiryQueueReleaseWaitsForExistingAdmissionContextRefresh(t *testing.T) {
	if !IsConnected() {
		t.Skip("DATABASE_URL not set")
	}
	ctx := context.Background()
	const ticker = "QUEUE_EXPIRY_ADMISSION_RACE_RT"
	const contextKey = "earnings_scheduled:race"
	t.Cleanup(func() { _, _ = GetDB().Exec(ctx, "DELETE FROM watchlist WHERE ticker=$1", ticker) })
	if err := AddWatchlistItem(ctx, WatchlistItem{
		Ticker: ticker, Platform: "stock", Source: "manual",
		Strategies: []string{"momentum"}, Timeframes: []string{"4H"},
	}); err != nil {
		t.Fatal(err)
	}
	expires := time.Now().UTC().Add(time.Hour)
	earningsContext := tradeprofile.DiscoveryContext{
		Version: tradeprofile.DiscoveryContextVersion, Sources: []string{"earnings_scheduled"},
		Thesis: "Coordinated earnings refresh.",
		EvidenceBySource: map[string]json.RawMessage{
			"earnings_scheduled": json.RawMessage(`{"event":"earnings"}`),
		},
	}
	if err := UpsertWatchlistDiscoveryContext(ctx, ticker, "stock", contextKey, earningsContext, &expires); err != nil {
		t.Fatal(err)
	}
	if _, err := GetDB().Exec(ctx, `
		INSERT INTO research_queue_memberships (watchlist_id,bucket,allocation,priority)
		SELECT id,'earnings_scheduled','reserved','{}'::jsonb FROM watchlist WHERE ticker=$1`, ticker); err != nil {
		t.Fatal(err)
	}

	// Hold an otherwise-live context while moving its expiry into the past.
	// Admission still sees the committed live context, reserves its existing
	// membership under the queue lock, and then waits to refresh this row.
	expiryTx, err := GetDB().Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer expiryTx.Rollback(ctx) //nolint:errcheck
	var expiryPID int
	if err := expiryTx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&expiryPID); err != nil {
		t.Fatal(err)
	}
	if _, err := expiryTx.Exec(ctx, `
		UPDATE watchlist_discovery_contexts SET expires_at=NOW()-INTERVAL '1 hour'
		WHERE context_key=$1`, contextKey); err != nil {
		t.Fatal(err)
	}

	admissionReady := make(chan int, 1)
	admissionDone := make(chan error, 1)
	go func() {
		tx, beginErr := GetDB().Begin(ctx)
		if beginErr != nil {
			admissionDone <- beginErr
			return
		}
		defer tx.Rollback(ctx) //nolint:errcheck
		if beginErr = lockResearchQueueTx(ctx, tx); beginErr != nil {
			admissionDone <- beginErr
			return
		}
		var existing bool
		if beginErr = tx.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM research_queue_memberships q
				JOIN watchlist w ON w.id=q.watchlist_id
				WHERE w.ticker=$1 AND w.platform='stock' AND w.active
			)`, ticker).Scan(&existing); beginErr != nil {
			admissionDone <- beginErr
			return
		}
		if !existing {
			admissionDone <- fmt.Errorf("admission did not retain existing membership")
			return
		}
		var admissionPID int
		if beginErr = tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&admissionPID); beginErr != nil {
			admissionDone <- beginErr
			return
		}
		admissionReady <- admissionPID
		raw, marshalErr := tradeprofile.MarshalDiscoveryContext(&earningsContext)
		if marshalErr != nil {
			admissionDone <- marshalErr
			return
		}
		if _, beginErr = tx.Exec(ctx, `
			UPDATE watchlist_discovery_contexts
			SET source='earnings_scheduled', context=$2::jsonb, expires_at=$3, active=true, updated_at=NOW()
			WHERE context_key=$1`, contextKey, raw, expires); beginErr != nil {
			admissionDone <- beginErr
			return
		}
		if beginErr = recordResearchQueueAdmissionTx(ctx, tx, WatchlistItem{Ticker: ticker, Platform: "stock"}, ResearchQueueAdmission{
			Bucket: "earnings_scheduled", Priority: json.RawMessage(`{"liquidity_rank":1}`),
		}, researchQueueReservation{existing: true}); beginErr != nil {
			admissionDone <- beginErr
			return
		}
		admissionDone <- tx.Commit(ctx)
	}()

	admissionPID := <-admissionReady
	blocked := false
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if err := GetDB().QueryRow(ctx, `SELECT $1 = ANY(pg_blocking_pids($2))`, expiryPID, admissionPID).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			break
		}
	}
	if !blocked {
		t.Fatal("admission did not wait for the expiry-held context row")
	}
	// Exercise the production row/context expiry phase while the admission is
	// holding the queue lock. Reintroducing the former unlocked membership
	// release here makes this regression end with zero memberships.
	if err := expireWatchlistDiscoveryContextsTx(ctx, expiryTx); err != nil {
		t.Fatal(err)
	}
	if err := expiryTx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-admissionDone:
		if err != nil {
			t.Fatalf("admission refresh: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("admission remained blocked after expiry committed")
	}
	queueTx, err := GetDB().Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer queueTx.Rollback(ctx) //nolint:errcheck
	if err := ReclassifyResearchQueueMemberships(ctx, queueTx); err != nil {
		t.Fatal(err)
	}
	if err := queueTx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var active bool
	var contexts, memberships int
	if err := GetDB().QueryRow(ctx, `
		SELECT w.active,
			(SELECT COUNT(*) FROM watchlist_discovery_contexts c WHERE c.watchlist_id=w.id AND c.active),
			(SELECT COUNT(*) FROM research_queue_memberships q WHERE q.watchlist_id=w.id)
		FROM watchlist w WHERE w.ticker=$1`, ticker).Scan(&active, &contexts, &memberships); err != nil {
		t.Fatal(err)
	}
	if !active || contexts != 1 || memberships != 1 {
		t.Fatalf("race result active=%v contexts=%d memberships=%d, want true/1/1", active, contexts, memberships)
	}
}
