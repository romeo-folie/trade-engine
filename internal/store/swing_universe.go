package store

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

const UnknownTaxonomy = "Unknown"

func RecordScannerDependencyBlock(
	ctx context.Context, scanner string, sessionDate, decisionAt time.Time,
	dependency string, cause error,
) error {
	message := ""
	if cause != nil {
		message = cause.Error()
	}
	_, err := GetDB().Exec(ctx, `
		INSERT INTO scanner_dependency_blocks (
			scanner, session_date, decision_at, dependency, status, last_error
		) VALUES ($1,$2::date,$3,$4,'blocked',$5)
		ON CONFLICT (scanner, session_date, decision_at, dependency) DO UPDATE SET
			status='blocked', attempt_count=scanner_dependency_blocks.attempt_count+1,
			last_error=EXCLUDED.last_error, last_blocked_at=clock_timestamp(),
			resolved_at=NULL`,
		scanner, sessionDate, decisionAt, dependency, nullableString(message),
	)
	if err != nil {
		return fmt.Errorf("record scanner dependency block: %w", err)
	}
	return nil
}

func ResolveScannerDependencyBlock(
	ctx context.Context, scanner string, sessionDate, decisionAt time.Time,
	dependency string,
) error {
	_, err := GetDB().Exec(ctx, `
		UPDATE scanner_dependency_blocks
		SET status='resolved', resolved_at=clock_timestamp(), last_error=NULL
		WHERE scanner=$1 AND session_date=$2::date AND decision_at=$3
		  AND dependency=$4 AND status='blocked'`,
		scanner, sessionDate, decisionAt, dependency,
	)
	if err != nil {
		return fmt.Errorf("resolve scanner dependency block: %w", err)
	}
	return nil
}

