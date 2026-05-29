-- 031_idx_analyses_created.sql
--
-- Standalone btree index on analyses(created_at DESC) for the
-- /api/analyses/recent list endpoint. EXPLAIN before this change showed:
--   Seq Scan on analyses (1510 rows) → top-N heapsort → Limit 100
-- because the existing idx_analyses_ticker_created (ticker, created_at DESC)
-- composite cannot service a no-ticker ORDER BY created_at query. With this
-- index the planner can do an Index Scan straight off the top — the seq scan
-- + sort goes away, and the cost stays flat as the analyses table grows.
--
-- ANALYZE on the three high-traffic tables refreshes planner statistics
-- which were noticeably stale (pg_stat_user_tables.n_live_tup showed 28 for a
-- 1435-row analyses table). Stale stats lead to bad plans as the table grows.
--
-- Single-file (no .down) per the runner convention: it globs every *.sql so a
-- .down.sql would be applied forward. CREATE INDEX IF NOT EXISTS + ANALYZE are
-- idempotent and safe to re-run.

CREATE INDEX IF NOT EXISTS idx_analyses_created
    ON analyses (created_at DESC);

ANALYZE analyses;
ANALYZE signals;
ANALYZE trades;
