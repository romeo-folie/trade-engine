-- 045: earnings-calendar cache for the options-ORB earnings-blackout gate (#419 follow-up).
-- One row per (symbol, earnings_date). Populated OUT-OF-BAND so no API call sits on the
-- hot gate path: `te earnings sync` pulls coming reports from Finnhub /calendar/earnings,
-- and a static seed fills the historical windows the free tier (~1 month history) can't
-- serve for backtests. Both the LIVE gate and the BACKTEST read NextEarningsOnOrAfter from
-- this same table, so the earnings blackout is identical across them (parity by shared store).
CREATE TABLE IF NOT EXISTS earnings_calendar (
    symbol        TEXT        NOT NULL,
    earnings_date DATE        NOT NULL,
    session_hour  TEXT        NOT NULL DEFAULT '',        -- Finnhub 'hour': 'bmo' | 'amc' | 'dmh' | ''
    source        TEXT        NOT NULL DEFAULT 'finnhub',  -- 'finnhub' (synced) | 'seed' (curated historical)
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (symbol, earnings_date)
);

-- The gate query is "earliest earnings_date >= as-of for this symbol".
CREATE INDEX IF NOT EXISTS idx_earnings_calendar_symbol_date
    ON earnings_calendar (symbol, earnings_date);
