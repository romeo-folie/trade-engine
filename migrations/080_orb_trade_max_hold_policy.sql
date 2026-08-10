-- Snapshot the launch-time time-exit policy on each ORB trade. NULL is reserved
-- for legacy rows created before this migration; lifecycle code may use the
-- current strategy default only for those rows.

ALTER TABLE orb_trades
    ADD COLUMN IF NOT EXISTS max_hold_minutes INTEGER;

ALTER TABLE orb_trades DROP CONSTRAINT IF EXISTS orb_trades_max_hold_minutes_check;
ALTER TABLE orb_trades ADD CONSTRAINT orb_trades_max_hold_minutes_check
    CHECK (max_hold_minutes IS NULL OR max_hold_minutes >= 0);
