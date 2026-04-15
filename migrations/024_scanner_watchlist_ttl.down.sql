-- 024_scanner_watchlist_ttl.down.sql
DROP INDEX IF EXISTS idx_watchlist_scanner_expiry;
ALTER TABLE watchlist DROP COLUMN IF EXISTS expires_at;
