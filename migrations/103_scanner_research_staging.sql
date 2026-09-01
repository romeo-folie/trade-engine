-- Scanner candidate staging is intentionally independent of the main
-- watchlist. A row exists only while a candidate is pending admission; the
-- older research_queue_memberships table continues to represent the durable
-- scanner admission allocation attached to a main-watchlist row.
CREATE TABLE research_queue_items (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    run_id TEXT NOT NULL,
    ticker TEXT NOT NULL,
    platform TEXT NOT NULL DEFAULT 'stock',
    bucket TEXT NOT NULL,
    priority JSONB NOT NULL DEFAULT '{}'::jsonb,
    expires_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CHECK (btrim(run_id) <> ''),
    CHECK (btrim(ticker) <> ''),
    CHECK (btrim(bucket) <> ''),
    CHECK (jsonb_typeof(priority) = 'object'),
    UNIQUE (run_id, ticker, platform)
);

CREATE INDEX idx_research_queue_items_expires_at
    ON research_queue_items (expires_at);
