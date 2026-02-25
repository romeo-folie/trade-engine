-- 007_portfolio.sql
-- Portfolio value snapshots and daily performance summaries.

CREATE TABLE portfolio_snapshots (
    id                  UUID         PRIMARY KEY DEFAULT uuid_generate_v4(),
    total_value         NUMERIC(20,8) NOT NULL,
    cash                NUMERIC(20,8) NOT NULL,
    invested            NUMERIC(20,8) NOT NULL,
    unrealized_pnl      NUMERIC(20,8) NOT NULL,
    realized_pnl_today  NUMERIC(20,8) NOT NULL DEFAULT 0,
    open_positions      INTEGER       NOT NULL DEFAULT 0,
    created_at          TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_portfolio_snapshots_created ON portfolio_snapshots (created_at DESC);

CREATE TABLE daily_summaries (
    id                  UUID         PRIMARY KEY DEFAULT uuid_generate_v4(),
    date                DATE         NOT NULL UNIQUE,
    starting_value      NUMERIC(20,8) NOT NULL,
    ending_value        NUMERIC(20,8) NOT NULL,
    pnl                 NUMERIC(20,8) NOT NULL,
    pnl_pct             NUMERIC(10,4),
    trades_opened       INTEGER      NOT NULL DEFAULT 0,
    trades_closed       INTEGER      NOT NULL DEFAULT 0,
    wins                INTEGER      NOT NULL DEFAULT 0,
    losses              INTEGER      NOT NULL DEFAULT 0,
    signals_generated   INTEGER      NOT NULL DEFAULT 0,
    signals_blocked     INTEGER      NOT NULL DEFAULT 0,
    model_tokens_input  BIGINT       NOT NULL DEFAULT 0,
    model_tokens_output BIGINT       NOT NULL DEFAULT 0,
    created_at          TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_daily_summaries_date ON daily_summaries (date DESC);
