-- 100: persist deterministic BUY timing evidence and specialist authority audit.
ALTER TABLE analyses
    ADD COLUMN IF NOT EXISTS timing_snapshot JSONB,
    ADD COLUMN IF NOT EXISTS decision_policy JSONB;

ALTER TABLE analyses
    ADD CONSTRAINT analyses_timing_snapshot_object
        CHECK (timing_snapshot IS NULL OR jsonb_typeof(timing_snapshot) = 'object'),
    ADD CONSTRAINT analyses_decision_policy_object
        CHECK (decision_policy IS NULL OR jsonb_typeof(decision_policy) = 'object');

COMMENT ON COLUMN analyses.timing_snapshot IS
    'Versioned deterministic daily entry-timing evidence supplied to every analyst and risk reviewer.';
COMMENT ON COLUMN analyses.decision_policy IS
    'Deterministic post-model decision policy audit, including specialist BUY authority enforcement.';
