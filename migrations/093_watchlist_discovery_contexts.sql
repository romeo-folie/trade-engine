-- Durable, independently expiring discovery reasons for a single watchlist
-- ticker. The legacy columns on watchlist remain the compatibility projection
-- for existing scanner promotions; new sources can attach context without
-- overwriting another source's watchlist ownership or expiry.
CREATE TABLE IF NOT EXISTS watchlist_discovery_contexts (
    id           UUID        PRIMARY KEY DEFAULT uuid_generate_v4(),
    watchlist_id UUID        NOT NULL REFERENCES watchlist(id) ON DELETE CASCADE,
    context_key  TEXT        NOT NULL,
    source       TEXT        NOT NULL,
    context      JSONB       NOT NULL,
    expires_at   TIMESTAMPTZ,
    active       BOOLEAN     NOT NULL DEFAULT TRUE,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT watchlist_discovery_contexts_context_object
        CHECK (jsonb_typeof(context) = 'object'),
    CONSTRAINT watchlist_discovery_contexts_unique
        UNIQUE (watchlist_id, context_key)
);

CREATE INDEX IF NOT EXISTS idx_watchlist_discovery_contexts_active
    ON watchlist_discovery_contexts (watchlist_id, expires_at)
    WHERE active = TRUE;

COMMENT ON TABLE watchlist_discovery_contexts IS
    'One-to-many, independently expiring discovery provenance supplied to ordinary recurring analysis.';

CREATE TABLE IF NOT EXISTS earnings_watchlist_runs (
    target_session_date DATE PRIMARY KEY,
    status              TEXT        NOT NULL CHECK (status IN ('running','completed','failed')),
    attempt_id          UUID        NOT NULL DEFAULT uuid_generate_v4(),
    lease_until         TIMESTAMPTZ NOT NULL,
    last_error          TEXT,
    completed_at        TIMESTAMPTZ,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS earnings_watchlist_candidates (
    target_session_date DATE NOT NULL REFERENCES earnings_watchlist_runs(target_session_date) ON DELETE CASCADE,
    ticker              TEXT NOT NULL,
    liquidity_rank      INT  NOT NULL,
    median_dollar_volume NUMERIC NOT NULL,
    selected_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (target_session_date, ticker)
);
