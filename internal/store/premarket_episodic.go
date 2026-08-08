package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

var ErrPremarketPromotionClaimLost = errors.New(
	"premarket episodic promotion claim is no longer current",
)

const premarketEpisodicCompletionLock = "premarket-episodic-completion"

type PremarketEpisodicRun struct {
	ID                    string
	SessionDate           time.Time
	CheckpointAt          time.Time
	HardCutoffAt          time.Time
	UniverseVersionID     string
	Status                string
	StartedAt             time.Time
	CompletedAt           *time.Time
	LeaseUntil            time.Time
	AttemptCount          int
	ConfigHash            string
	ConfigSnapshot        json.RawMessage
	FeatureVersion        string
	MarketDataProvider    string
	Feed                  string
	EntitlementStatus     string
	Error                 string
	EligibleUniverseCount int
	ShortlistedCount      int
	EvaluatedCount        int
	QualifiedCount        int
	FailureCount          int
}

type PremarketEpisodicCandidate struct {
	RunID               string
	UniverseVersionID   string
	UniverseAsOf        time.Time
	Ticker              string
	Outcome             string
	RemovalStage        string
	FailureReason       string
	Score               *float64
	Rank                *int
	GapPct              *float64
	PremarketVolume     *int64
	TimeAdjustedRVOL    *float64
	DollarVolume        *float64
	TradeCount          *int64
	SpreadBPS           *float64
	QuoteAgeSeconds     *float64
	BarCoverage         *float64
	PremarketHigh       *float64
	PremarketLow        *float64
	PremarketLocation   *float64
	LastPrice           *float64
	PreviousClose       *float64
	MarketCapMM         *float64
	MarketCapRisk       string
	FloatStatus         string
	ADR14Pct            *float64
	NATR14Pct           *float64
	CatalystStatus      string
	CatalystReference   json.RawMessage
	RawEvidence         json.RawMessage
	Thresholds          json.RawMessage
	PromotionStatus     string
	PromotionClaimToken string
	PromotionClaimedAt  *time.Time
	ScannerRunID        string
	PromotedAt          *time.Time
	PromotionReason     string
	Sector              string
	Industry            string
	TaxonomyStatus      string
	TaxonomyAsOf        *time.Time
	TaxonomyProvider    string
	TaxonomyEvidence    json.RawMessage
}

