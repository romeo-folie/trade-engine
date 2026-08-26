-- A research slot is distinct from a watchlist owner. One row can carry
-- several scanner leases and an earnings context but consumes exactly one slot.
CREATE TABLE research_queue_memberships (
    watchlist_id UUID PRIMARY KEY REFERENCES watchlist(id) ON DELETE CASCADE,
    bucket TEXT NOT NULL,
    allocation TEXT NOT NULL CHECK (allocation IN ('reserved', 'overflow')),
    priority JSONB NOT NULL DEFAULT '{}'::jsonb,
    admitted_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CHECK (btrim(bucket) <> ''),
    CHECK (jsonb_typeof(priority) = 'object')
);

CREATE INDEX idx_research_queue_memberships_bucket
    ON research_queue_memberships (bucket, allocation);

CREATE TABLE research_queue_decisions (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    watchlist_id UUID REFERENCES watchlist(id) ON DELETE SET NULL,
    ticker TEXT NOT NULL,
    bucket TEXT NOT NULL,
    decision TEXT NOT NULL CHECK (decision IN ('admitted', 'already_member', 'capacity_suppressed')),
    allocation TEXT,
    reason TEXT NOT NULL,
    priority JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CHECK (jsonb_typeof(priority) = 'object')
);

CREATE INDEX idx_research_queue_decisions_ticker_created
    ON research_queue_decisions (ticker, created_at DESC);

-- Existing live scanner rows already represent analyst work. Count them during
-- rollout but do not evict or rewrite their source provenance.
INSERT INTO research_queue_memberships (watchlist_id, bucket, allocation, priority)
SELECT w.id, 'legacy', 'overflow', '{}'::jsonb
FROM watchlist w
WHERE w.active AND w.source='scanner'
ON CONFLICT (watchlist_id) DO NOTHING;

-- Scheduled-earnings rows are ordinary scanner-source research as well. Their
-- additive context, rather than the row owner, is authoritative during the
-- rollout because an earnings context can coexist with scanner leases.
INSERT INTO research_queue_memberships (watchlist_id, bucket, allocation, priority)
SELECT DISTINCT w.id, 'earnings_scheduled', 'overflow', '{}'::jsonb
FROM watchlist w
JOIN watchlist_discovery_contexts c ON c.watchlist_id=w.id
WHERE w.active
  AND c.source='earnings_scheduled'
  AND c.active
  AND (c.expires_at IS NULL OR c.expires_at > NOW())
ON CONFLICT (watchlist_id) DO NOTHING;
