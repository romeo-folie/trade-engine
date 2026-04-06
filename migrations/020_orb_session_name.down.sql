DROP INDEX IF EXISTS idx_orb_sessions_date_session;
ALTER TABLE orb_sessions DROP COLUMN IF EXISTS session_name;