func ClaimPremarketEpisodicRun(
	ctx context.Context, run PremarketEpisodicRun, lease time.Duration,
) (*PremarketEpisodicRun, bool, error) {
	if run.SessionDate.IsZero() || run.CheckpointAt.IsZero() ||
		run.HardCutoffAt.IsZero() || run.UniverseVersionID == "" ||
		run.ConfigHash == "" || run.FeatureVersion == "" || run.Feed != "sip" {
		return nil, false, fmt.Errorf("premarket episodic run identity is incomplete")
	}
	if !run.CheckpointAt.Before(run.HardCutoffAt) {
		return nil, false, fmt.Errorf("premarket checkpoint must precede hard cutoff")
	}
	if lease <= 0 {
		lease = 15 * time.Minute
	}
	if err := validateApprovedUniverseForDependentRun(
		ctx, run.UniverseVersionID, run.CheckpointAt, nil,
	); err != nil {
		return nil, false, err
	}
	row := GetDB().QueryRow(ctx, `
		INSERT INTO premarket_episodic_runs (
			session_date, checkpoint_at, hard_cutoff_at, universe_version_id,
			started_at, lease_until, config_hash, config_snapshot, feature_version,
			market_data_provider, feed, entitlement_status
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
		ON CONFLICT (session_date, checkpoint_at, universe_version_id, config_hash)
		DO UPDATE SET
			status='running', started_at=EXCLUDED.started_at,
			lease_until=EXCLUDED.lease_until,
			attempt_count=premarket_episodic_runs.attempt_count+1,
			config_snapshot=EXCLUDED.config_snapshot,
			completed_at=NULL, error=NULL,
			entitlement_status=EXCLUDED.entitlement_status
		WHERE premarket_episodic_runs.status='failed'
		   OR (premarket_episodic_runs.status='running'
		       AND premarket_episodic_runs.lease_until <= EXCLUDED.started_at)
		RETURNING id, session_date, checkpoint_at, hard_cutoff_at,
		          universe_version_id, status, started_at, completed_at,
		          lease_until, attempt_count, config_hash, config_snapshot,
		          feature_version, market_data_provider, feed, entitlement_status,
		          COALESCE(error,''), eligible_universe_count, shortlisted_count,
		          evaluated_count, qualified_count, failure_count`,
		run.SessionDate, run.CheckpointAt, run.HardCutoffAt,
		run.UniverseVersionID, run.StartedAt, run.StartedAt.Add(lease),
		run.ConfigHash, nonNilJSON(run.ConfigSnapshot), run.FeatureVersion,
		run.MarketDataProvider, run.Feed, run.EntitlementStatus,
	)
	claimed, err := scanPremarketEpisodicRun(row)
	if err == nil {
		return claimed, true, nil
	}
	if !IsNoRows(err) {
		return nil, false, fmt.Errorf("claim premarket episodic run: %w", err)
	}
	existing, lookupErr := scanPremarketEpisodicRun(GetDB().QueryRow(ctx, `
		SELECT id, session_date, checkpoint_at, hard_cutoff_at,
		       universe_version_id, status, started_at, completed_at,
		       lease_until, attempt_count, config_hash, config_snapshot,
		       feature_version, market_data_provider, feed, entitlement_status,
		       COALESCE(error,''), eligible_universe_count, shortlisted_count,
		       evaluated_count, qualified_count, failure_count
		FROM premarket_episodic_runs
		WHERE session_date=$1 AND checkpoint_at=$2
		  AND universe_version_id=$3 AND config_hash=$4`,
		run.SessionDate, run.CheckpointAt, run.UniverseVersionID, run.ConfigHash,
	))
	if lookupErr != nil {
		return nil, false, fmt.Errorf("load declined premarket run claim: %w", lookupErr)
	}
	return existing, false, nil
}

