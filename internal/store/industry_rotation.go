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

const industryRotationCompletionLock = "industry-rotation-completion"

type IndustryRotationRun struct {
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
	MarketBenchmark        string
	BenchmarkEvidence      json.RawMessage
	HistoryProvider        string
	Error                  string
	EligibleGroupCount     int
	EvaluatedGroupCount    int
	PotentGroupCount       int
	FailedGroupCount       int
	EligibleMemberCount    int
	EvaluatedMemberCount   int
	SelectedMemberCount    int
	FailedMemberCount      int
	AddedGroupCount        int
	RemovedGroupCount      int
	PersistentGroupCount   int
	GroupCoverage          float64
	MemberCoverage         float64
	PreviousCompletedRunID string
}

type IndustryRotationGroup struct {
	RunID                       string
	GroupType                   string
	GroupKey                    string
	Sector                      string
	Industry                    string
	Benchmark                   string
	Outcome                     string
	Archetype                   string
	FailureStage                string
	FailureReason               string
	EligibleMemberCount         int
	EvaluatedMemberCount        int
	FreshTaxonomyCount          int
	MemberCoverage              float64
	FreshTaxonomyCoverage       float64
	EqualWeightReturn1          *float64
	EqualWeightReturn5          *float64
	EqualWeightReturn20         *float64
	CapWeightReturn1            *float64
	CapWeightReturn5            *float64
	CapWeightReturn20           *float64
	MedianMemberReturn1         *float64
	MedianMemberReturn5         *float64
	MedianMemberReturn20        *float64
	MarketRelativeReturn1       *float64
	MarketRelativeReturn5       *float64
	MarketRelativeReturn20      *float64
	SectorRelativeReturn1       *float64
	SectorRelativeReturn5       *float64
	SectorRelativeReturn20      *float64
	AdvanceShare                *float64
	AdvanceDecline              *float64
	UpVolumeShare               *float64
	PctAboveEMA20               *float64
	PctAboveEMA50               *float64
	PctAt20DayHigh              *float64
	PctAt52WeekHigh             *float64
	MedianRVOL20                *float64
	MedianDollarVolumeExpansion *float64
	BreadthAcceleration         *float64
	CapEqualDivergence          *float64
	PersistenceSessions         int
	PotencyScore                *float64
	ScorePercentile             *float64
	Rank                        *int
	FeatureCoverage             *float64
	GroupEvidence               json.RawMessage
	Components                  json.RawMessage
	WasPotentPrevious           bool
	PotencyStreak               int
	PotentSince                 *time.Time
}

type IndustryRotationMember struct {
	RunID               string
	UniverseVersionID   string
	GroupType           string
	GroupKey            string
	Ticker              string
	SourceTag           string
	Outcome             string
	FailureStage        string
	FailureReason       string
	HistoryFirstAt      *time.Time
	HistoryLastAt       *time.Time
	HistorySessions     int
	BarSetHash          string
	ClosePrice          *float64
	Return1             *float64
	Return5             *float64
	Return20            *float64
	StockVsIndustryRS   *float64
	StockVsSectorRS     *float64
	StockVsMarketRS     *float64
	LiquidityScore      *float64
	TrendQualityScore   *float64
	MemberScore         *float64
	MemberRank          *int
	FeatureCoverage     *float64
	SourceEvidence      json.RawMessage
	Components          json.RawMessage
	PromotionStatus     string
	PromotionClaimToken string
	PromotionClaimedAt  *time.Time
	ScannerRunID        string
	PromotedAt          *time.Time
	PromotionReason     string
}

type IndustryRotationTransition struct {
	RunID         string
	GroupType     string
	GroupKey      string
	Event         string
	PreviousRunID string
	PreviousScore *float64
	CurrentScore  *float64
	PreviousRank  *int
	CurrentRank   *int
	Reason        string
}

type IndustryRotationCandidate struct {
	RunID               string
	UniverseVersionID   string
	UniverseAsOf        time.Time
	GroupType           string
	GroupKey            string
	GroupArchetype      string
	GroupScore          float64
	GroupRank           int
	Ticker              string
	MemberScore         float64
	MemberRank          int
	Price               float64
	Return20            float64
	StockVsIndustryRS   float64
	StockVsSectorRS     float64
	StockVsMarketRS     float64
	MarketCapMM         float64
	DollarVolume        float64
	Sector              string
	Industry            string
	TaxonomyStatus      string
	TaxonomyAsOf        *time.Time
	TaxonomyProvider    string
	TaxonomyEvidence    json.RawMessage
	PromotionClaimToken string
}

// ClaimIndustryRotationRun creates or reclaims one deterministic run identity.
// Active leases and completed runs are immutable to concurrent engine processes.
func ClaimIndustryRotationRun(
	ctx context.Context, run IndustryRotationRun, lease time.Duration,
) (*IndustryRotationRun, bool, error) {
	if run.SessionDate.IsZero() || run.AsOf.IsZero() || run.DecisionAt.IsZero() ||
		run.StartedAt.IsZero() ||
		run.UniverseVersionID == "" || run.ConfigHash == "" ||
		run.FeatureVersion == "" || strings.TrimSpace(run.MarketBenchmark) == "" ||
		strings.TrimSpace(run.HistoryProvider) == "" {
		return nil, false, fmt.Errorf("industry rotation run identity is incomplete")
	}
	if lease <= 0 {
		lease = time.Hour
	}
	row := GetDB().QueryRow(ctx, `
		INSERT INTO industry_rotation_runs (
			session_date, as_of, decision_at, universe_version_id, started_at,
			lease_until, config_hash, config_snapshot, feature_version,
			market_benchmark, benchmark_evidence, history_provider
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
		ON CONFLICT (session_date, universe_version_id, config_hash) DO UPDATE SET
			status='running', as_of=EXCLUDED.as_of, decision_at=EXCLUDED.decision_at,
			started_at=EXCLUDED.started_at, lease_until=EXCLUDED.lease_until,
			attempt_count=industry_rotation_runs.attempt_count+1,
			config_snapshot=EXCLUDED.config_snapshot,
			feature_version=EXCLUDED.feature_version,
			market_benchmark=EXCLUDED.market_benchmark,
			benchmark_evidence=EXCLUDED.benchmark_evidence,
			history_provider=EXCLUDED.history_provider,
			completed_at=NULL, error=NULL
		WHERE industry_rotation_runs.status='failed'
		   OR (industry_rotation_runs.status='running'
		       AND industry_rotation_runs.lease_until <= EXCLUDED.started_at)
		RETURNING `+industryRotationRunColumns(), run.SessionDate, run.AsOf,
		run.DecisionAt, run.UniverseVersionID, run.StartedAt,
		run.StartedAt.Add(lease), run.ConfigHash, nonNilJSON(run.ConfigSnapshot),
		run.FeatureVersion, strings.ToUpper(strings.TrimSpace(run.MarketBenchmark)),
		nonNilJSON(run.BenchmarkEvidence), run.HistoryProvider,
	)
	claimed, err := scanIndustryRotationRun(row)
	if err == nil {
		return claimed, true, nil
	}
	if IsNoRows(err) {
		existing, lookupErr := scanIndustryRotationRun(GetDB().QueryRow(ctx, `
			SELECT `+industryRotationRunColumns()+`
			FROM industry_rotation_runs
			WHERE session_date=$1 AND universe_version_id=$2 AND config_hash=$3`,
			run.SessionDate, run.UniverseVersionID, run.ConfigHash,
		))
		if lookupErr != nil {
			return nil, false, fmt.Errorf("load existing industry rotation run: %w", lookupErr)
		}
		return existing, false, nil
	}
	return nil, false, fmt.Errorf("claim industry rotation run: %w", err)
}

