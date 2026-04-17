-- 026_retention_extended.down.sql
DROP FUNCTION IF EXISTS cleanup_portfolio_snapshots();
DROP FUNCTION IF EXISTS cleanup_news_items();
DROP FUNCTION IF EXISTS cleanup_config_history();
DROP FUNCTION IF EXISTS cleanup_feedback_flags();
