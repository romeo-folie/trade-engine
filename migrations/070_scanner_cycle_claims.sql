-- Cross-process lease for the regular-hours scanner. Source-specific
-- schedulers already have their own durable run leases; this closes the gap
-- for the legacy movers/actives cycle across restarts and concurrent engines.
CREATE TABLE scanner_cycle_claims (
    run_key TEXT PRIMARY KEY,
    status TEXT NOT NULL DEFAULT 'running' CHECK (status IN ('running','completed','failed')),
    owner_token TEXT NOT NULL,
    lease_until TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