func FailIndustryRotationRun(
	ctx context.Context, id string, attempt int, failedAt time.Time, cause error,
) error {
	message := ""
	if cause != nil {
		message = cause.Error()
	}
	tag, err := GetDB().Exec(ctx, `
		UPDATE industry_rotation_runs
		SET status='failed', completed_at=$2, error=$3
		WHERE id=$1 AND status='running' AND attempt_count=$4`,
		id, failedAt, nullableString(message), attempt,
	)
	if err != nil {
		return fmt.Errorf("fail industry rotation run: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("fail industry rotation run %q: claim is no longer current", id)
	}
	return nil
}

// CompleteIndustryRotationRun atomically publishes all group/member evidence
// and lineage. Readers can never observe a partially completed observation.
func CompleteIndustryRotationRun(
	ctx context.Context,
	id string,
	attempt int,
	completedAt time.Time,
	groups []IndustryRotationGroup,
	members []IndustryRotationMember,
	benchmarkEvidence json.RawMessage,
) error {
	tx, err := GetDB().Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	run, err := scanIndustryRotationRun(tx.QueryRow(ctx, `
		SELECT `+industryRotationRunColumns()+`
		FROM industry_rotation_runs WHERE id=$1 FOR UPDATE`, id))
	if err != nil {
		return fmt.Errorf("lock industry rotation run: %w", err)
	}
	if run.Status != "running" || run.AttemptCount != attempt {
		return fmt.Errorf("complete industry rotation run %q: claim is no longer current", id)
	}
	if _, err := tx.Exec(ctx, `
		SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`,
		industryRotationCompletionLock,
	); err != nil {
		return fmt.Errorf("serialize industry rotation completion: %w", err)
	}

	previousID, previous, err := previousPotentIndustryGroups(
		ctx, tx, run.SessionDate, run.ConfigHash,
	)
	if err != nil {
		return err
	}
	for _, table := range []string{
		"industry_rotation_transitions", "industry_rotation_members",
		"industry_rotation_groups",
	} {
		if _, err := tx.Exec(ctx, "DELETE FROM "+table+" WHERE run_id=$1", id); err != nil {
			return fmt.Errorf("clear %s: %w", table, err)
		}
	}

	current := make(map[string]IndustryRotationGroup, len(groups))
	for i := range groups {
		group := &groups[i]
		if group.RunID == "" {
			group.RunID = id
		}
		if group.RunID != id || !validIndustryGroupIdentity(
			group.GroupType, group.GroupKey,
		) {
			return fmt.Errorf("industry rotation group lineage mismatch for %q", group.GroupKey)
		}
		key := industryGroupMapKey(group.GroupType, group.GroupKey)
		if _, duplicate := current[key]; duplicate {
			return fmt.Errorf(
				"industry rotation run contains duplicate normalized group %q",
				group.GroupKey,
			)
		}
		prior, wasPotent := previous[key]
		group.WasPotentPrevious = wasPotent
		if group.Outcome == "qualified" {
			if wasPotent {
				group.PotencyStreak = prior.PotencyStreak + 1
				group.PotentSince = prior.PotentSince
			} else {
				group.PotencyStreak = 1
				date := run.SessionDate
				group.PotentSince = &date
			}
		} else {
			group.PotencyStreak = 0
			group.PotentSince = nil
		}
		if err := insertIndustryRotationGroup(ctx, tx, *group); err != nil {
			return err
		}
		current[key] = *group
	}
	type groupMemberCounts struct {
		eligible  int
		evaluated int
	}
	memberCounts := make(map[string]groupMemberCounts, len(current))
	for i := range members {
		member := &members[i]
		if member.RunID == "" {
			member.RunID = id
		}
		if member.RunID != id || member.UniverseVersionID != run.UniverseVersionID ||
			strings.TrimSpace(member.Ticker) == "" {
			return fmt.Errorf("industry rotation member lineage mismatch for %q", member.Ticker)
		}
		if _, ok := current[industryGroupMapKey(member.GroupType, member.GroupKey)]; !ok {
			return fmt.Errorf("industry rotation member %q references unknown group", member.Ticker)
		}
		if err := insertIndustryRotationMember(ctx, tx, *member); err != nil {
			return err
		}
		key := industryGroupMapKey(member.GroupType, member.GroupKey)
		counts := memberCounts[key]
		counts.eligible++
		if member.Outcome != "failed" {
			counts.evaluated++
		}
		memberCounts[key] = counts
	}
	for key, group := range current {
		counts := memberCounts[key]
		expectedMemberCoverage := ratio(counts.evaluated, counts.eligible)
		expectedTaxonomyCoverage := ratio(
			group.FreshTaxonomyCount, group.EligibleMemberCount,
		)
		if group.EligibleMemberCount != counts.eligible ||
			group.EvaluatedMemberCount != counts.evaluated ||
			math.Abs(group.MemberCoverage-expectedMemberCoverage) > 1e-12 ||
			math.Abs(group.FreshTaxonomyCoverage-expectedTaxonomyCoverage) > 1e-12 {
			return fmt.Errorf(
				"industry rotation group %q member denominators do not match evidence rows",
				group.GroupKey,
			)
		}
	}

	transitions := buildIndustryRotationTransitions(id, previousID, previous, current)
	if err := replaceIndustryRotationTransitions(ctx, tx, id, transitions); err != nil {
		return err
	}
	counts := summarizeIndustryRotation(groups, members, transitions)
	tag, err := tx.Exec(ctx, `
		UPDATE industry_rotation_runs SET
			status='completed', completed_at=$2, error=NULL,
			benchmark_evidence=$3, eligible_group_count=$4,
			evaluated_group_count=$5, potent_group_count=$6, failed_group_count=$7,
			eligible_member_count=$8, evaluated_member_count=$9,
			selected_member_count=$10, failed_member_count=$11,
			added_group_count=$12, removed_group_count=$13,
			persistent_group_count=$14, group_coverage=$15, member_coverage=$16,
			previous_completed_run_id=$17
		WHERE id=$1 AND status='running' AND attempt_count=$18`,
		id, completedAt, nonNilJSON(benchmarkEvidence),
		counts.EligibleGroupCount, counts.EvaluatedGroupCount,
		counts.PotentGroupCount, counts.FailedGroupCount,
		counts.EligibleMemberCount, counts.EvaluatedMemberCount,
		counts.SelectedMemberCount, counts.FailedMemberCount,
		counts.AddedGroupCount, counts.RemovedGroupCount,
		counts.PersistentGroupCount, counts.GroupCoverage, counts.MemberCoverage,
		nullableUUID(previousID), attempt,
	)
	if err != nil {
		return fmt.Errorf("complete industry rotation run: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("complete industry rotation run %q: lost claim", id)
	}
	if err := repairIndustryRotationSuccessors(
		ctx, tx, run.ConfigHash, run.SessionDate,
	); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		WITH latest AS (
			SELECT id FROM industry_rotation_runs
			WHERE status='completed'
			ORDER BY decision_at DESC, completed_at DESC, id DESC
			LIMIT 1
		)
		UPDATE industry_rotation_members m
		SET promotion_status='superseded',
		    promotion_reason='newer completed industry rotation run',
		    promotion_claim_token=NULL, promotion_claimed_at=NULL
		FROM industry_rotation_runs r, latest l
		WHERE m.run_id=r.id AND m.run_id<>l.id
		  AND m.promotion_status IN ('pending','processing')
		  AND r.status='completed'`,
	); err != nil {
		return fmt.Errorf("supersede stale potent industry promotions: %w", err)
	}
	return tx.Commit(ctx)
}

func ClaimIndustryRotationCandidatesAsOf(
	ctx context.Context,
	expectedSessionDate, asOf, claimedAt time.Time,
	lease time.Duration,
	claimToken string,
) ([]IndustryRotationCandidate, error) {
	if strings.TrimSpace(claimToken) == "" {
		return nil, fmt.Errorf("potent industry promotion claim token is required")
	}
	if expectedSessionDate.IsZero() {
		return nil, fmt.Errorf("expected potent industry promotion session is required")
	}
	if lease <= 0 {
		lease = time.Hour
	}
	rows, err := GetDB().Query(ctx, `
		WITH latest AS (
			SELECT id, session_date FROM industry_rotation_runs
			WHERE status='completed'
			  AND decision_at <= $2::timestamptz
			  AND completed_at <= $2::timestamptz
			ORDER BY decision_at DESC, completed_at DESC, id DESC LIMIT 1
		), claimable AS (
			SELECT m.run_id, m.group_type, m.group_key, m.ticker
			FROM industry_rotation_members m
			JOIN latest l ON l.id=m.run_id AND l.session_date=$1::date
			WHERE m.outcome='qualified'
			  AND (
			    m.promotion_status='pending'
			    OR (
			      m.promotion_status='processing'
			      AND (
			        m.promotion_claimed_at IS NULL
			        OR m.promotion_claimed_at <= $5
			      )
			    )
			  )
			FOR UPDATE OF m SKIP LOCKED
		), claimed AS (
			UPDATE industry_rotation_members m
			SET promotion_status='processing', promotion_claim_token=$4,
			    promotion_claimed_at=$3, promotion_reason=NULL
			FROM claimable c
			WHERE m.run_id=c.run_id AND m.group_type=c.group_type
			  AND m.group_key=c.group_key AND m.ticker=c.ticker
			RETURNING m.*
		)
		SELECT m.run_id, m.universe_version_id, v.as_of,
		       m.group_type, m.group_key, g.archetype, g.potency_score, g.rank,
		       m.ticker, m.member_score, m.member_rank, m.close_price,
		       COALESCE(m.return_20d,0), COALESCE(m.stock_vs_industry_rs,0),
		       COALESCE(m.stock_vs_sector_rs,0), COALESCE(m.stock_vs_market_rs,0),
		       COALESCE(u.market_cap_mm,0), COALESCE(u.dollar_volume,0),
		       u.sector, u.industry, u.taxonomy_status, u.taxonomy_as_of,
		       COALESCE(u.taxonomy_provider,''), u.taxonomy_evidence,
		       m.promotion_claim_token
		FROM claimed m
		JOIN industry_rotation_groups g
		  ON g.run_id=m.run_id AND g.group_type=m.group_type
		 AND g.group_key=m.group_key
		JOIN swing_universe_versions v ON v.id=m.universe_version_id
		JOIN swing_universe_members u
		  ON u.universe_version_id=m.universe_version_id AND u.ticker=m.ticker
		ORDER BY g.rank, m.member_rank, m.member_score DESC, m.ticker`,
		expectedSessionDate.Format("2006-01-02"), asOf, claimedAt, claimToken,
		claimedAt.Add(-lease),
	)
	if err != nil {
		return nil, fmt.Errorf("claim pending potent industry candidates: %w", err)
	}
	defer rows.Close()
	var out []IndustryRotationCandidate
	for rows.Next() {
		var candidate IndustryRotationCandidate
		if err := rows.Scan(
			&candidate.RunID, &candidate.UniverseVersionID, &candidate.UniverseAsOf,
			&candidate.GroupType, &candidate.GroupKey, &candidate.GroupArchetype,
			&candidate.GroupScore, &candidate.GroupRank, &candidate.Ticker,
			&candidate.MemberScore, &candidate.MemberRank, &candidate.Price,
			&candidate.Return20, &candidate.StockVsIndustryRS,
			&candidate.StockVsSectorRS, &candidate.StockVsMarketRS,
			&candidate.MarketCapMM, &candidate.DollarVolume,
			&candidate.Sector, &candidate.Industry, &candidate.TaxonomyStatus,
			&candidate.TaxonomyAsOf, &candidate.TaxonomyProvider,
			&candidate.TaxonomyEvidence, &candidate.PromotionClaimToken,
		); err != nil {
			return nil, err
		}
		out = append(out, candidate)
	}
	return out, rows.Err()
}

func ReleaseIndustryRotationPromotionClaims(
	ctx context.Context, claimToken, reason string,
) error {
	if strings.TrimSpace(claimToken) == "" {
		return nil
	}
	_, err := GetDB().Exec(ctx, `
		UPDATE industry_rotation_members
		SET promotion_status='pending', promotion_claim_token=NULL,
		    promotion_claimed_at=NULL, promotion_reason=$2
		WHERE promotion_status='processing' AND promotion_claim_token=$1`,
		claimToken, nullableString(reason),
	)
	if err != nil {
		return fmt.Errorf("release potent industry promotion claims: %w", err)
	}
	return nil
}

func MarkIndustryRotationPromotion(
	ctx context.Context,
	runID, groupType, groupKey, ticker, claimToken, status, scannerRunID, reason string,
	at time.Time,
) error {
	status = normalizePromotionStatus(status)
	if status == "pending" || status == "processing" ||
		status == "not_requested" || status == "superseded" {
		return fmt.Errorf("terminal potent industry promotion status required")
	}
	tag, err := GetDB().Exec(ctx, `
		UPDATE industry_rotation_members
		SET promotion_status=$6, scanner_run_id=$7, promoted_at=$8,
		    promotion_reason=$9, promotion_claim_token=NULL,
		    promotion_claimed_at=NULL
		WHERE run_id=$1 AND group_type=$2 AND group_key=$3 AND ticker=$4
		  AND outcome='qualified' AND promotion_status='processing'
		  AND promotion_claim_token=$5`,
		runID, groupType, groupKey, ticker, claimToken, status,
		nullableUUID(scannerRunID), at, nullableString(reason),
	)
	if err != nil {
		return fmt.Errorf("mark potent industry promotion: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("mark potent industry promotion: claim is no longer current")
	}
	return nil
}

func GetLatestCompletedIndustryRotationRunAsOf(
	ctx context.Context, asOf time.Time,
) (*IndustryRotationRun, error) {
	run, err := scanIndustryRotationRun(GetDB().QueryRow(ctx, `
		SELECT `+industryRotationRunColumns()+`
		FROM industry_rotation_runs
		WHERE status='completed' AND decision_at <= $1 AND completed_at <= $1
		ORDER BY decision_at DESC, completed_at DESC, id DESC
		LIMIT 1`, asOf))
	if IsNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get latest completed industry rotation run: %w", err)
	}
	return run, nil
}

// ListCompletedIndustryRotationRunsBySessionDate returns one canonical
// completed observation per exchange session date in the inclusive written-date
// range. A session may have multiple completed universe/config variants; reports
// must never combine those incompatible denominators.
func ListCompletedIndustryRotationRunsBySessionDate(
	ctx context.Context, fromDate, toDate time.Time,
) ([]IndustryRotationRun, error) {
	fromKey := fromDate.Format("2006-01-02")
	toKey := toDate.Format("2006-01-02")
	rows, err := GetDB().Query(ctx, `
		WITH canonical_ids AS (
			SELECT DISTINCT ON (session_date) id, session_date
			FROM industry_rotation_runs
			WHERE status='completed' AND completed_at IS NOT NULL
			  AND session_date >= $1::date
			  AND session_date <= $2::date
			ORDER BY session_date, decision_at DESC, completed_at DESC, id DESC
		)
		SELECT `+industryRotationRunColumnsFor("r")+`
		FROM industry_rotation_runs r
		JOIN canonical_ids c ON c.id=r.id
		ORDER BY r.session_date`, fromKey, toKey)
	if err != nil {
		return nil, fmt.Errorf("list completed industry rotation runs: %w", err)
	}
	defer rows.Close()
	var out []IndustryRotationRun
	for rows.Next() {
		run, err := scanIndustryRotationRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *run)
	}
	return out, rows.Err()
}

// ListIndustryRotationGroups returns every group evaluation for selected runs.
// Run chronology, outcome, rank, score, and identity form a stable report order.
func ListIndustryRotationGroups(
	ctx context.Context, runIDs []string,
) ([]IndustryRotationGroup, error) {
	if len(runIDs) == 0 {
		return nil, nil
	}
	rows, err := GetDB().Query(ctx, `
		SELECT `+industryRotationGroupColumns("g")+`
		FROM industry_rotation_groups g
		JOIN industry_rotation_runs r ON r.id=g.run_id
		WHERE g.run_id=ANY($1::uuid[]) AND r.status='completed'
		ORDER BY r.session_date, r.decision_at, r.completed_at, r.id,
		         CASE g.outcome
		             WHEN 'qualified' THEN 0
		             WHEN 'not_qualified' THEN 1
		             ELSE 2
		         END,
		         g.rank NULLS LAST, g.potency_score DESC NULLS LAST,
		         g.group_type, g.group_key`, runIDs)
	if err != nil {
		return nil, fmt.Errorf("list industry rotation groups: %w", err)
	}
	defer rows.Close()
	var out []IndustryRotationGroup
	for rows.Next() {
		group, err := scanIndustryRotationGroup(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *group)
	}
	return out, rows.Err()
}

// ListIndustryRotationTransitions returns group-level additions, persistence,
// removals, and their score/rank deltas for selected runs.
func ListIndustryRotationTransitions(
	ctx context.Context, runIDs []string,
) ([]IndustryRotationTransition, error) {
	if len(runIDs) == 0 {
		return nil, nil
	}
	rows, err := GetDB().Query(ctx, `
		SELECT t.run_id::text, t.group_type, t.group_key, t.event,
		       COALESCE(t.previous_run_id::text,''), t.previous_score,
		       t.current_score, t.previous_rank, t.current_rank, t.reason
		FROM industry_rotation_transitions t
		JOIN industry_rotation_runs r ON r.id=t.run_id
		WHERE t.run_id=ANY($1::uuid[]) AND r.status='completed'
		ORDER BY r.session_date, r.decision_at, r.completed_at, r.id,
		         t.group_type, t.group_key,
		         CASE t.event
		             WHEN 'added' THEN 0
		             WHEN 'persistent' THEN 1
		             ELSE 2
		         END`, runIDs)
	if err != nil {
		return nil, fmt.Errorf("list industry rotation transitions: %w", err)
	}
	defer rows.Close()
	var out []IndustryRotationTransition
	for rows.Next() {
		var transition IndustryRotationTransition
		if err := rows.Scan(
			&transition.RunID, &transition.GroupType, &transition.GroupKey,
			&transition.Event, &transition.PreviousRunID,
			&transition.PreviousScore, &transition.CurrentScore,
			&transition.PreviousRank, &transition.CurrentRank, &transition.Reason,
		); err != nil {
			return nil, err
		}
		out = append(out, transition)
	}
	return out, rows.Err()
}

// ListIndustryRotationTopMembers returns at most limitPerGroup qualified member
// rows for each selected run/group. Ranking ties are resolved by score then
// ticker so repeated reports are byte-for-byte stable.
func ListIndustryRotationTopMembers(
	ctx context.Context, runIDs []string, limitPerGroup int,
) ([]IndustryRotationMember, error) {
	if len(runIDs) == 0 {
		return nil, nil
	}
	if limitPerGroup <= 0 {
		return nil, fmt.Errorf("industry rotation member report limit must be > 0")
	}
	rows, err := GetDB().Query(ctx, `
		WITH ranked AS (
			SELECT m.*,
			       ROW_NUMBER() OVER (
			           PARTITION BY m.run_id, m.group_type, m.group_key
			           ORDER BY m.member_rank, m.member_score DESC, m.ticker
			       ) AS report_rank
			FROM industry_rotation_members m
			WHERE m.run_id=ANY($1::uuid[]) AND m.outcome='qualified'
		)
		SELECT `+industryRotationMemberColumns("m")+`
		FROM ranked m
		JOIN industry_rotation_runs r ON r.id=m.run_id
		WHERE m.report_rank <= $2 AND r.status='completed'
		ORDER BY r.session_date, r.decision_at, r.completed_at, r.id,
		         m.group_type, m.group_key, m.member_rank,
		         m.member_score DESC, m.ticker`, runIDs, limitPerGroup)
	if err != nil {
		return nil, fmt.Errorf("list top industry rotation members: %w", err)
	}
	defer rows.Close()
	var out []IndustryRotationMember
	for rows.Next() {
		member, err := scanIndustryRotationMember(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *member)
	}
	return out, rows.Err()
}

func insertIndustryRotationGroup(
	ctx context.Context, tx pgx.Tx, group IndustryRotationGroup,
) error {
	switch group.Outcome {
	case "qualified", "not_qualified", "failed":
	default:
		return fmt.Errorf("industry rotation group %q has invalid outcome %q",
			group.GroupKey, group.Outcome)
	}
	if group.Outcome != "failed" && strings.TrimSpace(group.Benchmark) == "" {
		return fmt.Errorf("industry rotation group %q lacks benchmark evidence",
			group.GroupKey)
	}
	if group.Outcome == "qualified" &&
		(group.PotencyScore == nil || group.ScorePercentile == nil || group.Rank == nil) {
		return fmt.Errorf("potent industry group %q lacks required score evidence", group.GroupKey)
	}
	archetype := strings.TrimSpace(group.Archetype)
	if archetype == "" {
		archetype = "weak"
	}
	switch archetype {
	case "broad_strong", "strong_narrow", "broad_weak", "weak":
	default:
		return fmt.Errorf("industry rotation group %q has invalid archetype %q",
			group.GroupKey, group.Archetype)
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO industry_rotation_groups (
			run_id, group_type, group_key, sector, industry, benchmark, outcome,
			archetype, failure_stage, failure_reason, eligible_member_count,
			evaluated_member_count, fresh_taxonomy_count, member_coverage,
			fresh_taxonomy_coverage, equal_weight_return_1d,
			equal_weight_return_5d, equal_weight_return_20d,
			cap_weight_return_1d, cap_weight_return_5d, cap_weight_return_20d,
			median_member_return_1d, median_member_return_5d,
			median_member_return_20d, market_relative_return_1d,
			market_relative_return_5d, market_relative_return_20d,
			sector_relative_return_1d, sector_relative_return_5d,
			sector_relative_return_20d, advance_share, advance_decline,
			up_volume_share,
			pct_above_ema20, pct_above_ema50, pct_at_20d_high, pct_at_52w_high,
			median_rvol_20d, median_dollar_volume_expansion,
			breadth_acceleration, cap_equal_divergence, persistence_sessions, potency_score,
			score_percentile, rank, feature_coverage, group_evidence, components,
			was_potent_previous, potency_streak, potent_since
		) VALUES (
			$1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,
			$19,$20,$21,$22,$23,$24,$25,$26,$27,$28,$29,$30,$31,$32,$33,$34,
			$35,$36,$37,$38,$39,$40,$41,$42,$43,$44,$45,$46,$47,$48,$49,$50,
			$51
		)`,
		group.RunID, group.GroupType, group.GroupKey,
		normalizeTaxonomyValue(group.Sector), normalizeTaxonomyValue(group.Industry),
		strings.ToUpper(strings.TrimSpace(group.Benchmark)), group.Outcome,
		archetype, nullableString(group.FailureStage),
		nullableString(group.FailureReason),
		group.EligibleMemberCount, group.EvaluatedMemberCount,
		group.FreshTaxonomyCount, group.MemberCoverage,
		group.FreshTaxonomyCoverage, group.EqualWeightReturn1,
		group.EqualWeightReturn5, group.EqualWeightReturn20,
		group.CapWeightReturn1, group.CapWeightReturn5, group.CapWeightReturn20,
		group.MedianMemberReturn1, group.MedianMemberReturn5,
		group.MedianMemberReturn20, group.MarketRelativeReturn1,
		group.MarketRelativeReturn5, group.MarketRelativeReturn20,
		group.SectorRelativeReturn1, group.SectorRelativeReturn5,
		group.SectorRelativeReturn20, group.AdvanceShare,
		group.AdvanceDecline, group.UpVolumeShare,
		group.PctAboveEMA20, group.PctAboveEMA50,
		group.PctAt20DayHigh, group.PctAt52WeekHigh, group.MedianRVOL20,
		group.MedianDollarVolumeExpansion, group.BreadthAcceleration,
		group.CapEqualDivergence, group.PersistenceSessions, group.PotencyScore,
		group.ScorePercentile, group.Rank, group.FeatureCoverage, nonNilJSON(group.GroupEvidence),
		nonNilJSON(group.Components), group.WasPotentPrevious,
		group.PotencyStreak, group.PotentSince,
	)
	if err != nil {
		return fmt.Errorf("insert industry rotation group %q: %w", group.GroupKey, err)
	}
	return nil
}

func insertIndustryRotationMember(
	ctx context.Context, tx pgx.Tx, member IndustryRotationMember,
) error {
	switch member.Outcome {
	case "qualified", "not_qualified", "failed":
	default:
		return fmt.Errorf("industry rotation member %q has invalid outcome %q",
			member.Ticker, member.Outcome)
	}
	if member.Outcome == "qualified" &&
		(member.ClosePrice == nil || member.MemberScore == nil ||
			member.MemberRank == nil || strings.TrimSpace(member.BarSetHash) == "") {
		return fmt.Errorf("selected industry rotation member %q lacks required evidence",
			member.Ticker)
	}
	promotionStatus := normalizePromotionStatus(member.PromotionStatus)
	_, err := tx.Exec(ctx, `
		INSERT INTO industry_rotation_members (
			run_id, universe_version_id, group_type, group_key, ticker, outcome,
			failure_stage, failure_reason, history_first_at, history_last_at,
			history_sessions, bar_set_hash, close_price, return_1d, return_5d,
			return_20d, stock_vs_industry_rs, stock_vs_sector_rs,
			stock_vs_market_rs, liquidity_score, trend_quality_score, member_score,
			member_rank, feature_coverage, source_evidence, components,
			promotion_status, promotion_claim_token, promotion_claimed_at,
			scanner_run_id, promoted_at, promotion_reason
		) VALUES (
			$1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,
			$19,$20,$21,$22,$23,$24,$25,$26,$27,$28,$29,$30,$31,$32
		)`,
		member.RunID, member.UniverseVersionID, member.GroupType, member.GroupKey,
		member.Ticker, member.Outcome, nullableString(member.FailureStage),
		nullableString(member.FailureReason), member.HistoryFirstAt,
		member.HistoryLastAt, member.HistorySessions, nullableString(member.BarSetHash),
		member.ClosePrice, member.Return1, member.Return5, member.Return20,
		member.StockVsIndustryRS, member.StockVsSectorRS, member.StockVsMarketRS,
		member.LiquidityScore, member.TrendQualityScore, member.MemberScore,
		member.MemberRank, member.FeatureCoverage, nonNilJSON(member.SourceEvidence),
		nonNilJSON(member.Components), promotionStatus,
		nullableString(member.PromotionClaimToken), member.PromotionClaimedAt,
		nullableUUID(member.ScannerRunID), member.PromotedAt,
		nullableString(member.PromotionReason),
	)
	if err != nil {
		return fmt.Errorf("insert industry rotation member %q: %w", member.Ticker, err)
	}
	return nil
}

func previousPotentIndustryGroups(
	ctx context.Context, tx pgx.Tx, sessionDate time.Time, configHash string,
) (string, map[string]IndustryRotationGroup, error) {
	var previousID string
	err := tx.QueryRow(ctx, `
		SELECT id FROM industry_rotation_runs
		WHERE status='completed' AND session_date < $1 AND config_hash=$2
		ORDER BY session_date DESC, decision_at DESC, completed_at DESC, id DESC
		LIMIT 1`, sessionDate, configHash).Scan(&previousID)
	if IsNoRows(err) {
		return "", map[string]IndustryRotationGroup{}, nil
	}
	if err != nil {
		return "", nil, fmt.Errorf("find previous industry rotation run: %w", err)
	}
	rows, err := tx.Query(ctx, `
		SELECT group_type, group_key, potency_score, rank, potency_streak, potent_since
		FROM industry_rotation_groups
		WHERE run_id=$1 AND outcome='qualified'`, previousID)
	if err != nil {
		return "", nil, fmt.Errorf("load previous potent industry groups: %w", err)
	}
	defer rows.Close()
	out := map[string]IndustryRotationGroup{}
	for rows.Next() {
		var group IndustryRotationGroup
		if err := rows.Scan(
			&group.GroupType, &group.GroupKey, &group.PotencyScore, &group.Rank,
			&group.PotencyStreak, &group.PotentSince,
		); err != nil {
			return "", nil, err
		}
		out[industryGroupMapKey(group.GroupType, group.GroupKey)] = group
	}
	return previousID, out, rows.Err()
}

func buildIndustryRotationTransitions(
	runID, previousID string,
	previous, current map[string]IndustryRotationGroup,
) []IndustryRotationTransition {
	out := make([]IndustryRotationTransition, 0)
	for key, group := range current {
		prior, existed := previous[key]
		if group.Outcome != "qualified" {
			if existed {
				reason := "no_longer_potent"
				if group.Outcome == "failed" {
					reason = "data_failure"
				}
				out = append(out, IndustryRotationTransition{
					RunID: runID, GroupType: group.GroupType, GroupKey: group.GroupKey,
					Event: "removed", PreviousRunID: previousID,
					PreviousScore: prior.PotencyScore, CurrentScore: group.PotencyScore,
					PreviousRank: prior.Rank, CurrentRank: group.Rank, Reason: reason,
				})
			}
			continue
		}
		event, reason := "added", "newly_potent"
		var previousScore *float64
		var previousRank *int
		if existed {
			event, reason = "persistent", "remained_potent"
			previousScore, previousRank = prior.PotencyScore, prior.Rank
		}
		out = append(out, IndustryRotationTransition{
			RunID: runID, GroupType: group.GroupType, GroupKey: group.GroupKey,
			Event: event, PreviousRunID: previousID, PreviousScore: previousScore,
			CurrentScore: group.PotencyScore, PreviousRank: previousRank,
			CurrentRank: group.Rank, Reason: reason,
		})
	}
	for key, prior := range previous {
		if _, ok := current[key]; ok {
			continue
		}
		out = append(out, IndustryRotationTransition{
			RunID: runID, GroupType: prior.GroupType, GroupKey: prior.GroupKey,
			Event: "removed", PreviousRunID: previousID,
			PreviousScore: prior.PotencyScore, PreviousRank: prior.Rank,
			Reason: "universe_exit",
		})
	}
	return out
}

func replaceIndustryRotationTransitions(
	ctx context.Context, tx pgx.Tx, runID string,
	transitions []IndustryRotationTransition,
) error {
	if _, err := tx.Exec(
		ctx, "DELETE FROM industry_rotation_transitions WHERE run_id=$1", runID,
	); err != nil {
		return fmt.Errorf("clear industry rotation transitions: %w", err)
	}
	for _, transition := range transitions {
		if _, err := tx.Exec(ctx, `
			INSERT INTO industry_rotation_transitions (
				run_id, group_type, group_key, event, previous_run_id,
				previous_score, current_score, previous_rank, current_rank, reason
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
			transition.RunID, transition.GroupType, transition.GroupKey,
			transition.Event, nullableUUID(transition.PreviousRunID),
			transition.PreviousScore, transition.CurrentScore,
			transition.PreviousRank, transition.CurrentRank, transition.Reason,
		); err != nil {
			return fmt.Errorf("insert industry rotation transition %q: %w",
				transition.GroupKey, err)
		}
	}
	return nil
}

func repairIndustryRotationSuccessors(
	ctx context.Context, tx pgx.Tx, configHash string, afterSession time.Time,
) error {
	rows, err := tx.Query(ctx, `
		SELECT id, session_date FROM industry_rotation_runs
		WHERE status='completed' AND config_hash=$1 AND session_date>$2
		ORDER BY session_date, decision_at, completed_at, id`,
		configHash, afterSession,
	)
	if err != nil {
		return fmt.Errorf("list industry rotation successors: %w", err)
	}
	type successor struct {
		id          string
		sessionDate time.Time
	}
	var successors []successor
	for rows.Next() {
		var item successor
		if err := rows.Scan(&item.id, &item.sessionDate); err != nil {
			rows.Close()
			return err
		}
		successors = append(successors, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	for _, item := range successors {
		previousID, previous, err := previousPotentIndustryGroups(
			ctx, tx, item.sessionDate, configHash,
		)
		if err != nil {
			return err
		}
		current, err := loadIndustryRotationLineageGroups(ctx, tx, item.id)
		if err != nil {
			return err
		}
		for key, group := range current {
			prior, wasPotent := previous[key]
			group.WasPotentPrevious = wasPotent
			if group.Outcome == "qualified" {
				if wasPotent {
					group.PotencyStreak = prior.PotencyStreak + 1
					group.PotentSince = prior.PotentSince
				} else {
					group.PotencyStreak = 1
					date := item.sessionDate
					group.PotentSince = &date
				}
			} else {
				group.PotencyStreak = 0
				group.PotentSince = nil
			}
			if _, err := tx.Exec(ctx, `
				UPDATE industry_rotation_groups
				SET was_potent_previous=$4, potency_streak=$5, potent_since=$6
				WHERE run_id=$1 AND group_type=$2 AND group_key=$3`,
				item.id, group.GroupType, group.GroupKey,
				group.WasPotentPrevious, group.PotencyStreak, group.PotentSince,
			); err != nil {
				return fmt.Errorf("repair industry rotation group lineage: %w", err)
			}
			current[key] = group
		}
		transitions := buildIndustryRotationTransitions(
			item.id, previousID, previous, current,
		)
		if err := replaceIndustryRotationTransitions(
			ctx, tx, item.id, transitions,
		); err != nil {
			return err
		}
		var added, removed, persistent int
		for _, transition := range transitions {
			switch transition.Event {
			case "added":
				added++
			case "removed":
				removed++
			case "persistent":
				persistent++
			}
		}
		if _, err := tx.Exec(ctx, `
			UPDATE industry_rotation_runs
			SET previous_completed_run_id=$2, added_group_count=$3,
			    removed_group_count=$4, persistent_group_count=$5
			WHERE id=$1 AND status='completed'`,
			item.id, nullableUUID(previousID), added, removed, persistent,
		); err != nil {
			return fmt.Errorf("repair industry rotation run lineage: %w", err)
		}
	}
	return nil
}

func loadIndustryRotationLineageGroups(
	ctx context.Context, tx pgx.Tx, runID string,
) (map[string]IndustryRotationGroup, error) {
	rows, err := tx.Query(ctx, `
		SELECT group_type, group_key, outcome, potency_score, rank,
		       was_potent_previous, potency_streak, potent_since
		FROM industry_rotation_groups WHERE run_id=$1`, runID)
	if err != nil {
		return nil, fmt.Errorf("load industry rotation lineage groups: %w", err)
	}
	defer rows.Close()
	out := map[string]IndustryRotationGroup{}
	for rows.Next() {
		var group IndustryRotationGroup
		group.RunID = runID
		if err := rows.Scan(
			&group.GroupType, &group.GroupKey, &group.Outcome,
			&group.PotencyScore, &group.Rank, &group.WasPotentPrevious,
			&group.PotencyStreak, &group.PotentSince,
		); err != nil {
			return nil, err
		}
		out[industryGroupMapKey(group.GroupType, group.GroupKey)] = group
	}
	return out, rows.Err()
}

func summarizeIndustryRotation(
	groups []IndustryRotationGroup,
	members []IndustryRotationMember,
	transitions []IndustryRotationTransition,
) IndustryRotationRun {
	out := IndustryRotationRun{
		EligibleGroupCount: len(groups), EligibleMemberCount: len(members),
	}
	for _, group := range groups {
		if group.Outcome == "failed" {
			out.FailedGroupCount++
		} else {
			out.EvaluatedGroupCount++
		}
		if group.Outcome == "qualified" {
			out.PotentGroupCount++
		}
	}
	for _, member := range members {
		if member.Outcome == "failed" {
			out.FailedMemberCount++
		} else {
			out.EvaluatedMemberCount++
		}
		if member.Outcome == "qualified" {
			out.SelectedMemberCount++
		}
	}
	for _, transition := range transitions {
		switch transition.Event {
		case "added":
			out.AddedGroupCount++
		case "removed":
			out.RemovedGroupCount++
		case "persistent":
			out.PersistentGroupCount++
		}
	}
	out.GroupCoverage = ratio(out.EvaluatedGroupCount, out.EligibleGroupCount)
	out.MemberCoverage = ratio(out.EvaluatedMemberCount, out.EligibleMemberCount)
	return out
}

func industryRotationGroupColumns(alias string) string {
	p := alias + "."
	return p + `run_id::text, ` + p + `group_type, ` + p + `group_key,
		` + p + `sector, ` + p + `industry, ` + p + `benchmark, ` + p + `outcome,
		` + p + `archetype, COALESCE(` + p + `failure_stage,''),
		COALESCE(` + p + `failure_reason,''),
		` + p + `eligible_member_count, ` + p + `evaluated_member_count,
		` + p + `fresh_taxonomy_count, ` + p + `member_coverage,
		` + p + `fresh_taxonomy_coverage, ` + p + `equal_weight_return_1d,
		` + p + `equal_weight_return_5d, ` + p + `equal_weight_return_20d,
		` + p + `cap_weight_return_1d, ` + p + `cap_weight_return_5d,
		` + p + `cap_weight_return_20d, ` + p + `median_member_return_1d,
		` + p + `median_member_return_5d, ` + p + `median_member_return_20d,
		` + p + `market_relative_return_1d, ` + p + `market_relative_return_5d,
		` + p + `market_relative_return_20d, ` + p + `sector_relative_return_1d,
		` + p + `sector_relative_return_5d, ` + p + `sector_relative_return_20d,
		` + p + `advance_share, ` + p + `advance_decline, ` + p + `up_volume_share,
		` + p + `pct_above_ema20, ` + p + `pct_above_ema50,
		` + p + `pct_at_20d_high, ` + p + `pct_at_52w_high,
		` + p + `median_rvol_20d, ` + p + `median_dollar_volume_expansion,
		` + p + `breadth_acceleration, ` + p + `cap_equal_divergence,
		` + p + `persistence_sessions, ` + p + `potency_score,
		` + p + `score_percentile, ` + p + `rank, ` + p + `feature_coverage,
		` + p + `group_evidence, ` + p + `components,
		` + p + `was_potent_previous, ` + p + `potency_streak, ` + p + `potent_since`
}

func scanIndustryRotationGroup(row pgx.Row) (*IndustryRotationGroup, error) {
	var group IndustryRotationGroup
	err := row.Scan(
		&group.RunID, &group.GroupType, &group.GroupKey, &group.Sector,
		&group.Industry, &group.Benchmark, &group.Outcome, &group.Archetype,
		&group.FailureStage, &group.FailureReason, &group.EligibleMemberCount,
		&group.EvaluatedMemberCount, &group.FreshTaxonomyCount,
		&group.MemberCoverage, &group.FreshTaxonomyCoverage,
		&group.EqualWeightReturn1, &group.EqualWeightReturn5,
		&group.EqualWeightReturn20, &group.CapWeightReturn1,
		&group.CapWeightReturn5, &group.CapWeightReturn20,
		&group.MedianMemberReturn1, &group.MedianMemberReturn5,
		&group.MedianMemberReturn20, &group.MarketRelativeReturn1,
		&group.MarketRelativeReturn5, &group.MarketRelativeReturn20,
		&group.SectorRelativeReturn1, &group.SectorRelativeReturn5,
		&group.SectorRelativeReturn20, &group.AdvanceShare,
		&group.AdvanceDecline, &group.UpVolumeShare, &group.PctAboveEMA20,
		&group.PctAboveEMA50, &group.PctAt20DayHigh, &group.PctAt52WeekHigh,
		&group.MedianRVOL20, &group.MedianDollarVolumeExpansion,
		&group.BreadthAcceleration, &group.CapEqualDivergence,
		&group.PersistenceSessions, &group.PotencyScore, &group.ScorePercentile,
		&group.Rank, &group.FeatureCoverage, &group.GroupEvidence,
		&group.Components, &group.WasPotentPrevious, &group.PotencyStreak,
		&group.PotentSince,
	)
	return &group, err
}

func industryRotationMemberColumns(alias string) string {
	p := alias + "."
	return p + `run_id::text, ` + p + `universe_version_id::text,
		` + p + `group_type, ` + p + `group_key, ` + p + `ticker, ` + p + `source_tag,
		` + p + `outcome, COALESCE(` + p + `failure_stage,''),
		COALESCE(` + p + `failure_reason,''), ` + p + `history_first_at,
		` + p + `history_last_at, ` + p + `history_sessions, ` + p + `bar_set_hash,
		` + p + `close_price, ` + p + `return_1d, ` + p + `return_5d,
		` + p + `return_20d, ` + p + `stock_vs_industry_rs,
		` + p + `stock_vs_sector_rs, ` + p + `stock_vs_market_rs,
		` + p + `liquidity_score, ` + p + `trend_quality_score, ` + p + `member_score,
		` + p + `member_rank, ` + p + `feature_coverage, ` + p + `source_evidence,
		` + p + `components, ` + p + `promotion_status,
		COALESCE(` + p + `promotion_claim_token,''), ` + p + `promotion_claimed_at,
		COALESCE(` + p + `scanner_run_id::text,''), ` + p + `promoted_at,
		COALESCE(` + p + `promotion_reason,'')`
}

func scanIndustryRotationMember(row pgx.Row) (*IndustryRotationMember, error) {
	var member IndustryRotationMember
	err := row.Scan(
		&member.RunID, &member.UniverseVersionID, &member.GroupType,
		&member.GroupKey, &member.Ticker, &member.SourceTag, &member.Outcome,
		&member.FailureStage, &member.FailureReason, &member.HistoryFirstAt,
		&member.HistoryLastAt, &member.HistorySessions, &member.BarSetHash,
		&member.ClosePrice, &member.Return1, &member.Return5, &member.Return20,
		&member.StockVsIndustryRS, &member.StockVsSectorRS,
		&member.StockVsMarketRS, &member.LiquidityScore,
		&member.TrendQualityScore, &member.MemberScore, &member.MemberRank,
		&member.FeatureCoverage, &member.SourceEvidence, &member.Components,
		&member.PromotionStatus, &member.PromotionClaimToken,
		&member.PromotionClaimedAt, &member.ScannerRunID, &member.PromotedAt,
		&member.PromotionReason,
	)
	return &member, err
}

func industryRotationRunColumns() string {
	return industryRotationRunColumnsFor("")
}

func industryRotationRunColumnsFor(alias string) string {
	prefix := ""
	if alias != "" {
		prefix = alias + "."
	}
	return prefix + `id, ` + prefix + `session_date, ` + prefix + `as_of, ` +
		prefix + `decision_at, ` + prefix + `universe_version_id, ` + prefix + `status,
		` + prefix + `started_at, ` + prefix + `completed_at, ` + prefix + `lease_until,
		` + prefix + `attempt_count, ` + prefix + `config_hash, ` +
		prefix + `config_snapshot, ` + prefix + `feature_version, ` +
		prefix + `market_benchmark, ` + prefix + `benchmark_evidence,
		` + prefix + `history_provider, COALESCE(` + prefix + `error,''),
		` + prefix + `eligible_group_count, ` + prefix + `evaluated_group_count,
		` + prefix + `potent_group_count, ` + prefix + `failed_group_count,
		` + prefix + `eligible_member_count, ` + prefix + `evaluated_member_count,
		` + prefix + `selected_member_count, ` + prefix + `failed_member_count,
		` + prefix + `added_group_count, ` + prefix + `removed_group_count,
		` + prefix + `persistent_group_count, ` + prefix + `group_coverage,
		` + prefix + `member_coverage,
		COALESCE(` + prefix + `previous_completed_run_id::text,'')`
}

func scanIndustryRotationRun(row pgx.Row) (*IndustryRotationRun, error) {
	var run IndustryRotationRun
	err := row.Scan(
		&run.ID, &run.SessionDate, &run.AsOf, &run.DecisionAt,
		&run.UniverseVersionID, &run.Status, &run.StartedAt, &run.CompletedAt,
		&run.LeaseUntil, &run.AttemptCount, &run.ConfigHash, &run.ConfigSnapshot,
		&run.FeatureVersion, &run.MarketBenchmark, &run.BenchmarkEvidence,
		&run.HistoryProvider, &run.Error, &run.EligibleGroupCount,
		&run.EvaluatedGroupCount, &run.PotentGroupCount, &run.FailedGroupCount,
		&run.EligibleMemberCount, &run.EvaluatedMemberCount,
		&run.SelectedMemberCount, &run.FailedMemberCount, &run.AddedGroupCount,
		&run.RemovedGroupCount, &run.PersistentGroupCount, &run.GroupCoverage,
		&run.MemberCoverage, &run.PreviousCompletedRunID,
	)
	return &run, err
}

func validIndustryGroupIdentity(groupType, groupKey string) bool {
	return (groupType == "industry" || groupType == "sector") &&
		strings.TrimSpace(groupKey) != ""
}

func industryGroupMapKey(groupType, groupKey string) string {
	normalizedKey := strings.ToUpper(strings.Join(strings.Fields(groupKey), " "))
	return groupType + "\x00" + normalizedKey
}
