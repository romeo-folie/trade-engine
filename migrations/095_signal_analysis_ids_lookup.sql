-- Analysis detail resolves the newest signal linked to one analysis. Array
-- containment is indexable with GIN, including the common no-signal/HOLD case.
CREATE INDEX IF NOT EXISTS idx_signals_analysis_ids_gin
    ON signals USING GIN (analysis_ids);