func validateApprovedUniverseForDependentRun(
	ctx context.Context, versionID string, decisionAt time.Time,
	expectedSession *time.Time,
) error {
	if strings.TrimSpace(versionID) == "" || decisionAt.IsZero() {
		return fmt.Errorf("dependent scanner universe lineage is incomplete")
	}
	row := GetDB().QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM swing_universe_versions
			WHERE id=$1 AND status='completed' AND quality_status='approved'
			  AND as_of <= $2 AND captured_at <= $2 AND completed_at <= $2
			  AND ($3::date IS NULL OR session_date=$3::date)
		)`, versionID, decisionAt, expectedSession,
	)
	var valid bool
	if err := row.Scan(&valid); err != nil {
		return fmt.Errorf("validate dependent scanner universe lineage: %w", err)
	}
	if !valid {
		return fmt.Errorf("universe version %s is not approved for the dependent run", versionID)
	}
	return nil
}

type SwingUniverseVersion struct {
	ID                   string
	Provider             string
	AsOf                 time.Time
	CapturedAt           time.Time
	CompletedAt          *time.Time
	Status               string
	Error                string
	Filters              json.RawMessage
	SourceEvidence       json.RawMessage
	MemberCount          int
	EligibleCount        int
	UnknownTaxonomyCount int
	StaleTaxonomyCount   int
	BuildClaimID         string
	BuildAttempt         int
	SessionDate          *time.Time
	ConfigHash           string
	QualityStatus        string
	QualityReason        string
	Quality              SwingUniverseQuality
}

type SwingUniverseQuality struct {
	EvidenceRequiredCount     int
	UsableSnapshotCount       int
	UsableBarHistoryCount     int
	CoherentMarketDataCount   int
	FundamentalsRequiredCount int
	UsableFundamentalsCount   int
	SnapshotFailureCount      int
	BarFailureCount           int
	FundamentalsFailureCount  int
	ProviderFailureCount      int
	PartialFailureCount       int
	MarketDataCoverage        float64
	FundamentalsCoverage      float64
	MinMarketDataCoverage     float64
}

type SwingUniverseBuildRequest struct {
	SessionDate    time.Time
	Provider       string
	ConfigHash     string
	ConfigSnapshot json.RawMessage
	StartedAt      time.Time
}

type SwingUniverseBuildClaim struct {
	ID                 string
	SessionDate        time.Time
	Provider           string
	ConfigHash         string
	Status             string
	OwnerToken         string
	LeaseUntil         time.Time
	AttemptCount       int
	ActiveVersionID    string
	PublishedVersionID string
	LastError          string
}

type SwingUniverseMember struct {
	UniverseVersionID string
	Ticker            string
	Eligible          bool
	RemovalStage      string
	FailureStage      string
	FailureReason     string
	AssetID           string
	Name              string
	SecurityType      string
	Exchange          string
	Active            *bool
	Tradable          *bool
	Price             *float64
	MarketCapMM       *float64
	DollarVolume      *float64
	SpreadBPS         *float64
	DataAvailable     *bool
	Sector            string
	Industry          string
	TaxonomyStatus    string
	TaxonomyAsOf      *time.Time
	TaxonomyProvider  string
	TaxonomyEvidence  json.RawMessage
	SourceEvidence    json.RawMessage
}

type CompanyTaxonomyObservation struct {
	ID          string
	Ticker      string
	Provider    string
	EffectiveAt time.Time
	ObservedAt  time.Time
	Sector      string
	Industry    string
	Status      string
	Evidence    json.RawMessage
}

func BeginSwingUniverseVersion(ctx context.Context, version SwingUniverseVersion) (string, error) {
	if version.Provider == "" {
		return "", fmt.Errorf("swing universe provider is required")
	}
	if version.AsOf.IsZero() || version.CapturedAt.IsZero() {
		return "", fmt.Errorf("swing universe as-of and captured-at are required")
	}
	var id string
	// Approval is reserved for the claimed, quality-validating completion path.
	// Ad-hoc and legacy callers remain auditable but never consumer-selectable.
	qualityStatus := "unassessed"
	err := GetDB().QueryRow(ctx, `
		INSERT INTO swing_universe_versions (
			provider, as_of, captured_at, filters, source_evidence, quality_status
		) VALUES ($1,$2,$3,$4,$5,$6)
		RETURNING id`,
		version.Provider, version.AsOf, version.CapturedAt,
		nonNilJSON(version.Filters), nonNilJSON(version.SourceEvidence), qualityStatus,
	).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("begin swing universe version: %w", err)
	}
	return id, nil
}

func UpdateSwingUniverseVersionMetadata(
	ctx context.Context, id string, version SwingUniverseVersion,
) error {
	tag, err := GetDB().Exec(ctx, `
		UPDATE swing_universe_versions
		SET as_of=$2, filters=$3, source_evidence=$4
		WHERE id=$1 AND status='building' AND build_claim_id IS NULL`,
		id, version.AsOf, nonNilJSON(version.Filters), nonNilJSON(version.SourceEvidence),
	)
	if err != nil {
		return fmt.Errorf("update swing universe version metadata: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("update swing universe version %q metadata: not building", id)
	}
	return nil
}

// ClaimSwingUniverseBuild durably serializes one configured producer for a US
// market session. Every reclaim creates a fresh immutable version attempt.
func ClaimSwingUniverseBuild(
	ctx context.Context, request SwingUniverseBuildRequest, lease time.Duration,
) (*SwingUniverseBuildClaim, bool, error) {
	if request.SessionDate.IsZero() || request.StartedAt.IsZero() ||
		strings.TrimSpace(request.Provider) == "" || strings.TrimSpace(request.ConfigHash) == "" {
		return nil, false, fmt.Errorf("swing universe build identity is incomplete")
	}
	if lease <= 0 {
		lease = 2 * time.Hour
	}
	tx, err := GetDB().Begin(ctx)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	var claim SwingUniverseBuildClaim
	inserted := false
	err = tx.QueryRow(ctx, `
		INSERT INTO swing_universe_build_claims (
			session_date, provider, config_hash, status, lease_until,
			last_started_at
		) VALUES ($1::date,$2,$3,'running',
		          clock_timestamp()+($4*interval '1 second'),$5)
		ON CONFLICT (session_date, provider, config_hash) DO NOTHING
		RETURNING id, session_date, provider, config_hash, status,
		          owner_token, lease_until, attempt_count,
		          COALESCE(active_version_id::text,''),
		          COALESCE(published_version_id::text,''), COALESCE(last_error,'')`,
		request.SessionDate, request.Provider, request.ConfigHash,
		lease.Seconds(), request.StartedAt,
	).Scan(
		&claim.ID, &claim.SessionDate, &claim.Provider, &claim.ConfigHash,
		&claim.Status, &claim.OwnerToken, &claim.LeaseUntil, &claim.AttemptCount,
		&claim.ActiveVersionID, &claim.PublishedVersionID, &claim.LastError,
	)
	if err == nil {
		inserted = true
	} else if err != pgx.ErrNoRows {
		return nil, false, fmt.Errorf("insert swing universe build claim: %w", err)
	}
	if !inserted {
		err = tx.QueryRow(ctx, `
			SELECT id, session_date, provider, config_hash, status,
			       owner_token, lease_until, attempt_count,
			       COALESCE(active_version_id::text,''),
			       COALESCE(published_version_id::text,''), COALESCE(last_error,'')
			FROM swing_universe_build_claims
			WHERE session_date=$1::date AND provider=$2 AND config_hash=$3
			FOR UPDATE`,
			request.SessionDate, request.Provider, request.ConfigHash,
		).Scan(
			&claim.ID, &claim.SessionDate, &claim.Provider, &claim.ConfigHash,
			&claim.Status, &claim.OwnerToken, &claim.LeaseUntil, &claim.AttemptCount,
			&claim.ActiveVersionID, &claim.PublishedVersionID, &claim.LastError,
		)
		if err != nil {
			return nil, false, fmt.Errorf("lock swing universe build claim: %w", err)
		}
		var leaseActive bool
		if err := tx.QueryRow(ctx, `
			SELECT status='running' AND lease_until > clock_timestamp()
			FROM swing_universe_build_claims WHERE id=$1`, claim.ID,
		).Scan(&leaseActive); err != nil {
			return nil, false, fmt.Errorf("check swing universe build lease: %w", err)
		}
		if claim.Status == "completed" || leaseActive {
			if err := tx.Commit(ctx); err != nil {
				return nil, false, err
			}
			return &claim, false, nil
		}
		if claim.Status == "running" && claim.ActiveVersionID != "" {
			_, _ = tx.Exec(ctx, `
				UPDATE swing_universe_versions
				SET status='failed', completed_at=$2, error='build lease expired',
				    quality_status='rejected', quality_reason='build lease expired'
				WHERE id=$1 AND status='building'`, claim.ActiveVersionID, request.StartedAt)
		}
		err = tx.QueryRow(ctx, `
			UPDATE swing_universe_build_claims
			SET status='running', owner_token=uuid_generate_v4(),
			    lease_until=clock_timestamp()+($2*interval '1 second'),
			    attempt_count=attempt_count+1,
			    last_started_at=$3, last_finished_at=NULL,
			    last_error=NULL, active_version_id=NULL, updated_at=$3
			WHERE id=$1
			RETURNING owner_token, lease_until, attempt_count`,
			claim.ID, lease.Seconds(), request.StartedAt,
		).Scan(&claim.OwnerToken, &claim.LeaseUntil, &claim.AttemptCount)
		if err != nil {
			return nil, false, fmt.Errorf("reclaim swing universe build: %w", err)
		}
		claim.Status = "running"
		claim.ActiveVersionID = ""
		claim.LastError = ""
	}

	err = tx.QueryRow(ctx, `
		INSERT INTO swing_universe_versions (
			provider, as_of, captured_at, filters, source_evidence,
			build_claim_id, build_attempt, session_date, config_hash, quality_status
		) VALUES ($1,$2,$2,$3,'{}'::jsonb,$4,$5,$6::date,$7,'pending')
		RETURNING id`,
		request.Provider, request.StartedAt, nonNilJSON(request.ConfigSnapshot),
		claim.ID, claim.AttemptCount, request.SessionDate, request.ConfigHash,
	).Scan(&claim.ActiveVersionID)
	if err != nil {
		return nil, false, fmt.Errorf("create swing universe build attempt: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE swing_universe_build_claims
		SET active_version_id=$2, updated_at=$3 WHERE id=$1`,
		claim.ID, claim.ActiveVersionID, request.StartedAt,
	); err != nil {
		return nil, false, fmt.Errorf("attach swing universe build attempt: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, false, err
	}
	return &claim, true, nil
}

