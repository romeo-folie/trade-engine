-- Repair script: portfolio history accuracy (2026-07-16)
--
-- Context: two data defects made the dashboard disagree with Alpaca.
--
-- 1) daily_summaries 2026-04-06 recorded pnl = -103,062.51. Before Apr 6 the
--    tracker double-counted cash (~$100k phantom paper seed on top of broker
--    equity), so snapshots read ~$205k. When the fixed tracker wrote its first
--    daily summary on Apr 6, it bridged the last corrupt snapshot (205,879.18,
--    Apr 2) to honest broker equity (102,816.67) and froze a fictional -$103k
--    day — the "WORST DAY" stat scans daily_summaries and surfaced it forever.
--    Repair: re-derive starting_value from the first honest snapshot of Apr 6
--    (104,783.36 @ 13:57 UTC — the same fallback rule updateDailySummary uses)
--    and recompute pnl/pnl_pct.
--
-- 2) portfolio_snapshots holds ~7,144 rows with source='backfill' whose
--    created_at is the engine *restart* time, not the historical point's time
--    (InsertPortfolioSnapshot never passed a timestamp). Year-old equity values
--    masquerade as current ones, and the backfill's per-day dedup could never
--    see its own prior inserts, so every restart re-inserted the batch.
--    Repair: delete them all. The code fix (InsertPortfolioSnapshotAt) makes
--    the next engine startup re-backfill once with correct historical stamps.

BEGIN;

UPDATE daily_summaries
SET starting_value = 104783.36,
    pnl            = 102816.67 - 104783.36,
    pnl_pct        = (102816.67 - 104783.36) / 104783.36 * 100
WHERE date = '2026-04-06'
  AND starting_value > 200000; -- only the corrupt row, idempotent

DELETE FROM portfolio_snapshots WHERE source = 'backfill';

COMMIT;
