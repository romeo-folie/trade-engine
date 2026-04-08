package portfolio

import (
	"context"
	"time"

	"github.com/romeo-folie/trade-engine/internal/execution"
	"github.com/romeo-folie/trade-engine/internal/logger"
	"github.com/romeo-folie/trade-engine/internal/store"
)

// Backfill fetches up to one year of Alpaca equity history and seeds
// portfolio_snapshots for any calendar days that have no existing row.
// It is idempotent — existing rows are never modified. Rows written by the
// backfill are tagged source='backfill' so they can be filtered later.
//
// Call once after DB connect, typically at startup, in a background goroutine.
func Backfill(ctx context.Context, bv execution.BrokerView, mode, broker string) {
	if !store.IsConnected() {
		return
	}

	logger.Info("portfolio backfill: fetching Alpaca equity history (1Y/1D)")
	hist, err := bv.EquityCurve(ctx, "all", "1D")
	if err != nil {
		// ErrBrokerUnavailable means IBKR — backfill not supported, silently skip.
		if err != execution.ErrBrokerUnavailable {
			logger.Warn("portfolio backfill: EquityCurve failed", "err", err)
		}
		return
	}

	// Load the set of existing snapshot dates (truncated to day) so we only
	// insert rows for gaps.
	existing := map[string]struct{}{}
	rows, err := store.GetPortfolioSnapshotsByMode(ctx, time.Time{}, time.Now(), "")
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
			continue // already have data for this day
		}

		// Use a context derived from the parent but not timed-out aggressively
		// since this is a batch insert that may take a few seconds.
		insertCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := store.InsertPortfolioSnapshot(
			insertCtx,
			pt.Equity,          // total_value = equity
			0,                  // cash unknown from history endpoint
			0,                  // invested unknown
			0,                  // unrealized_pnl unknown
			0,                  // realized_pnl_today unknown
			0,                  // open_positions unknown
			"backfill", mode, broker,
		)
		cancel()
		if err != nil {
			logger.Warn("portfolio backfill: insert failed", "date", key, "err", err)
			continue
		}
		existing[key] = struct{}{} // mark to prevent double-insert in same run
		inserted++
	}

	if inserted > 0 {
		logger.Info("portfolio backfill: complete", "rows_inserted", inserted)
	} else {
		logger.Info("portfolio backfill: no gaps found")
	}
}
