-- 004_knowledge_graph.sql
-- Entity graph: companies, sectors, people, and their relationships.

CREATE TABLE entities (
    id            UUID         PRIMARY KEY DEFAULT uuid_generate_v4(),
    entity_type   entity_type  NOT NULL,
    name          TEXT         NOT NULL,
    ticker        TEXT         UNIQUE,
    description   TEXT,
    sector        TEXT,
    country       TEXT,
    embedding     vector(1536),
    created_at    TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at    TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    CONSTRAINT    entities_unique UNIQUE (name, entity_type)
);

CREATE INDEX idx_entities_ticker ON entities (ticker);
CREATE INDEX idx_entities_type   ON entities (entity_type);

CREATE TABLE relationships (
    id                UUID              PRIMARY KEY DEFAULT uuid_generate_v4(),
    from_entity_id    UUID              NOT NULL REFERENCES entities(id) ON DELETE CASCADE,
    to_entity_id      UUID              NOT NULL REFERENCES entities(id) ON DELETE CASCADE,
    relationship_type relationship_type NOT NULL,
    strength          NUMERIC(3,2)      NOT NULL DEFAULT 1.0,
    description       TEXT,
    auto_generated    BOOLEAN           NOT NULL DEFAULT FALSE,
    confidence        NUMERIC(3,2),
    valid_from        TIMESTAMPTZ,
    valid_until       TIMESTAMPTZ,
    created_at        TIMESTAMPTZ       NOT NULL DEFAULT NOW(),
    CONSTRAINT relationships_unique UNIQUE (from_entity_id, to_entity_id, relationship_type)
);

CREATE INDEX idx_relationships_from ON relationships (from_entity_id);
CREATE INDEX idx_relationships_to   ON relationships (to_entity_id);

CREATE TABLE entity_mentions (
    id              UUID        PRIMARY KEY DEFAULT uuid_generate_v4(),
    entity_id       UUID        NOT NULL REFERENCES entities(id) ON DELETE CASCADE,
    news_item_id    UUID        NOT NULL REFERENCES news_items(id) ON DELETE CASCADE,
    relevance_score NUMERIC(3,2),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT entity_mentions_unique UNIQUE (entity_id, news_item_id)
);

CREATE INDEX idx_entity_mentions_entity   ON entity_mentions (entity_id);
CREATE INDEX idx_entity_mentions_news     ON entity_mentions (news_item_id);
