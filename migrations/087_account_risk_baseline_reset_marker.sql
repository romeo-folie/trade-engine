-- A manual capital reset must survive the broker's stale prior-session equity
-- across the following UTC rollover. Once the broker reference changes, normal
-- daily baseline updates resume automatically.
ALTER TABLE account_risk_baselines
    ADD COLUMN IF NOT EXISTS manual_reset_reference NUMERIC;
