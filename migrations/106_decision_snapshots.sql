-- Issue #589 step 3: immutable decision snapshots and per-stage model calls.
--
-- One decision_snapshots row per analysis attempt, written before any model
-- call (outcome = 'pending'), or directly as 'short_circuited' for pre-model
-- exits. The single permitted UPDATE moves pending -> completed | failed |
-- abandoned and fills only the completion columns. Stage calls may be
-- inserted only while their snapshot is pending, and neither table permits
-- UPDATE of settled rows or DELETE. These tables are deliberately excluded
-- from every retention job.
--
-- Linked ids (analysis_id, scanner_run_id) carry no foreign keys: retention
-- may prune analyses and scanner runs, and a cascade or SET NULL would
-- mutate an immutable record.

CREATE TABLE decision_snapshots (
    id               UUID        PRIMARY KEY DEFAULT uuid_generate_v4(),
    schema_version   INT         NOT NULL CHECK (schema_version > 0),
    ticker           TEXT        NOT NULL,
    timeframe        TEXT        NOT NULL,
    decision_cutoff  TIMESTAMPTZ NOT NULL,
    code_revision    TEXT        NOT NULL,
    workflow         TEXT        NOT NULL
                     CHECK (workflow IN ('single', 'ensemble', 'specialist', 'none')),
    policy_version   TEXT        NOT NULL,
    prompt_version   TEXT        NOT NULL,
    input_hash       TEXT        NOT NULL,
    decision_hash    TEXT        NOT NULL,
    inputs           JSONB       NOT NULL,
    missing          JSONB       NOT NULL DEFAULT '[]'::jsonb,
    effective_config JSONB       NOT NULL,
    config_hash      TEXT        NOT NULL,
    scanner_run_id   UUID,
    outcome          TEXT        NOT NULL
                     CHECK (outcome IN ('pending', 'completed', 'failed', 'short_circuited', 'abandoned')),
    outcome_reason   TEXT,
    analysis_id      UUID,
    final_output     JSONB,
    output_hash      TEXT,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at     TIMESTAMPTZ,
    CONSTRAINT decision_snapshots_completion_time
        CHECK ((outcome = 'pending') = (completed_at IS NULL)),
    CONSTRAINT decision_snapshots_completed_output
        CHECK (outcome <> 'completed' OR (final_output IS NOT NULL AND output_hash IS NOT NULL)),
    CONSTRAINT decision_snapshots_reasoned_exit
        CHECK (outcome NOT IN ('failed', 'short_circuited', 'abandoned') OR outcome_reason IS NOT NULL)
);

CREATE INDEX idx_decision_snapshots_ticker_created ON decision_snapshots (ticker, created_at DESC);
CREATE INDEX idx_decision_snapshots_decision_hash ON decision_snapshots (decision_hash);
CREATE INDEX idx_decision_snapshots_analysis ON decision_snapshots (analysis_id) WHERE analysis_id IS NOT NULL;
CREATE INDEX idx_decision_snapshots_pending ON decision_snapshots (created_at) WHERE outcome = 'pending';

CREATE TABLE decision_stage_calls (
    snapshot_id      UUID        NOT NULL REFERENCES decision_snapshots(id),
    stage            TEXT        NOT NULL,   -- analyst | specialist | arbiter
    ordinal          INT         NOT NULL CHECK (ordinal >= 0),
    attempt          INT         NOT NULL DEFAULT 0 CHECK (attempt >= 0),
    role             TEXT,
    provider         TEXT        NOT NULL,
    requested_model  TEXT        NOT NULL,
    resolved_model   TEXT,
    request_params   JSONB,
    raw_prompt       TEXT,
    raw_response     TEXT,
    parsed_response  JSONB,
    error            TEXT,
    input_tokens     INT,
    output_tokens    INT,
    latency_ms       BIGINT,
    extra            JSONB,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (snapshot_id, stage, ordinal, attempt)
);

CREATE OR REPLACE FUNCTION guard_decision_snapshot_mutation()
RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'decision snapshots are immutable and cannot be deleted';
    END IF;
    IF OLD.outcome <> 'pending' THEN
        RAISE EXCEPTION 'decision snapshot % is settled (%) and cannot change', OLD.id, OLD.outcome;
    END IF;
    IF NEW.outcome = 'pending' THEN
        RAISE EXCEPTION 'decision snapshot % update must settle the outcome', OLD.id;
    END IF;
    IF (NEW.id, NEW.schema_version, NEW.ticker, NEW.timeframe, NEW.decision_cutoff,
        NEW.code_revision, NEW.workflow, NEW.policy_version, NEW.prompt_version,
        NEW.input_hash, NEW.decision_hash, NEW.inputs, NEW.missing,
        NEW.effective_config, NEW.config_hash, NEW.scanner_run_id, NEW.created_at)
       IS DISTINCT FROM
       (OLD.id, OLD.schema_version, OLD.ticker, OLD.timeframe, OLD.decision_cutoff,
        OLD.code_revision, OLD.workflow, OLD.policy_version, OLD.prompt_version,
        OLD.input_hash, OLD.decision_hash, OLD.inputs, OLD.missing,
        OLD.effective_config, OLD.config_hash, OLD.scanner_run_id, OLD.created_at) THEN
        RAISE EXCEPTION 'decision snapshot % inputs are immutable', OLD.id;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_guard_decision_snapshot_mutation
    BEFORE UPDATE OR DELETE ON decision_snapshots
    FOR EACH ROW EXECUTE FUNCTION guard_decision_snapshot_mutation();

CREATE OR REPLACE FUNCTION guard_decision_stage_call_mutation()
RETURNS TRIGGER AS $$
DECLARE
    parent_outcome TEXT;
BEGIN
    IF TG_OP <> 'INSERT' THEN
        RAISE EXCEPTION 'decision stage calls are immutable';
    END IF;
    SELECT outcome INTO parent_outcome
    FROM decision_snapshots
    WHERE id = NEW.snapshot_id
    FOR SHARE;
    IF parent_outcome IS DISTINCT FROM 'pending' THEN
        RAISE EXCEPTION 'decision snapshot % is not pending (%); stage calls are closed',
            NEW.snapshot_id, COALESCE(parent_outcome, 'missing');
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_guard_decision_stage_call_mutation
    BEFORE INSERT OR UPDATE OR DELETE ON decision_stage_calls
    FOR EACH ROW EXECUTE FUNCTION guard_decision_stage_call_mutation();
