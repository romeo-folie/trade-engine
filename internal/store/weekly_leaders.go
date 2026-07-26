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

var ErrWeeklyLeaderPromotionClaimLost = errors.New("weekly leader promotion claim is no longer current")

const weeklyLeaderCompletionLock = "weekly-leader-completion"

type WeeklyLeaderRun struct {
	ID                     string
	SessionDate            time.Time
	AsOf                   time.Time
	DecisionAt             time.Time
	UniverseVersionID      string
	Status                 string
	StartedAt              time.Time
	CompletedAt            *time.Time
	LeaseUntil             time.Time
	AttemptCount           int
	ConfigHash             string
	ConfigSnapshot         json.RawMessage
	FeatureVersion         string
	Benchmark              string
	HistoryProvider        string
	Error                  string
	EligibleUniverseCount  int
	EvaluatedCount         int
	QualifiedCount         int
	FailureCount           int
	AddedCount             int
	RemovedCount           int
	PersistentCount        int
	LeaderBreadth          float64
	EvaluationCoverage     float64
	FailureRate            float64
	PreviousCompletedRunID string
}

type WeeklyLeaderQualification struct {
	RunID                     string
	UniverseVersionID         string
	Ticker                    string
	Outcome                   string
	FailureStage              string
	FailureReason             string
	HistoryFirstAt            *time.Time
	HistoryLastAt             *time.Time
	HistorySessions           int
	BarSetHash                string
	SourceEvidence            json.RawMessage
	Sector                    string
	Industry                  string
	MarketCapBand             string
	VolatilityBand            string
	Return5                   *float64
	Return20                  *float64
	Return60                  *float64
	Return126                 *float64
	High52WeekProximity       *float64
	MarketRelativeStrength    *float64
	IndustryRelativeStrength  *float64
	RSAcceleration            *float64
	RegressionAnnualizedSlope *float64
	RegressionR2              *float64
	Efficiency60              *float64
	MaxDrawdown126            *float64
	NATR14                    *float64
	VolatilityAdjustedReturn  *float64
	MedianDollarVolume20      *float64
	DollarVolumeStability20   *float64
	NormalizedOBV20           *float64
	CMF20                     *float64
	GapDomination60           *float64
	CompositeScore            *float64
	ScorePercentile           *float64
	Rank                      *int
	FeatureCoverage           *float64
	Features                  json.RawMessage
	Components                json.RawMessage
	WasQualifiedPrevious      bool
	QualificationStreak       int
	QualifiedSince            *time.Time
	PromotionStatus           string
	ScannerRunID              string
	PromotedAt                *time.Time
	PromotionReason           string
}

type WeeklyLeaderTransition struct {
	RunID         string
	Ticker        string
	Event         string
	PreviousRunID string
	PreviousScore *float64
	CurrentScore  *float64
	PreviousRank  *int
	CurrentRank   *int
	Reason        string
}

type WeeklyLeaderBreadth struct {
	RunID           string
	DimensionType   string
	DimensionValue  string
	EligibleCount   int
	EvaluatedCount  int
	QualifiedCount  int
	FailedCount     int
	AddedCount      int
	RemovedCount    int
	PersistentCount int
	Breadth         float64
	Coverage        float64
}

type WeeklyLeaderCandidate struct {
	RunID                string
	UniverseVersionID    string
	UniverseAsOf         time.Time
	Ticker               string
	Score                float64
	Rank                 int
	Sector               string
	Industry             string
	Price                float64
	MarketCapMM          float64
	Return20             float64
	Return60             float64
	Return126            float64
	MedianDollarVolume20 float64
	PromotionClaimToken  string
}

