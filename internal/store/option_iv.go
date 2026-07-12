package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// OptionDailyIV is one row of option_iv_history (migrations 040/042): an underlying's
// at-the-money implied volatility, stamped at the sample INSTANT (as_of).
type OptionDailyIV struct {
	Date   time.Time // the sample instant (as_of) — instant-precise for as-of filtering
	ATMIV  float64   // annualized IV as a fraction (0.18 = 18%)
	Source string    // "derived" | "live"
}

// UpsertOptionIV records (or overwrites) the ATM IV sampled at asOf. The ET calendar
// day is derived from asOf and is the uniqueness key (one row per day), so re-running
// with a different sample minute overwrites rather than duplicating. Idempotent.
func UpsertOptionIV(ctx context.Context, db *pgxpool.Pool, underlying string, asOf time.Time, atmIV float64, source string) error {
	// Source priority: a 'live' snapshot is the real observed IV for that day and
	// must not be overwritten by a re-run of the 'derived' Black-Scholes backfill.
	// So overwrite unless the existing row is 'live' and the incoming row is not.
	// The day (PK) is asOf's ET calendar date; as_of keeps the precise instant.
	_, err := db.Exec(ctx, `
		INSERT INTO option_iv_history (underlying, date, as_of, atm_iv, source)
		VALUES ($1, ($2 AT TIME ZONE 'America/New_York')::date, $2, $3, $4)
		ON CONFLICT (underlying, date)
		DO UPDATE SET atm_iv = EXCLUDED.atm_iv, source = EXCLUDED.source, as_of = EXCLUDED.as_of
		WHERE option_iv_history.source <> 'live' OR EXCLUDED.source = 'live'`,
		underlying, asOf, atmIV, source)
	return err
}

// GetOptionIVHistory returns an underlying's ATM IV over [from, to], oldest first,
// filtered on the sample INSTANT (as_of) so a same-day sample taken after `to` is
// excluded — the lookahead-safe behavior the IV-Rank gate needs.
func GetOptionIVHistory(ctx context.Context, db *pgxpool.Pool, underlying string, from, to time.Time) ([]OptionDailyIV, error) {
	rows, err := db.Query(ctx, `
		SELECT as_of, atm_iv, source
		FROM option_iv_history
		WHERE underlying = $1 AND as_of >= $2 AND as_of <= $3
		ORDER BY as_of ASC`,
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
