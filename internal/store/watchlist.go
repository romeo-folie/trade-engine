package store

import (
	"context"
	"time"
)

// WatchlistItem represents a row in the watchlist table.
type WatchlistItem struct {
	ID            string
	Ticker        string
	Platform      string // "stock"
	Strategies    []string
	Timeframes    []string
	MinConfidence float64
	Source        string // "manual" | "config" | "scanner"
	// GraphContext records WHY a scanner candidate was sourced (graph link or
	// news-sympathy trigger, migration 038) so the engine's deep-analysis
	// prompt can carry the origin thesis. Empty for manual/config items and
	// plain screener finds.
	GraphContext string
	Active       bool
	ExpiresAt    *time.Time // non-nil for scanner-sourced items
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// AddWatchlistItem inserts or re-activates a watchlist entry.
// If the ticker+platform pair already exists and was disabled, it is re-enabled.
// For scanner-sourced items, ExpiresAt is written and updated on re-activation.
func AddWatchlistItem(ctx context.Context, item WatchlistItem) error {
	if item.Platform == "" {
		item.Platform = "stock"
	}
	if item.Source == "" {
		item.Source = "manual"
	}
	_, err := GetDB().Exec(ctx, `
		INSERT INTO watchlist (ticker, platform, strategies, timeframes, min_confidence, source, graph_context, active, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, true, $8)
		ON CONFLICT (ticker, platform) DO UPDATE SET
			strategies     = EXCLUDED.strategies,
			timeframes     = EXCLUDED.timeframes,
			min_confidence = EXCLUDED.min_confidence,
			source         = EXCLUDED.source,
			graph_context  = EXCLUDED.graph_context,
			active         = true,
			expires_at     = EXCLUDED.expires_at,
			updated_at     = NOW()
	`,
		item.Ticker, item.Platform, item.Strategies, item.Timeframes,
		item.MinConfidence, item.Source, nullableString(item.GraphContext), item.ExpiresAt,
	)
	return err
}

// RemoveWatchlistItem soft-deletes a watchlist item by setting active = false.
func RemoveWatchlistItem(ctx context.Context, ticker, platform string) error {
	if platform == "" {
		platform = "stock"
	}
	_, err := GetDB().Exec(ctx, `
		UPDATE watchlist SET active = false, updated_at = NOW()
		WHERE ticker = $1 AND platform = $2
	`, ticker, platform)
	return err
}

// GetWatchlistItems returns watchlist entries. Pass activeOnly = true during engine
// execution; pass false to show all items in the CLI.
// Scanner-sourced items past their expires_at are excluded even when activeOnly = true.
func GetWatchlistItems(ctx context.Context, activeOnly bool) ([]WatchlistItem, error) {
	q := `
		SELECT id, ticker, platform, strategies, timeframes,
		       COALESCE(min_confidence, 0), source, COALESCE(graph_context, ''), active, expires_at, created_at, updated_at
		FROM watchlist
	`
	if activeOnly {
		q += " WHERE active = true AND (expires_at IS NULL OR expires_at > NOW())"
	}
	q += " ORDER BY created_at DESC"

	rows, err := GetDB().Query(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var items []WatchlistItem
	for rows.Next() {
		var w WatchlistItem
		if err := rows.Scan(
			&w.ID, &w.Ticker, &w.Platform, &w.Strategies, &w.Timeframes,
			&w.MinConfidence, &w.Source, &w.GraphContext, &w.Active, &w.ExpiresAt, &w.CreatedAt, &w.UpdatedAt,
		); err != nil {
			return nil, err
		}
		items = append(items, w)
	}
	return items, rows.Err()
}

// GetWatchlistItem returns a single watchlist entry by ticker and platform.
// Returns nil, nil if not found.
func GetWatchlistItem(ctx context.Context, ticker, platform string) (*WatchlistItem, error) {
	if platform == "" {
		platform = "stock"
	}
	row := GetDB().QueryRow(ctx, `
		SELECT id, ticker, platform, strategies, timeframes,
		       COALESCE(min_confidence, 0), source, COALESCE(graph_context, ''), active, expires_at, created_at, updated_at
		FROM watchlist
		WHERE ticker = $1 AND platform = $2
	`, ticker, platform)

	var w WatchlistItem
	err := row.Scan(
		&w.ID, &w.Ticker, &w.Platform, &w.Strategies, &w.Timeframes,
		&w.MinConfidence, &w.Source, &w.GraphContext, &w.Active, &w.ExpiresAt, &w.CreatedAt, &w.UpdatedAt,
	)
	if err != nil {
		if isNoRows(err) {
			return nil, nil
		}
		return nil, err
	}
	return &w, nil
}

// CountActiveScannerItems returns how many scanner-sourced items are currently
// active and unexpired on the watchlist. Used by the scanner to enforce the cap.
func CountActiveScannerItems(ctx context.Context) (int, error) {
	var count int
	err := GetDB().QueryRow(ctx, `
		SELECT COUNT(*) FROM watchlist
		WHERE source = 'scanner' AND active = true
		  AND (expires_at IS NULL OR expires_at > NOW())
	`).Scan(&count)
	return count, err
}

// ExpireScannedItems deactivates scanner-sourced watchlist items whose expires_at
// has passed. Returns the tickers that were expired.
func ExpireScannedItems(ctx context.Context) ([]string, error) {
	rows, err := GetDB().Query(ctx, `
		UPDATE watchlist
		SET active = false, updated_at = NOW()
		WHERE source = 'scanner' AND active = true AND expires_at IS NOT NULL AND expires_at <= NOW()
		RETURNING ticker
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var tickers []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		tickers = append(tickers, t)
	}
	return tickers, rows.Err()
}
