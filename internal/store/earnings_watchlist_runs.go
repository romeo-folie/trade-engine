package store

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/romeo-folie/trade-engine/internal/tradeprofile"
)

var ErrEarningsWatchlistRunLeased = fmt.Errorf("earnings watchlist run is leased by another worker")

// EarningsWatchlistCandidate is one fully prepared member of a run cohort.
// PublishEarningsWatchlistRun writes the complete cohort atomically.
type EarningsWatchlistCandidate struct {
	Ticker               string
	ContextKey           string
	Context              tradeprofile.DiscoveryContext
	ExpiresAt            time.Time
	LiquidityRank        int
	MedianDollarVolume   float64
	ResearchQueue        *ResearchQueueAdmission
	MaxActiveWatchlist   int
	ResearchStagingID    string
	ResearchStagingLease time.Duration
}

// EarningsWatchlistPublication records whether a committed candidate created a
// new research membership or attached earnings context to one already active.
// Callers publish this distinction to operators after the enclosing cohort
// transaction commits.
type EarningsWatchlistPublication struct {
	Ticker      string
	Disposition string // admitted or rediscovered
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
// audit row, and completes the claim in one transaction. A full research queue
// records a durable suppression decision but rolls back the entire cohort, so
// a retry cannot mix a changed ranking with partially admitted earlier peers.
func PublishEarningsWatchlistRun(ctx context.Context, target time.Time, attemptID string, candidates []EarningsWatchlistCandidate) ([]EarningsWatchlistPublication, error) {
	tx, err := GetDB().Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	orderedCandidates := orderedEarningsWatchlistCandidates(candidates)
	// Scanner admission locks a ticker before reserving a global research-queue
	// slot. Lock the complete earnings cohort in ticker order before any member
	// can acquire that queue lock, so a multi-candidate cohort cannot hold the
	// queue while waiting for a scanner-held ticker lock.
	for _, candidate := range orderedCandidates {
		if err := lockScannerWatchlistItem(ctx, tx, WatchlistItem{
			Ticker: candidate.Ticker, Platform: "stock",
		}); err != nil {
			return nil, err
		}
	}
	publications := make([]EarningsWatchlistPublication, 0, len(candidates))
	for _, candidate := range candidates {
		disposition, publishErr := publishEarningsCandidate(ctx, tx, candidate)
		if publishErr != nil && !errors.Is(publishErr, ErrResearchQueueCapacity) && !errors.Is(publishErr, ErrWatchlistCapacity) && !errors.Is(publishErr, ErrResearchStagingReservationLost) {
			return nil, publishErr
		}
		if errors.Is(publishErr, ErrResearchQueueCapacity) || errors.Is(publishErr, ErrWatchlistCapacity) || errors.Is(publishErr, ErrResearchStagingReservationLost) {
			if candidate.ResearchQueue == nil {
				return nil, publishErr
			}
			// Do not expose a partial liquidity-ranked cohort. Rolling back keeps
			// retries from mixing a newly fetched ranking with already-admitted
			// peers from an earlier capacity-limited attempt.
			if err := tx.Rollback(ctx); err != nil {
				return nil, err
			}
			return nil, recordEarningsCapacityFailure(ctx, target, attemptID, candidate, publishErr)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO earnings_watchlist_candidates (target_session_date,ticker,liquidity_rank,median_dollar_volume) VALUES ($1::date,$2,$3,$4) ON CONFLICT (target_session_date,ticker) DO NOTHING`, target, candidate.Ticker, candidate.LiquidityRank, candidate.MedianDollarVolume); err != nil {
			return nil, err
		}
		publications = append(publications, EarningsWatchlistPublication{Ticker: candidate.Ticker, Disposition: disposition})
	}
	tag, err := tx.Exec(ctx, `UPDATE earnings_watchlist_runs SET status='completed', completed_at=NOW(), updated_at=NOW() WHERE target_session_date=$1::date AND status='running' AND attempt_id=$2::uuid`, target, attemptID)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() != 1 {
		return nil, fmt.Errorf("complete earnings watchlist run %s: claim is no longer running", target.Format("2006-01-02"))
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return publications, nil
}

// recordEarningsCapacityFailure preserves the suppression decision without
// committing a partial cohort, then releases the run for a later full retry.
func recordEarningsCapacityFailure(
	ctx context.Context, target time.Time, attemptID string, candidate EarningsWatchlistCandidate, cause error,
) error {
	tx, err := GetDB().Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if errors.Is(cause, ErrResearchQueueCapacity) {
		if candidate.ResearchQueue == nil {
			return cause
		}
		if err := recordResearchQueueSuppressionTx(ctx, tx, WatchlistItem{Ticker: candidate.Ticker, Platform: "stock"}, *candidate.ResearchQueue); err != nil {
			return err
		}
	}
	tag, err := tx.Exec(ctx, `UPDATE earnings_watchlist_runs SET status='failed', last_error=$3, updated_at=NOW() WHERE target_session_date=$1::date AND status='running' AND attempt_id=$2::uuid`, target, attemptID, cause.Error())
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("fail earnings watchlist run %s: claim is no longer running", target.Format("2006-01-02"))
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	return cause
}

func orderedEarningsWatchlistCandidates(candidates []EarningsWatchlistCandidate) []EarningsWatchlistCandidate {
	ordered := append([]EarningsWatchlistCandidate(nil), candidates...)
	sort.SliceStable(ordered, func(i, j int) bool {
		return strings.ToUpper(strings.TrimSpace(ordered[i].Ticker)) <
			strings.ToUpper(strings.TrimSpace(ordered[j].Ticker))
	})
	return ordered
}

func publishEarningsCandidate(ctx context.Context, tx pgx.Tx, candidate EarningsWatchlistCandidate) (string, error) {
	ticker := strings.ToUpper(strings.TrimSpace(candidate.Ticker))
	if ticker == "" {
		return "", fmt.Errorf("earnings watchlist candidate ticker is required")
	}
	var active bool
	var expiresAt *time.Time
	err := tx.QueryRow(ctx, `SELECT active, expires_at FROM watchlist WHERE ticker=$1 AND platform='stock' FOR UPDATE`, ticker).Scan(&active, &expiresAt)
	if err != nil && !isNoRows(err) {
		return "", err
	}
	disposition := "admitted"
	if candidate.ResearchQueue == nil && err == nil && active && (expiresAt == nil || expiresAt.After(time.Now())) {
		disposition = "rediscovered"
	}
	// An earnings row can remain physically active after its last context has
	// expired. Reserving here, before either a row write or a context revival,
	// applies the global main-watchlist limit to that otherwise invisible path.
	if err := reserveMainWatchlistSlotTx(ctx, tx, WatchlistItem{
		Ticker: ticker, Platform: "stock",
	}, candidate.MaxActiveWatchlist); err != nil {
		return "", err
	}
	if candidate.ResearchStagingID != "" || candidate.ResearchStagingLease != 0 {
		if err := renewResearchCandidateTx(ctx, tx, candidate.ResearchStagingID, candidate.ResearchStagingLease); err != nil {
			return "", err
		}
	}
	queueRecorded := false
	if isNoRows(err) || !active || (expiresAt != nil && !expiresAt.After(time.Now())) {
		if err := addWatchlistItem(ctx, tx, WatchlistItem{
			Ticker: ticker, Platform: "stock", Source: "earnings_watchlist",
			Strategies: []string{"trend_following", "momentum", "news_sentiment"},
			Timeframes: []string{"1D", "4H", "1H"}, GraphContext: "Scheduled earnings watchlist candidate.",
			ResearchQueue:        candidate.ResearchQueue,
			MaxActiveWatchlist:   candidate.MaxActiveWatchlist,
			ResearchStagingID:    candidate.ResearchStagingID,
			ResearchStagingLease: candidate.ResearchStagingLease,
		}, true, nil); err != nil && !errors.Is(err, ErrWatchlistActiveItemExists) {
			return "", err
		} else if err == nil && candidate.ResearchQueue != nil {
			queueRecorded = true
		}
	}
	var queueReservation researchQueueReservation
	if candidate.ResearchQueue != nil && !queueRecorded {
		// Reserve before writing the context. A capacity-suppressed candidate
		// must not become analyst-visible without a queue membership.
		queueReservation, err = reserveResearchQueueSlotTx(ctx, tx, WatchlistItem{Ticker: ticker, Platform: "stock"}, *candidate.ResearchQueue)
		if err != nil {
			return "", err
		}
		if queueReservation.existing {
			disposition = "rediscovered"
		}
	}
	key := strings.TrimSpace(candidate.ContextKey)
	if key == "" {
		return "", fmt.Errorf("earnings watchlist context key is required")
	}
	canonical, err := candidate.Context.Canonicalize()
	if err != nil {
		return "", fmt.Errorf("canonicalize earnings discovery context: %w", err)
	}
	raw, err := tradeprofile.MarshalDiscoveryContext(&canonical)
	if err != nil {
		return "", err
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO watchlist_discovery_contexts (watchlist_id, context_key, source, context, expires_at, active)
		SELECT id, $3, $4, $5::jsonb, $6, true FROM watchlist WHERE ticker=$1 AND platform=$2
		ON CONFLICT (watchlist_id, context_key) DO UPDATE SET
			source=EXCLUDED.source, context=EXCLUDED.context, expires_at=EXCLUDED.expires_at,
			active=true, updated_at=NOW()`, ticker, "stock", key, canonical.Sources[0], raw, candidate.ExpiresAt)
	if err != nil {
		return "", err
	}
	if candidate.ResearchQueue != nil && !queueRecorded {
		if err := recordResearchQueueAdmissionTx(ctx, tx, WatchlistItem{Ticker: ticker, Platform: "stock"}, *candidate.ResearchQueue, queueReservation); err != nil {
			return "", err
		}
	}
	if candidate.ResearchQueue != nil {
		// A newly created earnings row records its initial membership before the
		// context is upserted. Recompute once the context is live as well, so an
		// expired scanner handoff cannot retain a stale scanner bucket.
		if err := ReclassifyResearchQueueMemberships(ctx, tx); err != nil {
			return "", err
		}
	}
	return disposition, nil
}
