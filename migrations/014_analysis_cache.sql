-- Migration 014: analysis_cache table for context-aware LLM result reuse.
-- Fingerprint-keyed cache with TTL, use tracking, and expiry cleanup.
-- See cost-reduction PRD §7.5.

-- uuid_generate_v4() from uuid-ossp, consistent with all other migrations
-- (see 001_extensions.sql: CREATE EXTENSION IF NOT EXISTS "uuid-ossp").
CREATE TABLE IF NOT EXISTS analysis_cache (
    id              UUID        PRIMARY KEY DEFAULT uuid_generate_v4(),
    fingerprint     TEXT        UNIQUE NOT NULL,
    ticker          TEXT        NOT NULL,
    timeframe       TEXT        NOT NULL,
    strategy        TEXT        NOT NULL,
    model_profile   TEXT        NOT NULL,
    analysis_json   JSONB       NOT NULL,
    decision        TEXT        NOT NULL,
    confidence      DOUBLE PRECISION NOT NULL,
    reason_code     TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at      TIMESTAMPTZ NOT NULL,
    last_used_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    use_count       INT         NOT NULL DEFAULT 0
);

-- Ticker + timeframe + expiry index for range queries.
CREATE INDEX IF NOT EXISTS idx_analysis_cache_ticker_expires
    ON analysis_cache(ticker, timeframe, expires_at DESC);

-- Expiry sweep index.
CREATE INDEX IF NOT EXISTS idx_analysis_cache_expires
    ON analysis_cache(expires_at);
