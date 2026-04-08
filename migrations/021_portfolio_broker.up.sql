-- 021_portfolio_broker.up.sql
-- Phase 18: add source/mode/broker tagging to portfolio_snapshots so the
-- independent poller, engine, and backfill rows can be distinguished and
-- filtered. The existing idx_portfolio_snapshots_created covers time-range
-- queries; add a covering index for mode-filtered history reads.

ALTER TABLE portfolio_snapshots
  ADD COLUMN IF NOT EXISTS source TEXT NOT NULL DEFAULT 'engine', -- 'engine' | 'poller' | 'backfill'
  ADD COLUMN IF NOT EXISTS mode   TEXT,                           -- 'live' | 'paper'
  ADD COLUMN IF NOT EXISTS broker TEXT;                           -- 'alpaca' | 'ibkr'

CREATE INDEX IF NOT EXISTS idx_portfolio_snapshots_mode
  ON portfolio_snapshots (mode, created_at DESC);
