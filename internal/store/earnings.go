package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNoEarningsCoverage means the symbol has NO rows in earnings_calendar at all — the
// calendar was never seeded/synced for it, so we're blind to its earnings. The gate must
// fail CLOSED on this (never trade a single name we can't earnings-check), distinct from a
// covered symbol that simply has no imminent report.
var ErrNoEarningsCoverage = errors.New("no earnings coverage for symbol")

// EarningsEvent is one scheduled or historical earnings release (migration 045).
type EarningsEvent struct {
	Symbol      string
	Date        time.Time // the announcement DATE (day-granular; the gate's blackout is day-based)
	SessionHour string    // 'bmo' | 'amc' | 'dmh' | '' (Finnhub 'hour')
	Source      string    // 'finnhub' | 'seed'
}

// UpsertEarnings records (or overwrites) one earnings date for a symbol, idempotent on
// (symbol, earnings_date). A 'seed' row (curated historical, filling the free-tier history
// gap for backtests) is AUTHORITATIVE and is never overwritten by a 'finnhub' refresh — so
// re-syncing coming earnings can't clobber a hand-verified historical date. A 'finnhub' row
// overwrites a prior 'finnhub' row (dates/timing get revised as the report nears).
func UpsertEarnings(ctx context.Context, db *pgxpool.Pool, symbol string, date time.Time, sessionHour, source string) error {
	_, err := db.Exec(ctx, `
		INSERT INTO earnings_calendar (symbol, earnings_date, session_hour, source, updated_at)
		VALUES ($1, $2::date, $3, $4, now())
		ON CONFLICT (symbol, earnings_date)
		DO UPDATE SET session_hour = EXCLUDED.session_hour, source = EXCLUDED.source, updated_at = now()
		WHERE earnings_calendar.source <> 'seed' OR EXCLUDED.source = 'seed'`,
		symbol, date, sessionHour, source)
	return err
}

// NextEarningsOnOrAfter returns the earliest earnings date on or after asOf's ET calendar
// day for symbol. Three outcomes the gate must treat differently:
//
//	(date, true, nil)   — a next earnings date was found; the gate checks the blackout.
//	(zero, false, nil)  — the symbol IS covered but has no imminent/upcoming date → SAFE:
//	                      absence within a covered calendar is real signal (public dates),
//	                      so the gate passes.
//	(zero, false, err)  — a read error, OR ErrNoEarningsCoverage (symbol never seeded/synced)
//	                      → BLIND: the gate must fail CLOSED, never trade unchecked.
//
// The date comparison is day-granular, matching the gate's N-day blackout window.
func NextEarningsOnOrAfter(ctx context.Context, db *pgxpool.Pool, symbol string, asOf time.Time) (time.Time, bool, error) {
	var d time.Time
	err := db.QueryRow(ctx, `
		SELECT earnings_date FROM earnings_calendar
		WHERE symbol = $1 AND earnings_date >= ($2 AT TIME ZONE 'America/New_York')::date
		ORDER BY earnings_date ASC
		LIMIT 1`,
		symbol, asOf).Scan(&d)
	if err == nil {
		return d, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, false, err // real read error (DB down, migration missing) → fail closed
	}
	// No upcoming date. Distinguish a covered-but-quiet calendar (safe) from a symbol with
	// zero coverage (blind → fail closed).
	var covered bool
	if cErr := db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM earnings_calendar WHERE symbol = $1)`, symbol).Scan(&covered); cErr != nil {
		return time.Time{}, false, cErr
	}
	if !covered {
		return time.Time{}, false, ErrNoEarningsCoverage
	}
	return time.Time{}, false, nil // covered, nothing imminent → safe pass
}

// GetEarnings returns a symbol's earnings dates within [from, to], oldest first (for
// inspection / sync idempotency checks).
func GetEarnings(ctx context.Context, db *pgxpool.Pool, symbol string, from, to time.Time) ([]EarningsEvent, error) {
	rows, err := db.Query(ctx, `
		SELECT symbol, earnings_date, session_hour, source
		FROM earnings_calendar
		WHERE symbol = $1 AND earnings_date >= $2::date AND earnings_date <= $3::date
		ORDER BY earnings_date ASC`,
		symbol, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []EarningsEvent
	for rows.Next() {
		var e EarningsEvent
		if err := rows.Scan(&e.Symbol, &e.Date, &e.SessionHour, &e.Source); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
