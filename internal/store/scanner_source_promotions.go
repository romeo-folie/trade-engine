package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var ErrScannerSourcePromotionClaimLost = errors.New(
	"scanner source promotion claim is no longer current",
)

type ScannerSourcePromotionClaimError struct {
	Source string
}

func (e *ScannerSourcePromotionClaimError) Error() string {
	return fmt.Sprintf("%s: %s", e.Source, ErrScannerSourcePromotionClaimLost)
}

func (e *ScannerSourcePromotionClaimError) Unwrap() error {
	return ErrScannerSourcePromotionClaimLost
}

type WeeklyLeaderPromotionClaim struct {
	RunID  string
	Ticker string
	Token  string
}

type PotentIndustryPromotionClaim struct {
	RunID     string
	GroupType string
	GroupKey  string
	Ticker    string
	Token     string
}

type PremarketEpisodicPromotionClaim struct {
	RunID  string
	Ticker string
	Token  string
}

type PoliticalDisclosurePromotionClaim struct {
	Ticker    string
	Token     string
	ReportIDs []string
}

type ScannerSourcePromotionClaims struct {
	WeeklyLeader        *WeeklyLeaderPromotionClaim
	PotentIndustry      *PotentIndustryPromotionClaim
	PremarketEpisodic   *PremarketEpisodicPromotionClaim
	PoliticalDisclosure *PoliticalDisclosurePromotionClaim
}

// PromoteScannerSourcesToWatchlist validates every discovery-source claim,
// writes the watchlist, and consumes all claims in one transaction. A stale
// source can therefore never produce a watchlist row with partial attribution.
func PromoteScannerSourcesToWatchlist(
	ctx context.Context,
	item WatchlistItem,
	claims ScannerSourcePromotionClaims,
	reason string,
	at time.Time,
) error {
	return promoteScannerSourcesToWatchlist(ctx, item, claims, reason, at, false, false, nil)
}

// PromoteScannerSourcesToWatchlistWithScannerLease fences source-claim
// consumption and the watchlist write with the scanner cycle lease.
func PromoteScannerSourcesToWatchlistWithScannerLease(
	ctx context.Context,
	item WatchlistItem,
	claims ScannerSourcePromotionClaims,
	reason string,
	at time.Time,
	lease ScannerCycleLease,
) error {
	return promoteScannerSourcesToWatchlist(ctx, item, claims, reason, at, false, false, &lease)
}

// PromoteScannerSourcesToWatchlistUnlessActive consumes all still-current
// source claims without replacing a watchlist row that won the race after the
// scanner's initial active-row filter.
func PromoteScannerSourcesToWatchlistUnlessActive(
	ctx context.Context,
	item WatchlistItem,
	claims ScannerSourcePromotionClaims,
	reason string,
	at time.Time,
) error {
	return promoteScannerSourcesToWatchlist(ctx, item, claims, reason, at, true, false, nil)
}

// PromoteScannerSourcesToWatchlistUnlessActiveWithScannerLease is the fenced
// fallback variant that preserves an already-active winner.
func PromoteScannerSourcesToWatchlistUnlessActiveWithScannerLease(
	ctx context.Context,
	item WatchlistItem,
	claims ScannerSourcePromotionClaims,
	reason string,
	at time.Time,
	lease ScannerCycleLease,
) error {
	return promoteScannerSourcesToWatchlist(ctx, item, claims, reason, at, true, false, &lease)
}

// RefreshScannerSourcesOnActiveWatchlist consumes current source claims and
// refreshes only an already-live scanner row. It cannot re-admit an expired or
// removed ticker without the normal scoring and cap-selection path.
func RefreshScannerSourcesOnActiveWatchlist(
	ctx context.Context, item WatchlistItem, claims ScannerSourcePromotionClaims,
	reason string, at time.Time, lease *ScannerCycleLease,
) error {
	return promoteScannerSourcesToWatchlist(ctx, item, claims, reason, at, true, true, lease)
}

