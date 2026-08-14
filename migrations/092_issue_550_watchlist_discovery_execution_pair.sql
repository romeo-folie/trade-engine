-- Issue #550: require scanner provenance and its bounded execution profile to
-- arrive together. This intentionally follows 090 so development databases
-- that already applied the column migration receive the new invariant.

ALTER TABLE watchlist
    ADD CONSTRAINT watchlist_discovery_execution_pair
        CHECK ((discovery_context IS NULL) = (execution_profile IS NULL));
