-- Persistent operator buy control and broker-account-scoped risk baselines.
-- These are runtime state, not YAML: restarting the engine must not silently
-- resume buys or carry a paper-account high-water mark into a live account.

CREATE TABLE IF NOT EXISTS buy_controls (
    scope       TEXT PRIMARY KEY,
    enabled     BOOLEAN NOT NULL DEFAULT TRUE,
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_by  TEXT NOT NULL DEFAULT 'operator'
);

CREATE TABLE IF NOT EXISTS account_risk_baselines (
    account_id       TEXT PRIMARY KEY,
    peak_equity      NUMERIC NOT NULL,
    last_equity      NUMERIC NOT NULL,
    baseline_date    DATE NOT NULL,
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
