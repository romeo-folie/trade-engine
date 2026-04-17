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

// CleanupPromptCache removes analysis rows older than 6 months that have no
// associated signal or trade (preserving the audit trail for executed trades).
func CleanupPromptCache(ctx context.Context) (int64, error) {
	cutoff := time.Now().AddDate(0, -6, 0)
	tag, err := GetDB().Exec(ctx, `
		DELETE FROM analyses a
		WHERE a.created_at < $1
		  AND NOT EXISTS (
		    SELECT 1 FROM signals s WHERE a.id = ANY(s.analysis_ids)
		  )`, cutoff)
	if err != nil {
		return 0, err
	}
	n := tag.RowsAffected()
	if n > 0 {
		logger.Info("retention: prompt cache pruned", "rows", n, "cutoff", cutoff.Format("2006-01-02"))
	}
	return n, nil
}

// CleanupPortfolioSnapshots removes snapshots older than 180 days.
func CleanupPortfolioSnapshots(ctx context.Context) (int64, error) {
	cutoff := time.Now().AddDate(0, 0, -180)
	tag, err := GetDB().Exec(ctx,
		`DELETE FROM portfolio_snapshots WHERE created_at < $1`, cutoff)
	if err != nil {
		return 0, err
	}
	n := tag.RowsAffected()
	if n > 0 {
		logger.Info("retention: portfolio snapshots pruned", "rows", n, "cutoff", cutoff.Format("2006-01-02"))
	}
	return n, nil
}

// CleanupNewsItems removes news items older than 90 days.
// Cascades to entity_mentions via ON DELETE CASCADE.
func CleanupNewsItems(ctx context.Context) (int64, error) {
	cutoff := time.Now().AddDate(0, 0, -90)
	tag, err := GetDB().Exec(ctx,
		`DELETE FROM news_items WHERE created_at < $1`, cutoff)
	if err != nil {
		return 0, err
	}
	n := tag.RowsAffected()
	if n > 0 {
		logger.Info("retention: news items pruned", "rows", n, "cutoff", cutoff.Format("2006-01-02"))
	}
	return n, nil
}

// CleanupConfigHistory removes config_history entries older than 365 days.
func CleanupConfigHistory(ctx context.Context) (int64, error) {
	cutoff := time.Now().AddDate(0, 0, -365)
	tag, err := GetDB().Exec(ctx,
		`DELETE FROM config_history WHERE created_at < $1`, cutoff)
	if err != nil {
		return 0, err
	}
	n := tag.RowsAffected()
	if n > 0 {
		logger.Info("retention: config history pruned", "rows", n, "cutoff", cutoff.Format("2006-01-02"))
	}
	return n, nil
}

// CleanupFeedbackFlags removes resolved feedback flags older than 180 days.
// Unresolved flags are always preserved regardless of age.
func CleanupFeedbackFlags(ctx context.Context) (int64, error) {
	cutoff := time.Now().AddDate(0, 0, -180)
	tag, err := GetDB().Exec(ctx,
		`DELETE FROM feedback_flags WHERE created_at < $1 AND resolved = TRUE`, cutoff)
	if err != nil {
		return 0, err
	}
	n := tag.RowsAffected()
	if n > 0 {
		logger.Info("retention: feedback flags pruned", "rows", n, "cutoff", cutoff.Format("2006-01-02"))
	}
	return n, nil
}
