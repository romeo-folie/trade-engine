package store

import (
	"context"
	"time"
)

// AccountRiskBaseline separates broker identities. A paper reset or a live
// deployment must start with its own peak and daily reference rather than
// inheriting another account's circuit-breaker state.
type AccountRiskBaseline struct {
	AccountID    string
	PeakEquity   float64
	LastEquity   float64
	BaselineDate time.Time
	UpdatedAt    time.Time
}

// ObserveAccountRiskBaseline atomically initializes or advances one account's
// peak using the broker snapshot itself as the peak observation. Callers that
// retain a higher in-memory observation across a transient DB failure should
// use ObserveAccountRiskBaselineWithPeak.
func ObserveAccountRiskBaseline(ctx context.Context, accountID string, equity, dailyReference float64, observedAt time.Time) (AccountRiskBaseline, error) {
	return ObserveAccountRiskBaselineWithPeak(ctx, accountID, equity, equity, dailyReference, observedAt, observedAt)
}

// ObserveAccountRiskBaselineWithPeak atomically initializes or advances one account's
// peak. peakEquity and peakObservedAt may carry a higher in-memory observation
// retained across a transient database failure; dailyReference and observedAt
// always describe the current broker snapshot. A new
// account uses the broker's supplied prior equity when available, while keeping
// its peak scoped to that account. If that prior-session value is unavailable,
// only initialization falls back to current equity; existing baselines remain
// fixed until Alpaca supplies a real reference. Existing accounts adopt a
// changed broker reference even if the process observes the rollover before
// UTC midnight.
func ObserveAccountRiskBaselineWithPeak(ctx context.Context, accountID string, equity, peakEquity, dailyReference float64, observedAt, peakObservedAt time.Time) (AccountRiskBaseline, error) {
	if peakEquity < equity {
		peakEquity = equity
		peakObservedAt = observedAt
	}
	if peakObservedAt.IsZero() {
		peakObservedAt = observedAt
	}
	date := observedAt.UTC().Truncate(24 * time.Hour)
	initialDailyReference := dailyReference
	if initialDailyReference <= 0 {
		initialDailyReference = equity
	}
	var baseline AccountRiskBaseline
	err := GetDB().QueryRow(ctx, `
		INSERT INTO account_risk_baselines (account_id, peak_equity, last_equity, baseline_date, manual_reset_reference, manual_reset_at, last_observed_at)
		VALUES ($1, GREATEST($2::numeric, $5::numeric), $5, $3, NULL, NULL, $6)
		ON CONFLICT (account_id) DO UPDATE
		-- A supplied daily reference is only an initialization aid. On an
		-- existing row, advancing the peak from it would undo an explicit
		-- operator reset after a deposit or withdrawal.
			SET peak_equity = CASE
				-- A response that began before an accepted newer observation cannot
				-- change this account baseline.
				WHEN $6::timestamptz < account_risk_baselines.last_observed_at
					THEN account_risk_baselines.peak_equity
				WHEN account_risk_baselines.manual_reset_at IS NOT NULL
					AND $6::timestamptz < account_risk_baselines.manual_reset_at
					THEN account_risk_baselines.peak_equity
				-- A retained process-local high can predate a manual reset even
				-- though the broker snapshot is newer. Retain only current equity.
				WHEN account_risk_baselines.manual_reset_at IS NOT NULL
					AND $8::timestamptz < account_risk_baselines.manual_reset_at
					THEN GREATEST(account_risk_baselines.peak_equity, $2::numeric)
				ELSE GREATEST(account_risk_baselines.peak_equity, $7::numeric)
			END,
			last_equity = CASE
					-- A reset marker pins the previous broker reference only while
					-- Alpaca is still returning that exact stale value. As soon as
					-- it changes, accept it even if an earlier stale poll happened
					-- on this same UTC date.
						WHEN $6::timestamptz < account_risk_baselines.last_observed_at
							THEN account_risk_baselines.last_equity
						WHEN account_risk_baselines.manual_reset_at IS NOT NULL
							AND $6::timestamptz < account_risk_baselines.manual_reset_at
							THEN account_risk_baselines.last_equity
						WHEN account_risk_baselines.manual_reset_reference IS NOT NULL
						AND account_risk_baselines.manual_reset_reference <> $4::numeric
						AND $4::numeric > 0
						THEN $4
					WHEN account_risk_baselines.manual_reset_reference IS NULL
						AND $4::numeric > 0
						AND (
							account_risk_baselines.baseline_date < EXCLUDED.baseline_date
							OR account_risk_baselines.last_equity <> $4::numeric
						)
						THEN $4
					ELSE account_risk_baselines.last_equity
				END,
				-- Do not consume a new date while the marked broker reference is
				-- still stale. A later poll on that date must be able to adopt the
				-- broker's new session reference.
				baseline_date = CASE
						WHEN $6::timestamptz < account_risk_baselines.last_observed_at
							THEN account_risk_baselines.baseline_date
						WHEN account_risk_baselines.manual_reset_at IS NOT NULL
							AND $6::timestamptz < account_risk_baselines.manual_reset_at
							THEN account_risk_baselines.baseline_date
						WHEN account_risk_baselines.manual_reset_reference IS NOT NULL
						AND account_risk_baselines.manual_reset_reference = $4::numeric
						THEN account_risk_baselines.baseline_date
					ELSE GREATEST(account_risk_baselines.baseline_date, EXCLUDED.baseline_date)
				END,
			manual_reset_reference = CASE
					WHEN $6::timestamptz < account_risk_baselines.last_observed_at
					THEN account_risk_baselines.manual_reset_reference
					WHEN account_risk_baselines.manual_reset_at IS NOT NULL
						AND $6::timestamptz < account_risk_baselines.manual_reset_at
					THEN account_risk_baselines.manual_reset_reference
					WHEN account_risk_baselines.manual_reset_reference IS NOT NULL
					AND account_risk_baselines.manual_reset_reference <> $4::numeric
					AND $4::numeric > 0
				THEN NULL
				ELSE account_risk_baselines.manual_reset_reference
			END,
			-- Keep this watermark after the broker's prior-session reference
			-- advances. A request that began before the reset may complete after a
			-- newer poll has already cleared manual_reset_reference; it must still
			-- be unable to restore the retired high-water mark.
			manual_reset_at = account_risk_baselines.manual_reset_at,
			last_observed_at = GREATEST(account_risk_baselines.last_observed_at, $6::timestamptz),
			updated_at = NOW()
		RETURNING account_id, peak_equity, last_equity, baseline_date, updated_at`,
		accountID, equity, date, dailyReference, initialDailyReference, observedAt.UTC(), peakEquity, peakObservedAt.UTC(),
	).Scan(&baseline.AccountID, &baseline.PeakEquity, &baseline.LastEquity, &baseline.BaselineDate, &baseline.UpdatedAt)
	return baseline, err
}

