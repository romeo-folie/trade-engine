-- Phase 2 review hardening.
--
-- Existing bare-symbol candle rows were fetched with Alpaca adjustment=raw.
-- The application now requests split-adjusted bars, so retaining those rows
-- would mix incompatible price scales with newly refreshed history. Purge the
-- ambiguous bare-symbol cache once; stock/futures histories are recoverable
-- market-data caches and will be refetched under the new mode. FX/crypto pairs
-- containing separators are not Alpaca equity histories and are retained.
DELETE FROM candles
WHERE ticker ~ '^[[:alpha:]][[:alnum:].-]*$';

ALTER TABLE signals
    ADD COLUMN pullback_strength BOOLEAN NOT NULL DEFAULT FALSE;

ALTER TABLE backtest_runs
    ADD COLUMN run_options JSONB NOT NULL DEFAULT '{}'::jsonb,
    ADD COLUMN entry_diagnostics JSONB NOT NULL DEFAULT '{}'::jsonb;
