CREATE TABLE weekly_leader_runs (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    session_date DATE NOT NULL,
    as_of TIMESTAMPTZ NOT NULL,
    decision_at TIMESTAMPTZ NOT NULL,
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
    benchmark TEXT NOT NULL,
    benchmark_bar_set_hash TEXT,
    history_provider TEXT NOT NULL,
    error TEXT,
    eligible_universe_count INTEGER NOT NULL DEFAULT 0,
    evaluated_count INTEGER NOT NULL DEFAULT 0,
    qualified_count INTEGER NOT NULL DEFAULT 0,
    failure_count INTEGER NOT NULL DEFAULT 0,
    added_count INTEGER NOT NULL DEFAULT 0,
    removed_count INTEGER NOT NULL DEFAULT 0,
    persistent_count INTEGER NOT NULL DEFAULT 0,
    leader_breadth DOUBLE PRECISION NOT NULL DEFAULT 0,
    evaluation_coverage DOUBLE PRECISION NOT NULL DEFAULT 0,
    failure_rate DOUBLE PRECISION NOT NULL DEFAULT 0,
    previous_completed_run_id UUID REFERENCES weekly_leader_runs(id),
    UNIQUE (session_date, universe_version_id, config_hash)
);

CREATE INDEX idx_weekly_leader_runs_completed_as_of
    ON weekly_leader_runs (as_of DESC)
    WHERE status = 'completed';

CREATE TABLE weekly_leader_qualifications (
    run_id UUID NOT NULL REFERENCES weekly_leader_runs(id) ON DELETE CASCADE,
    universe_version_id UUID NOT NULL,
    ticker TEXT NOT NULL,
    outcome TEXT NOT NULL CHECK (outcome IN ('qualified', 'not_qualified', 'failed')),
    failure_stage TEXT,
    failure_reason TEXT,
    history_first_at TIMESTAMPTZ,
    history_last_at TIMESTAMPTZ,
    history_sessions INTEGER NOT NULL DEFAULT 0,
    bar_set_hash TEXT,
    source_evidence JSONB NOT NULL DEFAULT '{}'::jsonb,
    sector TEXT NOT NULL DEFAULT 'Unknown',
    industry TEXT NOT NULL DEFAULT 'Unknown',
    market_cap_band TEXT NOT NULL DEFAULT 'unknown',
    volatility_band TEXT NOT NULL DEFAULT 'unknown',
    close_price DOUBLE PRECISION CHECK (close_price > 0),
    return_5d DOUBLE PRECISION,
    return_20d DOUBLE PRECISION,
    return_60d DOUBLE PRECISION,
    return_126d DOUBLE PRECISION,
    high_52w_proximity DOUBLE PRECISION,
    market_relative_strength DOUBLE PRECISION,
    industry_relative_strength DOUBLE PRECISION,
    rs_acceleration DOUBLE PRECISION,
    regression_annualized_slope DOUBLE PRECISION,
    regression_r2 DOUBLE PRECISION,
    efficiency_60d DOUBLE PRECISION,
    max_drawdown_126d DOUBLE PRECISION,
    natr_14 DOUBLE PRECISION,
    volatility_adjusted_return DOUBLE PRECISION,
    median_dollar_volume_20d DOUBLE PRECISION,
    dollar_volume_stability_20d DOUBLE PRECISION,
    normalized_obv_20d DOUBLE PRECISION,
    cmf_20d DOUBLE PRECISION,
    gap_domination_60d DOUBLE PRECISION,
    composite_score DOUBLE PRECISION,
    score_percentile DOUBLE PRECISION,
    rank INTEGER,
    feature_coverage DOUBLE PRECISION,
    features JSONB NOT NULL DEFAULT '{}'::jsonb,
    components JSONB NOT NULL DEFAULT '{}'::jsonb,
    was_qualified_previous BOOLEAN NOT NULL DEFAULT false,
    qualification_streak INTEGER NOT NULL DEFAULT 0,
    qualified_since DATE,
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
    CHECK (outcome <> 'qualified' OR close_price IS NOT NULL),
    PRIMARY KEY (run_id, ticker),
    FOREIGN KEY (universe_version_id, ticker)
        REFERENCES swing_universe_members(universe_version_id, ticker)
);

CREATE INDEX idx_weekly_leader_qualifications_latest
    ON weekly_leader_qualifications (ticker, run_id)
    WHERE outcome = 'qualified';

CREATE INDEX idx_weekly_leader_pending_promotions
    ON weekly_leader_qualifications (run_id, rank, ticker)
    WHERE outcome = 'qualified' AND promotion_status = 'pending';

CREATE TABLE weekly_leader_transitions (
    run_id UUID NOT NULL REFERENCES weekly_leader_runs(id) ON DELETE CASCADE,
    ticker TEXT NOT NULL,
    event TEXT NOT NULL CHECK (event IN ('added', 'persistent', 'removed')),
    previous_run_id UUID REFERENCES weekly_leader_runs(id),
    previous_score DOUBLE PRECISION,
    current_score DOUBLE PRECISION,
    previous_rank INTEGER,
    current_rank INTEGER,
    reason TEXT NOT NULL,
    PRIMARY KEY (run_id, ticker, event)
);

CREATE TABLE weekly_leader_breadth (
    run_id UUID NOT NULL REFERENCES weekly_leader_runs(id) ON DELETE CASCADE,
    dimension_type TEXT NOT NULL,
    dimension_value TEXT NOT NULL,
    eligible_count INTEGER NOT NULL,
    evaluated_count INTEGER NOT NULL,
    qualified_count INTEGER NOT NULL,
    failed_count INTEGER NOT NULL,
    added_count INTEGER NOT NULL DEFAULT 0,
    removed_count INTEGER NOT NULL DEFAULT 0,
    persistent_count INTEGER NOT NULL DEFAULT 0,
    breadth DOUBLE PRECISION NOT NULL,
    coverage DOUBLE PRECISION NOT NULL,
    PRIMARY KEY (run_id, dimension_type, dimension_value)
);

COMMENT ON TABLE weekly_leader_qualifications IS
    'One immutable evaluation per eligible point-in-time universe member; only qualified rows may enter the ordinary scanner funnel.';
COMMENT ON COLUMN weekly_leader_runs.leader_breadth IS
    'Qualified names divided by the full eligible universe denominator, never only successfully evaluated names.';
COMMENT ON COLUMN weekly_leader_qualifications.bar_set_hash IS
    'SHA-256 over the normalized, ordered completed daily OHLCV inputs.';
COMMENT ON COLUMN weekly_leader_qualifications.close_price IS
    'Final close of the required target-session bar used by this evaluation.';
COMMENT ON COLUMN weekly_leader_runs.benchmark_bar_set_hash IS
    'SHA-256 over the normalized benchmark bars used for relative strength.';
COMMENT ON COLUMN weekly_leader_runs.as_of IS
    'Official completed-session close bounding all daily bars.';
COMMENT ON COLUMN weekly_leader_runs.decision_at IS
    'Fixed point-in-time decision cutoff bounding universe and completion visibility.';
