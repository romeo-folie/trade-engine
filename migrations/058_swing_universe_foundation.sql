-- Point-in-time swing-universe and taxonomy foundation.
--
-- Universe versions are append-only evidence. A version may be populated while
-- it is building, but its members become immutable once the version completes
-- or fails. Historical scanner rows intentionally keep NULL references.

CREATE TABLE swing_universe_versions (
    id                      UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    provider                TEXT NOT NULL,
    as_of                   TIMESTAMPTZ NOT NULL,
    captured_at             TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    completed_at            TIMESTAMPTZ,
    status                  TEXT NOT NULL DEFAULT 'building'
                            CHECK (status IN ('building', 'completed', 'failed')),
    error                   TEXT,
    filters                 JSONB NOT NULL DEFAULT '{}'::jsonb,
    source_evidence         JSONB NOT NULL DEFAULT '{}'::jsonb,
    member_count            INTEGER NOT NULL DEFAULT 0,
    eligible_count          INTEGER NOT NULL DEFAULT 0,
    unknown_taxonomy_count  INTEGER NOT NULL DEFAULT 0,
    stale_taxonomy_count    INTEGER NOT NULL DEFAULT 0,
    created_at              TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (provider, as_of, captured_at)
);

CREATE INDEX idx_swing_universe_versions_as_of
    ON swing_universe_versions(as_of DESC, captured_at DESC)
    WHERE status = 'completed';

CREATE TABLE swing_universe_members (
    universe_version_id UUID NOT NULL REFERENCES swing_universe_versions(id) ON DELETE CASCADE,
    ticker              TEXT NOT NULL,
    eligible            BOOLEAN NOT NULL,
    removal_stage       TEXT,
    failure_stage       TEXT,
    failure_reason      TEXT,
    asset_id            TEXT,
    name                TEXT,
    security_type       TEXT,
    exchange            TEXT,
    active              BOOLEAN,
    tradable            BOOLEAN,
    price               NUMERIC,
    market_cap_mm       NUMERIC,
    dollar_volume       NUMERIC,
    spread_bps          NUMERIC,
    data_available      BOOLEAN,
    sector              TEXT NOT NULL DEFAULT 'Unknown',
    industry            TEXT NOT NULL DEFAULT 'Unknown',
    taxonomy_status     TEXT NOT NULL DEFAULT 'unknown'
                        CHECK (taxonomy_status IN ('known', 'unknown', 'stale', 'unavailable')),
    taxonomy_as_of      TIMESTAMPTZ,
    taxonomy_provider   TEXT,
    taxonomy_evidence   JSONB NOT NULL DEFAULT '{}'::jsonb,
    source_evidence     JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (universe_version_id, ticker),
    CHECK (sector <> ''),
    CHECK (industry <> '')
);

CREATE INDEX idx_swing_universe_members_eligible
    ON swing_universe_members(universe_version_id, ticker)
    WHERE eligible;
CREATE INDEX idx_swing_universe_members_industry
    ON swing_universe_members(universe_version_id, industry)
    WHERE eligible AND taxonomy_status = 'known';

CREATE OR REPLACE FUNCTION guard_swing_universe_member_mutation()
RETURNS TRIGGER AS $$
DECLARE
    parent_status TEXT;
BEGIN
    SELECT status INTO parent_status
    FROM swing_universe_versions
    WHERE id = COALESCE(NEW.universe_version_id, OLD.universe_version_id);
    IF parent_status IS DISTINCT FROM 'building' THEN
        RAISE EXCEPTION 'swing universe version is immutable after building';
    END IF;
    RETURN COALESCE(NEW, OLD);
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER swing_universe_members_building_only
BEFORE INSERT OR UPDATE OR DELETE ON swing_universe_members
FOR EACH ROW EXECUTE FUNCTION guard_swing_universe_member_mutation();

CREATE TABLE company_taxonomy_observations (
    id                  UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    ticker              TEXT NOT NULL,
    provider            TEXT NOT NULL,
    effective_at        TIMESTAMPTZ NOT NULL,
    observed_at         TIMESTAMPTZ NOT NULL,
    sector              TEXT NOT NULL DEFAULT 'Unknown',
    industry            TEXT NOT NULL DEFAULT 'Unknown',
    status              TEXT NOT NULL
                        CHECK (status IN ('known', 'unknown', 'unavailable')),
    evidence            JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (ticker, provider, effective_at, observed_at),
    CHECK (sector <> ''),
    CHECK (industry <> '')
);

CREATE INDEX idx_company_taxonomy_observations_as_of
    ON company_taxonomy_observations(ticker, effective_at DESC, observed_at DESC);

ALTER TABLE scanner_runs
    ADD COLUMN universe_version_id UUID REFERENCES swing_universe_versions(id) ON DELETE SET NULL,
    ADD COLUMN universe_as_of TIMESTAMPTZ,
    ADD COLUMN regime_reference TEXT;

ALTER TABLE scanner_candidate_snapshots
    ADD COLUMN source_scores JSONB NOT NULL DEFAULT '{}'::jsonb,
    ADD COLUMN universe_member BOOLEAN,
    ADD COLUMN sector TEXT,
    ADD COLUMN industry TEXT,
    ADD COLUMN taxonomy_status TEXT
        CHECK (taxonomy_status IS NULL OR taxonomy_status IN ('known', 'unknown', 'stale', 'unavailable')),
    ADD COLUMN taxonomy_as_of TIMESTAMPTZ,
    ADD COLUMN taxonomy_provider TEXT,
    ADD COLUMN taxonomy_evidence JSONB NOT NULL DEFAULT '{}'::jsonb,
    ADD COLUMN removal_stage TEXT,
    ADD COLUMN failure_stage TEXT,
    ADD COLUMN failure_reason TEXT;

CREATE INDEX idx_scanner_runs_universe_version
    ON scanner_runs(universe_version_id);
