package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/romeo-folie/trade-engine/internal/tradeprofile"
)

var ErrEarningsWatchlistRunLeased = fmt.Errorf("earnings watchlist run is leased by another worker")

// EarningsWatchlistCandidate is one fully prepared member of a run cohort.
// PublishEarningsWatchlistRun writes the complete cohort atomically.
type EarningsWatchlistCandidate struct {
	Ticker             string
	ContextKey         string
	Context            tradeprofile.DiscoveryContext
	ExpiresAt          time.Time
	LiquidityRank      int
	MedianDollarVolume float64
}

// ClaimEarningsWatchlistRun serializes one target-session selection. Completed
// cohorts are immutable; failed/expired work can be safely retried.
func ClaimEarningsWatchlistRun(ctx context.Context, target time.Time, now time.Time) (string, bool, error) {
	var attemptID string
	err := GetDB().QueryRow(ctx, `
		INSERT INTO earnings_watchlist_runs (target_session_date,status,lease_until)
		VALUES ($1::date,'running',$2)
		ON CONFLICT (target_session_date) DO UPDATE SET
			status='running', attempt_id=uuid_generate_v4(), lease_until=EXCLUDED.lease_until, last_error=NULL, updated_at=NOW()
		WHERE earnings_watchlist_runs.status='failed'
		   OR (earnings_watchlist_runs.status='running' AND earnings_watchlist_runs.lease_until <= $3)
		RETURNING attempt_id::text`, target, now.Add(30*time.Minute), now).Scan(&attemptID)
	if isNoRows(err) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("claim earnings watchlist run: %w", err)
	}
	return attemptID, true, nil
}

// EarningsWatchlistRunLeaseHeld distinguishes an already completed cohort from
// a running cohort owned by another process. The latter must be retried once
// its lease expires rather than being mistaken for successful completion.
func EarningsWatchlistRunLeaseHeld(ctx context.Context, target, now time.Time) (bool, error) {
	var status string
	var leaseUntil time.Time
	err := GetDB().QueryRow(ctx, `SELECT status, lease_until FROM earnings_watchlist_runs WHERE target_session_date=$1::date`, target).Scan(&status, &leaseUntil)
	if isNoRows(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return status == "running" && leaseUntil.After(now), nil
}

func CompleteEarningsWatchlistRun(ctx context.Context, target time.Time, attemptID string) error {
	tag, err := GetDB().Exec(ctx, `UPDATE earnings_watchlist_runs SET status='completed', completed_at=NOW(), updated_at=NOW() WHERE target_session_date=$1::date AND status='running' AND attempt_id=$2::uuid`, target, attemptID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("complete earnings watchlist run %s: claim is no longer running", target.Format("2006-01-02"))
	}
	return nil
}

func FailEarningsWatchlistRun(ctx context.Context, target time.Time, attemptID string, cause error) error {
	_, err := GetDB().Exec(ctx, `UPDATE earnings_watchlist_runs SET status='failed', last_error=$3, updated_at=NOW() WHERE target_session_date=$1::date AND status='running' AND attempt_id=$2::uuid`, target, attemptID, cause.Error())
	return err
}

func RecordEarningsWatchlistCandidate(ctx context.Context, target time.Time, ticker string, rank int, medianDollarVolume float64) error {
	_, err := GetDB().Exec(ctx, `INSERT INTO earnings_watchlist_candidates (target_session_date,ticker,liquidity_rank,median_dollar_volume) VALUES ($1::date,$2,$3,$4) ON CONFLICT (target_session_date,ticker) DO NOTHING`, target, ticker, rank, medianDollarVolume)
	return err
}

// PublishEarningsWatchlistRun attaches every selected context, records its
// audit row, and completes the claim in one transaction. A failed publish
// leaves no partial cohort visible to analysts.
func PublishEarningsWatchlistRun(ctx context.Context, target time.Time, attemptID string, candidates []EarningsWatchlistCandidate) error {
	tx, err := GetDB().Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	for _, candidate := range candidates {
		if err := publishEarningsCandidate(ctx, tx, candidate); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO earnings_watchlist_candidates (target_session_date,ticker,liquidity_rank,median_dollar_volume) VALUES ($1::date,$2,$3,$4) ON CONFLICT (target_session_date,ticker) DO NOTHING`, target, candidate.Ticker, candidate.LiquidityRank, candidate.MedianDollarVolume); err != nil {
			return err
		}
	}
	tag, err := tx.Exec(ctx, `UPDATE earnings_watchlist_runs SET status='completed', completed_at=NOW(), updated_at=NOW() WHERE target_session_date=$1::date AND status='running' AND attempt_id=$2::uuid`, target, attemptID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("complete earnings watchlist run %s: claim is no longer running", target.Format("2006-01-02"))
	}
	return tx.Commit(ctx)
}

func publishEarningsCandidate(ctx context.Context, tx pgx.Tx, candidate EarningsWatchlistCandidate) error {
	ticker := strings.ToUpper(strings.TrimSpace(candidate.Ticker))
	if ticker == "" {
		return fmt.Errorf("earnings watchlist candidate ticker is required")
	}
	var active bool
	var expiresAt *time.Time
	err := tx.QueryRow(ctx, `SELECT active, expires_at FROM watchlist WHERE ticker=$1 AND platform='stock' FOR UPDATE`, ticker).Scan(&active, &expiresAt)
	if err != nil && !isNoRows(err) {
		return err
	}
	if isNoRows(err) || !active || (expiresAt != nil && !expiresAt.After(time.Now())) {
		if err := addWatchlistItem(ctx, tx, WatchlistItem{
			Ticker: ticker, Platform: "stock", Source: "earnings_watchlist",
			Strategies: []string{"trend_following", "momentum", "news_sentiment"},
			Timeframes: []string{"1D", "4H", "1H"}, GraphContext: "Scheduled earnings watchlist candidate.",
		}, true); err != nil && !errors.Is(err, ErrWatchlistActiveItemExists) {
			return err
		}
	}
	key := strings.TrimSpace(candidate.ContextKey)
	if key == "" {
		return fmt.Errorf("earnings watchlist context key is required")
	}
	canonical, err := candidate.Context.Canonicalize()
	if err != nil {
		return fmt.Errorf("canonicalize earnings discovery context: %w", err)
	}
	raw, err := tradeprofile.MarshalDiscoveryContext(&canonical)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO watchlist_discovery_contexts (watchlist_id, context_key, source, context, expires_at, active)
		SELECT id, $3, $4, $5::jsonb, $6, true FROM watchlist WHERE ticker=$1 AND platform=$2
		ON CONFLICT (watchlist_id, context_key) DO UPDATE SET
			source=EXCLUDED.source, context=EXCLUDED.context, expires_at=EXCLUDED.expires_at,
			active=true, updated_at=NOW()`, ticker, "stock", key, canonical.Sources[0], raw, candidate.ExpiresAt)
	return err
}
