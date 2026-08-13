-- Distinguish a broker account observation fetched before a manual reset from
-- a genuine post-reset equity move while Alpaca still reports the old
-- prior-session reference.
ALTER TABLE account_risk_baselines
    ADD COLUMN IF NOT EXISTS manual_reset_at TIMESTAMPTZ;
