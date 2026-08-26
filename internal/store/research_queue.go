package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

var ErrResearchQueueCapacity = errors.New("research queue capacity is unavailable")

// ResearchQueueAdmission carries deterministic source-owned allocation data.
// Priority is audit-only and is never execution confidence.
type ResearchQueueAdmission struct {
	Capacity    int
	Bucket      string
	Reservation int
	Overflow    int
	Priority    json.RawMessage
}

type researchQueueReservation struct {
	allocation string
	existing   bool
}

func reserveResearchQueueSlotTx(
	ctx context.Context, tx pgx.Tx, item WatchlistItem, request ResearchQueueAdmission,
) (researchQueueReservation, error) {
	bucket := strings.TrimSpace(request.Bucket)
	if bucket == "" || request.Capacity <= 0 || request.Reservation < 0 || request.Overflow < 0 {
		return researchQueueReservation{}, fmt.Errorf("invalid research queue admission")
	}
	if err := lockResearchQueueTx(ctx, tx); err != nil {
		return researchQueueReservation{}, err
	}
	if err := releaseInactiveResearchQueueMemberships(ctx, tx); err != nil {
		return researchQueueReservation{}, err
	}
	// Normalize live retained memberships under the same queue lock before
	// counting protected lanes; this closes expiry/reclassification gaps.
	if err := ReclassifyResearchQueueMemberships(ctx, tx); err != nil {
		return researchQueueReservation{}, err
	}
	var existing bool
	err := tx.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM research_queue_memberships q
			JOIN watchlist w ON w.id=q.watchlist_id
			WHERE w.ticker=$1 AND w.platform=$2 AND w.active
		)`, strings.ToUpper(strings.TrimSpace(item.Ticker)), item.Platform).Scan(&existing)
	if err != nil {
		return researchQueueReservation{}, err
	}
	if existing {
		return researchQueueReservation{existing: true}, nil
	}
	var used int
	if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM research_queue_memberships`).Scan(&used); err != nil {
		return researchQueueReservation{}, err
	}
	if used >= request.Capacity {
		return researchQueueReservation{}, ErrResearchQueueCapacity
	}
	if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM research_queue_memberships WHERE bucket=$1 AND allocation='reserved'`, bucket).Scan(&used); err != nil {
		return researchQueueReservation{}, err
	}
	if used < request.Reservation {
		return researchQueueReservation{allocation: "reserved"}, nil
	}
	if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM research_queue_memberships WHERE allocation='overflow'`).Scan(&used); err != nil {
		return researchQueueReservation{}, err
	}
	if used < request.Overflow {
		return researchQueueReservation{allocation: "overflow"}, nil
	}
	return researchQueueReservation{}, ErrResearchQueueCapacity
}

func lockResearchQueueTx(ctx context.Context, tx pgx.Tx) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('research-queue:v1', 0))`); err != nil {
		return fmt.Errorf("lock research queue: %w", err)
	}
	return nil
}

// releaseInactiveResearchQueueMemberships frees a queue slot as soon as its
// final research-retaining reason disappears. A watchlist row may remain active
// for manual/config ownership after an earnings context expires, but it must no
// longer consume scanner research capacity. Legacy lease-less scanner rows with
// a NULL expiry retain their established indefinite-liveness behavior.
func releaseInactiveResearchQueueMemberships(ctx context.Context, db watchlistExecer) error {
	_, err := db.Exec(ctx, `
		DELETE FROM research_queue_memberships q
		USING watchlist w
		WHERE w.id=q.watchlist_id
		AND NOT (
			w.active
			AND (
				EXISTS (
					SELECT 1 FROM watchlist_source_leases l
					WHERE l.watchlist_id=w.id AND l.expires_at > NOW()
				)
				OR (
					w.source='scanner'
					AND NOT EXISTS (SELECT 1 FROM watchlist_source_leases l WHERE l.watchlist_id=w.id)
					AND (w.expires_at IS NULL OR w.expires_at > NOW())
				)
				OR EXISTS (
					SELECT 1 FROM watchlist_discovery_contexts c
					WHERE c.watchlist_id=w.id AND c.source='earnings_scheduled'
					  AND c.active AND (c.expires_at IS NULL OR c.expires_at > NOW())
				)
			)
		)`)
	return err
}

func recordResearchQueueAdmissionTx(
	ctx context.Context, tx pgx.Tx, item WatchlistItem, request ResearchQueueAdmission, reservation researchQueueReservation,
) error {
	priority := request.Priority
	if len(priority) == 0 {
		priority = json.RawMessage(`{}`)
	}
	if !json.Valid(priority) {
		return fmt.Errorf("research queue priority must be JSON")
	}
	var watchlistID string
	if err := tx.QueryRow(ctx, `SELECT id::text FROM watchlist WHERE ticker=$1 AND platform=$2`, strings.ToUpper(strings.TrimSpace(item.Ticker)), item.Platform).Scan(&watchlistID); err != nil {
		return err
	}
	decision := "admitted"
	reason := "deterministic source qualification admitted to research queue"
	if reservation.existing {
		decision, reason = "already_member", "active research row retained; source evidence attached without another slot"
		// The source lease has already been refreshed by addWatchlistItem. Rebuild
		// the lane from all live evidence so a lower-precedence incoming context
		// (for example, earnings on a political row) cannot downgrade coverage.
		if err := ReclassifyResearchQueueMemberships(ctx, tx); err != nil {
			return err
		}
		// The incoming priority is authoritative only when its source owns the
		// reclassified lane. A lower-precedence source must not overwrite the
		// audit priority of an existing political/weekly membership.
		if _, err := tx.Exec(ctx, `
			UPDATE research_queue_memberships
			SET priority=$2::jsonb, updated_at=NOW()
			WHERE watchlist_id=$1::uuid AND bucket=$3`, watchlistID, priority, request.Bucket); err != nil {
			return err
		}
	} else if _, err := tx.Exec(ctx, `
		INSERT INTO research_queue_memberships (watchlist_id,bucket,allocation,priority)
		VALUES ($1::uuid,$2,$3,$4::jsonb)`, watchlistID, request.Bucket, reservation.allocation, priority); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO research_queue_decisions (watchlist_id,ticker,bucket,decision,allocation,reason,priority)
		VALUES ($1::uuid,$2,$3,$4,$5,$6,$7::jsonb)`, watchlistID,
		strings.ToUpper(strings.TrimSpace(item.Ticker)), request.Bucket, decision,
		nullableString(reservation.allocation), reason, priority)
	return err
}

