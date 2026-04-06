DROP INDEX IF EXISTS idx_orb_sessions_date_session;
ALTER TABLE orb_sessions DROP COLUMN IF EXISTS session_name;
CREATE UNIQUE INDEX idx_orb_sessions_date ON orb_sessions (date);
ALTER TABLE orb_sessions ADD CONSTRAINT orb_sessions_date_key UNIQUE (date);
