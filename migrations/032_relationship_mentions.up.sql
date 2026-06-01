-- 032_relationship_mentions.up.sql
-- Tracks which news items corroborate each auto-extracted relationship edge.
--
-- The knowledge enricher extracts company relationships from individual news
-- articles. A single edge (e.g. "TSMC supplies_to NVDA") may be asserted by many
-- separate articles over time. Counting DISTINCT corroborating news items lets
-- downstream consumers (the scanner's graph-neighbour sourcing) require an edge to
-- be seen in >= N independent articles before trusting it as a candidate source.
--
-- The UNIQUE (relationship_id, news_item_id) constraint makes inserts idempotent:
-- re-processing the same article on restart cannot inflate the corroboration count.

CREATE TABLE relationship_mentions (
    id              UUID        PRIMARY KEY DEFAULT uuid_generate_v4(),
    relationship_id UUID        NOT NULL REFERENCES relationships(id) ON DELETE CASCADE,
    news_item_id    UUID        NOT NULL REFERENCES news_items(id)    ON DELETE CASCADE,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT relationship_mentions_unique UNIQUE (relationship_id, news_item_id)
);

CREATE INDEX idx_relationship_mentions_rel ON relationship_mentions (relationship_id);
