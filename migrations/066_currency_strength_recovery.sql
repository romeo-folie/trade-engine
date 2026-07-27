ALTER TABLE currency_strength_runs
    ADD COLUMN account_id TEXT NOT NULL DEFAULT '';

ALTER TABLE currency_strength_runs
    DROP CONSTRAINT currency_strength_runs_session_date_key;

ALTER TABLE currency_strength_runs
    ADD CONSTRAINT currency_strength_runs_account_session_key
        UNIQUE (account_id, session_date);

CREATE TABLE currency_strength_exit_attempts (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    run_id UUID NOT NULL REFERENCES currency_strength_runs(id) ON DELETE CASCADE,
    broker_order_id TEXT NOT NULL,
    status TEXT NOT NULL,
    fill_price DOUBLE PRECISION,
    filled_qty DOUBLE PRECISION NOT NULL DEFAULT 0,
    exited_at TIMESTAMPTZ,
    realized_pnl DOUBLE PRECISION,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (run_id, broker_order_id)
);

CREATE INDEX idx_currency_strength_exit_attempts_run
    ON currency_strength_exit_attempts (run_id, created_at);

COMMENT ON COLUMN currency_strength_runs.account_id IS
    'IBKR account that owns this strategy run; blank only for pre-migration history.';

COMMENT ON TABLE currency_strength_exit_attempts IS
    'Idempotent broker exit acknowledgements used to preserve cumulative fills and P&L across residual retries.';
