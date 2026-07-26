package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

var (
	ErrWatchlistRequalificationRequired = errors.New("watchlist item requires pullback requalification")
	// ErrWatchlistActiveItemExists means a fallback write lost a race to an
	// already-active watchlist entry. The caller must leave that newer entry
	// untouched rather than replacing its strategy or scanner lineage.
	ErrWatchlistActiveItemExists = errors.New("active watchlist item already exists")
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
	// ScannerRunID links scanner-sourced rows to their prospective universe
	// snapshot. Empty for manual/config watchlist items.
	ScannerRunID string
	Active       bool
	ExpiresAt    *time.Time // non-nil for scanner-sourced items
	// PullbackRequalificationRequired quarantines legacy scanner rows whose
	// pullback-universe status was not persisted before migration 055.
	PullbackRequalificationRequired bool
	// PullbackRequalificationComplete is a write-only acknowledgement from the
	// scanner that all objective universe inputs were available. It permits a
	// quarantined row to be reactivated as either qualified or standard.
	PullbackRequalificationComplete bool
	CreatedAt                       time.Time
	UpdatedAt                       time.Time
}

// AddWatchlistItem inserts or re-activates a watchlist entry.
// If the ticker+platform pair already exists and was disabled, it is re-enabled.
// For scanner-sourced items, ExpiresAt is written and updated on re-activation.
func AddWatchlistItem(ctx context.Context, item WatchlistItem) error {
	return addWatchlistItem(ctx, GetDB(), item, false)
}

// AddWatchlistItemUnlessActive inserts a watchlist item, or reactivates an
// inactive/expired item, without replacing a currently active item. It is used
// after a weekly-leader claim loss, where another worker may have promoted the
// same ticker between the failed fenced promotion and the ordinary fallback.
func AddWatchlistItemUnlessActive(ctx context.Context, item WatchlistItem) error {
	return addWatchlistItem(ctx, GetDB(), item, true)
}

type watchlistExecer interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}

func addWatchlistItem(
	ctx context.Context, db watchlistExecer, item WatchlistItem,
	preserveActive bool,
) error {
	if item.Platform == "" {
		item.Platform = "stock"
	}
	if item.Source == "" {
		item.Source = "manual"
	}
	tag, err := db.Exec(ctx, `
		INSERT INTO watchlist (
			ticker, platform, strategies, timeframes, min_confidence, source,
			graph_context, scanner_run_id, active, expires_at
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, true, $9)
		ON CONFLICT (ticker, platform) DO UPDATE SET
			strategies     = EXCLUDED.strategies,
			timeframes     = EXCLUDED.timeframes,
			min_confidence = EXCLUDED.min_confidence,
			source         = EXCLUDED.source,
			graph_context  = EXCLUDED.graph_context,
			scanner_run_id = EXCLUDED.scanner_run_id,
			active         = true,
			expires_at     = EXCLUDED.expires_at,
			pullback_requalification_required = false,
			updated_at     = NOW()
		WHERE NOT (
			watchlist.pullback_requalification_required
			AND EXCLUDED.source = 'scanner'
			AND NOT $10
		)
		AND NOT (
			$11
			AND watchlist.active
			AND (watchlist.expires_at IS NULL OR watchlist.expires_at > NOW())
		)
	`,
		item.Ticker, item.Platform, item.Strategies, item.Timeframes,
		item.MinConfidence, item.Source, nullableString(item.GraphContext),
		nullableString(item.ScannerRunID), item.ExpiresAt,
		item.PullbackRequalificationComplete, preserveActive,
	)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		if preserveActive {
			var currentlyActive bool
			err := GetDB().QueryRow(ctx, `
				SELECT active AND (expires_at IS NULL OR expires_at > NOW())
				FROM watchlist
				WHERE ticker=$1 AND platform=$2
			`, item.Ticker, item.Platform).Scan(&currentlyActive)
			if err == nil && currentlyActive {
				return ErrWatchlistActiveItemExists
			}
		}
		return ErrWatchlistRequalificationRequired
	}
	return nil
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
		       COALESCE(min_confidence, 0), source, COALESCE(graph_context, ''),
		       COALESCE(scanner_run_id::text, ''), active, expires_at,
		       pullback_requalification_required, created_at, updated_at
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
			&w.MinConfidence, &w.Source, &w.GraphContext, &w.ScannerRunID,
			&w.Active, &w.ExpiresAt,
			&w.PullbackRequalificationRequired, &w.CreatedAt, &w.UpdatedAt,
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
		       COALESCE(min_confidence, 0), source, COALESCE(graph_context, ''),
		       COALESCE(scanner_run_id::text, ''), active, expires_at,
		       pullback_requalification_required, created_at, updated_at
		FROM watchlist
		WHERE ticker = $1 AND platform = $2
	`, ticker, platform)

	var w WatchlistItem
	err := row.Scan(
		&w.ID, &w.Ticker, &w.Platform, &w.Strategies, &w.Timeframes,
		&w.MinConfidence, &w.Source, &w.GraphContext, &w.ScannerRunID,
		&w.Active, &w.ExpiresAt,
		&w.PullbackRequalificationRequired, &w.CreatedAt, &w.UpdatedAt,
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
