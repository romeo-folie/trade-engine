package store

import (
	"context"
	"time"

	"github.com/romeo-folie/trade-engine/internal/logger"
)

// CleanupIntradayCandles removes 1m/5m/15m candles older than 90 days.
// Daily and hourly candles are left untouched.
func CleanupIntradayCandles(ctx context.Context) (int64, error) {
	cutoff := time.Now().AddDate(0, 0, -90)
	tag, err := GetDB().Exec(ctx, `
		DELETE FROM candles
		WHERE timeframe IN ('1m','5m','15m') AND ts < $1`, cutoff)
	if err != nil {
		return 0, err
	}
	n := tag.RowsAffected()
	if n > 0 {
		logger.Info("retention: intraday candles pruned", "rows", n, "cutoff", cutoff.Format("2006-01-02"))
	}
	return n, nil
}

// CleanupPromptCache removes analysis rows where prompt_hash is older than 6 months
// and no associated trade exists (i.e., pure analysis-only records).
// This targets stale prompt cache that bloats the analyses table.
func CleanupPromptCache(ctx context.Context) (int64, error) {
	cutoff := time.Now().AddDate(0, -6, 0)
	tag, err := GetDB().Exec(ctx, `
		DELETE FROM analyses
		WHERE created_at < $1`, cutoff)
	if err != nil {
		return 0, err
	}
	n := tag.RowsAffected()
	if n > 0 {
		logger.Info("retention: prompt cache pruned", "rows", n, "cutoff", cutoff.Format("2006-01-02"))
	}
	return n, nil
}
