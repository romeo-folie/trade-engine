CREATE TABLE currency_strength_runs (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    session_date DATE NOT NULL UNIQUE,
    status TEXT NOT NULL DEFAULT 'scheduled'
        CHECK (status IN (
            'scheduled', 'evaluating', 'no_signal', 'entered',
            'exit_submitted', 'exited', 'failed'
        )),
    timezone TEXT NOT NULL,
    strength_start TEXT NOT NULL,
    strength_end TEXT NOT NULL,
    exit_time TEXT NOT NULL,
    rank_instruments TEXT[] NOT NULL DEFAULT '{}',
    trade_instruments TEXT[] NOT NULL DEFAULT '{}',
    units INTEGER NOT NULL CHECK (units > 0),
    strongest_currency TEXT,
    weakest_currency TEXT,
    strength_gap DOUBLE PRECISION,
    signal_symbol TEXT,
    signal_side TEXT CHECK (signal_side IS NULL OR signal_side IN ('buy', 'sell')),
    skip_reason TEXT,
    entry_order_id TEXT,
    entry_status TEXT,
    entry_fill_price DOUBLE PRECISION,
    entry_filled_qty DOUBLE PRECISION,
    entered_at TIMESTAMPTZ,
    exit_order_id TEXT,
    exit_status TEXT,
    exit_fill_price DOUBLE PRECISION,
    exit_filled_qty DOUBLE PRECISION,
    exited_at TIMESTAMPTZ,
    realized_pnl DOUBLE PRECISION,
    error TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_currency_strength_runs_recent
    ON currency_strength_runs (session_date DESC, created_at DESC);

COMMENT ON TABLE currency_strength_runs IS
    'Durable daily decision and execution history for the London currency-strength strategy.';
