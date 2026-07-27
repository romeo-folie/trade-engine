CREATE TABLE industry_rotation_runs (
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
    market_benchmark TEXT NOT NULL,
    benchmark_evidence JSONB NOT NULL DEFAULT '{}'::jsonb,
    history_provider TEXT NOT NULL,
    error TEXT,
    eligible_group_count INTEGER NOT NULL DEFAULT 0,
    evaluated_group_count INTEGER NOT NULL DEFAULT 0,
    potent_group_count INTEGER NOT NULL DEFAULT 0,
    failed_group_count INTEGER NOT NULL DEFAULT 0,
    eligible_member_count INTEGER NOT NULL DEFAULT 0,
    evaluated_member_count INTEGER NOT NULL DEFAULT 0,
    selected_member_count INTEGER NOT NULL DEFAULT 0,
    failed_member_count INTEGER NOT NULL DEFAULT 0,
    added_group_count INTEGER NOT NULL DEFAULT 0,
    removed_group_count INTEGER NOT NULL DEFAULT 0,
    persistent_group_count INTEGER NOT NULL DEFAULT 0,
    group_coverage DOUBLE PRECISION NOT NULL DEFAULT 0,
    member_coverage DOUBLE PRECISION NOT NULL DEFAULT 0,
    previous_completed_run_id UUID REFERENCES industry_rotation_runs(id),
    CHECK (eligible_group_count >= 0 AND evaluated_group_count >= 0
           AND evaluated_group_count <= eligible_group_count),
    CHECK (potent_group_count >= 0 AND potent_group_count <= evaluated_group_count),
    CHECK (failed_group_count >= 0 AND failed_group_count <= eligible_group_count),
    CHECK (eligible_member_count >= 0 AND evaluated_member_count >= 0
           AND evaluated_member_count <= eligible_member_count),
    CHECK (selected_member_count >= 0 AND selected_member_count <= evaluated_member_count),
    CHECK (failed_member_count >= 0 AND failed_member_count <= eligible_member_count),
    CHECK (group_coverage BETWEEN 0 AND 1),
    CHECK (member_coverage BETWEEN 0 AND 1),
    UNIQUE (session_date, universe_version_id, config_hash)
);

CREATE INDEX idx_industry_rotation_runs_completed_as_of
    ON industry_rotation_runs (as_of DESC)
    WHERE status = 'completed';

