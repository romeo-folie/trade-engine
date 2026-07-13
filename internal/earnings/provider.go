// Package earnings supplies scheduled/historical earnings dates for the options-ORB
// earnings-blackout gate. A single shared table (migration 045) is the source of truth,
// read by both the live gate and the backtest via one query; population is out-of-band
// (Sync from Finnhub for coming reports + SeedHistorical for windows the free tier can't
// serve). The StoreProvider implements options.EarningsProvider structurally.
package earnings

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/romeo-folie/trade-engine/internal/store"
)

// StoreProvider answers "next earnings for this symbol" from the earnings_calendar table.
// The as-of instant is supplied per call by asOf(ctx): live passes wall-clock now; the
// backtest passes the replay's as-of from context — so the SAME provider serves both, and
// the blackout is computed identically live and in backtest.
type StoreProvider struct {
	db   *pgxpool.Pool
	asOf func(context.Context) time.Time
}

// NewStoreProvider builds the provider. A nil asOf defaults to wall-clock now (live use);
// the backtest passes a context-reading asOf so NextEarnings is evaluated as of the replay
// instant, never "now".
func NewStoreProvider(db *pgxpool.Pool, asOf func(context.Context) time.Time) *StoreProvider {
	if asOf == nil {
		asOf = func(context.Context) time.Time { return time.Now() }
	}
	return &StoreProvider{db: db, asOf: asOf}
}

// NextEarnings returns the next scheduled earnings date on or after the as-of day for
// underlying, or ok=false when none is stored (no imminent earnings). Satisfies
// options.EarningsProvider.
func (p *StoreProvider) NextEarnings(ctx context.Context, underlying string) (time.Time, bool) {
	return store.NextEarningsOnOrAfter(ctx, p.db, underlying, p.asOf(ctx))
}
