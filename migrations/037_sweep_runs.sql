-- 037_sweep_runs.sql
--
-- Parameter-sweep runs for the ORB strategy-discovery search (te backtest orb-sweep).
--
-- A sweep RUN is one invocation in a mode (coarse | refine | validate | full). It holds many
-- RESULTS: one row per (parameter set × period split × instrument scope). The coarse stage
-- writes a broad, low-resolution grid; a refine/validate run references its parent via
-- parent_id and re-scores a narrowed set (see --from-run). resolved_parameters is the full
-- concrete parameter set as JSON; parameter_set_hash is its stable identity so the same
-- combination is recognisable across runs. metrics is the orbreplay.Metrics summary; rank_score
-- is the robustness score used to order candidates (NOT net profit — see internal/backtest/sweep).
--
-- Single-file (no .down) by convention: the runner globs every *.sql and applies it forward, so
-- every statement is idempotent.

CREATE TABLE IF NOT EXISTS sweep_runs (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    parent_id    UUID REFERENCES sweep_runs(id),
    mode         TEXT NOT NULL,                 -- coarse | refine | validate | full
    period_from  DATE NOT NULL,
    period_to    DATE NOT NULL,
    param_count  INTEGER NOT NULL DEFAULT 0,
    notes        TEXT,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS sweep_results (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    sweep_id            UUID NOT NULL REFERENCES sweep_runs(id) ON DELETE CASCADE,
    parameter_set_hash  TEXT NOT NULL,
    resolved_parameters JSONB NOT NULL,
    instrument_scope    TEXT NOT NULL,          -- 'ALL' (portfolio) or a symbol e.g. 'EUR/USD'
    period_split        TEXT NOT NULL,          -- train | validate | forward | full
    metrics             JSONB NOT NULL,
    trades              INTEGER NOT NULL DEFAULT 0,
    rank_score          DOUBLE PRECISION,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_sweep_results_sweep ON sweep_results(sweep_id);
CREATE INDEX IF NOT EXISTS idx_sweep_results_rank
    ON sweep_results(sweep_id, instrument_scope, period_split, rank_score DESC);
CREATE INDEX IF NOT EXISTS idx_sweep_results_hash ON sweep_results(parameter_set_hash);
