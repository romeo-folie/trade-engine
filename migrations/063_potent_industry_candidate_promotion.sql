ALTER TABLE scanner_candidate_snapshots
    ADD COLUMN potent_industry_evidence TEXT;

COMMENT ON COLUMN scanner_candidate_snapshots.potent_industry_evidence IS
    'Immutable group-potency and member-ranking evidence carried by a potent-industry discovery candidate.';

CREATE INDEX idx_industry_rotation_members_pending_promotion
    ON industry_rotation_members (run_id, member_rank, ticker)
    WHERE outcome = 'qualified' AND promotion_status = 'pending';
