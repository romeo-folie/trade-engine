-- Allow multiple ORB sessions per day (e.g., London + NY forex).
ALTER TABLE orb_sessions ADD COLUMN session_name TEXT NOT NULL DEFAULT '';
ALTER TABLE orb_sessions DROP CONSTRAINT IF EXISTS orb_sessions_date_key;
DROP INDEX IF EXISTS idx_orb_sessions_date;
CREATE UNIQUE INDEX idx_orb_sessions_date_session ON orb_sessions (date, session_name);
