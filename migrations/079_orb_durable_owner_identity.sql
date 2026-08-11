-- Durable ORB ownership and futures P&L identity.
--
-- owner_scope is intentionally independent of the current instrument config: an
-- open position must keep an owner after its symbol is removed from tomorrow's
-- entry universe. point_value is separate from the integer options multiplier;
-- futures such as MYM require fractional dollars-per-point (0.50).

ALTER TABLE orb_sessions
    ADD COLUMN IF NOT EXISTS owner_scope TEXT NOT NULL DEFAULT 'equity_orb';

ALTER TABLE orb_trades
    ADD COLUMN IF NOT EXISTS owner_scope TEXT NOT NULL DEFAULT 'equity_orb';

ALTER TABLE orb_trades
    ADD COLUMN IF NOT EXISTS point_value NUMERIC(20,8);

UPDATE orb_sessions
SET owner_scope = CASE
        WHEN session_name = 'futures' AND EXISTS (
            SELECT 1 FROM orb_trades t WHERE t.session_id = orb_sessions.id
              AND t.ticker IN ('GC','MGC','SI','SIL')
        ) AND NOT EXISTS (
            SELECT 1 FROM orb_trades t WHERE t.session_id = orb_sessions.id
              AND t.ticker NOT IN ('GC','MGC','SI','SIL')
        ) THEN 'futures_metals_orb'
        WHEN session_name = 'futures' AND EXISTS (
            SELECT 1 FROM orb_trades t WHERE t.session_id = orb_sessions.id
              AND t.ticker IN ('ES','MES','NQ','MNQ','YM','MYM','RTY','M2K')
        ) AND NOT EXISTS (
            SELECT 1 FROM orb_trades t WHERE t.session_id = orb_sessions.id
              AND t.ticker NOT IN ('ES','MES','NQ','MNQ','YM','MYM','RTY','M2K')
        ) THEN 'futures_index_orb'
        -- A mixed or unknown-root legacy futures session has no trustworthy
        -- single owner. Keep the default equity_orb marker so the explicit
        -- repair workflow must resolve it instead of guessing.
        WHEN session_name = 'futures' THEN owner_scope
        WHEN session_name IN ('london', 'newyork', 'both', 'sydney', 'tokyo') THEN 'forex_orb'
        ELSE 'equity_orb'
    END,
    asset_class = CASE
        WHEN session_name = 'futures' THEN 'futures'
        WHEN session_name IN ('london', 'newyork', 'both', 'sydney', 'tokyo') THEN 'forex'
        ELSE COALESCE(NULLIF(asset_class, ''), 'equity')
    END;

UPDATE orb_trades t
SET owner_scope = CASE
        WHEN t.asset_class = 'option' THEN 'options_orb'
        WHEN (t.asset_class = 'futures' OR s.session_name = 'futures')
          AND t.ticker IN ('GC','MGC','SI','SIL') THEN 'futures_metals_orb'
        WHEN (t.asset_class = 'futures' OR s.session_name = 'futures')
          AND t.ticker IN ('ES','MES','NQ','MNQ','YM','MYM','RTY','M2K') THEN 'futures_index_orb'
        WHEN t.asset_class = 'futures' OR s.session_name = 'futures' THEN t.owner_scope
        WHEN t.asset_class = 'forex'
          OR s.session_name IN ('london', 'newyork', 'both', 'sydney', 'tokyo')
          OR t.ticker LIKE '%/%' THEN 'forex_orb'
        ELSE 'equity_orb'
    END,
    asset_class = CASE
        WHEN t.asset_class = 'option' THEN 'option'
        WHEN t.asset_class = 'futures' OR s.session_name = 'futures' THEN 'futures'
        WHEN t.asset_class = 'forex'
          OR s.session_name IN ('london', 'newyork', 'both', 'sydney', 'tokyo')
          OR t.ticker LIKE '%/%' THEN 'forex'
        ELSE COALESCE(NULLIF(t.asset_class, ''), 'equity')
    END,
    point_value = CASE
        WHEN t.asset_class = 'futures' OR s.session_name = 'futures' THEN t.point_value
        ELSE COALESCE(t.point_value, 1)
    END
FROM orb_sessions s
WHERE s.id = t.session_id;

ALTER TABLE orb_sessions DROP CONSTRAINT IF EXISTS orb_sessions_owner_scope_check;
ALTER TABLE orb_sessions ADD CONSTRAINT orb_sessions_owner_scope_check
    CHECK (owner_scope IN ('equity_orb', 'forex_orb', 'futures_index_orb', 'futures_metals_orb', 'options_orb'));

ALTER TABLE orb_trades DROP CONSTRAINT IF EXISTS orb_trades_owner_scope_check;
ALTER TABLE orb_trades ADD CONSTRAINT orb_trades_owner_scope_check
    CHECK (owner_scope IN ('equity_orb', 'forex_orb', 'futures_index_orb', 'futures_metals_orb', 'options_orb'));

ALTER TABLE orb_trades DROP CONSTRAINT IF EXISTS orb_trades_point_value_check;
ALTER TABLE orb_trades ADD CONSTRAINT orb_trades_point_value_check
    CHECK (point_value IS NULL OR point_value > 0);

-- Index and metals ORBs use the same legacy session_name ("futures") but are
-- independent owners and may both create a session on the same trading date.
DROP INDEX IF EXISTS idx_orb_sessions_date_session;
CREATE UNIQUE INDEX IF NOT EXISTS idx_orb_sessions_date_session_owner
    ON orb_sessions (date, session_name, owner_scope);

CREATE INDEX IF NOT EXISTS idx_orb_trades_owner_open
    ON orb_trades (owner_scope, status, created_at);
