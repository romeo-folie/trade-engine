-- 008_backtesting.sql
-- Backtest run results, simulated trades, and prompt response cache.

CREATE TABLE backtest_runs (
    id                    UUID         PRIMARY KEY DEFAULT uuid_generate_v4(),
    label                 TEXT         NOT NULL UNIQUE,
    ticker                TEXT         NOT NULL,
    strategy              TEXT         NOT NULL,
    model_provider        TEXT         NOT NULL,
    model_name            TEXT         NOT NULL,
    from_date             DATE         NOT NULL,
    to_date               DATE         NOT NULL,
    config_snapshot       JSONB        NOT NULL,
    total_return_pct      NUMERIC(10,4),
    annualized_return_pct NUMERIC(10,4),
    sharpe_ratio          NUMERIC(8,4),
    sortino_ratio         NUMERIC(8,4),
    calmar_ratio          NUMERIC(8,4),
    max_drawdown_pct      NUMERIC(8,4),
    win_rate_pct          NUMERIC(8,4),
    profit_factor         NUMERIC(8,4),
    expectancy            NUMERIC(20,8),
    total_trades          INTEGER      NOT NULL DEFAULT 0,
    avg_hold_hours        NUMERIC(10,2),
    best_trade_pct        NUMERIC(8,4),
    worst_trade_pct       NUMERIC(8,4),
    cache_hit_rate        NUMERIC(5,2),
    created_at            TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

CREATE TABLE backtest_trades (
    id              UUID            PRIMARY KEY DEFAULT uuid_generate_v4(),
    run_id          UUID            NOT NULL REFERENCES backtest_runs(id) ON DELETE CASCADE,
    ticker          TEXT            NOT NULL,
    direction       trade_direction NOT NULL,
    entry_candle_ts TIMESTAMPTZ     NOT NULL,
    exit_candle_ts  TIMESTAMPTZ,
    entry_price     NUMERIC(20,8)   NOT NULL,
    exit_price      NUMERIC(20,8),
    quantity        NUMERIC(20,8)   NOT NULL,
    pnl             NUMERIC(20,8),
    pnl_pct         NUMERIC(10,4),
    exit_reason     TEXT,
    created_at      TIMESTAMPTZ     NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_backtest_trades_run ON backtest_trades (run_id);

CREATE TABLE prompt_cache (
    id                 UUID         PRIMARY KEY DEFAULT uuid_generate_v4(),
    prompt_hash        TEXT         NOT NULL,
    model_provider     TEXT         NOT NULL,
    model_name         TEXT         NOT NULL,
    response           TEXT         NOT NULL,
    token_count_input  INTEGER,
    token_count_output INTEGER,
    created_at         TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    CONSTRAINT prompt_cache_unique UNIQUE (prompt_hash, model_provider, model_name)
);

CREATE INDEX idx_prompt_cache_hash ON prompt_cache (prompt_hash, model_provider, model_name);
CREATE INDEX idx_prompt_cache_age  ON prompt_cache (created_at);
