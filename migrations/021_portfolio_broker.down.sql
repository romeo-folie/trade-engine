-- 021_portfolio_broker.down.sql

DROP INDEX IF EXISTS idx_portfolio_snapshots_mode;

ALTER TABLE portfolio_snapshots
  DROP COLUMN IF EXISTS source,
  DROP COLUMN IF EXISTS mode,
  DROP COLUMN IF EXISTS broker;
