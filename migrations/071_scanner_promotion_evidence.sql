ALTER TABLE scanner_candidate_snapshots
    ADD COLUMN promotion_evidence JSONB NOT NULL DEFAULT '{}'::jsonb;
