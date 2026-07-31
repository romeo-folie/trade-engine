package portfolio

import (
	"context"
	"time"

	"github.com/romeo-folie/trade-engine/internal/execution"
	"github.com/romeo-folie/trade-engine/internal/logger"
	"github.com/romeo-folie/trade-engine/internal/store"
)

// backfillWindow is the maximum history window fetched from Alpaca.
// "1A" = 1 year, the maximum the portfolio/history endpoint supports.
const backfillWindow = "1A"

// Backfill fetches up to one year of Alpaca equity history and seeds
// portfolio_snapshots for any calendar days that have no existing row for the
// given mode. It is idempotent — existing rows are never modified. Rows written
// by the backfill are tagged source='backfill' so they can be filtered later.
//
// Existence checks are scoped to the same mode to avoid cross-mode suppression
// (e.g. existing paper rows should not block live backfill rows for the same day).
//
// Call once after DB connect, typically at startup, in a background goroutine.
func Backfill(ctx context.Context, bv execution.BrokerView, mode, broker string) {
	if !store.IsConnected() {
		return
	}

	logger.Info("portfolio backfill: fetching Alpaca equity history (1A/1D)")
	hist, err := bv.EquityCurve(ctx, backfillWindow, "1D")
	if err != nil {
		// ErrBrokerUnavailable means IBKR — backfill not supported, silently skip.
		if err != execution.ErrBrokerUnavailable {
			logger.Warn("portfolio backfill: EquityCurve failed", "err", err)
		}
		return
	}

	// Scope the existence check to the same mode so paper and live rows don't
	// suppress each other. Query only within the backfill window to keep startup
	// work bounded.
	windowStart := time.Now().AddDate(-1, 0, 0)
	existing := map[string]struct{}{}
	rows, err := store.GetPortfolioSnapshotsByMode(ctx, windowStart, time.Now(), mode)
	if err == nil {
		for _, s := range rows {
			existing[s.CreatedAt.UTC().Truncate(24*time.Hour).Format("2006-01-02")] = struct{}{}
		}
	}

	inserted := 0
	for _, pt := range hist.Points {
		if pt.Equity <= 0 {
			continue // skip null/gap points Alpaca returns as 0
		}
		day := pt.Time.UTC().Truncate(24 * time.Hour)
		key := day.Format("2006-01-02")
		if _, ok := existing[key]; ok {
			continue // already have data for this day+mode
		}

		// Stamp the row with the history point's own timestamp. With created_at
		// defaulting to now(), a year of historical equity would land on the
		// restart day — masquerading as current values and hiding from the
		// per-day dedup above, so every restart re-inserted the whole batch.
		insertCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := store.InsertPortfolioSnapshotAt(
			insertCtx,
			pt.Time,
			pt.Equity, // total_value = equity
			0,         // cash unknown from history endpoint
			0,         // invested unknown
			0,         // unrealized_pnl unknown
			0,         // realized_pnl_today unknown
			0,         // open_positions unknown
			false,     // position statistics unavailable from equity history
			"backfill", mode, broker,
		)
		cancel()
		if err != nil {
			logger.Warn("portfolio backfill: insert failed", "date", key, "err", err)
			continue
		}
		existing[key] = struct{}{} // prevent double-insert in same run
		inserted++
	}

	if inserted > 0 {
		logger.Info("portfolio backfill: complete", "rows_inserted", inserted)
	} else {
		logger.Info("portfolio backfill: no gaps found")
	}
}
