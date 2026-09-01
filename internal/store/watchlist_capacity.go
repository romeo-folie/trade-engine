package store

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

const mainWatchlistLivePredicate = `
	active AND (
		(source = 'earnings_watchlist' AND EXISTS (
			SELECT 1 FROM watchlist_discovery_contexts c
			WHERE c.watchlist_id=watchlist.id AND c.active=true
			  AND (c.expires_at IS NULL OR c.expires_at > clock_timestamp())
		)) OR
		(source = 'scanner' AND (
			EXISTS (
				SELECT 1 FROM watchlist_source_leases l
				WHERE l.watchlist_id=watchlist.id AND l.expires_at > clock_timestamp()
			) OR
			(NOT EXISTS (
				SELECT 1 FROM watchlist_source_leases l
				WHERE l.watchlist_id=watchlist.id
			) AND (expires_at IS NULL OR expires_at > clock_timestamp())) OR
			EXISTS (
				SELECT 1 FROM watchlist_discovery_contexts c
				WHERE c.watchlist_id=watchlist.id AND c.active=true
				  AND (c.expires_at IS NULL OR c.expires_at > clock_timestamp())
			)
		)) OR
		(source <> 'scanner' AND source <> 'earnings_watchlist' AND (
			(expires_at IS NULL OR expires_at > clock_timestamp()) OR EXISTS (
				SELECT 1 FROM watchlist_discovery_contexts c
				WHERE c.watchlist_id=watchlist.id AND c.active=true
				  AND (c.expires_at IS NULL OR c.expires_at > clock_timestamp())
			)
		))
	)`

// reserveMainWatchlistSlotTx serializes new main-watchlist admissions. An
// already-live entry is a refresh and therefore does not consume another slot;
// an expired physical row does consume one because the write revives it.
func reserveMainWatchlistSlotTx(ctx context.Context, tx pgx.Tx, item WatchlistItem, capacity int) error {
	if capacity <= 0 {
		return nil
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('main-watchlist:v1', 0))`); err != nil {
		return fmt.Errorf("lock main watchlist capacity: %w", err)
	}
	platform := strings.TrimSpace(item.Platform)
	if platform == "" {
		platform = "stock"
	}
	ticker := strings.ToUpper(strings.TrimSpace(item.Ticker))
	var alreadyLive bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM watchlist
			WHERE ticker=$1 AND platform=$2 AND `+mainWatchlistLivePredicate+`
		)`, ticker, platform).Scan(&alreadyLive); err != nil {
		return err
	}
	if alreadyLive {
		return nil
	}
	var used int
	if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM watchlist WHERE `+mainWatchlistLivePredicate).Scan(&used); err != nil {
		return err
	}
	if used >= capacity {
		return ErrWatchlistCapacity
	}
	return nil
}
