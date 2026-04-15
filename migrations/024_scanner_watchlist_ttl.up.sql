-- 024_scanner_watchlist_ttl.up.sql
-- Adds expires_at to watchlist to support scanner-sourced items with automatic TTL.
-- Scanner-added rows (source = 'scanner') are deactivated by ExpireScannedItems()
-- once expires_at passes without a signal firing on that ticker.

ALTER TABLE watchlist ADD COLUMN expires_at TIMESTAMPTZ;

CREATE INDEX idx_watchlist_scanner_expiry
    ON watchlist (expires_at)
    WHERE source = 'scanner' AND active = TRUE;