// ClaimWeeklyLeaderRun atomically creates or reclaims a failed/expired run.
// Completed and actively leased runs return claimed=false, making restarts and
// multiple engine processes safe.
func ClaimWeeklyLeaderRun(
	ctx context.Context, run WeeklyLeaderRun, lease time.Duration,
) (*WeeklyLeaderRun, bool, error) {
	if run.SessionDate.IsZero() || run.AsOf.IsZero() || run.DecisionAt.IsZero() ||
		run.UniverseVersionID == "" ||
		run.ConfigHash == "" || run.FeatureVersion == "" || run.Benchmark == "" {
		return nil, false, fmt.Errorf("weekly leader run identity is incomplete")
	}
	if lease <= 0 {
		lease = time.Hour
	}
	row := GetDB().QueryRow(ctx, `
		INSERT INTO weekly_leader_runs (
			session_date, as_of, decision_at, universe_version_id, started_at, lease_until,
			config_hash, config_snapshot, feature_version, benchmark, history_provider
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
		ON CONFLICT (session_date, universe_version_id, config_hash) DO UPDATE SET
			status='running',
			as_of=EXCLUDED.as_of,
			decision_at=EXCLUDED.decision_at,
			started_at=EXCLUDED.started_at,
			lease_until=EXCLUDED.lease_until,
			attempt_count=weekly_leader_runs.attempt_count+1,
			config_snapshot=EXCLUDED.config_snapshot,
			feature_version=EXCLUDED.feature_version,
			benchmark=EXCLUDED.benchmark,
			benchmark_bar_set_hash=NULL,
			history_provider=EXCLUDED.history_provider,
			completed_at=NULL,
			error=NULL
		WHERE weekly_leader_runs.status='failed'
		   OR (weekly_leader_runs.status='running'
		       AND weekly_leader_runs.lease_until <= EXCLUDED.started_at)
		RETURNING id, session_date, as_of, decision_at, universe_version_id, status, started_at,
		          completed_at, lease_until, attempt_count, config_hash,
		          config_snapshot, feature_version, benchmark, history_provider,
		          COALESCE(error,''), eligible_universe_count, evaluated_count,
		          qualified_count, failure_count, added_count, removed_count,
		          persistent_count, leader_breadth, evaluation_coverage, failure_rate,
		          COALESCE(previous_completed_run_id::text,'')`,
		run.SessionDate, run.AsOf, run.DecisionAt, run.UniverseVersionID, run.StartedAt,
		run.StartedAt.Add(lease), run.ConfigHash, nonNilJSON(run.ConfigSnapshot),
		run.FeatureVersion, run.Benchmark, run.HistoryProvider,
	)
	claimed, err := scanWeeklyLeaderRun(row)
	if err != nil {
		if IsNoRows(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("claim weekly leader run: %w", err)
	}
	return claimed, true, nil
}

func FailWeeklyLeaderRun(
	ctx context.Context, id string, attempt int, failedAt time.Time, cause error,
) error {
	message := ""
	if cause != nil {
		message = cause.Error()
	}
	tag, err := GetDB().Exec(ctx, `
		UPDATE weekly_leader_runs
		SET status='failed', completed_at=$2, error=$3
		WHERE id=$1 AND status='running' AND attempt_count=$4`,
		id, failedAt, nullableString(message), attempt)
	if err != nil {
		return fmt.Errorf("fail weekly leader run: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("fail weekly leader run %q: claim is no longer current", id)
	}
	return nil
}

// CompleteWeeklyLeaderRun replaces all child evidence and publishes it in the
// same transaction as the completed status. Readers can never observe partial
// qualifications, transitions, or breadth.
func CompleteWeeklyLeaderRun(
	ctx context.Context,
	id string,
	attempt int,
	completedAt time.Time,
	qualifications []WeeklyLeaderQualification,
	benchmarkBarSetHash string,
) error {
	tx, err := GetDB().Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	var run WeeklyLeaderRun
	err = tx.QueryRow(ctx, `
		SELECT id, session_date, as_of, decision_at, universe_version_id, status, started_at,
		       completed_at, lease_until, attempt_count, config_hash, config_snapshot,
		       feature_version, benchmark, history_provider, COALESCE(error,''),
		       eligible_universe_count, evaluated_count, qualified_count, failure_count,
		       added_count, removed_count, persistent_count, leader_breadth,
		       evaluation_coverage, failure_rate,
		       COALESCE(previous_completed_run_id::text,'')
		FROM weekly_leader_runs WHERE id=$1 FOR UPDATE`, id,
	).Scan(
		&run.ID, &run.SessionDate, &run.AsOf, &run.DecisionAt, &run.UniverseVersionID, &run.Status,
		&run.StartedAt, &run.CompletedAt, &run.LeaseUntil, &run.AttemptCount,
		&run.ConfigHash, &run.ConfigSnapshot, &run.FeatureVersion, &run.Benchmark,
		&run.HistoryProvider, &run.Error, &run.EligibleUniverseCount,
		&run.EvaluatedCount, &run.QualifiedCount, &run.FailureCount,
		&run.AddedCount, &run.RemovedCount, &run.PersistentCount,
		&run.LeaderBreadth, &run.EvaluationCoverage, &run.FailureRate,
		&run.PreviousCompletedRunID,
	)
	if err != nil {
		return fmt.Errorf("lock weekly leader run: %w", err)
	}
	if run.Status != "running" {
		return fmt.Errorf("complete weekly leader run %q: status is %s", id, run.Status)
	}
	if run.AttemptCount != attempt {
		return fmt.Errorf("complete weekly leader run %q: claim attempt %d was superseded by %d",
			id, attempt, run.AttemptCount)
	}
	if _, err := tx.Exec(ctx, `
		SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`,
		weeklyLeaderCompletionLock,
	); err != nil {
		return fmt.Errorf("serialize weekly leader completion: %w", err)
	}

	previousID, previous, err := previousWeeklyLeaders(
		ctx, tx, run.SessionDate, run.ConfigHash,
	)
	if err != nil {
		return err
	}
	for _, table := range []string{
		"weekly_leader_breadth", "weekly_leader_transitions", "weekly_leader_qualifications",
	} {
		if _, err := tx.Exec(ctx, "DELETE FROM "+table+" WHERE run_id=$1", id); err != nil {
			return fmt.Errorf("clear %s: %w", table, err)
		}
	}
	current := make(map[string]WeeklyLeaderQualification, len(qualifications))
	for i := range qualifications {
		q := &qualifications[i]
		if q.RunID == "" {
			q.RunID = id
		}
		if q.RunID != id || q.UniverseVersionID != run.UniverseVersionID ||
			strings.TrimSpace(q.Ticker) == "" {
			return fmt.Errorf("weekly leader qualification lineage mismatch for %q", q.Ticker)
		}
		prior, wasLeader := previous[q.Ticker]
		q.WasQualifiedPrevious = wasLeader
		if q.Outcome == "qualified" {
			q.PromotionStatus = normalizePromotionStatus(q.PromotionStatus)
			if wasLeader {
				q.QualificationStreak = prior.QualificationStreak + 1
				q.QualifiedSince = prior.QualifiedSince
			} else {
				q.QualificationStreak = 1
				date := run.SessionDate
				q.QualifiedSince = &date
			}
		} else {
			q.PromotionStatus = "not_requested"
		}
		if err := insertWeeklyLeaderQualification(ctx, tx, *q); err != nil {
			return err
		}
		current[q.Ticker] = *q
	}

	transitions := buildWeeklyLeaderTransitions(id, previousID, previous, current)
	for _, transition := range transitions {
		if _, err := tx.Exec(ctx, `
			INSERT INTO weekly_leader_transitions (
				run_id, ticker, event, previous_run_id, previous_score, current_score,
				previous_rank, current_rank, reason
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
			transition.RunID, transition.Ticker, transition.Event,
			nullableUUID(transition.PreviousRunID), transition.PreviousScore,
			transition.CurrentScore, transition.PreviousRank, transition.CurrentRank,
			transition.Reason,
		); err != nil {
			return fmt.Errorf("insert weekly leader transition %s: %w", transition.Ticker, err)
		}
	}
	breadth := buildWeeklyLeaderBreadth(id, qualifications, transitions)
	for _, row := range breadth {
		if _, err := tx.Exec(ctx, `
			INSERT INTO weekly_leader_breadth (
				run_id, dimension_type, dimension_value, eligible_count,
				evaluated_count, qualified_count, failed_count, added_count,
				removed_count, persistent_count, breadth, coverage
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`,
			row.RunID, row.DimensionType, row.DimensionValue, row.EligibleCount,
			row.EvaluatedCount, row.QualifiedCount, row.FailedCount, row.AddedCount,
			row.RemovedCount, row.PersistentCount, row.Breadth, row.Coverage,
		); err != nil {
			return fmt.Errorf("insert weekly leader breadth: %w", err)
		}
	}
	overall := breadth[0]
	tag, err := tx.Exec(ctx, `
		UPDATE weekly_leader_runs SET
			status='completed', completed_at=$2, error=NULL,
			eligible_universe_count=$3, evaluated_count=$4, qualified_count=$5,
			failure_count=$6, added_count=$7, removed_count=$8,
			persistent_count=$9, leader_breadth=$10, evaluation_coverage=$11,
			failure_rate=$12, previous_completed_run_id=$13,
			benchmark_bar_set_hash=$14
		WHERE id=$1 AND status='running' AND attempt_count=$15`,
		id, completedAt, overall.EligibleCount, overall.EvaluatedCount,
		overall.QualifiedCount, overall.FailedCount, overall.AddedCount,
		overall.RemovedCount, overall.PersistentCount, overall.Breadth,
		overall.Coverage, ratio(overall.FailedCount, overall.EligibleCount),
		nullableUUID(previousID), nullableString(benchmarkBarSetHash), attempt,
	)
	if err != nil {
		return fmt.Errorf("complete weekly leader run: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("complete weekly leader run %q: lost claim", id)
	}
	if _, err := tx.Exec(ctx, `
		WITH latest AS (
			SELECT id FROM weekly_leader_runs
			WHERE status='completed'
			ORDER BY decision_at DESC, completed_at DESC, id DESC
			LIMIT 1
		)
		UPDATE weekly_leader_qualifications q
		SET promotion_status='superseded',
		    promotion_reason='newer completed leader run',
		    promotion_claim_token=NULL, promotion_claimed_at=NULL
		FROM weekly_leader_runs r, latest l
		WHERE q.run_id=r.id AND q.run_id<>l.id
		  AND q.promotion_status IN ('pending','processing')
		  AND r.status='completed'`,
	); err != nil {
		return fmt.Errorf("supersede stale weekly leader promotions: %w", err)
	}
	return tx.Commit(ctx)
}

// ClaimWeeklyLeaderCandidatesAsOf atomically moves every pending candidate, or
// processing candidate whose lease expired, in the latest visible run to a
// token-fenced processing state. Concurrent scanner processes therefore cannot
// score the same leader, while a crashed process cannot strand work forever.
func ClaimWeeklyLeaderCandidatesAsOf(
	ctx context.Context,
	asOf, claimedAt time.Time,
	lease time.Duration,
	claimToken string,
) ([]WeeklyLeaderCandidate, error) {
	if strings.TrimSpace(claimToken) == "" {
		return nil, fmt.Errorf("weekly leader promotion claim token is required")
	}
	if lease <= 0 {
		lease = time.Hour
	}
	rows, err := GetDB().Query(ctx, `
		WITH latest AS (
			SELECT id FROM weekly_leader_runs
			WHERE status='completed'
			  AND decision_at <= $1::timestamptz
			  AND completed_at <= $1::timestamptz
			ORDER BY decision_at DESC, completed_at DESC, id DESC LIMIT 1
		), claimable AS (
			SELECT q.run_id, q.ticker
			FROM weekly_leader_qualifications q
			JOIN latest l ON l.id=q.run_id
			WHERE q.outcome='qualified'
			  AND (
			    q.promotion_status='pending'
			    OR (
			      q.promotion_status='processing'
			      AND (
			        q.promotion_claimed_at IS NULL
			        OR q.promotion_claimed_at <= $4
			      )
			    )
			  )
			FOR UPDATE OF q SKIP LOCKED
		), claimed AS (
			UPDATE weekly_leader_qualifications q
			SET promotion_status='processing',
			    promotion_claim_token=$3,
			    promotion_claimed_at=$2,
			    promotion_reason=NULL
			FROM claimable c
			WHERE q.run_id=c.run_id AND q.ticker=c.ticker
			RETURNING q.*
		)
		SELECT q.run_id, q.universe_version_id, v.as_of, q.ticker,
		       q.composite_score, q.rank, q.sector, q.industry,
		       COALESCE(m.price,0), COALESCE(m.market_cap_mm,0),
		       q.return_20d, q.return_60d, q.return_126d,
		       q.median_dollar_volume_20d, q.promotion_claim_token
		FROM claimed q
		JOIN swing_universe_versions v ON v.id=q.universe_version_id
		JOIN swing_universe_members m
		  ON m.universe_version_id=q.universe_version_id AND m.ticker=q.ticker
		ORDER BY q.rank, q.ticker`,
		asOf, claimedAt, claimToken, claimedAt.Add(-lease))
	if err != nil {
		return nil, fmt.Errorf("claim pending weekly leader candidates: %w", err)
	}
	defer rows.Close()
	var out []WeeklyLeaderCandidate
	for rows.Next() {
		var candidate WeeklyLeaderCandidate
		if err := rows.Scan(
			&candidate.RunID, &candidate.UniverseVersionID, &candidate.UniverseAsOf,
			&candidate.Ticker, &candidate.Score, &candidate.Rank,
			&candidate.Sector, &candidate.Industry, &candidate.Price,
			&candidate.MarketCapMM, &candidate.Return20,
			&candidate.Return60, &candidate.Return126,
			&candidate.MedianDollarVolume20, &candidate.PromotionClaimToken,
		); err != nil {
			return nil, err
		}
		out = append(out, candidate)
	}
	return out, rows.Err()
}

// ReleaseWeeklyLeaderPromotionClaims returns only unfinished rows owned by the
// caller's token to pending; terminal outcomes are untouched.
func ReleaseWeeklyLeaderPromotionClaims(
	ctx context.Context, claimToken, reason string,
) error {
	if strings.TrimSpace(claimToken) == "" {
		return nil
	}
	_, err := GetDB().Exec(ctx, `
		UPDATE weekly_leader_qualifications
		SET promotion_status='pending', promotion_claim_token=NULL,
		    promotion_claimed_at=NULL, promotion_reason=$2
		WHERE promotion_status='processing' AND promotion_claim_token=$1`,
		claimToken, nullableString(reason))
	if err != nil {
		return fmt.Errorf("release weekly leader promotion claims: %w", err)
	}
	return nil
}

func MarkWeeklyLeaderPromotion(
	ctx context.Context,
	runID, ticker, claimToken, status, scannerRunID, reason string,
	at time.Time,
) error {
	status = normalizePromotionStatus(status)
	if status == "pending" || status == "processing" ||
		status == "not_requested" || status == "superseded" {
		return fmt.Errorf("terminal weekly leader promotion status required")
	}
	tag, err := GetDB().Exec(ctx, `
		UPDATE weekly_leader_qualifications
		SET promotion_status=$4, scanner_run_id=$5, promoted_at=$6,
		    promotion_reason=$7, promotion_claim_token=NULL,
		    promotion_claimed_at=NULL
		WHERE run_id=$1 AND ticker=$2 AND outcome='qualified'
		  AND promotion_status='processing' AND promotion_claim_token=$3`,
		runID, ticker, claimToken, status, nullableUUID(scannerRunID), at,
		nullableString(reason))
	if err != nil {
		return fmt.Errorf("mark weekly leader promotion: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("mark weekly leader promotion: claim is no longer current")
	}
	return nil
}

// PromoteWeeklyLeaderToWatchlist validates the token fence, writes the
// watchlist, and records the successful promotion in one transaction. A stale
// worker can neither overwrite the watchlist nor publish a terminal status.
func PromoteWeeklyLeaderToWatchlist(
	ctx context.Context,
	item WatchlistItem,
	runID, ticker, claimToken, reason string,
	at time.Time,
) error {
	if item.Ticker != ticker || strings.TrimSpace(runID) == "" ||
		strings.TrimSpace(claimToken) == "" {
		return fmt.Errorf("weekly leader watchlist promotion lineage is incomplete")
	}
	tx, err := GetDB().Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, `
		SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`,
		weeklyLeaderCompletionLock,
	); err != nil {
		return fmt.Errorf("serialize weekly leader promotion: %w", err)
	}
	var currentToken, status string
	var currentRun bool
	if err := tx.QueryRow(ctx, `
		SELECT COALESCE(q.promotion_claim_token,''), q.promotion_status,
		       COALESCE(q.run_id = (
		         SELECT r.id FROM weekly_leader_runs r
		         WHERE r.status='completed'
		         ORDER BY r.decision_at DESC, r.completed_at DESC, r.id DESC
		         LIMIT 1
		       ), false)
		FROM weekly_leader_qualifications q
		WHERE q.run_id=$1 AND q.ticker=$2 AND q.outcome='qualified'
		FOR UPDATE OF q`, runID, ticker,
	).Scan(&currentToken, &status, &currentRun); err != nil {
		return fmt.Errorf("lock weekly leader promotion: %w", err)
	}
	if !currentRun || status != "processing" || currentToken != claimToken {
		return ErrWeeklyLeaderPromotionClaimLost
	}
	if err := addWatchlistItem(ctx, tx, item); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `
		UPDATE weekly_leader_qualifications
		SET promotion_status='promoted', scanner_run_id=$4, promoted_at=$5,
		    promotion_reason=$6, promotion_claim_token=NULL,
		    promotion_claimed_at=NULL
		WHERE run_id=$1 AND ticker=$2 AND promotion_status='processing'
		  AND promotion_claim_token=$3`,
		runID, ticker, claimToken, nullableUUID(item.ScannerRunID), at,
		nullableString(reason))
	if err != nil {
		return fmt.Errorf("complete weekly leader watchlist promotion: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrWeeklyLeaderPromotionClaimLost
	}
	return tx.Commit(ctx)
}

func GetLatestCompletedWeeklyLeaderRunAsOf(
	ctx context.Context, asOf time.Time,
) (*WeeklyLeaderRun, error) {
	row := GetDB().QueryRow(ctx, `
		SELECT id, session_date, as_of, decision_at, universe_version_id, status, started_at,
		       completed_at, lease_until, attempt_count, config_hash, config_snapshot,
		       feature_version, benchmark, history_provider, COALESCE(error,''),
		       eligible_universe_count, evaluated_count, qualified_count, failure_count,
		       added_count, removed_count, persistent_count, leader_breadth,
		       evaluation_coverage, failure_rate,
		       COALESCE(previous_completed_run_id::text,'')
		FROM weekly_leader_runs
		WHERE status='completed' AND decision_at <= $1 AND completed_at <= $1
		ORDER BY decision_at DESC, completed_at DESC, id DESC LIMIT 1`, asOf)
	run, err := scanWeeklyLeaderRun(row)
	if err != nil {
		if IsNoRows(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("get latest completed weekly leader run: %w", err)
	}
	return run, nil
}

func ListCompletedWeeklyLeaderRuns(
	ctx context.Context, from, to time.Time,
) ([]WeeklyLeaderRun, error) {
	rows, err := GetDB().Query(ctx, `
		SELECT id, session_date, as_of, decision_at, universe_version_id, status,
		       started_at, completed_at, lease_until, attempt_count, config_hash,
		       config_snapshot, feature_version, benchmark, history_provider,
		       COALESCE(error,''), eligible_universe_count, evaluated_count,
		       qualified_count, failure_count, added_count, removed_count,
		       persistent_count, leader_breadth, evaluation_coverage, failure_rate,
		       COALESCE(previous_completed_run_id::text,'')
		FROM weekly_leader_runs
		WHERE status='completed' AND decision_at >= $1 AND decision_at < $2
		ORDER BY session_date, completed_at`, from, to)
	if err != nil {
		return nil, fmt.Errorf("list completed weekly leader runs: %w", err)
	}
	defer rows.Close()
	var out []WeeklyLeaderRun
	for rows.Next() {
		run, err := scanWeeklyLeaderRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *run)
	}
	return out, rows.Err()
}

func insertWeeklyLeaderQualification(
	ctx context.Context, tx pgx.Tx, q WeeklyLeaderQualification,
) error {
	if q.Outcome != "qualified" && q.Outcome != "not_qualified" && q.Outcome != "failed" {
		return fmt.Errorf("weekly leader qualification %q has invalid outcome %q",
			q.Ticker, q.Outcome)
	}
	if q.Outcome == "qualified" &&
		(q.CompositeScore == nil || q.ScorePercentile == nil || q.Rank == nil ||
			q.Return20 == nil || q.Return60 == nil || q.Return126 == nil ||
			q.MedianDollarVolume20 == nil || strings.TrimSpace(q.BarSetHash) == "") {
		return fmt.Errorf("weekly leader qualification %q lacks required score/input evidence",
			q.Ticker)
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO weekly_leader_qualifications (
			run_id, universe_version_id, ticker, outcome, failure_stage, failure_reason,
			history_first_at, history_last_at, history_sessions, bar_set_hash,
			source_evidence, sector, industry,
			market_cap_band, volatility_band, return_5d, return_20d, return_60d,
			return_126d, high_52w_proximity, market_relative_strength,
			industry_relative_strength, rs_acceleration, regression_annualized_slope,
			regression_r2, efficiency_60d, max_drawdown_126d, natr_14,
			volatility_adjusted_return, median_dollar_volume_20d,
			dollar_volume_stability_20d, normalized_obv_20d, cmf_20d,
			gap_domination_60d, composite_score, score_percentile, rank,
			feature_coverage, features, components, was_qualified_previous,
			qualification_streak, qualified_since, promotion_status
		) VALUES (
			$1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,
			$19,$20,$21,$22,$23,$24,$25,$26,$27,$28,$29,$30,$31,$32,$33,$34,
			$35,$36,$37,$38,$39,$40,$41,$42,$43,$44
		)`,
		q.RunID, q.UniverseVersionID, q.Ticker, q.Outcome,
		nullableString(q.FailureStage), nullableString(q.FailureReason),
		q.HistoryFirstAt, q.HistoryLastAt, q.HistorySessions,
		nullableString(q.BarSetHash), nonNilJSON(q.SourceEvidence),
		normalizeTaxonomyValue(q.Sector), normalizeTaxonomyValue(q.Industry),
		defaultString(q.MarketCapBand, "unknown"),
		defaultString(q.VolatilityBand, "unknown"),
		q.Return5, q.Return20, q.Return60, q.Return126, q.High52WeekProximity,
		q.MarketRelativeStrength, q.IndustryRelativeStrength, q.RSAcceleration,
		q.RegressionAnnualizedSlope, q.RegressionR2, q.Efficiency60,
		q.MaxDrawdown126, q.NATR14, q.VolatilityAdjustedReturn,
		q.MedianDollarVolume20, q.DollarVolumeStability20, q.NormalizedOBV20,
		q.CMF20, q.GapDomination60, q.CompositeScore, q.ScorePercentile, q.Rank,
		q.FeatureCoverage, nonNilJSON(q.Features), nonNilJSON(q.Components),
		q.WasQualifiedPrevious, q.QualificationStreak, q.QualifiedSince,
		normalizePromotionStatus(q.PromotionStatus),
	)
	if err != nil {
		return fmt.Errorf("insert weekly leader qualification %q: %w", q.Ticker, err)
	}
	return nil
}

func previousWeeklyLeaders(
	ctx context.Context, tx pgx.Tx, sessionDate time.Time, configHash string,
) (string, map[string]WeeklyLeaderQualification, error) {
	var previousID string
	err := tx.QueryRow(ctx, `
		SELECT id FROM weekly_leader_runs
		WHERE status='completed' AND session_date < $1 AND config_hash=$2
		ORDER BY session_date DESC, completed_at DESC LIMIT 1`,
		sessionDate, configHash,
	).Scan(&previousID)
	if err != nil {
		if IsNoRows(err) {
			return "", map[string]WeeklyLeaderQualification{}, nil
		}
		return "", nil, fmt.Errorf("find previous weekly leader run: %w", err)
	}
	rows, err := tx.Query(ctx, `
		SELECT ticker, composite_score, rank, qualification_streak, qualified_since
		FROM weekly_leader_qualifications
		WHERE run_id=$1 AND outcome='qualified'`, previousID)
	if err != nil {
		return "", nil, fmt.Errorf("load previous weekly leaders: %w", err)
	}
	defer rows.Close()
	out := make(map[string]WeeklyLeaderQualification)
	for rows.Next() {
		var q WeeklyLeaderQualification
		if err := rows.Scan(
			&q.Ticker, &q.CompositeScore, &q.Rank, &q.QualificationStreak,
			&q.QualifiedSince,
		); err != nil {
			return "", nil, err
		}
		out[q.Ticker] = q
	}
	return previousID, out, rows.Err()
}

func buildWeeklyLeaderTransitions(
	runID, previousID string,
	previous, current map[string]WeeklyLeaderQualification,
) []WeeklyLeaderTransition {
	out := make([]WeeklyLeaderTransition, 0)
	for ticker, q := range current {
		if q.Outcome != "qualified" {
			if prior, ok := previous[ticker]; ok {
				reason := "no_longer_qualified"
				if q.Outcome == "failed" {
					reason = "data_failure"
				}
				out = append(out, WeeklyLeaderTransition{
					RunID: runID, Ticker: ticker, Event: "removed",
					PreviousRunID: previousID, PreviousScore: prior.CompositeScore,
					PreviousRank: prior.Rank, CurrentScore: q.CompositeScore,
					CurrentRank: q.Rank, Reason: reason,
				})
			}
			continue
		}
		event, reason := "added", "newly_qualified"
		var previousScore *float64
		var previousRank *int
		if prior, ok := previous[ticker]; ok {
			event, reason = "persistent", "remained_qualified"
			previousScore, previousRank = prior.CompositeScore, prior.Rank
		}
		out = append(out, WeeklyLeaderTransition{
			RunID: runID, Ticker: ticker, Event: event, PreviousRunID: previousID,
			PreviousScore: previousScore, CurrentScore: q.CompositeScore,
			PreviousRank: previousRank, CurrentRank: q.Rank, Reason: reason,
		})
	}
	for ticker, prior := range previous {
		if _, ok := current[ticker]; ok {
			continue
		}
		out = append(out, WeeklyLeaderTransition{
			RunID: runID, Ticker: ticker, Event: "removed",
			PreviousRunID: previousID, PreviousScore: prior.CompositeScore,
			PreviousRank: prior.Rank, Reason: "universe_exit",
		})
	}
	return out
}

func buildWeeklyLeaderBreadth(
	runID string,
	qualifications []WeeklyLeaderQualification,
	transitions []WeeklyLeaderTransition,
) []WeeklyLeaderBreadth {
	type key struct{ dimension, value string }
	groups := map[key]*WeeklyLeaderBreadth{
		{"overall", "all"}: {
			RunID: runID, DimensionType: "overall", DimensionValue: "all",
		},
	}
	add := func(dimension, value string, q WeeklyLeaderQualification) {
		k := key{dimension, normalizeTaxonomyValue(value)}
		row := groups[k]
		if row == nil {
			row = &WeeklyLeaderBreadth{
				RunID: runID, DimensionType: dimension, DimensionValue: k.value,
			}
			groups[k] = row
		}
		row.EligibleCount++
		if q.Outcome != "failed" {
			row.EvaluatedCount++
		} else {
			row.FailedCount++
		}
		if q.Outcome == "qualified" {
			row.QualifiedCount++
		}
	}
	for _, q := range qualifications {
		add("overall", "all", q)
		add("sector", q.Sector, q)
		add("industry", q.Industry, q)
		add("market_cap_band", q.MarketCapBand, q)
	}
	for _, transition := range transitions {
		overall := groups[key{"overall", "all"}]
		switch transition.Event {
		case "added":
			overall.AddedCount++
		case "removed":
			overall.RemovedCount++
		case "persistent":
			overall.PersistentCount++
		}
	}
	out := make([]WeeklyLeaderBreadth, 0, len(groups))
	overall := groups[key{"overall", "all"}]
	overall.Breadth = ratio(overall.QualifiedCount, overall.EligibleCount)
	overall.Coverage = ratio(overall.EvaluatedCount, overall.EligibleCount)
	out = append(out, *overall)
	for k, row := range groups {
		if k.dimension == "overall" {
			continue
		}
		row.Breadth = ratio(row.QualifiedCount, row.EligibleCount)
		row.Coverage = ratio(row.EvaluatedCount, row.EligibleCount)
		out = append(out, *row)
	}
	return out
}

func scanWeeklyLeaderRun(row pgx.Row) (*WeeklyLeaderRun, error) {
	var run WeeklyLeaderRun
	err := row.Scan(
		&run.ID, &run.SessionDate, &run.AsOf, &run.DecisionAt, &run.UniverseVersionID, &run.Status,
		&run.StartedAt, &run.CompletedAt, &run.LeaseUntil, &run.AttemptCount,
		&run.ConfigHash, &run.ConfigSnapshot, &run.FeatureVersion, &run.Benchmark,
		&run.HistoryProvider, &run.Error, &run.EligibleUniverseCount,
		&run.EvaluatedCount, &run.QualifiedCount, &run.FailureCount,
		&run.AddedCount, &run.RemovedCount, &run.PersistentCount,
		&run.LeaderBreadth, &run.EvaluationCoverage, &run.FailureRate,
		&run.PreviousCompletedRunID,
	)
	return &run, err
}

func normalizePromotionStatus(status string) string {
	switch status {
	case "pending", "processing", "promoted", "rejected", "active_watchlist",
		"cap_suppressed", "failed", "superseded":
		return status
	default:
		return "not_requested"
	}
}

func normalizeTaxonomyValue(value string) string {
	if strings.TrimSpace(value) == "" {
		return UnknownTaxonomy
	}
	return value
}

func defaultString(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func nullableUUID(value string) any {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return value
}

func ratio(numerator, denominator int) float64 {
	if denominator == 0 {
		return 0
	}
	return float64(numerator) / float64(denominator)
}
