ALTER TABLE orb_trades
    ADD COLUMN IF NOT EXISTS tp_order_id TEXT,
    ADD COLUMN IF NOT EXISTS sl_order_id TEXT,
    ADD COLUMN IF NOT EXISTS exit_price  NUMERIC(18,8),
    ADD COLUMN IF NOT EXISTS exit_type   TEXT; -- 'tp' | 'stop' | 'expired'
