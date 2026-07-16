-- 047_snapshot_retention_exempt_backfill.sql
-- Align the ad-hoc cleanup function with internal/store/retention.go: the
-- 180-day snapshot retention must not prune source='backfill' rows. Backfill
-- rows are the compact one-row-per-day historical equity record (stamped at
-- the point's own time since PR #457); deleting them truncates the 1Y/ALL
-- portfolio chart and makes the startup backfill re-fetch and re-insert the
-- pruned span on every restart. Retention targets the high-frequency
-- poller/engine rows, which dominate volume.
CREATE OR REPLACE FUNCTION cleanup_portfolio_snapshots()
RETURNS INTEGER
LANGUAGE plpgsql AS $$
DECLARE
    deleted INTEGER;
BEGIN
    DELETE FROM portfolio_snapshots
    WHERE created_at < NOW() - INTERVAL '180 days'
      AND source <> 'backfill';
    GET DIAGNOSTICS deleted = ROW_COUNT;
    RETURN deleted;
END;
$$;
