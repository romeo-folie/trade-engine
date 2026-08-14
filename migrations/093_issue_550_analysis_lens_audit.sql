-- Issue #550: persist the validated final lens choice separately from the
-- immutable raw model response. Nullable values preserve all historical rows.

ALTER TABLE analyses
    ADD COLUMN IF NOT EXISTS selected_lens TEXT,
    ADD COLUMN IF NOT EXISTS rejected_lenses JSONB,
    ADD COLUMN IF NOT EXISTS regime TEXT,
    ADD COLUMN IF NOT EXISTS lens_selection_status TEXT;

ALTER TABLE analyses
    ADD CONSTRAINT analyses_rejected_lenses_object
        CHECK (rejected_lenses IS NULL OR jsonb_typeof(rejected_lenses) = 'object'),
    ADD CONSTRAINT analyses_lens_selection_status_valid
        CHECK (lens_selection_status IS NULL OR lens_selection_status IN
            ('absent', 'none', 'selected', 'invalid', 'no_consensus'));

COMMENT ON COLUMN analyses.selected_lens IS
    'Final analyst-selected lens; invalid values retain their raw selection for audit but cannot drive execution.';
COMMENT ON COLUMN analyses.rejected_lenses IS
    'JSON object mapping non-selected candidate lenses to concise rejection reasons.';
COMMENT ON COLUMN analyses.regime IS
    'Analyst regime classification paired with the final lens selection.';
COMMENT ON COLUMN analyses.lens_selection_status IS
    'Whether selection was absent, none, selected, invalid, or no consensus.';
