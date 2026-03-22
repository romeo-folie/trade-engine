-- Rollback: remove ORB session and trade tracking tables added in 000017_orb_sessions.up.sql
DROP INDEX IF EXISTS idx_orb_trades_session;
DROP INDEX IF EXISTS idx_orb_sessions_date;
DROP TABLE IF EXISTS orb_trades;
DROP TABLE IF EXISTS orb_sessions;
