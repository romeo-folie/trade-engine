package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

const UnknownTaxonomy = "Unknown"

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
	err := GetDB().QueryRow(ctx, `
		INSERT INTO swing_universe_versions (
			provider, as_of, captured_at, filters, source_evidence
		) VALUES ($1,$2,$3,$4,$5)
		RETURNING id`,
		version.Provider, version.AsOf, version.CapturedAt,
		nonNilJSON(version.Filters), nonNilJSON(version.SourceEvidence),
	).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("begin swing universe version: %w", err)
	}
	return id, nil
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
		WHERE id=$1 AND status='building'`,
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
		WHERE id=$1 AND status='building'`,
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

func ReplaceSwingUniverseMembers(
	ctx context.Context, versionID string, members []SwingUniverseMember,
) error {
	tx, err := GetDB().Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	var status string
	if err := tx.QueryRow(ctx,
		`SELECT status FROM swing_universe_versions WHERE id=$1 FOR UPDATE`,
		versionID,
	).Scan(&status); err != nil {
		return fmt.Errorf("lock swing universe version: %w", err)
	}
	if status != "building" {
		return fmt.Errorf("replace swing universe members: version is %s", status)
	}
	if _, err := tx.Exec(ctx,
		`DELETE FROM swing_universe_members WHERE universe_version_id=$1`,
		versionID,
	); err != nil {
		return fmt.Errorf("clear swing universe members: %w", err)
	}
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
	return tx.Commit(ctx)
}

func ListEligibleSwingUniverseMembers(
	ctx context.Context, versionID string,
) ([]SwingUniverseMember, error) {
	return ListSwingUniverseMembers(ctx, versionID, true)
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
		completedPredicate = " AND v.status='completed'"
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
		       eligible_count, unknown_taxonomy_count, stale_taxonomy_count
		FROM swing_universe_versions
		WHERE id=$1
		  AND status='completed'
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
		       eligible_count, unknown_taxonomy_count, stale_taxonomy_count
		FROM swing_universe_versions
		WHERE status='completed'
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
	)
	if err != nil {
		return nil, err
	}
	return &version, nil
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
