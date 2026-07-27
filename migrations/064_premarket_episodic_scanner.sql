CREATE TABLE premarket_episodic_runs (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    session_date DATE NOT NULL,
    checkpoint_at TIMESTAMPTZ NOT NULL,
    hard_cutoff_at TIMESTAMPTZ NOT NULL,
    universe_version_id UUID NOT NULL REFERENCES swing_universe_versions(id),
    status TEXT NOT NULL DEFAULT 'running'
        CHECK (status IN ('running', 'completed', 'failed')),
    started_at TIMESTAMPTZ NOT NULL,
    completed_at TIMESTAMPTZ,
    lease_until TIMESTAMPTZ NOT NULL,
    attempt_count INTEGER NOT NULL DEFAULT 1,
    config_hash TEXT NOT NULL,
    config_snapshot JSONB NOT NULL DEFAULT '{}'::jsonb,
    feature_version TEXT NOT NULL,
    market_data_provider TEXT NOT NULL,
    feed TEXT NOT NULL CHECK (feed = 'sip'),
    entitlement_status TEXT NOT NULL DEFAULT 'available'
        CHECK (entitlement_status IN ('available', 'unavailable')),
    error TEXT,
    eligible_universe_count INTEGER NOT NULL DEFAULT 0,
    shortlisted_count INTEGER NOT NULL DEFAULT 0,
    evaluated_count INTEGER NOT NULL DEFAULT 0,
    qualified_count INTEGER NOT NULL DEFAULT 0,
    failure_count INTEGER NOT NULL DEFAULT 0,
    CHECK (checkpoint_at < hard_cutoff_at),
    UNIQUE (session_date, checkpoint_at, universe_version_id, config_hash)
);

CREATE INDEX idx_premarket_episodic_runs_completed
    ON premarket_episodic_runs (checkpoint_at DESC, completed_at DESC)
    WHERE status = 'completed';

CREATE TABLE premarket_episodic_candidates (
    run_id UUID NOT NULL REFERENCES premarket_episodic_runs(id) ON DELETE CASCADE,
    universe_version_id UUID NOT NULL,
    ticker TEXT NOT NULL,
    source_tag TEXT NOT NULL DEFAULT 'premarket_episodic'
        CHECK (source_tag = 'premarket_episodic'),
    outcome TEXT NOT NULL CHECK (outcome IN ('qualified', 'not_qualified', 'failed')),
    removal_stage TEXT,
    failure_reason TEXT,
    score DOUBLE PRECISION,
    rank INTEGER,
    gap_pct DOUBLE PRECISION,
    premarket_volume BIGINT,
    time_adjusted_rvol DOUBLE PRECISION,
    dollar_volume DOUBLE PRECISION,
    trade_count BIGINT,
    spread_bps DOUBLE PRECISION,
    quote_age_seconds DOUBLE PRECISION,
    bar_coverage DOUBLE PRECISION,
    premarket_high DOUBLE PRECISION,
    premarket_low DOUBLE PRECISION,
    premarket_location DOUBLE PRECISION,
    last_price DOUBLE PRECISION,
    previous_close DOUBLE PRECISION,
    market_cap_mm DOUBLE PRECISION,
    market_cap_risk TEXT,
    float_status TEXT NOT NULL DEFAULT 'unavailable',
    adr_14_pct DOUBLE PRECISION,
    natr_14_pct DOUBLE PRECISION,
    catalyst_status TEXT NOT NULL
        CHECK (catalyst_status IN ('confirmed', 'confirmed_absent', 'provider_unavailable')),
    catalyst_reference JSONB NOT NULL DEFAULT '{}'::jsonb,
    raw_evidence JSONB NOT NULL DEFAULT '{}'::jsonb,
    thresholds JSONB NOT NULL DEFAULT '{}'::jsonb,
    promotion_status TEXT NOT NULL DEFAULT 'not_requested'
        CHECK (promotion_status IN (
            'not_requested', 'pending', 'processing', 'promoted', 'rejected',
            'active_watchlist', 'cap_suppressed', 'failed', 'superseded'
        )),
    promotion_claim_token TEXT,
    promotion_claimed_at TIMESTAMPTZ,
    scanner_run_id UUID REFERENCES scanner_runs(id),
    promoted_at TIMESTAMPTZ,
    promotion_reason TEXT,
    CHECK (score IS NULL OR score BETWEEN 0 AND 1),
    CHECK (bar_coverage IS NULL OR bar_coverage BETWEEN 0 AND 1),
    CHECK (premarket_location IS NULL OR premarket_location BETWEEN 0 AND 1),
    CHECK (outcome <> 'qualified' OR (
        score IS NOT NULL AND rank IS NOT NULL AND last_price IS NOT NULL
    )),
    PRIMARY KEY (run_id, ticker),
    FOREIGN KEY (universe_version_id, ticker)
        REFERENCES swing_universe_members(universe_version_id, ticker)
);

CREATE INDEX idx_premarket_episodic_pending
    ON premarket_episodic_candidates (run_id, rank, ticker)
    WHERE outcome = 'qualified' AND promotion_status = 'pending';

ALTER TABLE scanner_candidate_snapshots
    ADD COLUMN premarket_episodic_evidence TEXT;

COMMENT ON TABLE premarket_episodic_runs IS
    'Idempotent, point-in-time pre-open discovery observations fenced by a hard opening-range cutoff.';
COMMENT ON TABLE premarket_episodic_candidates IS
    'Raw premarket evidence and removal lineage; qualified rows may only enter the ordinary scanner funnel.';
COMMENT ON COLUMN scanner_candidate_snapshots.premarket_episodic_evidence IS
    'Immutable premarket gap, participation, liquidity, volatility, and catalyst summary carried into scanner lineage.';
