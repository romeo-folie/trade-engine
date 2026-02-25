-- 009_system.sql
-- Watchlist management and config change history.

CREATE TABLE watchlist (
    id             UUID        PRIMARY KEY DEFAULT uuid_generate_v4(),
    ticker         TEXT        NOT NULL,
    platform       TEXT        NOT NULL DEFAULT 'stock',
    strategies     TEXT[]      NOT NULL,
    timeframes     TEXT[],
    min_confidence NUMERIC(4,3),
    source         TEXT        NOT NULL DEFAULT 'manual',
    active         BOOLEAN     NOT NULL DEFAULT TRUE,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT watchlist_unique UNIQUE (ticker, platform)
);

CREATE INDEX idx_watchlist_active ON watchlist (active) WHERE active = TRUE;

CREATE TABLE config_history (
    id         UUID        PRIMARY KEY DEFAULT uuid_generate_v4(),
    key        TEXT        NOT NULL,
    old_value  TEXT,
    new_value  TEXT        NOT NULL,
    source     TEXT        NOT NULL,  -- 'yaml', 'db_override', 'cli'
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_config_history_key ON config_history (key, created_at DESC);