func FailPremarketEpisodicRun(
	ctx context.Context, id string, attempt int, failedAt time.Time,
	entitlementUnavailable bool, cause error,
) error {
	message := ""
	if cause != nil {
		message = cause.Error()
	}
	entitlement := "available"
	if entitlementUnavailable {
		entitlement = "unavailable"
	}
	tag, err := GetDB().Exec(ctx, `
		UPDATE premarket_episodic_runs
		SET status='failed', completed_at=$2, error=$3, entitlement_status=$5
		WHERE id=$1 AND status='running' AND attempt_count=$4`,
		id, failedAt, nullableString(message), attempt, entitlement)
	if err != nil {
		return fmt.Errorf("fail premarket episodic run: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("fail premarket episodic run %q: claim is no longer current", id)
	}
	return nil
}

func CompletePremarketEpisodicRun(
	ctx context.Context, id string, attempt int, completedAt time.Time,
	eligible, shortlisted int, candidates []PremarketEpisodicCandidate,
) error {
	tx, err := GetDB().Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, `
		SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`,
		premarketEpisodicCompletionLock,
	); err != nil {
		return err
	}
	var cutoff time.Time
	var status string
	var currentAttempt int
	if err := tx.QueryRow(ctx, `
		SELECT hard_cutoff_at, status, attempt_count
		FROM premarket_episodic_runs WHERE id=$1 FOR UPDATE`,
		id,
	).Scan(&cutoff, &status, &currentAttempt); err != nil {
		return err
	}
	if status != "running" || currentAttempt != attempt {
		return fmt.Errorf("complete premarket run: claim is no longer current")
	}
	if completedAt.After(cutoff) {
		return fmt.Errorf("premarket run completed after hard cutoff")
	}
	if _, err := tx.Exec(ctx,
		`DELETE FROM premarket_episodic_candidates WHERE run_id=$1`, id,
	); err != nil {
		return err
	}
	qualified, failed := 0, 0
	for _, candidate := range candidates {
		if candidate.Outcome == "qualified" {
			qualified++
		}
		if candidate.Outcome == "failed" {
			failed++
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO premarket_episodic_candidates (
				run_id, universe_version_id, ticker, outcome, removal_stage,
				failure_reason, score, rank, gap_pct, premarket_volume,
				time_adjusted_rvol, dollar_volume, trade_count, spread_bps,
				quote_age_seconds, bar_coverage, premarket_high, premarket_low,
				premarket_location, last_price, previous_close, market_cap_mm,
				market_cap_risk, float_status, adr_14_pct, natr_14_pct,
				catalyst_status, catalyst_reference, raw_evidence, thresholds,
				promotion_status
			) VALUES (
				$1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,
				$17,$18,$19,$20,$21,$22,$23,$24,$25,$26,$27,$28,$29,$30,$31
			)`,
			id, candidate.UniverseVersionID, candidate.Ticker, candidate.Outcome,
			nullableString(candidate.RemovalStage), nullableString(candidate.FailureReason),
			candidate.Score, candidate.Rank, candidate.GapPct,
			candidate.PremarketVolume, candidate.TimeAdjustedRVOL,
			candidate.DollarVolume, candidate.TradeCount, candidate.SpreadBPS,
			candidate.QuoteAgeSeconds, candidate.BarCoverage,
			candidate.PremarketHigh, candidate.PremarketLow,
			candidate.PremarketLocation, candidate.LastPrice,
			candidate.PreviousClose, candidate.MarketCapMM,
			nullableString(candidate.MarketCapRisk), candidate.FloatStatus,
			candidate.ADR14Pct, candidate.NATR14Pct, candidate.CatalystStatus,
			nonNilJSON(candidate.CatalystReference),
			nonNilJSON(candidate.RawEvidence), nonNilJSON(candidate.Thresholds),
			candidate.PromotionStatus,
		)
		if err != nil {
			return fmt.Errorf("insert premarket candidate %s: %w", candidate.Ticker, err)
		}
	}
	// A later checkpoint is authoritative for the same session. Older
	// unconsumed candidates stay auditable but can no longer be promoted.
	if _, err := tx.Exec(ctx, `
		UPDATE premarket_episodic_candidates c
		SET promotion_status='superseded',
		    promotion_reason='newer premarket checkpoint completed'
		FROM premarket_episodic_runs r, premarket_episodic_runs current
		WHERE c.run_id=r.id AND current.id=$1
		  AND r.session_date=current.session_date
		  AND r.checkpoint_at < current.checkpoint_at
		  AND c.promotion_status='pending'`,
		id,
	); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `
		UPDATE premarket_episodic_runs
		SET status='completed', completed_at=$2, error=NULL,
		    eligible_universe_count=$3, shortlisted_count=$4,
		    evaluated_count=$5, qualified_count=$6, failure_count=$7
		WHERE id=$1 AND status='running' AND attempt_count=$8
		  AND $2 <= hard_cutoff_at
		  AND clock_timestamp() <= hard_cutoff_at`,
		id, completedAt, eligible, shortlisted, len(candidates), qualified, failed, attempt,
	)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("complete premarket run: cutoff or claim fence lost")
	}
	return tx.Commit(ctx)
}

func ClaimPremarketEpisodicCandidatesAsOf(
	ctx context.Context, expectedSessionDate, asOf, claimedAt time.Time,
	lease time.Duration, claimToken string,
) ([]PremarketEpisodicCandidate, error) {
	if strings.TrimSpace(claimToken) == "" || expectedSessionDate.IsZero() {
		return nil, fmt.Errorf("premarket promotion claim identity is incomplete")
	}
	if lease <= 0 {
		lease = 15 * time.Minute
	}
	rows, err := GetDB().Query(ctx, `
		WITH latest AS (
			SELECT id FROM premarket_episodic_runs
			WHERE status='completed' AND session_date=$1::date
			  AND checkpoint_at <= $2 AND completed_at <= $2
			ORDER BY checkpoint_at DESC, completed_at DESC, id DESC LIMIT 1
		), claimable AS (
			SELECT c.run_id, c.ticker
			FROM premarket_episodic_candidates c JOIN latest l ON l.id=c.run_id
			WHERE c.outcome='qualified' AND (
				c.promotion_status='pending' OR
				(c.promotion_status='processing' AND
				 (c.promotion_claimed_at IS NULL OR c.promotion_claimed_at <= $5))
			)
			FOR UPDATE OF c SKIP LOCKED
		), claimed AS (
			UPDATE premarket_episodic_candidates c
			SET promotion_status='processing', promotion_claim_token=$4,
			    promotion_claimed_at=$3, promotion_reason=NULL
			FROM claimable x
			WHERE c.run_id=x.run_id AND c.ticker=x.ticker
			RETURNING c.*
		)
		SELECT c.run_id, c.universe_version_id, v.as_of, c.ticker,
		       c.score, c.rank, c.gap_pct, c.premarket_volume,
		       c.time_adjusted_rvol, c.dollar_volume, c.trade_count,
		       c.spread_bps, c.last_price, c.market_cap_mm,
		       c.catalyst_status, c.catalyst_reference, c.raw_evidence,
		       c.promotion_claim_token, m.sector, m.industry,
		       m.taxonomy_status, m.taxonomy_as_of,
		       COALESCE(m.taxonomy_provider,''), m.taxonomy_evidence
		FROM claimed c
		JOIN swing_universe_versions v ON v.id=c.universe_version_id
		JOIN swing_universe_members m
		  ON m.universe_version_id=c.universe_version_id AND m.ticker=c.ticker
		ORDER BY c.rank, c.ticker`,
		expectedSessionDate.Format("2006-01-02"), asOf, claimedAt, claimToken,
		claimedAt.Add(-lease),
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PremarketEpisodicCandidate
	for rows.Next() {
		var c PremarketEpisodicCandidate
		if err := rows.Scan(
			&c.RunID, &c.UniverseVersionID, &c.UniverseAsOf, &c.Ticker,
			&c.Score, &c.Rank, &c.GapPct, &c.PremarketVolume,
			&c.TimeAdjustedRVOL, &c.DollarVolume, &c.TradeCount,
			&c.SpreadBPS, &c.LastPrice, &c.MarketCapMM,
			&c.CatalystStatus, &c.CatalystReference, &c.RawEvidence,
			&c.PromotionClaimToken, &c.Sector, &c.Industry,
			&c.TaxonomyStatus, &c.TaxonomyAsOf,
			&c.TaxonomyProvider, &c.TaxonomyEvidence,
		); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func ReleasePremarketEpisodicPromotionClaims(
	ctx context.Context, claimToken, reason string,
) error {
	if strings.TrimSpace(claimToken) == "" {
		return nil
	}
	_, err := GetDB().Exec(ctx, `
		UPDATE premarket_episodic_candidates
		SET promotion_status='pending', promotion_claim_token=NULL,
		    promotion_claimed_at=NULL, promotion_reason=$2
		WHERE promotion_status='processing' AND promotion_claim_token=$1`,
		claimToken, nullableString(reason))
	return err
}

func MarkPremarketEpisodicPromotion(
	ctx context.Context, runID, ticker, token, status, scannerRunID, reason string,
	at time.Time,
) error {
	status = normalizePromotionStatus(status)
	if status == "pending" || status == "processing" ||
		status == "not_requested" || status == "superseded" {
		return fmt.Errorf("terminal premarket episodic promotion status required")
	}
	tag, err := GetDB().Exec(ctx, `
		UPDATE premarket_episodic_candidates
		SET promotion_status=$4, scanner_run_id=$5, promoted_at=$6,
		    promotion_reason=$7, promotion_claim_token=NULL,
		    promotion_claimed_at=NULL
		WHERE run_id=$1 AND ticker=$2 AND outcome='qualified'
		  AND promotion_status='processing' AND promotion_claim_token=$3`,
		runID, ticker, token, status, nullableUUID(scannerRunID), at,
		nullableString(reason),
	)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrPremarketPromotionClaimLost
	}
	return nil
}

func ListCompletedPremarketEpisodicRunsBySessionDate(
	ctx context.Context, fromDate, toDate time.Time,
) ([]PremarketEpisodicRun, error) {
	rows, err := GetDB().Query(ctx, `
		WITH canonical AS (
			SELECT DISTINCT ON (session_date, checkpoint_at)
			       id, session_date, checkpoint_at, hard_cutoff_at,
			       universe_version_id, status, started_at, completed_at,
			       lease_until, attempt_count, config_hash, config_snapshot,
			       feature_version, market_data_provider, feed, entitlement_status,
			       error, eligible_universe_count, shortlisted_count,
			       evaluated_count, qualified_count, failure_count
			FROM premarket_episodic_runs
			WHERE status='completed'
			  AND session_date >= $1::date AND session_date <= $2::date
			ORDER BY session_date, checkpoint_at, completed_at DESC, id DESC
		)
		SELECT id, session_date, checkpoint_at, hard_cutoff_at,
		       universe_version_id, status, started_at, completed_at,
		       lease_until, attempt_count, config_hash, config_snapshot,
		       feature_version, market_data_provider, feed, entitlement_status,
		       COALESCE(error,''), eligible_universe_count, shortlisted_count,
		       evaluated_count, qualified_count, failure_count
		FROM canonical ORDER BY session_date, checkpoint_at`,
		fromDate.Format("2006-01-02"), toDate.Format("2006-01-02"),
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PremarketEpisodicRun
	for rows.Next() {
		run, err := scanPremarketEpisodicRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *run)
	}
	return out, rows.Err()
}

func ListPremarketEpisodicCandidates(
	ctx context.Context, runIDs []string,
) ([]PremarketEpisodicCandidate, error) {
	if len(runIDs) == 0 {
		return nil, nil
	}
	rows, err := GetDB().Query(ctx, `
		SELECT run_id, ticker, outcome, COALESCE(removal_stage,''),
		       COALESCE(failure_reason,''), score, rank, gap_pct,
		       premarket_volume, time_adjusted_rvol, dollar_volume, trade_count,
		       spread_bps, bar_coverage, premarket_location, last_price,
		       market_cap_mm, adr_14_pct, natr_14_pct, catalyst_status,
		       catalyst_reference, promotion_status
		FROM premarket_episodic_candidates
		WHERE run_id = ANY($1::uuid[])
		ORDER BY run_id, COALESCE(rank, 2147483647), ticker`,
		runIDs,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PremarketEpisodicCandidate
	for rows.Next() {
		var candidate PremarketEpisodicCandidate
		if err := rows.Scan(
			&candidate.RunID, &candidate.Ticker, &candidate.Outcome,
			&candidate.RemovalStage, &candidate.FailureReason,
			&candidate.Score, &candidate.Rank, &candidate.GapPct,
			&candidate.PremarketVolume, &candidate.TimeAdjustedRVOL,
			&candidate.DollarVolume, &candidate.TradeCount,
			&candidate.SpreadBPS, &candidate.BarCoverage,
			&candidate.PremarketLocation, &candidate.LastPrice,
			&candidate.MarketCapMM, &candidate.ADR14Pct, &candidate.NATR14Pct,
			&candidate.CatalystStatus, &candidate.CatalystReference,
			&candidate.PromotionStatus,
		); err != nil {
			return nil, err
		}
		out = append(out, candidate)
	}
	return out, rows.Err()
}

func scanPremarketEpisodicRun(row rowScanner) (*PremarketEpisodicRun, error) {
	var run PremarketEpisodicRun
	err := row.Scan(
		&run.ID, &run.SessionDate, &run.CheckpointAt, &run.HardCutoffAt,
		&run.UniverseVersionID, &run.Status, &run.StartedAt, &run.CompletedAt,
		&run.LeaseUntil, &run.AttemptCount, &run.ConfigHash, &run.ConfigSnapshot,
		&run.FeatureVersion, &run.MarketDataProvider, &run.Feed,
		&run.EntitlementStatus, &run.Error, &run.EligibleUniverseCount,
		&run.ShortlistedCount, &run.EvaluatedCount, &run.QualifiedCount,
		&run.FailureCount,
	)
	return &run, err
}