CREATE TABLE industry_rotation_groups (
    run_id UUID NOT NULL REFERENCES industry_rotation_runs(id) ON DELETE CASCADE,
    group_type TEXT NOT NULL CHECK (group_type IN ('industry', 'sector')),
    group_key TEXT NOT NULL,
    sector TEXT NOT NULL DEFAULT 'Unknown',
    industry TEXT NOT NULL DEFAULT 'Unknown',
    benchmark TEXT NOT NULL,
    outcome TEXT NOT NULL CHECK (outcome IN ('qualified', 'not_qualified', 'failed')),
    archetype TEXT NOT NULL CHECK (
        archetype IN ('broad_strong', 'strong_narrow', 'broad_weak', 'weak')
    ),
    failure_stage TEXT,
    failure_reason TEXT,
    eligible_member_count INTEGER NOT NULL DEFAULT 0,
    evaluated_member_count INTEGER NOT NULL DEFAULT 0,
    fresh_taxonomy_count INTEGER NOT NULL DEFAULT 0,
    member_coverage DOUBLE PRECISION NOT NULL DEFAULT 0,
    fresh_taxonomy_coverage DOUBLE PRECISION NOT NULL DEFAULT 0,
    equal_weight_return_1d DOUBLE PRECISION,
    equal_weight_return_5d DOUBLE PRECISION,
    equal_weight_return_20d DOUBLE PRECISION,
    cap_weight_return_1d DOUBLE PRECISION,
    cap_weight_return_5d DOUBLE PRECISION,
    cap_weight_return_20d DOUBLE PRECISION,
    median_member_return_1d DOUBLE PRECISION,
    median_member_return_5d DOUBLE PRECISION,
    median_member_return_20d DOUBLE PRECISION,
    market_relative_return_1d DOUBLE PRECISION,
    market_relative_return_5d DOUBLE PRECISION,
    market_relative_return_20d DOUBLE PRECISION,
    sector_relative_return_1d DOUBLE PRECISION,
    sector_relative_return_5d DOUBLE PRECISION,
    sector_relative_return_20d DOUBLE PRECISION,
    advance_share DOUBLE PRECISION,
    advance_decline DOUBLE PRECISION,
    up_volume_share DOUBLE PRECISION,
    pct_above_ema20 DOUBLE PRECISION,
    pct_above_ema50 DOUBLE PRECISION,
    pct_at_20d_high DOUBLE PRECISION,
    pct_at_52w_high DOUBLE PRECISION,
    median_rvol_20d DOUBLE PRECISION,
    median_dollar_volume_expansion DOUBLE PRECISION,
    breadth_acceleration DOUBLE PRECISION,
    cap_equal_divergence DOUBLE PRECISION,
    persistence_sessions INTEGER NOT NULL DEFAULT 0,
    potency_score DOUBLE PRECISION,
    score_percentile DOUBLE PRECISION,
    rank INTEGER,
    feature_coverage DOUBLE PRECISION,
    group_evidence JSONB NOT NULL DEFAULT '{}'::jsonb,
    components JSONB NOT NULL DEFAULT '{}'::jsonb,
    was_potent_previous BOOLEAN NOT NULL DEFAULT false,
    potency_streak INTEGER NOT NULL DEFAULT 0,
    potent_since DATE,
    CHECK (eligible_member_count >= 0 AND evaluated_member_count >= 0
           AND evaluated_member_count <= eligible_member_count),
    CHECK (fresh_taxonomy_count >= 0
           AND fresh_taxonomy_count <= eligible_member_count),
    CHECK (member_coverage BETWEEN 0 AND 1),
    CHECK (fresh_taxonomy_coverage BETWEEN 0 AND 1),
    CHECK (advance_share IS NULL OR advance_share BETWEEN 0 AND 1),
    CHECK (advance_decline IS NULL OR advance_decline BETWEEN -1 AND 1),
    CHECK (up_volume_share IS NULL OR up_volume_share BETWEEN 0 AND 1),
    CHECK (pct_above_ema20 IS NULL OR pct_above_ema20 BETWEEN 0 AND 1),
    CHECK (pct_above_ema50 IS NULL OR pct_above_ema50 BETWEEN 0 AND 1),
    CHECK (pct_at_20d_high IS NULL OR pct_at_20d_high BETWEEN 0 AND 1),
    CHECK (pct_at_52w_high IS NULL OR pct_at_52w_high BETWEEN 0 AND 1),
    CHECK (potency_score IS NULL OR potency_score BETWEEN 0 AND 1),
    CHECK (score_percentile IS NULL OR score_percentile BETWEEN 0 AND 1),
    CHECK (feature_coverage IS NULL OR feature_coverage BETWEEN 0 AND 1),
    CHECK (rank IS NULL OR rank > 0),
    CHECK (persistence_sessions >= 0),
    CHECK (potency_streak >= 0),
    CHECK (outcome <> 'qualified' OR (
        potency_score IS NOT NULL AND score_percentile IS NOT NULL AND rank IS NOT NULL
    )),
    PRIMARY KEY (run_id, group_type, group_key)
);

CREATE INDEX idx_industry_rotation_groups_latest
    ON industry_rotation_groups (group_type, group_key, run_id)
    WHERE outcome = 'qualified';

