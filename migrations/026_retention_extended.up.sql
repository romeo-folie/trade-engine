-- 026_retention_extended.up.sql
-- SQL cleanup functions for tables that previously had no retention policy.
-- Note: the engine's daily retention loop uses direct DELETE statements in
-- internal/store/retention.go (matching the existing pattern from migration 011).
-- These functions are provided for ad-hoc manual invocation (SELECT cleanup_*())
-- and as documentation of the intended retention windows.

-- Delete portfolio snapshots older than 180 days (no FKs; derivable from trades).
CREATE OR REPLACE FUNCTION cleanup_portfolio_snapshots()
RETURNS INTEGER
LANGUAGE plpgsql AS $$
DECLARE
    deleted INTEGER;
BEGIN
    DELETE FROM portfolio_snapshots
    WHERE created_at < NOW() - INTERVAL '180 days';
    GET DIAGNOSTICS deleted = ROW_COUNT;
    RETURN deleted;
END;
$$;

-- Delete news items older than 90 days.
-- entity_mentions rows cascade automatically (ON DELETE CASCADE).
CREATE OR REPLACE FUNCTION cleanup_news_items()
RETURNS INTEGER
LANGUAGE plpgsql AS $$
DECLARE
    deleted INTEGER;
BEGIN
    DELETE FROM news_items
    WHERE created_at < NOW() - INTERVAL '90 days';
    GET DIAGNOSTICS deleted = ROW_COUNT;
    RETURN deleted;
END;
$$;

-- Delete config_history entries older than 365 days.
CREATE OR REPLACE FUNCTION cleanup_config_history()
RETURNS INTEGER
LANGUAGE plpgsql AS $$
DECLARE
    deleted INTEGER;
BEGIN
    DELETE FROM config_history
    WHERE created_at < NOW() - INTERVAL '365 days';
    GET DIAGNOSTICS deleted = ROW_COUNT;
    RETURN deleted;
END;
$$;

-- Delete resolved feedback_flags older than 180 days.
-- Unresolved flags are always preserved regardless of age.
CREATE OR REPLACE FUNCTION cleanup_feedback_flags()
RETURNS INTEGER
LANGUAGE plpgsql AS $$
DECLARE
    deleted INTEGER;
BEGIN
    DELETE FROM feedback_flags
    WHERE created_at < NOW() - INTERVAL '180 days'
      AND resolved = TRUE;
    GET DIAGNOSTICS deleted = ROW_COUNT;
    RETURN deleted;
END;
$$;
