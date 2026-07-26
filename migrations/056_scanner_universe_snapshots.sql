-- Prospective scanner-universe study.
--
-- Each scanner cycle records the full candidate funnel before any candidate
-- is discarded. scanner_run_id then follows accepted names through watchlist
-- analysis into signals; trades already reference signals.

CREATE TABLE scanner_runs (
    id                              UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    started_at                      TIMESTAMPTZ NOT NULL,
    completed_at                    TIMESTAMPTZ,
    status                          TEXT NOT NULL DEFAULT 'running'
                                    CHECK (status IN ('running', 'completed', 'failed')),
    error                           TEXT,
    pullback_universe_enabled       BOOLEAN NOT NULL,
    pullback_min_momentum_return_pct NUMERIC NOT NULL,
    pullback_min_market_cap_mm      NUMERIC NOT NULL,
    pullback_min_dollar_volume      NUMERIC NOT NULL,
    candidate_cap                   INTEGER NOT NULL,
    min_conviction                  NUMERIC NOT NULL,
    ttl_days                        INTEGER NOT NULL,
    raw_candidate_count             INTEGER NOT NULL DEFAULT 0,
    evaluated_candidate_count       INTEGER NOT NULL DEFAULT 0,
    scored_candidate_count          INTEGER NOT NULL DEFAULT 0,
    added_candidate_count           INTEGER NOT NULL DEFAULT 0,
    expired_item_count              INTEGER NOT NULL DEFAULT 0,
    created_at                      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_scanner_runs_started_at ON scanner_runs(started_at DESC);

CREATE TABLE scanner_candidate_snapshots (
    id                              UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    scanner_run_id                  UUID NOT NULL REFERENCES scanner_runs(id) ON DELETE CASCADE,
    ticker                          TEXT NOT NULL,
    sources                         TEXT[] NOT NULL DEFAULT '{}',
    graph_context                   TEXT,
    price                           NUMERIC,
    change_pct                      NUMERIC,
    volume                          BIGINT,
    trade_count                     BIGINT,
    dollar_volume                   NUMERIC,
    market_cap_mm                   NUMERIC,
    return_1m                       NUMERIC,
    return_3m                       NUMERIC,
    return_6m                       NUMERIC,
    momentum_composite              NUMERIC,
    momentum_rank                   INTEGER,
    qualification_known             BOOLEAN NOT NULL DEFAULT FALSE,
    pullback_qualified              BOOLEAN NOT NULL DEFAULT FALSE,
    qualification_stage             TEXT NOT NULL DEFAULT 'not_evaluated',
    qualification_reason            TEXT,
    quarantine_required             BOOLEAN NOT NULL DEFAULT FALSE,
    active_watchlist_duplicate      BOOLEAN NOT NULL DEFAULT FALSE,
    eligible_for_scoring            BOOLEAN NOT NULL DEFAULT FALSE,
    llm_action                      TEXT,
    llm_conviction                  NUMERIC,
    llm_setup_type                  TEXT,
    llm_strategy                    TEXT,
    llm_timeframes                  TEXT[] NOT NULL DEFAULT '{}',
    llm_reasoning                   TEXT,
    llm_risks                       TEXT[] NOT NULL DEFAULT '{}',
    llm_error                       TEXT,
    passed_min_conviction           BOOLEAN NOT NULL DEFAULT FALSE,
    cap_selected                    BOOLEAN NOT NULL DEFAULT FALSE,
    watchlist_added                 BOOLEAN NOT NULL DEFAULT FALSE,
    watchlist_error                 TEXT,
    strategies                      TEXT[] NOT NULL DEFAULT '{}',
    final_stage                     TEXT NOT NULL DEFAULT 'sourced',
    created_at                      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at                      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (scanner_run_id, ticker)
);

CREATE INDEX idx_scanner_candidate_snapshots_ticker
    ON scanner_candidate_snapshots(ticker, scanner_run_id);
CREATE INDEX idx_scanner_candidate_snapshots_final_stage
    ON scanner_candidate_snapshots(scanner_run_id, final_stage);
CREATE INDEX idx_scanner_candidate_snapshots_watchlist_added
    ON scanner_candidate_snapshots(scanner_run_id) WHERE watchlist_added;

ALTER TABLE watchlist
    ADD COLUMN scanner_run_id UUID REFERENCES scanner_runs(id) ON DELETE SET NULL;
ALTER TABLE signals
    ADD COLUMN scanner_run_id UUID REFERENCES scanner_runs(id) ON DELETE SET NULL;
ALTER TABLE analyses
    ADD COLUMN scanner_run_id UUID REFERENCES scanner_runs(id) ON DELETE SET NULL;

CREATE INDEX idx_watchlist_scanner_run_id ON watchlist(scanner_run_id);
CREATE INDEX idx_signals_scanner_run_id ON signals(scanner_run_id);
CREATE INDEX idx_analyses_scanner_run_id ON analyses(scanner_run_id);

-- Completed model decisions live in analyses. This companion table records
-- terminal engine paths that intentionally produce no analysis row, so an
-- unavailable model or data/indicator/material-change exit is not confused
-- with a ticker the engine never attempted.
CREATE TABLE scanner_engine_outcomes (
    id              UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    scanner_run_id  UUID NOT NULL REFERENCES scanner_runs(id) ON DELETE CASCADE,
    ticker          TEXT NOT NULL,
    stage           TEXT NOT NULL,
    reason          TEXT,
    signal_id       UUID REFERENCES signals(id) ON DELETE SET NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_scanner_engine_outcomes_run_ticker
    ON scanner_engine_outcomes(scanner_run_id, ticker, created_at DESC);