CREATE TABLE industry_rotation_members (
    run_id UUID NOT NULL REFERENCES industry_rotation_runs(id) ON DELETE CASCADE,
    universe_version_id UUID NOT NULL,
    group_type TEXT NOT NULL,
    group_key TEXT NOT NULL,
    ticker TEXT NOT NULL,
    source_tag TEXT NOT NULL DEFAULT 'potent_industry'
        CHECK (source_tag = 'potent_industry'),
    outcome TEXT NOT NULL CHECK (outcome IN ('qualified', 'not_qualified', 'failed')),
    failure_stage TEXT,
    failure_reason TEXT,
    history_first_at TIMESTAMPTZ,
    history_last_at TIMESTAMPTZ,
    history_sessions INTEGER NOT NULL DEFAULT 0,
    bar_set_hash TEXT,
    close_price DOUBLE PRECISION CHECK (close_price > 0),
    return_1d DOUBLE PRECISION,
    return_5d DOUBLE PRECISION,
    return_20d DOUBLE PRECISION,
    stock_vs_industry_rs DOUBLE PRECISION,
    stock_vs_sector_rs DOUBLE PRECISION,
    stock_vs_market_rs DOUBLE PRECISION,
    liquidity_score DOUBLE PRECISION,
    trend_quality_score DOUBLE PRECISION,
    member_score DOUBLE PRECISION,
    member_rank INTEGER,
    feature_coverage DOUBLE PRECISION,
    source_evidence JSONB NOT NULL DEFAULT '{}'::jsonb,
    components JSONB NOT NULL DEFAULT '{}'::jsonb,
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
    CHECK (history_sessions >= 0),
    CHECK (liquidity_score IS NULL OR liquidity_score BETWEEN 0 AND 1),
    CHECK (trend_quality_score IS NULL OR trend_quality_score BETWEEN 0 AND 1),
    CHECK (member_score IS NULL OR member_score BETWEEN 0 AND 1),
    CHECK (member_rank IS NULL OR member_rank > 0),
    CHECK (feature_coverage IS NULL OR feature_coverage BETWEEN 0 AND 1),
    CHECK (outcome <> 'qualified' OR (
        close_price IS NOT NULL AND member_score IS NOT NULL
        AND member_rank IS NOT NULL AND bar_set_hash IS NOT NULL
    )),
    PRIMARY KEY (run_id, group_type, group_key, ticker),
    FOREIGN KEY (run_id, group_type, group_key)
        REFERENCES industry_rotation_groups(run_id, group_type, group_key)
        ON DELETE CASCADE,
    FOREIGN KEY (universe_version_id, ticker)
        REFERENCES swing_universe_members(universe_version_id, ticker)
);

CREATE INDEX idx_industry_rotation_members_selected
    ON industry_rotation_members (run_id, member_rank, ticker)
    WHERE outcome = 'qualified';

CREATE TABLE industry_rotation_transitions (
    run_id UUID NOT NULL REFERENCES industry_rotation_runs(id) ON DELETE CASCADE,
    group_type TEXT NOT NULL,
    group_key TEXT NOT NULL,
    event TEXT NOT NULL CHECK (event IN ('added', 'persistent', 'removed')),
    previous_run_id UUID REFERENCES industry_rotation_runs(id),
    previous_score DOUBLE PRECISION,
    current_score DOUBLE PRECISION,
    previous_rank INTEGER,
    current_rank INTEGER,
    reason TEXT NOT NULL,
    PRIMARY KEY (run_id, group_type, group_key, event)
);

COMMENT ON TABLE industry_rotation_groups IS
    'One immutable point-in-time breadth evaluation per industry, or sector fallback, including failed and non-potent denominators.';
COMMENT ON TABLE industry_rotation_members IS
    'Member ranking evidence for an evaluated group; observation-only rows default to not_requested promotion state.';
COMMENT ON COLUMN industry_rotation_runs.decision_at IS
    'Fixed point-in-time cutoff bounding universe, taxonomy, history, and completion visibility.';
COMMENT ON COLUMN industry_rotation_groups.cap_equal_divergence IS
    'Positive cap-weighted leadership over equal-weighted return used to prevent one large constituent from masking weak breadth.';
