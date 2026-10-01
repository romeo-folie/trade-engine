package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/romeo-folie/trade-engine/internal/tradeprofile"
)

var ErrEarningsWatchlistRunLeased = fmt.Errorf("earnings watchlist run is leased by another worker")

// EarningsWatchlistCandidate is one fully prepared member of a run cohort.
// Each candidate admission commits independently so capacity pressure cannot
// roll back successful peers.
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
// Callers publish this distinction to operators after that candidate commits.
type EarningsWatchlistPublication struct {
	Ticker      string
	Disposition string // admitted or rediscovered
}

type EarningsWatchlistDeferral struct {
	Candidate EarningsWatchlistCandidate
	Reason    error
}

type earningsWatchlistCohortCandidate struct {
	Ticker             string                        `json:"ticker"`
	ContextKey         string                        `json:"context_key"`
	Context            tradeprofile.DiscoveryContext `json:"context"`
	ExpiresAt          time.Time                     `json:"expires_at"`
	LiquidityRank      int                           `json:"liquidity_rank"`
	MedianDollarVolume float64                       `json:"median_dollar_volume"`
}

// LoadEarningsWatchlistCohort returns the immutable selection for a target
// session. A nil JSON value means selection has not yet been persisted; an
// empty array is a durable empty cohort.
func LoadEarningsWatchlistCohort(ctx context.Context, target time.Time) ([]EarningsWatchlistCandidate, bool, error) {
	var raw []byte
	if err := GetDB().QueryRow(ctx, `SELECT selected_candidates FROM earnings_watchlist_runs WHERE target_session_date=$1::date`, target).Scan(&raw); err != nil {
		return nil, false, err
	}
	if raw == nil {
		return nil, false, nil
	}
	var stored []earningsWatchlistCohortCandidate
	if err := json.Unmarshal(raw, &stored); err != nil {
		return nil, false, fmt.Errorf("decode earnings watchlist cohort: %w", err)
	}
	candidates := make([]EarningsWatchlistCandidate, 0, len(stored))
	for _, item := range stored {
		candidates = append(candidates, EarningsWatchlistCandidate{
			Ticker: item.Ticker, ContextKey: item.ContextKey, Context: item.Context,
			ExpiresAt: item.ExpiresAt, LiquidityRank: item.LiquidityRank,
			MedianDollarVolume: item.MedianDollarVolume,
		})
	}
	return candidates, true, nil
}

