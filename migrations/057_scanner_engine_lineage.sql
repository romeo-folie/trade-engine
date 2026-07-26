-- Complete scanner-run lineage through engine analysis.
--
-- Kept separate from migration 056 because migration filenames are immutable:
-- databases that applied the original 056 must still receive these additions.

ALTER TABLE analyses
    ADD COLUMN IF NOT EXISTS scanner_run_id UUID REFERENCES scanner_runs(id) ON DELETE SET NULL;

CREATE INDEX IF NOT EXISTS idx_analyses_scanner_run_id ON analyses(scanner_run_id);

-- Completed model decisions live in analyses. This companion table records
-- terminal engine paths that intentionally produce no analysis row, so an
-- unavailable model or data/indicator/material-change exit is not confused
-- with a ticker the engine never attempted.
CREATE TABLE IF NOT EXISTS scanner_engine_outcomes (
    id              UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    scanner_run_id  UUID NOT NULL REFERENCES scanner_runs(id) ON DELETE CASCADE,
    ticker          TEXT NOT NULL,
    stage           TEXT NOT NULL,
    reason          TEXT,
    signal_id       UUID REFERENCES signals(id) ON DELETE SET NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_scanner_engine_outcomes_run_ticker
    ON scanner_engine_outcomes(scanner_run_id, ticker, created_at DESC);
