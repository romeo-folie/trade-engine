package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// OptionDailyIV is one row of option_iv_history (migration 040): an underlying's
// at-the-money implied volatility on one date.
type OptionDailyIV struct {
	Date   time.Time
	ATMIV  float64 // annualized IV as a fraction (0.18 = 18%)
	Source string  // "derived" | "live"
}

// UpsertOptionIV records (or overwrites) the ATM IV for an underlying on a date.
// Re-running the bounded derivation or the daily snapshot is idempotent.
func UpsertOptionIV(ctx context.Context, db *pgxpool.Pool, underlying string, date time.Time, atmIV float64, source string) error {
	// Source priority: a 'live' snapshot is the real observed IV for that day and
	// must not be overwritten by a re-run of the 'derived' Black-Scholes backfill.
	// So overwrite unless the existing row is 'live' and the incoming row is not.
	_, err := db.Exec(ctx, `
		INSERT INTO option_iv_history (underlying, date, atm_iv, source)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (underlying, date)
		DO UPDATE SET atm_iv = EXCLUDED.atm_iv, source = EXCLUDED.source
		WHERE option_iv_history.source <> 'live' OR EXCLUDED.source = 'live'`,
		underlying, date, atmIV, source)
	return err
}

// GetOptionIVHistory returns an underlying's daily ATM IV over [from, to],
// oldest first. The range is inclusive on the date column.
func GetOptionIVHistory(ctx context.Context, db *pgxpool.Pool, underlying string, from, to time.Time) ([]OptionDailyIV, error) {
	rows, err := db.Query(ctx, `
		SELECT date, atm_iv, source
		FROM option_iv_history
		WHERE underlying = $1 AND date >= $2::date AND date <= $3::date
		ORDER BY date ASC`,
		underlying, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []OptionDailyIV
	for rows.Next() {
		var d OptionDailyIV
		if err := rows.Scan(&d.Date, &d.ATMIV, &d.Source); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}