func promoteScannerSourcesToWatchlist(
	ctx context.Context,
	item WatchlistItem,
	claims ScannerSourcePromotionClaims,
	reason string,
	at time.Time,
	preserveActive bool,
	refreshOnly bool,
	lease *ScannerCycleLease,
) error {
	if strings.TrimSpace(item.Ticker) == "" ||
		(claims.WeeklyLeader == nil && claims.PotentIndustry == nil &&
			claims.PremarketEpisodic == nil && claims.PoliticalDisclosure == nil) {
		return fmt.Errorf("scanner source watchlist promotion lineage is incomplete")
	}
	if claims.WeeklyLeader != nil &&
		(claims.WeeklyLeader.Ticker != item.Ticker ||
			strings.TrimSpace(claims.WeeklyLeader.RunID) == "" ||
			strings.TrimSpace(claims.WeeklyLeader.Token) == "") {
		return fmt.Errorf("weekly leader watchlist promotion lineage is incomplete")
	}
	if claims.PotentIndustry != nil &&
		(claims.PotentIndustry.Ticker != item.Ticker ||
			strings.TrimSpace(claims.PotentIndustry.RunID) == "" ||
			strings.TrimSpace(claims.PotentIndustry.GroupType) == "" ||
			strings.TrimSpace(claims.PotentIndustry.GroupKey) == "" ||
			strings.TrimSpace(claims.PotentIndustry.Token) == "") {
		return fmt.Errorf("potent industry watchlist promotion lineage is incomplete")
	}
	if claims.PremarketEpisodic != nil &&
		(claims.PremarketEpisodic.Ticker != item.Ticker ||
			strings.TrimSpace(claims.PremarketEpisodic.RunID) == "" ||
			strings.TrimSpace(claims.PremarketEpisodic.Token) == "") {
		return fmt.Errorf("premarket episodic watchlist promotion lineage is incomplete")
	}
	if claim := claims.PoliticalDisclosure; claim != nil {
		reportIDs, ok := normalizePoliticalDisclosureReportIDs(claim.ReportIDs)
		if claim.Ticker != item.Ticker || strings.TrimSpace(claim.Token) == "" || !ok {
			return fmt.Errorf("political disclosure watchlist promotion lineage is incomplete")
		}
		// Use canonical, deduplicated UUIDs for each lock and terminal update.
		// This makes the parent-first lock ordering stable even when a caller
		// supplied equivalent UUID spellings.
		politicalClaim := *claim
		politicalClaim.ReportIDs = reportIDs
		claims.PoliticalDisclosure = &politicalClaim
	}

	tx, err := GetDB().Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	// Political evidence is an aggregate by ticker. Take its namespace before
	// any parent/candidate row locks, matching ingestion and claim ordering.
	if claim := claims.PoliticalDisclosure; claim != nil {
		if err := lockPoliticalDisclosureTickers(ctx, tx, []string{claim.Ticker}); err != nil {
			return err
		}
	}

	// Match the completion locks in a fixed order. This prevents a newer
	// completed observation from superseding a claim between validation and
	// terminal publication.
	if claims.WeeklyLeader != nil {
		if _, err := tx.Exec(ctx, `
			SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`,
			weeklyLeaderCompletionLock,
		); err != nil {
			return fmt.Errorf("serialize weekly leader promotion: %w", err)
		}
	}
	if claims.PotentIndustry != nil {
		if _, err := tx.Exec(ctx, `
			SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`,
			industryRotationCompletionLock,
		); err != nil {
			return fmt.Errorf("serialize potent industry promotion: %w", err)
		}
	}
	if claims.PremarketEpisodic != nil {
		if _, err := tx.Exec(ctx, `
			SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`,
			premarketEpisodicCompletionLock,
		); err != nil {
			return fmt.Errorf("serialize premarket episodic promotion: %w", err)
		}
	}

	if claim := claims.WeeklyLeader; claim != nil {
		var token, status string
		var currentRun bool
		err := tx.QueryRow(ctx, `
			SELECT COALESCE(q.promotion_claim_token,''), q.promotion_status,
			       COALESCE(q.run_id = (
			         SELECT r.id FROM weekly_leader_runs r
			         WHERE r.status='completed'
			         ORDER BY r.decision_at DESC, r.completed_at DESC, r.id DESC
			         LIMIT 1
			       ), false)
			FROM weekly_leader_qualifications q
			WHERE q.run_id=$1 AND q.ticker=$2 AND q.outcome='qualified'
			FOR UPDATE OF q`, claim.RunID, claim.Ticker,
		).Scan(&token, &status, &currentRun)
		if err != nil {
			if IsNoRows(err) {
				return &ScannerSourcePromotionClaimError{Source: "weekly_leader"}
			}
			return fmt.Errorf("lock weekly leader promotion: %w", err)
		}
		if !currentRun || status != "processing" || token != claim.Token {
			return &ScannerSourcePromotionClaimError{Source: "weekly_leader"}
		}
	}
	if claim := claims.PotentIndustry; claim != nil {
		var token, status string
		var currentRun bool
		err := tx.QueryRow(ctx, `
			SELECT COALESCE(m.promotion_claim_token,''), m.promotion_status,
			       COALESCE(m.run_id = (
			         SELECT r.id FROM industry_rotation_runs r
			         WHERE r.status='completed'
			         ORDER BY r.decision_at DESC, r.completed_at DESC, r.id DESC
			         LIMIT 1
			       ), false)
			FROM industry_rotation_members m
			WHERE m.run_id=$1 AND m.group_type=$2 AND m.group_key=$3
			  AND m.ticker=$4 AND m.outcome='qualified'
			FOR UPDATE OF m`,
			claim.RunID, claim.GroupType, claim.GroupKey, claim.Ticker,
		).Scan(&token, &status, &currentRun)
		if err != nil {
			if IsNoRows(err) {
				return &ScannerSourcePromotionClaimError{Source: "potent_industry"}
			}
			return fmt.Errorf("lock potent industry promotion: %w", err)
		}
		if !currentRun || status != "processing" || token != claim.Token {
			return &ScannerSourcePromotionClaimError{Source: "potent_industry"}
		}
	}
	if claim := claims.PremarketEpisodic; claim != nil {
		var token, status string
		var currentRun bool
		err := tx.QueryRow(ctx, `
			SELECT COALESCE(c.promotion_claim_token,''), c.promotion_status,
			       COALESCE(c.run_id = (
			         SELECT r.id FROM premarket_episodic_runs r
			         WHERE r.status='completed'
			         ORDER BY r.checkpoint_at DESC, r.completed_at DESC, r.id DESC
			         LIMIT 1
			       ), false)
			FROM premarket_episodic_candidates c
			WHERE c.run_id=$1 AND c.ticker=$2 AND c.outcome='qualified'
			FOR UPDATE OF c`,
			claim.RunID, claim.Ticker,
		).Scan(&token, &status, &currentRun)
		if err != nil {
			if IsNoRows(err) {
				return &ScannerSourcePromotionClaimError{Source: "premarket_episodic"}
			}
			return fmt.Errorf("lock premarket episodic promotion: %w", err)
		}
		if !currentRun || status != "processing" || token != claim.Token {
			return &ScannerSourcePromotionClaimError{Source: "premarket_episodic"}
		}
	}
	if claim := claims.PoliticalDisclosure; claim != nil {
		parentRows, err := tx.Query(ctx, `
			SELECT id::text, extraction_status
			FROM political_disclosure_reports
			WHERE id = ANY($1::uuid[])
			ORDER BY id
			FOR UPDATE`, claim.ReportIDs)
		if err != nil {
			return fmt.Errorf("lock political disclosure parent reports: %w", err)
		}
		parentCount := 0
		parentsComplete := true
		for parentRows.Next() {
			var reportID, extractionStatus string
			if err := parentRows.Scan(&reportID, &extractionStatus); err != nil {
				parentRows.Close()
				return err
			}
			parentCount++
			parentsComplete = parentsComplete && extractionStatus == "completed"
		}
		err = parentRows.Err()
		parentRows.Close()
		if err != nil {
			return err
		}
		if parentCount != len(claim.ReportIDs) || !parentsComplete {
			return &ScannerSourcePromotionClaimError{Source: "political_disclosure"}
		}

		rows, err := tx.Query(ctx, `
			SELECT c.report_id::text, COALESCE(c.promotion_claim_token,''),
			       c.promotion_status
			FROM political_disclosure_candidates c
			JOIN unnest($2::uuid[]) expected(report_id)
			  ON c.report_id=expected.report_id
			WHERE c.ticker=$1
			ORDER BY c.report_id
			FOR UPDATE OF c`, claim.Ticker, claim.ReportIDs)
		if err != nil {
			return fmt.Errorf("lock political disclosure promotion: %w", err)
		}
		found := make(map[string]struct{}, len(claim.ReportIDs))
		valid := true
		for rows.Next() {
			var reportID, token, status string
			if err := rows.Scan(&reportID, &token, &status); err != nil {
				rows.Close()
				return err
			}
			_, duplicate := found[reportID]
			found[reportID] = struct{}{}
			// Terminal history is fenced alongside the one processing row that
			// made this aggregate eligible. It must still be owned by this
			// promotion, but its terminal outcome is deliberately immutable.
			valid = valid && !duplicate && token == claim.Token
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if len(found) != len(claim.ReportIDs) || !valid {
			return &ScannerSourcePromotionClaimError{Source: "political_disclosure"}
		}
	}

	// Take all potentially blocking source/advisory locks before touching the
	// lease row. This leaves an expired lease reclaimable while this transaction
	// waits, then fences the final watchlist write with the current owner.
	if lease != nil || refreshOnly {
		if err := lockScannerWatchlistItem(ctx, tx, item); err != nil {
			return err
		}
	}
	if lease != nil {
		if err := renewScannerCycleTx(ctx, tx, *lease); err != nil {
			return err
		}
	}
	if refreshOnly {
		var active bool
		err := tx.QueryRow(ctx, `
			SELECT active AND (
					EXISTS (SELECT 1 FROM watchlist_source_leases l WHERE l.watchlist_id=watchlist.id AND l.expires_at > clock_timestamp())
					OR (NOT EXISTS (SELECT 1 FROM watchlist_source_leases l WHERE l.watchlist_id=watchlist.id)
						AND (expires_at IS NULL OR expires_at > clock_timestamp()))
			)
			FROM watchlist WHERE ticker=$1 AND platform=$2 AND source='scanner' FOR UPDATE`,
			strings.ToUpper(strings.TrimSpace(item.Ticker)), item.Platform,
		).Scan(&active)
		if err := refreshActivityResult(active, err); err != nil {
			return err
		}
		if _, err := refreshWatchlistSourceLeases(ctx, tx, item); err != nil {
			return err
		}
		if err := completeScannerSourcePromotionClaims(ctx, tx, item, claims,
			"active_watchlist", reason, at); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	if err := addWatchlistItem(ctx, tx, item, preserveActive); err != nil {
		if !errors.Is(err, ErrWatchlistActiveItemExists) {
			return err
		}
		if completionErr := completeScannerSourcePromotionClaims(
			ctx, tx, item, claims, "active_watchlist",
			"active watchlist item won fallback promotion race", at,
		); completionErr != nil {
			return completionErr
		}
		if commitErr := tx.Commit(ctx); commitErr != nil {
			return commitErr
		}
		return ErrWatchlistActiveItemExists
	}
	if err := completeScannerSourcePromotionClaims(
		ctx, tx, item, claims, "promoted", reason, at,
	); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func refreshActivityResult(active bool, err error) error {
	if err != nil {
		if isNoRows(err) {
			return ErrWatchlistRefreshNotActive
		}
		return err
	}
	if !active {
		return ErrWatchlistRefreshNotActive
	}
	return nil
}

func completeScannerSourcePromotionClaims(
	ctx context.Context,
	tx pgx.Tx,
	item WatchlistItem,
	claims ScannerSourcePromotionClaims,
	status, reason string,
	at time.Time,
) error {
	if claim := claims.WeeklyLeader; claim != nil {
		tag, err := tx.Exec(ctx, `
			UPDATE weekly_leader_qualifications
			SET promotion_status=$4, scanner_run_id=$5, promoted_at=$6,
			    promotion_reason=$7, promotion_claim_token=NULL,
			    promotion_claimed_at=NULL
			WHERE run_id=$1 AND ticker=$2 AND promotion_status='processing'
			  AND promotion_claim_token=$3`,
			claim.RunID, claim.Ticker, claim.Token, status,
			nullableUUID(item.ScannerRunID), at, nullableString(reason),
		)
		if err != nil {
			return fmt.Errorf("complete weekly leader watchlist promotion: %w", err)
		}
		if tag.RowsAffected() != 1 {
			return &ScannerSourcePromotionClaimError{Source: "weekly_leader"}
		}
	}
	if claim := claims.PotentIndustry; claim != nil {
		tag, err := tx.Exec(ctx, `
			UPDATE industry_rotation_members
			SET promotion_status=$6, scanner_run_id=$7, promoted_at=$8,
			    promotion_reason=$9, promotion_claim_token=NULL,
			    promotion_claimed_at=NULL
			WHERE run_id=$1 AND group_type=$2 AND group_key=$3 AND ticker=$4
			  AND promotion_status='processing' AND promotion_claim_token=$5`,
			claim.RunID, claim.GroupType, claim.GroupKey, claim.Ticker, claim.Token,
			status, nullableUUID(item.ScannerRunID), at, nullableString(reason),
		)
		if err != nil {
			return fmt.Errorf("complete potent industry watchlist promotion: %w", err)
		}
		if tag.RowsAffected() != 1 {
			return &ScannerSourcePromotionClaimError{Source: "potent_industry"}
		}
	}
	if claim := claims.PremarketEpisodic; claim != nil {
		tag, err := tx.Exec(ctx, `
			UPDATE premarket_episodic_candidates
			SET promotion_status=$4, scanner_run_id=$5, promoted_at=$6,
			    promotion_reason=$7, promotion_claim_token=NULL,
			    promotion_claimed_at=NULL
			WHERE run_id=$1 AND ticker=$2 AND promotion_status='processing'
			  AND promotion_claim_token=$3`,
			claim.RunID, claim.Ticker, claim.Token, status,
			nullableUUID(item.ScannerRunID), at, nullableString(reason),
		)
		if err != nil {
			return fmt.Errorf("complete premarket episodic watchlist promotion: %w", err)
		}
		if tag.RowsAffected() != 1 {
			return &ScannerSourcePromotionClaimError{Source: "premarket_episodic"}
		}
	}
	if claim := claims.PoliticalDisclosure; claim != nil {
		tag, err := tx.Exec(ctx, `
		UPDATE political_disclosure_candidates
			SET promotion_status=CASE WHEN promotion_status='processing' THEN $4 ELSE promotion_status END,
			    scanner_run_id=CASE WHEN promotion_status='processing' THEN $5 ELSE scanner_run_id END,
			    promoted_at=CASE WHEN promotion_status='processing' THEN $6 ELSE promoted_at END,
			    promotion_reason=CASE WHEN promotion_status='processing' THEN $7 ELSE promotion_reason END,
			    promotion_claim_token=NULL, promotion_claimed_at=NULL
			WHERE ticker=$1
			  AND report_id IN (
			    SELECT report_id::uuid FROM unnest($2::text[]) expected(report_id)
			  )
			  AND promotion_claim_token=$3`,
			claim.Ticker, claim.ReportIDs, claim.Token, status,
			nullableUUID(item.ScannerRunID), at, nullableString(reason),
		)
		if err != nil {
			return fmt.Errorf("complete political disclosure watchlist promotion: %w", err)
		}
		if tag.RowsAffected() != int64(len(claim.ReportIDs)) {
			return &ScannerSourcePromotionClaimError{Source: "political_disclosure"}
		}
	}
	return nil
}

func normalizePoliticalDisclosureReportIDs(reportIDs []string) ([]string, bool) {
	if len(reportIDs) == 0 {
		return nil, false
	}
	seen := make(map[string]struct{}, len(reportIDs))
	normalized := make([]string, 0, len(reportIDs))
	for _, reportID := range reportIDs {
		parsed, err := uuid.Parse(strings.TrimSpace(reportID))
		if err != nil {
			return nil, false
		}
		reportID = parsed.String()
		if _, duplicate := seen[reportID]; duplicate {
			return nil, false
		}
		seen[reportID] = struct{}{}
		normalized = append(normalized, reportID)
	}
	return normalized, true
}
