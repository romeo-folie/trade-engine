ALTER TABLE trades
    ADD COLUMN IF NOT EXISTS exit_claimed_at TIMESTAMPTZ;

CREATE INDEX IF NOT EXISTS idx_trades_orderless_exits
    ON trades (exit_claimed_at)
    WHERE status = 'exiting';
