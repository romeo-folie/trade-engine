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
// peak. dailyReference is normally the broker's previous-session equity; it is
// applied once per UTC date. A new account starts from current equity so it is
// never blocked by historical state from a different broker identity.
func ObserveAccountRiskBaseline(ctx context.Context, accountID string, equity, dailyReference float64, now time.Time) (AccountRiskBaseline, error) {
	date := now.UTC().Truncate(24 * time.Hour)
	if dailyReference <= 0 {
		dailyReference = equity
	}
	var baseline AccountRiskBaseline
	err := GetDB().QueryRow(ctx, `
		INSERT INTO account_risk_baselines (account_id, peak_equity, last_equity, baseline_date)
		VALUES ($1, $2, $2, $3)
		ON CONFLICT (account_id) DO UPDATE
		SET peak_equity = GREATEST(account_risk_baselines.peak_equity, EXCLUDED.peak_equity),
			last_equity = CASE WHEN account_risk_baselines.baseline_date < EXCLUDED.baseline_date
				THEN $4 ELSE account_risk_baselines.last_equity END,
			baseline_date = GREATEST(account_risk_baselines.baseline_date, EXCLUDED.baseline_date),
			updated_at = NOW()
		RETURNING account_id, peak_equity, last_equity, baseline_date, updated_at`,
		accountID, equity, date, dailyReference,
	).Scan(&baseline.AccountID, &baseline.PeakEquity, &baseline.LastEquity, &baseline.BaselineDate, &baseline.UpdatedAt)
	return baseline, err
}