// EarningsWatchlistRunHasLivePersistedCandidates reports whether an incomplete
// run has at least one selected candidate whose stored lease has not elapsed.
// It lets a post-open retry distinguish durable deferred work from a startup
// catch-up that never claimed or selected the target session.
func EarningsWatchlistRunHasLivePersistedCandidates(ctx context.Context, target, now time.Time) (bool, error) {
	var live bool
	err := GetDB().QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM earnings_watchlist_runs r
			CROSS JOIN LATERAL jsonb_array_elements(
				CASE WHEN jsonb_typeof(r.selected_candidates)='array' THEN r.selected_candidates ELSE '[]'::jsonb END
			) candidate
			WHERE r.target_session_date=$1::date
			  AND r.status IN ('failed','running')
			  AND NULLIF(candidate->>'expires_at','')::timestamptz > $2
		)`, target, now).Scan(&live)
	return live, err
}

// OldestLiveEarningsWatchlistTarget returns the oldest incomplete target with
// at least one unexpired persisted candidate. Retrying it first prevents a
// capacity deferral from being stranded by a later market session or restart.
func OldestLiveEarningsWatchlistTarget(ctx context.Context, now time.Time) (time.Time, bool, error) {
	var target time.Time
	err := GetDB().QueryRow(ctx, `
		SELECT r.target_session_date
		FROM earnings_watchlist_runs r
		WHERE r.status IN ('failed','running')
		  AND EXISTS (
			SELECT 1
			FROM jsonb_array_elements(
				CASE WHEN jsonb_typeof(r.selected_candidates)='array' THEN r.selected_candidates ELSE '[]'::jsonb END
			) candidate
			WHERE NULLIF(candidate->>'expires_at','')::timestamptz > $1
		  )
		ORDER BY r.target_session_date
		LIMIT 1`, now).Scan(&target)
	if isNoRows(err) {
		return time.Time{}, false, nil
	}
	return target, err == nil, err
}

// PersistEarningsWatchlistCohort stores the first ranked cohort under the
// current run claim. A retry reuses that original selection even if provider
// data or liquidity ranking changes before the retry.
func PersistEarningsWatchlistCohort(ctx context.Context, target time.Time, attemptID string, candidates []EarningsWatchlistCandidate) ([]EarningsWatchlistCandidate, error) {
	stored := make([]earningsWatchlistCohortCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		stored = append(stored, earningsWatchlistCohortCandidate{
			Ticker: candidate.Ticker, ContextKey: candidate.ContextKey, Context: candidate.Context,
			ExpiresAt: candidate.ExpiresAt, LiquidityRank: candidate.LiquidityRank,
			MedianDollarVolume: candidate.MedianDollarVolume,
		})
	}
	encoded, err := json.Marshal(stored)
	if err != nil {
		return nil, fmt.Errorf("encode earnings watchlist cohort: %w", err)
	}
	tx, err := GetDB().Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var status, currentAttempt string
	var existing []byte
	err = tx.QueryRow(ctx, `SELECT status, attempt_id::text, selected_candidates FROM earnings_watchlist_runs WHERE target_session_date=$1::date FOR UPDATE`, target).Scan(&status, &currentAttempt, &existing)
	if err != nil {
		return nil, err
	}
	if status != "running" || currentAttempt != attemptID {
		return nil, fmt.Errorf("persist earnings watchlist cohort %s: claim is no longer running", target.Format("2006-01-02"))
	}
	if existing != nil {
		if err := tx.Commit(ctx); err != nil {
			return nil, err
		}
		var persisted []earningsWatchlistCohortCandidate
		if err := json.Unmarshal(existing, &persisted); err != nil {
			return nil, fmt.Errorf("decode persisted earnings watchlist cohort: %w", err)
		}
		return cohortCandidates(persisted), nil
	}
	if _, err := tx.Exec(ctx, `UPDATE earnings_watchlist_runs SET selected_candidates=$3::jsonb, updated_at=NOW() WHERE target_session_date=$1::date AND attempt_id=$2::uuid`, target, attemptID, encoded); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return candidates, nil
}

func cohortCandidates(stored []earningsWatchlistCohortCandidate) []EarningsWatchlistCandidate {
	candidates := make([]EarningsWatchlistCandidate, 0, len(stored))
	for _, item := range stored {
		candidates = append(candidates, EarningsWatchlistCandidate{
			Ticker: item.Ticker, ContextKey: item.ContextKey, Context: item.Context,
			ExpiresAt: item.ExpiresAt, LiquidityRank: item.LiquidityRank,
			MedianDollarVolume: item.MedianDollarVolume,
		})
	}
	return candidates
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

// PublishedEarningsWatchlistCandidates returns names already committed from a
// partially admitted run. Failed peers remain eligible for the next retry.
func PublishedEarningsWatchlistCandidates(ctx context.Context, target time.Time) (map[string]struct{}, error) {
	rows, err := GetDB().Query(ctx, `SELECT ticker FROM earnings_watchlist_candidates WHERE target_session_date=$1::date`, target)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	published := make(map[string]struct{})
	for rows.Next() {
		var ticker string
		if err := rows.Scan(&ticker); err != nil {
			return nil, err
		}
		published[strings.ToUpper(strings.TrimSpace(ticker))] = struct{}{}
	}
	return published, rows.Err()
}

// PublishEarningsWatchlistRun commits candidates independently in caller rank
// order. A capacity-limited candidate is deferred while later candidates are
// still attempted; previously admitted peers remain durable for retry.
func PublishEarningsWatchlistRun(ctx context.Context, target time.Time, attemptID string, candidates []EarningsWatchlistCandidate) ([]EarningsWatchlistPublication, []EarningsWatchlistDeferral, error) {
	publications := make([]EarningsWatchlistPublication, 0, len(candidates))
	deferrals := make([]EarningsWatchlistDeferral, 0)
	for _, candidate := range candidates {
		tx, err := GetDB().Begin(ctx)
		if err != nil {
			return publications, deferrals, err
		}
		var status, currentAttempt string
		err = tx.QueryRow(ctx, `SELECT status, attempt_id::text FROM earnings_watchlist_runs WHERE target_session_date=$1::date FOR UPDATE`, target).Scan(&status, &currentAttempt)
		if err != nil || status != "running" || currentAttempt != attemptID {
			_ = tx.Rollback(ctx)
			if err != nil {
				return publications, deferrals, err
			}
			return publications, deferrals, fmt.Errorf("publish earnings watchlist run %s: claim is no longer running", target.Format("2006-01-02"))
		}
		if err := lockScannerWatchlistItem(ctx, tx, WatchlistItem{Ticker: candidate.Ticker, Platform: "stock"}); err != nil {
			_ = tx.Rollback(ctx)
			return publications, deferrals, err
		}
		disposition, publishErr := publishEarningsCandidate(ctx, tx, candidate)
		if publishErr != nil {
			_ = tx.Rollback(ctx)
			if errors.Is(publishErr, ErrResearchQueueCapacity) || errors.Is(publishErr, ErrWatchlistCapacity) || errors.Is(publishErr, ErrResearchStagingReservationLost) {
				if errors.Is(publishErr, ErrResearchQueueCapacity) && candidate.ResearchQueue != nil {
					if err := RecordResearchQueueSuppression(ctx, WatchlistItem{Ticker: candidate.Ticker, Platform: "stock"}, *candidate.ResearchQueue); err != nil {
						return publications, deferrals, err
					}
				}
				deferrals = append(deferrals, EarningsWatchlistDeferral{Candidate: candidate, Reason: publishErr})
				continue
			}
			return publications, deferrals, publishErr
		}
		if _, err := tx.Exec(ctx, `INSERT INTO earnings_watchlist_candidates (target_session_date,ticker,liquidity_rank,median_dollar_volume) VALUES ($1::date,$2,$3,$4) ON CONFLICT (target_session_date,ticker) DO NOTHING`, target, candidate.Ticker, candidate.LiquidityRank, candidate.MedianDollarVolume); err != nil {
			_ = tx.Rollback(ctx)
			return publications, deferrals, err
		}
		if err := tx.Commit(ctx); err != nil {
			return publications, deferrals, err
		}
		publications = append(publications, EarningsWatchlistPublication{Ticker: candidate.Ticker, Disposition: disposition})
	}
	return publications, deferrals, nil
}

// recordEarningsCapacityFailure preserves a legacy suppression decision and
// releases the run for a later retry.
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
