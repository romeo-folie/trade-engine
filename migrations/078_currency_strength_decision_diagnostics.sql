ALTER TABLE currency_strength_runs
    ADD COLUMN decision_diagnostics JSONB NOT NULL DEFAULT '{}'::jsonb,
    ADD CONSTRAINT currency_strength_decision_diagnostics_object
        CHECK (jsonb_typeof(decision_diagnostics) = 'object');

COMMENT ON COLUMN currency_strength_runs.decision_diagnostics IS
    'Versioned inputs, scores, thresholds, route, and gate outcomes for one currency-strength evaluation.';
