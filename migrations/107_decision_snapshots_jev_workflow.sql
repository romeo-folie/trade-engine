-- Issue #590: authoritative Jev decisions are their own workflow. Snapshots
-- recorded under the Jev decision mode carry workflow = 'jev'; shadow Jev
-- calls are stage calls (stage = 'jev_shadow') on the baseline's snapshot.
ALTER TABLE decision_snapshots DROP CONSTRAINT decision_snapshots_workflow_check;
ALTER TABLE decision_snapshots ADD CONSTRAINT decision_snapshots_workflow_check
    CHECK (workflow IN ('single', 'ensemble', 'specialist', 'jev', 'none'));
