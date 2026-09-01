package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

var ErrResearchStagingCapacity = errors.New("research staging capacity is unavailable")
var ErrResearchStagingReservationLost = errors.New("research staging reservation is no longer live")

// ResearchStagingRequest describes a short-lived scanner candidate awaiting
// main-watchlist admission. The durable scanner allocation is intentionally
// not created until that admission commits.
type ResearchStagingRequest struct {
	RunID    string
	Ticker   string
	Platform string
	Bucket   string
	Priority json.RawMessage
	Capacity int
	Lease    time.Duration
}

// QueueResearchCandidate creates one pending candidate row under a shared
// admission lock. Exact retries from the same run return the existing row and
// do not consume a second staging slot. Independent runs receive independent
// reservations, so no concurrent promotion can rely on another run's cleanup.
func QueueResearchCandidate(ctx context.Context, request ResearchStagingRequest) (string, bool, error) {
	if strings.TrimSpace(request.RunID) == "" || strings.TrimSpace(request.Ticker) == "" ||
		strings.TrimSpace(request.Bucket) == "" || request.Capacity <= 0 || request.Lease <= 0 {
		return "", false, fmt.Errorf("invalid research staging request")
	}
	priority := request.Priority
	if len(priority) == 0 {
		priority = json.RawMessage(`{}`)
	}
	if !json.Valid(priority) {
		return "", false, fmt.Errorf("research staging priority must be JSON")
	}
	platform := strings.TrimSpace(request.Platform)
	if platform == "" {
		platform = "stock"
	}
	ticker := strings.ToUpper(strings.TrimSpace(request.Ticker))
	tx, err := GetDB().Begin(ctx)
	if err != nil {
		return "", false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('research-staging:v1', 0))`); err != nil {
		return "", false, fmt.Errorf("lock research staging: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM research_queue_items WHERE expires_at <= clock_timestamp()`); err != nil {
		return "", false, err
	}
	var existingID string
	err = tx.QueryRow(ctx, `
		SELECT id::text FROM research_queue_items
		WHERE run_id=$1 AND ticker=$2 AND platform=$3`, request.RunID, ticker, platform,
	).Scan(&existingID)
	if err == nil {
		if err := tx.Commit(ctx); err != nil {
			return "", false, err
		}
		return existingID, false, nil
	}
	if !isNoRows(err) {
		return "", false, err
	}
	var used int
	if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM research_queue_items`).Scan(&used); err != nil {
		return "", false, err
	}
	if used >= request.Capacity {
		return "", false, ErrResearchStagingCapacity
	}
	var id string
	if err := tx.QueryRow(ctx, `
		INSERT INTO research_queue_items (run_id,ticker,platform,bucket,priority,expires_at)
		VALUES ($1,$2,$3,$4,$5,clock_timestamp()+$6::interval)
		RETURNING id::text`,
		request.RunID, ticker, platform, request.Bucket, priority,
		fmt.Sprintf("%f seconds", request.Lease.Seconds()),
	).Scan(&id); err != nil {
		return "", false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", false, err
	}
	return id, true, nil
}

// ReleaseResearchCandidate removes a staging row after an admission has
// committed or has been deferred for a later scanner run. The durable scanner
// allocation and decision history are stored elsewhere.
func ReleaseResearchCandidate(ctx context.Context, id string) error {
	if strings.TrimSpace(id) == "" {
		return nil
	}
	_, err := GetDB().Exec(ctx, `DELETE FROM research_queue_items WHERE id=$1::uuid`, id)
	return err
}

// RenewResearchCandidate fences a pending admission against its short lease.
// Once a lease has expired, it cannot be revived: a later producer may already
// have reclaimed that capacity, so the caller must defer the candidate instead.
func RenewResearchCandidate(ctx context.Context, id string, lease time.Duration) error {
	if strings.TrimSpace(id) == "" || lease <= 0 {
		return fmt.Errorf("invalid research staging renewal")
	}
	tag, err := GetDB().Exec(ctx, `
		UPDATE research_queue_items
		SET expires_at=clock_timestamp()+$2::interval, updated_at=clock_timestamp()
		WHERE id=$1::uuid AND expires_at > clock_timestamp()`,
		id, fmt.Sprintf("%f seconds", lease.Seconds()))
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrResearchStagingReservationLost
	}
	return nil
}

func renewResearchCandidateTx(ctx context.Context, tx pgx.Tx, id string, lease time.Duration) error {
	if strings.TrimSpace(id) == "" || lease <= 0 {
		return fmt.Errorf("invalid research staging renewal")
	}
	tag, err := tx.Exec(ctx, `
		UPDATE research_queue_items
		SET expires_at=clock_timestamp()+$2::interval, updated_at=clock_timestamp()
		WHERE id=$1::uuid AND expires_at > clock_timestamp()`,
		id, fmt.Sprintf("%f seconds", lease.Seconds()))
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrResearchStagingReservationLost
	}
	return nil
}
