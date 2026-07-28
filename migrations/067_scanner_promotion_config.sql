-- Persist scanner promotion diversification configuration with each run.

ALTER TABLE scanner_runs
    ADD COLUMN promotion_max_per_industry INTEGER NOT NULL DEFAULT 0
        CHECK (promotion_max_per_industry >= 0);