// ResetAccountRiskBaseline explicitly starts a new high-water mark for an
// account after an operator-confirmed capital reset, deposit, or withdrawal.
// Broker account snapshots do not identify cash flows, so inferring one from a
// balance drop could incorrectly erase a real drawdown circuit breaker.
func ResetAccountRiskBaseline(ctx context.Context, accountID string, equity, dailyReference float64, now time.Time) (AccountRiskBaseline, error) {
	date := now.UTC().Truncate(24 * time.Hour)
	var baseline AccountRiskBaseline
	err := GetDB().QueryRow(ctx, `
		INSERT INTO account_risk_baselines (account_id, peak_equity, last_equity, baseline_date, manual_reset_reference, manual_reset_at, last_observed_at)
			VALUES ($1, $2, $2, $3, $4, $5, $5)
		ON CONFLICT (account_id) DO UPDATE
		SET peak_equity = EXCLUDED.peak_equity,
			last_equity = EXCLUDED.last_equity,
			baseline_date = EXCLUDED.baseline_date,
			-- The broker's prior-session reference can predate this capital
			-- change. Keep the reset baseline until that broker value advances;
			-- a zero prior-session value is unavailable, not a new baseline.
				manual_reset_reference = EXCLUDED.manual_reset_reference,
			manual_reset_at = EXCLUDED.manual_reset_at,
			last_observed_at = EXCLUDED.last_observed_at,
			updated_at = NOW()
		RETURNING account_id, peak_equity, last_equity, baseline_date, updated_at`,
		accountID, equity, date, dailyReference, now.UTC(),
	).Scan(&baseline.AccountID, &baseline.PeakEquity, &baseline.LastEquity, &baseline.BaselineDate, &baseline.UpdatedAt)
	return baseline, err
}

// GetAccountRiskBaseline returns the current account-scoped circuit-breaker
// state without mutating it. Operator readiness uses this same source as BUY
// admission rather than historical, account-agnostic portfolio snapshots.
func GetAccountRiskBaseline(ctx context.Context, accountID string) (*AccountRiskBaseline, error) {
	var baseline AccountRiskBaseline
	err := GetDB().QueryRow(ctx, `
		SELECT account_id, peak_equity, last_equity, baseline_date, updated_at
		FROM account_risk_baselines WHERE account_id=$1`, accountID,
	).Scan(&baseline.AccountID, &baseline.PeakEquity, &baseline.LastEquity, &baseline.BaselineDate, &baseline.UpdatedAt)
	if IsNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &baseline, nil
}
