-- Issue #550: keep scanner provenance separate from the bounded execution
-- profile that deep analysis may use. Nullable columns preserve legacy rows;
-- version validation lives in the shared Go contract package.

ALTER TABLE watchlist
    ADD COLUMN IF NOT EXISTS discovery_context JSONB,
    ADD COLUMN IF NOT EXISTS execution_profile JSONB;

ALTER TABLE watchlist
    ADD CONSTRAINT watchlist_discovery_context_object
        CHECK (discovery_context IS NULL OR jsonb_typeof(discovery_context) = 'object'),
    ADD CONSTRAINT watchlist_execution_profile_object
        CHECK (execution_profile IS NULL OR jsonb_typeof(execution_profile) = 'object');

COMMENT ON COLUMN watchlist.discovery_context IS
    'Versioned scanner provenance: attribution sources, thesis, and structured evidence.';
COMMENT ON COLUMN watchlist.execution_profile IS
    'Versioned bounded execution-lens and timeframe profile selected by scanner rules.';
