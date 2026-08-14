ALTER TABLE analyses
    ADD COLUMN IF NOT EXISTS timeframe_workflow JSONB;

ALTER TABLE analyses
    ADD CONSTRAINT analyses_timeframe_workflow_object
        CHECK (timeframe_workflow IS NULL OR jsonb_typeof(timeframe_workflow) = 'object');

ALTER TABLE signals
    ADD COLUMN IF NOT EXISTS entry_gate_decisions JSONB;

ALTER TABLE signals
    ADD CONSTRAINT signals_entry_gate_decisions_array
        CHECK (entry_gate_decisions IS NULL OR jsonb_typeof(entry_gate_decisions) = 'array');

COMMENT ON COLUMN analyses.timeframe_workflow IS
    'Versioned primary-thesis and optional entry-confirmation workflow audit.';
COMMENT ON COLUMN signals.entry_gate_decisions IS
    'Ordered deterministic entry-gate decisions applied before signal admission.';
