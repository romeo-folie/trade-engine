-- Options-ORB (Issue #413): per-underlying daily at-the-money implied volatility.
--
-- The IV-Rank gate ranks today's IV within its own trailing window. Alpaca serves
-- no historical greeks/IV, so this table is filled two ways: a bounded up-front
-- Black-Scholes derivation from historical option prices (source='derived') and a
-- daily snapshot of the live chain's ATM IV going forward (source='live'). One row
-- per (underlying, date); re-runs upsert.
CREATE TABLE IF NOT EXISTS option_iv_history (
    underlying TEXT        NOT NULL,
    date       DATE        NOT NULL,
    atm_iv     NUMERIC     NOT NULL,          -- annualized IV as a fraction (0.18 = 18%)
    source     TEXT        NOT NULL DEFAULT 'live', -- 'derived' | 'live'
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (underlying, date)
);
