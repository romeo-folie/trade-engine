-- Keep the dashboard's current-day terminal-exit totals bounded as the
-- strategy ledgers grow. These partial indexes match the three predicates used
-- by GetRealizedPnLByDateRange.

CREATE INDEX IF NOT EXISTS idx_trades_closed_pnl_at
    ON trades (closed_at)
    WHERE status = 'closed';

CREATE INDEX IF NOT EXISTS idx_orb_trades_closed_pnl_at
    ON orb_trades (closed_at)
    WHERE status = 'closed';

CREATE INDEX IF NOT EXISTS idx_currency_strength_exited_pnl_at
    ON currency_strength_runs (exited_at)
    WHERE status = 'exited';
