-- Broker calls can complete out of order around a session rollover. Keep the
-- timestamp captured before each request so an older response cannot restore a
-- stale prior-session reference after a newer observation has been accepted.
ALTER TABLE account_risk_baselines
    ADD COLUMN IF NOT EXISTS last_observed_at TIMESTAMPTZ NOT NULL DEFAULT NOW();
