-- Durable, quality-gated swing-universe production (#528).
--
-- Claims are stable per market session/provider/configuration. Every retry owns
-- a distinct immutable universe version so rejected evidence remains auditable.

CREATE TABLE swing_universe_build_claims (
    id                   UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    session_date         DATE NOT NULL,
    provider             TEXT NOT NULL,
    config_hash          TEXT NOT NULL,
    status               TEXT NOT NULL DEFAULT 'running'
                         CHECK (status IN ('running', 'completed', 'failed')),
    owner_token          UUID NOT NULL DEFAULT uuid_generate_v4(),
    lease_until          TIMESTAMPTZ NOT NULL,
    attempt_count        INTEGER NOT NULL DEFAULT 1 CHECK (attempt_count > 0),
    active_version_id    UUID,
    published_version_id UUID,
    last_started_at      TIMESTAMPTZ NOT NULL,
    last_finished_at     TIMESTAMPTZ,
    last_error           TEXT,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (session_date, provider, config_hash)
);

ALTER TABLE swing_universe_versions
    ADD COLUMN build_claim_id UUID REFERENCES swing_universe_build_claims(id),
    ADD COLUMN build_attempt INTEGER,
    ADD COLUMN session_date DATE,
    ADD COLUMN config_hash TEXT,
    ADD COLUMN quality_status TEXT NOT NULL DEFAULT 'unassessed'
        CHECK (quality_status IN ('unassessed', 'pending', 'approved', 'rejected')),
    ADD COLUMN quality_reason TEXT,
    ADD COLUMN evidence_required_count INTEGER NOT NULL DEFAULT 0 CHECK (evidence_required_count >= 0),
    ADD COLUMN usable_snapshot_count INTEGER NOT NULL DEFAULT 0 CHECK (usable_snapshot_count >= 0),
    ADD COLUMN usable_bar_history_count INTEGER NOT NULL DEFAULT 0 CHECK (usable_bar_history_count >= 0),
    ADD COLUMN coherent_market_data_count INTEGER NOT NULL DEFAULT 0 CHECK (coherent_market_data_count >= 0),
    ADD COLUMN fundamentals_required_count INTEGER NOT NULL DEFAULT 0 CHECK (fundamentals_required_count >= 0),
    ADD COLUMN usable_fundamentals_count INTEGER NOT NULL DEFAULT 0 CHECK (usable_fundamentals_count >= 0),
    ADD COLUMN snapshot_failure_count INTEGER NOT NULL DEFAULT 0 CHECK (snapshot_failure_count >= 0),
    ADD COLUMN bar_failure_count INTEGER NOT NULL DEFAULT 0 CHECK (bar_failure_count >= 0),
    ADD COLUMN fundamentals_failure_count INTEGER NOT NULL DEFAULT 0 CHECK (fundamentals_failure_count >= 0),
    ADD COLUMN provider_failure_count INTEGER NOT NULL DEFAULT 0 CHECK (provider_failure_count >= 0),
    ADD COLUMN partial_failure_count INTEGER NOT NULL DEFAULT 0 CHECK (partial_failure_count >= 0),
    ADD COLUMN market_data_coverage DOUBLE PRECISION NOT NULL DEFAULT 0
        CHECK (market_data_coverage >= 0 AND market_data_coverage <= 1),
    ADD COLUMN fundamentals_coverage DOUBLE PRECISION NOT NULL DEFAULT 0
        CHECK (fundamentals_coverage >= 0 AND fundamentals_coverage <= 1),
    ADD COLUMN min_market_data_coverage DOUBLE PRECISION NOT NULL DEFAULT 0
        CHECK (min_market_data_coverage >= 0 AND min_market_data_coverage <= 1);

CREATE UNIQUE INDEX idx_swing_universe_versions_build_attempt
    ON swing_universe_versions(build_claim_id, build_attempt)
    WHERE build_claim_id IS NOT NULL;

CREATE INDEX idx_swing_universe_versions_approved_as_of
    ON swing_universe_versions(as_of DESC, captured_at DESC)
    WHERE status = 'completed' AND quality_status = 'approved';

ALTER TABLE swing_universe_build_claims
    ADD CONSTRAINT swing_universe_build_claims_active_version_fk
        FOREIGN KEY (active_version_id) REFERENCES swing_universe_versions(id),
    ADD CONSTRAINT swing_universe_build_claims_published_version_fk
        FOREIGN KEY (published_version_id) REFERENCES swing_universe_versions(id);

ALTER TABLE swing_universe_versions
    ADD CONSTRAINT swing_universe_approved_quality_lineage_check CHECK (
        quality_status <> 'approved' OR (
            status = 'completed'
            AND build_claim_id IS NOT NULL
            AND build_attempt IS NOT NULL
            AND session_date IS NOT NULL
            AND config_hash IS NOT NULL
            AND min_market_data_coverage > 0
            AND market_data_coverage >= min_market_data_coverage
        )
    );

CREATE TABLE scanner_dependency_blocks (
    scanner         TEXT NOT NULL,
    session_date    DATE NOT NULL,
    decision_at     TIMESTAMPTZ NOT NULL,
    dependency      TEXT NOT NULL,
    status          TEXT NOT NULL CHECK (status IN ('blocked', 'resolved')),
    attempt_count   INTEGER NOT NULL DEFAULT 1 CHECK (attempt_count > 0),
    last_error      TEXT,
    first_blocked_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    last_blocked_at  TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    resolved_at      TIMESTAMPTZ,
    PRIMARY KEY (scanner, session_date, decision_at, dependency)
);

CREATE INDEX idx_scanner_dependency_blocks_active
    ON scanner_dependency_blocks (scanner, session_date DESC, decision_at DESC)
    WHERE status = 'blocked';
