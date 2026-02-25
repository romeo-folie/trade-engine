-- 011_retention_policy.sql
-- Cleanup functions for data retention (called by the engine scheduler daily).

-- Delete intraday candles older than 90 days.
CREATE OR REPLACE FUNCTION cleanup_intraday_candles()
RETURNS INTEGER
LANGUAGE plpgsql AS $$
DECLARE
    deleted INTEGER;
BEGIN
    DELETE FROM candles
    WHERE timeframe IN ('1m', '5m', '15m')
      AND ts < NOW() - INTERVAL '90 days';
    GET DIAGNOSTICS deleted = ROW_COUNT;
    RETURN deleted;
END;
$$;

-- Delete prompt cache entries older than 6 months.
CREATE OR REPLACE FUNCTION cleanup_prompt_cache()
RETURNS INTEGER
LANGUAGE plpgsql AS $$
DECLARE
    deleted INTEGER;
BEGIN
    DELETE FROM prompt_cache
    WHERE created_at < NOW() - INTERVAL '6 months';
    GET DIAGNOSTICS deleted = ROW_COUNT;
    RETURN deleted;
END;
$$;
