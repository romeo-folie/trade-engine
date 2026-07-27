package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
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

type ScannerSourcePromotionClaims struct {
	WeeklyLeader   *WeeklyLeaderPromotionClaim
	PotentIndustry *PotentIndustryPromotionClaim
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
	if strings.TrimSpace(item.Ticker) == "" ||
		(claims.WeeklyLeader == nil && claims.PotentIndustry == nil) {
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

	tx, err := GetDB().Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

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

	if err := addWatchlistItem(ctx, tx, item, false); err != nil {
		return err
	}
	if claim := claims.WeeklyLeader; claim != nil {
		tag, err := tx.Exec(ctx, `
			UPDATE weekly_leader_qualifications
			SET promotion_status='promoted', scanner_run_id=$4, promoted_at=$5,
			    promotion_reason=$6, promotion_claim_token=NULL,
			    promotion_claimed_at=NULL
			WHERE run_id=$1 AND ticker=$2 AND promotion_status='processing'
			  AND promotion_claim_token=$3`,
			claim.RunID, claim.Ticker, claim.Token, nullableUUID(item.ScannerRunID),
			at, nullableString(reason),
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
			SET promotion_status='promoted', scanner_run_id=$6, promoted_at=$7,
			    promotion_reason=$8, promotion_claim_token=NULL,
			    promotion_claimed_at=NULL
			WHERE run_id=$1 AND group_type=$2 AND group_key=$3 AND ticker=$4
			  AND promotion_status='processing' AND promotion_claim_token=$5`,
			claim.RunID, claim.GroupType, claim.GroupKey, claim.Ticker, claim.Token,
			nullableUUID(item.ScannerRunID), at, nullableString(reason),
		)
		if err != nil {
			return fmt.Errorf("complete potent industry watchlist promotion: %w", err)
		}
		if tag.RowsAffected() != 1 {
			return &ScannerSourcePromotionClaimError{Source: "potent_industry"}
		}
	}
	return tx.Commit(ctx)
}
