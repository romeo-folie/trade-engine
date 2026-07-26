-- 061_weekly_leader_candidate_evidence.sql
-- Keep quantitative weekly-leader ranking evidence distinct from knowledge-
-- graph relationship context in the prospective scanner audit trail.

ALTER TABLE scanner_candidate_snapshots
    ADD COLUMN weekly_leader_evidence TEXT;

COMMENT ON COLUMN scanner_candidate_snapshots.weekly_leader_evidence IS
    'Quantitative full-universe leader outcome, kept separate from graph relationship context.';
