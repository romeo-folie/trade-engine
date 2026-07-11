-- Options-ORB (Issue #410): option-contract identity on orb_trades.
--
-- asset_class already exists on orb_trades/orb_sessions (migration 018) and now
-- also carries the value 'option'. These columns identify the specific option
-- contract an options-ORB trade holds; they stay NULL / default for equity,
-- forex, and futures trades, so existing rows and read paths are unaffected.
--
-- multiplier is the contract multiplier (100 for standard US equity options,
-- 1 for shares/forex/futures units) and makes P&L math instrument-correct.
ALTER TABLE orb_trades ADD COLUMN IF NOT EXISTS underlying  TEXT;
ALTER TABLE orb_trades ADD COLUMN IF NOT EXISTS expiry      DATE;
ALTER TABLE orb_trades ADD COLUMN IF NOT EXISTS strike      NUMERIC;
ALTER TABLE orb_trades ADD COLUMN IF NOT EXISTS "right"     TEXT;   -- 'C' | 'P'
ALTER TABLE orb_trades ADD COLUMN IF NOT EXISTS occ_symbol  TEXT;
ALTER TABLE orb_trades ADD COLUMN IF NOT EXISTS multiplier  INTEGER NOT NULL DEFAULT 1;

-- Look up an option trade by its OCC symbol (the broker's contract key). Used by
-- the options reconciler (Issue #416) to match broker option positions to DB rows.
CREATE INDEX IF NOT EXISTS idx_orb_trades_occ_symbol ON orb_trades (occ_symbol) WHERE occ_symbol IS NOT NULL;