func CompleteSwingUniverseVersion(ctx context.Context, id string, completedAt time.Time) error {
	tag, err := GetDB().Exec(ctx, `
		UPDATE swing_universe_versions v SET
			status='completed', completed_at=$2, error=NULL,
			member_count=(SELECT COUNT(*) FROM swing_universe_members m WHERE m.universe_version_id=v.id),
			eligible_count=(SELECT COUNT(*) FROM swing_universe_members m WHERE m.universe_version_id=v.id AND m.eligible),
			unknown_taxonomy_count=(SELECT COUNT(*) FROM swing_universe_members m
				WHERE m.universe_version_id=v.id AND m.taxonomy_status IN ('unknown','unavailable')),
			stale_taxonomy_count=(SELECT COUNT(*) FROM swing_universe_members m
				WHERE m.universe_version_id=v.id AND m.taxonomy_status='stale')
		WHERE id=$1 AND status='building' AND build_claim_id IS NULL`,
		id, completedAt,
	)
	if err != nil {
		return fmt.Errorf("complete swing universe version: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("complete swing universe version %q: not building", id)
	}
	return nil
}

func FailSwingUniverseVersion(ctx context.Context, id string, failedAt time.Time, cause error) error {
	message := ""
	if cause != nil {
		message = cause.Error()
	}
	tag, err := GetDB().Exec(ctx, `
		UPDATE swing_universe_versions
		SET status='failed', completed_at=$2, error=$3
		WHERE id=$1 AND status='building' AND build_claim_id IS NULL`,
		id, failedAt, nullableString(message),
	)
	if err != nil {
		return fmt.Errorf("fail swing universe version: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("fail swing universe version %q: not building", id)
	}
	return nil
}

func RenewSwingUniverseBuildLease(
	ctx context.Context, claimID, ownerToken string, attempt int,
	now time.Time, lease time.Duration,
) error {
	if lease <= 0 {
		return fmt.Errorf("swing universe build lease must be positive")
	}
	tag, err := GetDB().Exec(ctx, `
		UPDATE swing_universe_build_claims
		SET lease_until=clock_timestamp()+($5*interval '1 second'), updated_at=$4
		WHERE id=$1 AND owner_token=$2::uuid AND attempt_count=$3
		  AND status='running' AND lease_until > clock_timestamp()`,
		claimID, ownerToken, attempt, now, lease.Seconds(),
	)
	if err != nil {
		return fmt.Errorf("renew swing universe build lease: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("renew swing universe build lease: claim is no longer current")
	}
	return nil
}

func UpdateClaimedSwingUniverseMetadata(
	ctx context.Context, claim SwingUniverseBuildClaim, version SwingUniverseVersion,
) error {
	tag, err := GetDB().Exec(ctx, `
		UPDATE swing_universe_versions v
		SET as_of=$5, filters=$6, source_evidence=$7
		FROM swing_universe_build_claims c
		WHERE v.id=$1 AND v.build_claim_id=c.id AND c.id=$2
		  AND c.owner_token=$3::uuid AND c.attempt_count=$4
		  AND c.status='running' AND c.lease_until > clock_timestamp()
		  AND v.status='building'
		  AND v.build_attempt=$4`,
		claim.ActiveVersionID, claim.ID, claim.OwnerToken, claim.AttemptCount,
		version.AsOf, nonNilJSON(version.Filters), nonNilJSON(version.SourceEvidence),
	)
	if err != nil {
		return fmt.Errorf("update claimed swing universe metadata: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("update claimed swing universe metadata: claim is no longer current")
	}
	return nil
}

func CompleteClaimedSwingUniverseVersion(
	ctx context.Context, claim SwingUniverseBuildClaim, completedAt time.Time,
	quality SwingUniverseQuality,
) error {
	if err := validateSwingUniverseQuality(quality); err != nil {
		return err
	}
	tx, err := GetDB().Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	tag, err := tx.Exec(ctx, `
		UPDATE swing_universe_versions v SET
			status='completed', quality_status='approved', quality_reason=NULL,
			completed_at=$5, error=NULL,
			member_count=(SELECT COUNT(*) FROM swing_universe_members m WHERE m.universe_version_id=v.id),
			eligible_count=(SELECT COUNT(*) FROM swing_universe_members m WHERE m.universe_version_id=v.id AND m.eligible),
			unknown_taxonomy_count=(SELECT COUNT(*) FROM swing_universe_members m
				WHERE m.universe_version_id=v.id AND m.taxonomy_status IN ('unknown','unavailable')),
			stale_taxonomy_count=(SELECT COUNT(*) FROM swing_universe_members m
				WHERE m.universe_version_id=v.id AND m.taxonomy_status='stale'),
			evidence_required_count=$6, usable_snapshot_count=$7,
			usable_bar_history_count=$8, coherent_market_data_count=$9,
			fundamentals_required_count=$10, usable_fundamentals_count=$11,
			snapshot_failure_count=$12, bar_failure_count=$13,
			fundamentals_failure_count=$14, provider_failure_count=$15,
			partial_failure_count=$16, market_data_coverage=$17,
			fundamentals_coverage=$18, min_market_data_coverage=$19
		FROM swing_universe_build_claims c
		WHERE v.id=$1 AND v.build_claim_id=c.id AND c.id=$2
		  AND c.owner_token=$3::uuid AND c.attempt_count=$4
		  AND c.status='running' AND c.lease_until > clock_timestamp()
		  AND v.status='building' AND v.build_attempt=$4`,
		claim.ActiveVersionID, claim.ID, claim.OwnerToken, claim.AttemptCount,
		completedAt, quality.EvidenceRequiredCount, quality.UsableSnapshotCount,
		quality.UsableBarHistoryCount, quality.CoherentMarketDataCount,
		quality.FundamentalsRequiredCount, quality.UsableFundamentalsCount,
		quality.SnapshotFailureCount, quality.BarFailureCount,
		quality.FundamentalsFailureCount, quality.ProviderFailureCount,
		quality.PartialFailureCount, quality.MarketDataCoverage,
		quality.FundamentalsCoverage, quality.MinMarketDataCoverage,
	)
	if err != nil {
		return fmt.Errorf("complete claimed swing universe version: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("complete claimed swing universe version: claim is no longer current")
	}
	tag, err = tx.Exec(ctx, `
		UPDATE swing_universe_build_claims
		SET status='completed', published_version_id=$4, active_version_id=$4,
		    last_finished_at=$5, last_error=NULL, updated_at=$5
		WHERE id=$1 AND owner_token=$2::uuid AND attempt_count=$3
		  AND status='running' AND lease_until > clock_timestamp()`,
		claim.ID, claim.OwnerToken, claim.AttemptCount,
		claim.ActiveVersionID, completedAt,
	)
	if err != nil {
		return fmt.Errorf("complete swing universe build claim: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("complete swing universe build claim: claim is no longer current")
	}
	return tx.Commit(ctx)
}

func validateSwingUniverseQuality(quality SwingUniverseQuality) error {
	counts := []int{
		quality.EvidenceRequiredCount, quality.UsableSnapshotCount,
		quality.UsableBarHistoryCount, quality.CoherentMarketDataCount,
		quality.FundamentalsRequiredCount, quality.UsableFundamentalsCount,
		quality.SnapshotFailureCount, quality.BarFailureCount,
		quality.FundamentalsFailureCount, quality.ProviderFailureCount,
		quality.PartialFailureCount,
	}
	for _, count := range counts {
		if count < 0 {
			return fmt.Errorf("swing universe quality counts must be non-negative")
		}
	}
	if quality.UsableSnapshotCount > quality.EvidenceRequiredCount ||
		quality.UsableBarHistoryCount > quality.EvidenceRequiredCount ||
		quality.CoherentMarketDataCount > quality.UsableSnapshotCount ||
		quality.CoherentMarketDataCount > quality.UsableBarHistoryCount {
		return fmt.Errorf("swing universe market-data quality counts are inconsistent")
	}
	if quality.SnapshotFailureCount != quality.EvidenceRequiredCount-quality.UsableSnapshotCount ||
		quality.BarFailureCount != quality.EvidenceRequiredCount-quality.UsableBarHistoryCount ||
		quality.UsableFundamentalsCount > quality.FundamentalsRequiredCount ||
		quality.FundamentalsFailureCount != quality.FundamentalsRequiredCount-quality.UsableFundamentalsCount {
		return fmt.Errorf("swing universe quality success/failure counts are inconsistent")
	}
	minimumProviderFailures := quality.SnapshotFailureCount
	if quality.BarFailureCount > minimumProviderFailures {
		minimumProviderFailures = quality.BarFailureCount
	}
	if quality.ProviderFailureCount < minimumProviderFailures ||
		quality.ProviderFailureCount > quality.EvidenceRequiredCount {
		return fmt.Errorf("swing universe provider failure count is inconsistent")
	}
	if !finiteUnitInterval(quality.MarketDataCoverage) ||
		!finiteUnitInterval(quality.FundamentalsCoverage) ||
		!finiteUnitInterval(quality.MinMarketDataCoverage) ||
		quality.MinMarketDataCoverage == 0 {
		return fmt.Errorf("swing universe quality coverage is invalid")
	}
	expectedMarketCoverage := 1.0
	if quality.EvidenceRequiredCount > 0 {
		expectedMarketCoverage = float64(quality.CoherentMarketDataCount) /
			float64(quality.EvidenceRequiredCount)
	}
	expectedFundamentalsCoverage := 1.0
	if quality.FundamentalsRequiredCount > 0 {
		expectedFundamentalsCoverage = float64(quality.UsableFundamentalsCount) /
			float64(quality.FundamentalsRequiredCount)
	}
	if math.Abs(quality.MarketDataCoverage-expectedMarketCoverage) > 1e-12 ||
		math.Abs(quality.FundamentalsCoverage-expectedFundamentalsCoverage) > 1e-12 {
		return fmt.Errorf("swing universe quality coverage does not match its counts")
	}
	if quality.MarketDataCoverage < quality.MinMarketDataCoverage {
		return fmt.Errorf("swing universe market-data coverage is below its minimum")
	}
	return nil
}

func finiteUnitInterval(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0 && value <= 1
}

func FailClaimedSwingUniverseVersion(
	ctx context.Context, claim SwingUniverseBuildClaim, failedAt time.Time,
	quality SwingUniverseQuality, cause error,
) error {
	message := ""
	if cause != nil {
		message = cause.Error()
	}
	tx, err := GetDB().Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	tag, err := tx.Exec(ctx, `
		UPDATE swing_universe_versions v SET
			status='failed', quality_status='rejected', quality_reason=$6,
			completed_at=$5, error=$6,
			member_count=(SELECT COUNT(*) FROM swing_universe_members m WHERE m.universe_version_id=v.id),
			eligible_count=(SELECT COUNT(*) FROM swing_universe_members m WHERE m.universe_version_id=v.id AND m.eligible),
			evidence_required_count=$7, usable_snapshot_count=$8,
			usable_bar_history_count=$9, coherent_market_data_count=$10,
			fundamentals_required_count=$11, usable_fundamentals_count=$12,
			snapshot_failure_count=$13, bar_failure_count=$14,
			fundamentals_failure_count=$15, provider_failure_count=$16,
			partial_failure_count=$17, market_data_coverage=$18,
			fundamentals_coverage=$19, min_market_data_coverage=$20
		FROM swing_universe_build_claims c
		WHERE v.id=$1 AND v.build_claim_id=c.id AND c.id=$2
		  AND c.owner_token=$3::uuid AND c.attempt_count=$4
		  AND c.status='running' AND c.lease_until > clock_timestamp()
		  AND v.status='building' AND v.build_attempt=$4`,
		claim.ActiveVersionID, claim.ID, claim.OwnerToken, claim.AttemptCount,
		failedAt, nullableString(message), quality.EvidenceRequiredCount,
		quality.UsableSnapshotCount, quality.UsableBarHistoryCount,
		quality.CoherentMarketDataCount, quality.FundamentalsRequiredCount,
		quality.UsableFundamentalsCount, quality.SnapshotFailureCount,
		quality.BarFailureCount, quality.FundamentalsFailureCount,
		quality.ProviderFailureCount, quality.PartialFailureCount,
		quality.MarketDataCoverage, quality.FundamentalsCoverage,
		quality.MinMarketDataCoverage,
	)
	if err != nil {
		return fmt.Errorf("fail claimed swing universe version: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("fail claimed swing universe version: claim is no longer current")
	}
	tag, err = tx.Exec(ctx, `
		UPDATE swing_universe_build_claims
		SET status='failed', last_finished_at=$4, last_error=$5,
		    updated_at=$4
		WHERE id=$1 AND owner_token=$2::uuid AND attempt_count=$3
		  AND status='running' AND lease_until > clock_timestamp()`,
		claim.ID, claim.OwnerToken, claim.AttemptCount, failedAt,
		nullableString(message),
	)
	if err != nil {
		return fmt.Errorf("fail swing universe build claim: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("fail swing universe build claim: claim is no longer current")
	}
	return tx.Commit(ctx)
}

func ReplaceSwingUniverseMembers(
	ctx context.Context, versionID string, members []SwingUniverseMember,
) error {
	tx, err := GetDB().Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	var status string
	var unclaimed bool
	if err := tx.QueryRow(ctx,
		`SELECT status, build_claim_id IS NULL FROM swing_universe_versions WHERE id=$1 FOR UPDATE`,
		versionID,
	).Scan(&status, &unclaimed); err != nil {
		return fmt.Errorf("lock swing universe version: %w", err)
	}
	if status != "building" {
		return fmt.Errorf("replace swing universe members: version is %s", status)
	}
	if !unclaimed {
		return fmt.Errorf("replace swing universe members: claimed versions require owner fencing")
	}
	if _, err := tx.Exec(ctx,
		`DELETE FROM swing_universe_members WHERE universe_version_id=$1`,
		versionID,
	); err != nil {
		return fmt.Errorf("clear swing universe members: %w", err)
	}
	if err := insertSwingUniverseMembers(ctx, tx, versionID, members); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ReplaceClaimedSwingUniverseMembers writes only while this exact owner and
// attempt still hold an unexpired lease. This keeps a reclaimed worker from
// mutating even its unpublished attempt after ownership has moved on.
func ReplaceClaimedSwingUniverseMembers(
	ctx context.Context, claim SwingUniverseBuildClaim, members []SwingUniverseMember,
) error {
	tx, err := GetDB().Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var valid bool
	err = tx.QueryRow(ctx, `
		SELECT true FROM swing_universe_build_claims c
		JOIN swing_universe_versions v ON v.id=c.active_version_id
		WHERE c.id=$1 AND c.owner_token=$2::uuid AND c.attempt_count=$3
		  AND c.status='running' AND c.lease_until > clock_timestamp()
		  AND v.id=$4 AND v.build_claim_id=c.id
		  AND v.build_attempt=$3 AND v.status='building'
		FOR UPDATE OF c, v`,
		claim.ID, claim.OwnerToken, claim.AttemptCount, claim.ActiveVersionID,
	).Scan(&valid)
	if err == pgx.ErrNoRows {
		return fmt.Errorf("replace claimed swing universe members: claim is no longer current")
	}
	if err != nil {
		return fmt.Errorf("validate claimed swing universe member write: %w", err)
	}
	if !valid {
		return fmt.Errorf("replace claimed swing universe members: claim is no longer current")
	}
	if _, err := tx.Exec(ctx,
		`DELETE FROM swing_universe_members WHERE universe_version_id=$1`,
		claim.ActiveVersionID,
	); err != nil {
		return fmt.Errorf("clear claimed swing universe members: %w", err)
	}
	if err := insertSwingUniverseMembers(ctx, tx, claim.ActiveVersionID, members); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func insertSwingUniverseMembers(
	ctx context.Context, tx pgx.Tx, versionID string, members []SwingUniverseMember,
) error {
	for _, member := range members {
		if member.Ticker == "" {
			return fmt.Errorf("swing universe member ticker is required")
		}
		sector, industry, taxonomyStatus := normalizeTaxonomy(
			member.Sector, member.Industry, member.TaxonomyStatus,
		)
		_, err := tx.Exec(ctx, `
			INSERT INTO swing_universe_members (
				universe_version_id, ticker, eligible, removal_stage, failure_stage,
				failure_reason, asset_id, name, security_type, exchange, active, tradable,
				price, market_cap_mm,
				dollar_volume, spread_bps, data_available, sector, industry,
				taxonomy_status, taxonomy_as_of, taxonomy_provider,
				taxonomy_evidence, source_evidence
			) VALUES (
				$1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,
				$21,$22,$23,$24
			)`,
			versionID, member.Ticker, member.Eligible,
			nullableString(member.RemovalStage), nullableString(member.FailureStage),
			nullableString(member.FailureReason), nullableString(member.AssetID),
			nullableString(member.Name), nullableString(member.SecurityType),
			nullableString(member.Exchange), member.Active, member.Tradable,
			member.Price, member.MarketCapMM,
			member.DollarVolume, member.SpreadBPS, member.DataAvailable,
			sector, industry, taxonomyStatus, member.TaxonomyAsOf,
			nullableString(member.TaxonomyProvider),
			nonNilJSON(member.TaxonomyEvidence), nonNilJSON(member.SourceEvidence),
		)
		if err != nil {
			return fmt.Errorf("insert swing universe member %q: %w", member.Ticker, err)
		}
	}
	return nil
}

func ListEligibleSwingUniverseMembers(
	ctx context.Context, versionID string,
) ([]SwingUniverseMember, error) {
	return ListSwingUniverseMembers(ctx, versionID, true)
}

// GetEligibleSwingUniverseMemberAsOf returns the immutable taxonomy evidence
// for one eligible ticker in a completed universe that was available at asOf.
// Missing rows are returned as (nil, nil); callers must leave candidates
// uncapped when point-in-time taxonomy is unavailable.
func GetEligibleSwingUniverseMemberAsOf(
	ctx context.Context, versionID, ticker string, asOf time.Time,
) (*SwingUniverseMember, error) {
	if strings.TrimSpace(versionID) == "" || strings.TrimSpace(ticker) == "" {
		return nil, nil
	}
	row := GetDB().QueryRow(ctx, `
		SELECT m.universe_version_id, m.ticker, m.eligible, COALESCE(m.removal_stage,''),
		       COALESCE(m.failure_stage,''), COALESCE(m.failure_reason,''),
		       COALESCE(m.asset_id,''), COALESCE(m.name,''),
		       COALESCE(m.security_type,''), COALESCE(m.exchange,''), m.active, m.tradable,
		       m.price, m.market_cap_mm, m.dollar_volume, m.spread_bps, m.data_available,
		       m.sector, m.industry, m.taxonomy_status, m.taxonomy_as_of,
		       COALESCE(m.taxonomy_provider,''), m.taxonomy_evidence, m.source_evidence
		FROM swing_universe_members m
		JOIN swing_universe_versions v ON v.id=m.universe_version_id
		WHERE m.universe_version_id=$1
		  AND UPPER(m.ticker)=UPPER($2)
		  AND m.eligible
		  AND v.status='completed'
		  AND v.quality_status='approved'
		  AND v.as_of <= $3
		  AND v.captured_at <= $3
		  AND v.completed_at <= $3
		  AND m.taxonomy_as_of IS NOT NULL
		  AND m.taxonomy_as_of <= $3`,
		versionID, ticker, asOf,
	)
	member, err := scanSwingUniverseMember(row)
	if err != nil {
		if IsNoRows(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("get eligible swing universe member %q as of: %w", ticker, err)
	}
	return &member, nil
}

// ListSwingUniverseMembers enumerates the complete immutable asset-master
// snapshot, but only after its parent version completed successfully.
// eligibleOnly selects the scanner denominator.
func ListSwingUniverseMembers(
	ctx context.Context, versionID string, eligibleOnly bool,
) ([]SwingUniverseMember, error) {
	return listSwingUniverseMembers(ctx, versionID, eligibleOnly, true)
}

// ListSwingUniverseMembersForAudit is the explicit forensic path for inspecting
// partial members from building or failed versions. Trading and research
// consumers must use ListSwingUniverseMembers or ListEligibleSwingUniverseMembers.
func ListSwingUniverseMembersForAudit(
	ctx context.Context, versionID string,
) ([]SwingUniverseMember, error) {
	return listSwingUniverseMembers(ctx, versionID, false, false)
}

func listSwingUniverseMembers(
	ctx context.Context, versionID string, eligibleOnly, completedOnly bool,
) ([]SwingUniverseMember, error) {
	eligiblePredicate := ""
	if eligibleOnly {
		eligiblePredicate = " AND m.eligible"
	}
	completedPredicate := ""
	if completedOnly {
		completedPredicate = " AND v.status='completed' AND v.quality_status='approved'"
	}
	rows, err := GetDB().Query(ctx, `
		SELECT m.universe_version_id, m.ticker, m.eligible, COALESCE(m.removal_stage,''),
		       COALESCE(m.failure_stage,''), COALESCE(m.failure_reason,''),
		       COALESCE(m.asset_id,''), COALESCE(m.name,''),
		       COALESCE(m.security_type,''), COALESCE(m.exchange,''), m.active, m.tradable, m.price,
		       m.market_cap_mm, m.dollar_volume, m.spread_bps, m.data_available,
		       m.sector, m.industry, m.taxonomy_status, m.taxonomy_as_of,
		       COALESCE(m.taxonomy_provider,''), m.taxonomy_evidence, m.source_evidence
		FROM swing_universe_members m
		JOIN swing_universe_versions v ON v.id=m.universe_version_id
		WHERE m.universe_version_id=$1`+completedPredicate+eligiblePredicate+`
		ORDER BY m.ticker`, versionID,
	)
	if err != nil {
		return nil, fmt.Errorf("list swing universe members: %w", err)
	}
	defer rows.Close()
	var members []SwingUniverseMember
	for rows.Next() {
		member, err := scanSwingUniverseMember(rows)
		if err != nil {
			return nil, err
		}
		members = append(members, member)
	}
	return members, rows.Err()
}

// GetCompletedSwingUniverseVersionByIDAsOf validates exact scanner lineage.
// The version must have been effective, captured, and completed by asOf; an
// invalid, unfinished, failed, or future version returns nil without fallback.
func GetCompletedSwingUniverseVersionByIDAsOf(
	ctx context.Context, versionID string, asOf time.Time,
) (*SwingUniverseVersion, error) {
	if strings.TrimSpace(versionID) == "" {
		return nil, nil
	}
	row := GetDB().QueryRow(ctx, `
		SELECT id, provider, as_of, captured_at, completed_at, status,
		       COALESCE(error,''), filters, source_evidence, member_count,
		       eligible_count, unknown_taxonomy_count, stale_taxonomy_count,
		       COALESCE(build_claim_id::text,''), COALESCE(build_attempt,0),
		       session_date, COALESCE(config_hash,''), quality_status,
		       COALESCE(quality_reason,''), evidence_required_count,
		       usable_snapshot_count, usable_bar_history_count,
		       coherent_market_data_count, fundamentals_required_count,
		       usable_fundamentals_count, snapshot_failure_count,
		       bar_failure_count, fundamentals_failure_count,
		       provider_failure_count, partial_failure_count,
		       market_data_coverage, fundamentals_coverage,
		       min_market_data_coverage
		FROM swing_universe_versions
		WHERE id=$1
		  AND status='completed'
		  AND quality_status='approved'
		  AND as_of <= $2
		  AND captured_at <= $2
		  AND completed_at <= $2`,
		versionID, asOf,
	)
	version, err := scanSwingUniverseVersion(row)
	if err != nil {
		if IsNoRows(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("get completed swing universe %q as of: %w", versionID, err)
	}
	return version, nil
}

func GetLatestCompletedSwingUniverseVersionAsOf(
	ctx context.Context, asOf time.Time,
) (*SwingUniverseVersion, error) {
	row := GetDB().QueryRow(ctx, `
		SELECT id, provider, as_of, captured_at, completed_at, status,
		       COALESCE(error,''), filters, source_evidence, member_count,
		       eligible_count, unknown_taxonomy_count, stale_taxonomy_count,
		       COALESCE(build_claim_id::text,''), COALESCE(build_attempt,0),
		       session_date, COALESCE(config_hash,''), quality_status,
		       COALESCE(quality_reason,''), evidence_required_count,
		       usable_snapshot_count, usable_bar_history_count,
		       coherent_market_data_count, fundamentals_required_count,
		       usable_fundamentals_count, snapshot_failure_count,
		       bar_failure_count, fundamentals_failure_count,
		       provider_failure_count, partial_failure_count,
		       market_data_coverage, fundamentals_coverage,
		       min_market_data_coverage
		FROM swing_universe_versions
		WHERE status='completed'
		  AND quality_status='approved'
		  AND as_of <= $1
		  AND captured_at <= $1
		  AND completed_at <= $1
		ORDER BY as_of DESC, captured_at DESC
		LIMIT 1`,
		asOf,
	)
	version, err := scanSwingUniverseVersion(row)
	if err != nil {
		if IsNoRows(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("get completed swing universe as of: %w", err)
	}
	return version, nil
}

// GetUsableSwingUniverseVersion returns only a version that both existed at the
// decision time and remains inside the caller's cache TTL. It never falls
// forward to a version captured or completed later.
func GetUsableSwingUniverseVersion(
	ctx context.Context, asOf time.Time, maxAge time.Duration,
) (*SwingUniverseVersion, error) {
	version, err := GetLatestCompletedSwingUniverseVersionAsOf(ctx, asOf)
	if err != nil || version == nil {
		return version, err
	}
	if maxAge > 0 && asOf.Sub(version.AsOf) > maxAge {
		return nil, nil
	}
	return version, nil
}

// GetApprovedSwingUniverseForSessionAsOf resolves the exact quality-approved
// producer artifact for one market session. After-close consumers use this
// instead of silently falling back to a previous session's universe.
func GetApprovedSwingUniverseForSessionAsOf(
	ctx context.Context, sessionDate, asOf time.Time, provider, configHash string,
) (*SwingUniverseVersion, error) {
	row := GetDB().QueryRow(ctx, `
		SELECT id, provider, as_of, captured_at, completed_at, status,
		       COALESCE(error,''), filters, source_evidence, member_count,
		       eligible_count, unknown_taxonomy_count, stale_taxonomy_count,
		       COALESCE(build_claim_id::text,''), COALESCE(build_attempt,0),
		       session_date, COALESCE(config_hash,''), quality_status,
		       COALESCE(quality_reason,''), evidence_required_count,
		       usable_snapshot_count, usable_bar_history_count,
		       coherent_market_data_count, fundamentals_required_count,
		       usable_fundamentals_count, snapshot_failure_count,
		       bar_failure_count, fundamentals_failure_count,
		       provider_failure_count, partial_failure_count,
		       market_data_coverage, fundamentals_coverage,
		       min_market_data_coverage
		FROM swing_universe_versions
		WHERE session_date=$1::date AND provider=$2 AND config_hash=$3
		  AND status='completed' AND quality_status='approved'
		  AND as_of <= $4 AND captured_at <= $4 AND completed_at <= $4
		ORDER BY completed_at DESC, id DESC LIMIT 1`,
		sessionDate, provider, configHash, asOf,
	)
	version, err := scanSwingUniverseVersion(row)
	if err != nil {
		if IsNoRows(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("get approved swing universe for session: %w", err)
	}
	return version, nil
}

// ListIndustrySwingUniverseMembers fails closed on unknown or stale
// classification. This is the safe entry point for industry-level scanners;
// the general eligible list intentionally retains unknown taxonomy names for
// non-industry strategies.
func ListIndustrySwingUniverseMembers(
	ctx context.Context,
	versionID, industry string,
	asOf time.Time,
	maxAge time.Duration,
) ([]SwingUniverseMember, error) {
	if strings.TrimSpace(industry) == "" {
		return nil, fmt.Errorf("industry is required")
	}
	rows, err := GetDB().Query(ctx, `
		SELECT universe_version_id, ticker, eligible, COALESCE(removal_stage,''),
		       COALESCE(failure_stage,''), COALESCE(failure_reason,''),
		       COALESCE(asset_id,''), COALESCE(name,''),
		       COALESCE(security_type,''), COALESCE(exchange,''), active, tradable, price,
		       market_cap_mm, dollar_volume, spread_bps, data_available,
		       sector, industry, taxonomy_status, taxonomy_as_of,
		       COALESCE(taxonomy_provider,''), taxonomy_evidence, m.source_evidence
		FROM swing_universe_members m
		JOIN swing_universe_versions v ON v.id=m.universe_version_id
		WHERE m.universe_version_id=$1
		  AND v.status='completed'
		  AND v.quality_status='approved'
		  AND v.as_of <= $3
		  AND v.captured_at <= $3
		  AND v.completed_at <= $3
		  AND m.eligible
		  AND m.taxonomy_status='known'
		  AND UPPER(m.industry)=UPPER($2)
		  AND m.taxonomy_as_of IS NOT NULL
		  AND m.taxonomy_as_of <= $3
		  AND ($4::interval IS NULL OR m.taxonomy_as_of >= $3 - $4::interval)
		ORDER BY m.ticker`,
		versionID, industry, asOf, nullableInterval(maxAge),
	)
	if err != nil {
		return nil, fmt.Errorf("list industry swing universe members: %w", err)
	}
	defer rows.Close()
	var members []SwingUniverseMember
	for rows.Next() {
		member, err := scanSwingUniverseMember(rows)
		if err != nil {
			return nil, err
		}
		members = append(members, member)
	}
	return members, rows.Err()
}

func InsertCompanyTaxonomyObservation(
	ctx context.Context, observation CompanyTaxonomyObservation,
) (string, error) {
	if observation.Ticker == "" || observation.Provider == "" {
		return "", fmt.Errorf("taxonomy ticker and provider are required")
	}
	if observation.EffectiveAt.IsZero() || observation.ObservedAt.IsZero() {
		return "", fmt.Errorf("taxonomy effective-at and observed-at are required")
	}
	sector, industry, status := normalizeTaxonomy(
		observation.Sector, observation.Industry, observation.Status,
	)
	if status == "stale" {
		return "", fmt.Errorf("taxonomy observations cannot be inserted as stale")
	}
	var id string
	err := GetDB().QueryRow(ctx, `
		INSERT INTO company_taxonomy_observations (
			ticker, provider, effective_at, observed_at, sector, industry, status, evidence
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		RETURNING id`,
		observation.Ticker, observation.Provider, observation.EffectiveAt,
		observation.ObservedAt, sector, industry, status, nonNilJSON(observation.Evidence),
	).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("insert company taxonomy observation: %w", err)
	}
	return id, nil
}

func GetCompanyTaxonomyAsOf(
	ctx context.Context, ticker string, asOf time.Time, maxAge time.Duration,
) (*CompanyTaxonomyObservation, error) {
	row := GetDB().QueryRow(ctx, `
		SELECT id, ticker, provider, effective_at, observed_at, sector, industry,
		       status, evidence
		FROM company_taxonomy_observations
		WHERE UPPER(ticker)=UPPER($1)
		  AND effective_at <= $2
		  AND observed_at <= $2
		ORDER BY effective_at DESC, observed_at DESC
		LIMIT 1`,
		ticker, asOf,
	)
	var observation CompanyTaxonomyObservation
	err := row.Scan(
		&observation.ID, &observation.Ticker, &observation.Provider,
		&observation.EffectiveAt, &observation.ObservedAt, &observation.Sector,
		&observation.Industry, &observation.Status, &observation.Evidence,
	)
	if err != nil {
		if IsNoRows(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("get company taxonomy as of: %w", err)
	}
	observation.Status = taxonomyStatusAt(
		observation.Status, observation.ObservedAt, asOf, maxAge,
	)
	return &observation, nil
}

func taxonomyStatusAt(
	status string, observedAt, asOf time.Time, maxAge time.Duration,
) string {
	if status != "known" || maxAge <= 0 {
		return status
	}
	if asOf.Sub(observedAt) > maxAge {
		return "stale"
	}
	return status
}

func normalizeTaxonomy(sector, industry, status string) (string, string, string) {
	if sector == "" {
		sector = UnknownTaxonomy
	}
	if industry == "" {
		industry = UnknownTaxonomy
	}
	switch status {
	case "known", "unknown", "stale", "unavailable":
	default:
		status = "unknown"
	}
	// Some providers (including Finnhub) expose a reliable broad industry but
	// not a separate sector hierarchy. Industry scanners may use that label;
	// they still fail closed when the industry itself is unknown.
	if status == "known" && industry == UnknownTaxonomy {
		status = "unknown"
	}
	if status == "unknown" || status == "unavailable" {
		sector = UnknownTaxonomy
		industry = UnknownTaxonomy
	}
	return sector, industry, status
}

func nonNilJSON(value json.RawMessage) json.RawMessage {
	if len(value) == 0 {
		return json.RawMessage(`{}`)
	}
	return value
}

func nullableInterval(value time.Duration) any {
	if value <= 0 {
		return nil
	}
	return value.String()
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanSwingUniverseVersion(row rowScanner) (*SwingUniverseVersion, error) {
	var version SwingUniverseVersion
	err := row.Scan(
		&version.ID, &version.Provider, &version.AsOf, &version.CapturedAt,
		&version.CompletedAt, &version.Status, &version.Error, &version.Filters,
		&version.SourceEvidence, &version.MemberCount, &version.EligibleCount,
		&version.UnknownTaxonomyCount, &version.StaleTaxonomyCount,
		&version.BuildClaimID, &version.BuildAttempt, &version.SessionDate,
		&version.ConfigHash, &version.QualityStatus, &version.QualityReason,
		&version.Quality.EvidenceRequiredCount,
		&version.Quality.UsableSnapshotCount,
		&version.Quality.UsableBarHistoryCount,
		&version.Quality.CoherentMarketDataCount,
		&version.Quality.FundamentalsRequiredCount,
		&version.Quality.UsableFundamentalsCount,
		&version.Quality.SnapshotFailureCount,
		&version.Quality.BarFailureCount,
		&version.Quality.FundamentalsFailureCount,
		&version.Quality.ProviderFailureCount,
		&version.Quality.PartialFailureCount,
		&version.Quality.MarketDataCoverage,
		&version.Quality.FundamentalsCoverage,
		&version.Quality.MinMarketDataCoverage,
	)
	if err != nil {
		return nil, err
	}
	return &version, nil
}

// GetLatestApprovedSwingUniverseForSessionAsOf returns only an approved build
// for the exact exchange session. Friday therefore remains valid on Monday,
// while an older Thursday build cannot hide a missing Friday artifact.
func GetLatestApprovedSwingUniverseForSessionAsOf(
	ctx context.Context, sessionDate, asOf time.Time,
) (*SwingUniverseVersion, error) {
	row := GetDB().QueryRow(ctx, `
		SELECT id, provider, as_of, captured_at, completed_at, status,
		       COALESCE(error,''), filters, source_evidence, member_count,
		       eligible_count, unknown_taxonomy_count, stale_taxonomy_count,
		       COALESCE(build_claim_id::text,''), COALESCE(build_attempt,0),
		       session_date, COALESCE(config_hash,''), quality_status,
		       COALESCE(quality_reason,''), evidence_required_count,
		       usable_snapshot_count, usable_bar_history_count,
		       coherent_market_data_count, fundamentals_required_count,
		       usable_fundamentals_count, snapshot_failure_count,
		       bar_failure_count, fundamentals_failure_count,
		       provider_failure_count, partial_failure_count,
		       market_data_coverage, fundamentals_coverage,
		       min_market_data_coverage
		FROM swing_universe_versions
		WHERE session_date=$1::date
		  AND status='completed' AND quality_status='approved'
		  AND as_of <= $2 AND captured_at <= $2 AND completed_at <= $2
		ORDER BY completed_at DESC, id DESC LIMIT 1`,
		sessionDate, asOf,
	)
	version, err := scanSwingUniverseVersion(row)
	if err != nil {
		if IsNoRows(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("get approved swing universe for exact session: %w", err)
	}
	return version, nil
}

func scanSwingUniverseMember(row rowScanner) (SwingUniverseMember, error) {
	var member SwingUniverseMember
	err := row.Scan(
		&member.UniverseVersionID, &member.Ticker, &member.Eligible,
		&member.RemovalStage, &member.FailureStage, &member.FailureReason,
		&member.AssetID, &member.Name, &member.SecurityType, &member.Exchange,
		&member.Active, &member.Tradable, &member.Price, &member.MarketCapMM,
		&member.DollarVolume, &member.SpreadBPS, &member.DataAvailable,
		&member.Sector, &member.Industry, &member.TaxonomyStatus,
		&member.TaxonomyAsOf, &member.TaxonomyProvider,
		&member.TaxonomyEvidence, &member.SourceEvidence,
	)
	if err != nil {
		if err == pgx.ErrNoRows {
			return member, err
		}
		return member, fmt.Errorf("scan swing universe member: %w", err)
	}
	return member, nil
}
