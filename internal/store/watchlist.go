package store

import (
	"context"
	"time"
)

// WatchlistItem represents a row in the watchlist table.
type WatchlistItem struct {
	ID            string
	Ticker        string
	Platform      string // "stock" | "polymarket" | "kalshi"
	Strategies    []string
	Timeframes    []string
	MinConfidence float64
	Source        string // "manual" | "config"
	Active        bool
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// AddWatchlistItem inserts or re-activates a watchlist entry.
// If the ticker+platform pair already exists and was disabled, it is re-enabled.
func AddWatchlistItem(ctx context.Context, item WatchlistItem) error {
	if item.Platform == "" {
		item.Platform = "stock"
	}
	if item.Source == "" {
		item.Source = "manual"
	}
	_, err := GetDB().Exec(ctx, `
		INSERT INTO watchlist (ticker, platform, strategies, timeframes, min_confidence, source, active)
		VALUES ($1, $2, $3, $4, $5, $6, true)
		ON CONFLICT (ticker, platform) DO UPDATE SET
			strategies    = EXCLUDED.strategies,
			timeframes    = EXCLUDED.timeframes,
			min_confidence = EXCLUDED.min_confidence,
			active        = true,
			updated_at    = NOW()
	`,
		item.Ticker, item.Platform, item.Strategies, item.Timeframes, item.MinConfidence, item.Source,
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
func GetWatchlistItems(ctx context.Context, activeOnly bool) ([]WatchlistItem, error) {
	q := `
		SELECT id, ticker, platform, strategies, timeframes,
		       COALESCE(min_confidence, 0), source, active, created_at, updated_at
		FROM watchlist
	`
	if activeOnly {
		q += " WHERE active = true"
	}
	q += " ORDER BY created_at ASC"

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
			&w.MinConfidence, &w.Source, &w.Active, &w.CreatedAt, &w.UpdatedAt,
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
		       COALESCE(min_confidence, 0), source, active, created_at, updated_at
		FROM watchlist
		WHERE ticker = $1 AND platform = $2
	`, ticker, platform)

	var w WatchlistItem
	err := row.Scan(
		&w.ID, &w.Ticker, &w.Platform, &w.Strategies, &w.Timeframes,
		&w.MinConfidence, &w.Source, &w.Active, &w.CreatedAt, &w.UpdatedAt,
	)
	if err != nil {
		if isNoRows(err) {
			return nil, nil
		}
		return nil, err
	}
	return &w, nil
}