func recordResearchQueueSuppressionTx(
	ctx context.Context, tx pgx.Tx, item WatchlistItem, request ResearchQueueAdmission,
) error {
	priority := request.Priority
	if len(priority) == 0 {
		priority = json.RawMessage(`{}`)
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO research_queue_decisions (ticker,bucket,decision,reason,priority)
		VALUES ($1,$2,'capacity_suppressed',$3,$4::jsonb)`,
		strings.ToUpper(strings.TrimSpace(item.Ticker)), request.Bucket,
		"source reservation and shared overflow are full", priority)
	return err
}

// RecordResearchQueueSuppression persists a capacity decision outside the
// candidate admission transaction. Admission transactions intentionally roll
// back on a full queue, so recording this decision inside that transaction
// would lose the audit trail with the attempted watchlist write.
func RecordResearchQueueSuppression(
	ctx context.Context, item WatchlistItem, request ResearchQueueAdmission,
) error {
	tx, err := GetDB().Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if err := recordResearchQueueSuppressionTx(ctx, tx, item, request); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ReclassifyResearchQueueMemberships updates a retained row's protection lane
// as individual source leases expire. Allocation remains unchanged: existing
// analyst work is never evicted or moved merely to rebalance reservations.
// The new bucket does, however, immediately free the former source's protected
// capacity for a newly qualified candidate.
func ReclassifyResearchQueueMemberships(ctx context.Context, db watchlistExecer) error {
	if tx, ok := db.(pgx.Tx); ok {
		// Bucket changes affect the same protected-lane counts used by admission.
		// Serialize both operations with the shared queue advisory lock.
		if err := lockResearchQueueTx(ctx, tx); err != nil {
			return err
		}
	}
	if err := releaseInactiveResearchQueueMemberships(ctx, db); err != nil {
		return err
	}
	_, err := db.Exec(ctx, `
		UPDATE research_queue_memberships q
		SET bucket = CASE
			WHEN EXISTS (SELECT 1 FROM watchlist_source_leases l WHERE l.watchlist_id=q.watchlist_id AND l.source='political_disclosure' AND l.expires_at > NOW()) THEN 'political_disclosure'
			WHEN EXISTS (SELECT 1 FROM watchlist_source_leases l WHERE l.watchlist_id=q.watchlist_id AND l.source='weekly_leader' AND l.expires_at > NOW()) THEN 'weekly_leader'
			WHEN EXISTS (SELECT 1 FROM watchlist_source_leases l WHERE l.watchlist_id=q.watchlist_id AND l.source='potent_industry' AND l.expires_at > NOW()) THEN 'potent_industry'
			WHEN EXISTS (SELECT 1 FROM watchlist_source_leases l WHERE l.watchlist_id=q.watchlist_id AND l.source='premarket_episodic' AND l.expires_at > NOW()) THEN 'premarket_episodic'
			WHEN EXISTS (SELECT 1 FROM watchlist_source_leases l WHERE l.watchlist_id=q.watchlist_id AND l.source='graph_neighbor' AND l.expires_at > NOW()) THEN 'graph_neighbor'
			WHEN EXISTS (SELECT 1 FROM watchlist_source_leases l WHERE l.watchlist_id=q.watchlist_id AND l.source='news_sympathy' AND l.expires_at > NOW()) THEN 'news_sympathy'
			WHEN EXISTS (SELECT 1 FROM watchlist_source_leases l WHERE l.watchlist_id=q.watchlist_id AND l.source IN ('mover','active') AND l.expires_at > NOW()) THEN 'market_activity'
			WHEN EXISTS (SELECT 1 FROM watchlist_discovery_contexts c WHERE c.watchlist_id=q.watchlist_id AND c.source='earnings_scheduled' AND c.active AND (c.expires_at IS NULL OR c.expires_at > NOW())) THEN 'earnings_scheduled'
			ELSE q.bucket
		END,
		updated_at=NOW()
		WHERE EXISTS (SELECT 1 FROM watchlist w WHERE w.id=q.watchlist_id AND w.active)`)
	return err
}
