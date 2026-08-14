-- Issue #550: snapshot the discovery/execution contract independently from
-- watchlist. This remains a later migration so databases that already applied
-- 090 can acquire immutable scanner-run provenance without replaying it.

ALTER TABLE scanner_candidate_snapshots
    ADD COLUMN IF NOT EXISTS discovery_context JSONB,
    ADD COLUMN IF NOT EXISTS execution_profile JSONB;

ALTER TABLE scanner_candidate_snapshots
    ADD CONSTRAINT scanner_candidate_snapshots_discovery_context_object
        CHECK (discovery_context IS NULL OR jsonb_typeof(discovery_context) = 'object'),
    ADD CONSTRAINT scanner_candidate_snapshots_execution_profile_object
        CHECK (execution_profile IS NULL OR jsonb_typeof(execution_profile) = 'object'),
    ADD CONSTRAINT scanner_candidate_snapshots_discovery_execution_pair
        CHECK ((discovery_context IS NULL) = (execution_profile IS NULL));

COMMENT ON COLUMN scanner_candidate_snapshots.discovery_context IS
    'Immutable versioned scanner provenance captured when the scanner run finishes.';
COMMENT ON COLUMN scanner_candidate_snapshots.execution_profile IS
    'Immutable versioned scanner execution profile captured when the scanner run finishes.';
