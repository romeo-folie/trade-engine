package earnings

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/romeo-folie/trade-engine/internal/data"
	"github.com/romeo-folie/trade-engine/internal/store"
)

// Fetcher is the subset of data.FinnhubClient this package needs (interface for testing).
type Fetcher interface {
	FetchEarningsCalendar(ctx context.Context, symbol string, from, to time.Time) ([]data.EarningsRelease, error)
}

// Sync pulls each symbol's earnings releases in [from, to] from Finnhub and upserts them
// as source='finnhub'. Free tier serves ~1 month history + all upcoming, so a live/near
// window returns the coming reports the gate needs; historical windows come back empty
// (seed those via SeedHistorical). Returns the number of rows upserted. A per-symbol fetch
// error aborts (the caller decides whether a partial sync is acceptable).
func Sync(ctx context.Context, db *pgxpool.Pool, f Fetcher, symbols []string, from, to time.Time) (int, error) {
	n := 0
	for _, sym := range symbols {
		releases, err := f.FetchEarningsCalendar(ctx, sym, from, to)
		if err != nil {
			return n, fmt.Errorf("fetch earnings for %s: %w", sym, err)
		}
		for _, r := range releases {
			if err := store.UpsertEarnings(ctx, db, r.Symbol, r.Date, r.Hour, "finnhub"); err != nil {
				return n, fmt.Errorf("upsert earnings %s %s: %w", r.Symbol, r.Date.Format("2006-01-02"), err)
			}
			n++
		}
	}
	return n, nil
}

// HistoricalSeed holds hand-curated earnings dates the Finnhub free tier (~1 month history)
// cannot serve, so backtests over past windows have a blackout calendar. These are public
// announcement dates; VERIFY against the issuer's record before trusting backtest P&L —
// a wrong date lets a trade slip a real print or blacks out a clean day.
var HistoricalSeed = map[string][]string{
	// NVIDIA — reports after market close (amc). Fiscal quarters end Jan/Apr/Jul/Oct.
	"NVDA": {
		"2024-02-21", "2024-05-22", "2024-08-28", "2024-11-20",
		"2025-02-26", "2025-05-28", "2025-08-27", "2025-11-19",
	},
	// Tesla — reports after market close (amc).
	"TSLA": {
		"2024-01-24", "2024-04-23", "2024-07-23", "2024-10-23",
		"2025-01-29", "2025-04-22", "2025-07-23", "2025-10-22",
	},
}

// SeedHistorical upserts the curated HistoricalSeed dates as source='seed' (authoritative —
// a later Finnhub sync will not overwrite them). Idempotent. Returns rows written.
func SeedHistorical(ctx context.Context, db *pgxpool.Pool) (int, error) {
	n := 0
	for sym, dates := range HistoricalSeed {
		for _, ds := range dates {
			d, err := time.Parse("2006-01-02", ds)
			if err != nil {
				return n, fmt.Errorf("bad seed date %q for %s: %w", ds, sym, err)
			}
			if err := store.UpsertEarnings(ctx, db, sym, d, "amc", "seed"); err != nil {
				return n, fmt.Errorf("seed %s %s: %w", sym, ds, err)
			}
			n++
		}
	}
	return n, nil
}
