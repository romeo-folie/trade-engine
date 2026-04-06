-- Phase 16: Forex ORB support — distinguish equity vs forex sessions and trades.
ALTER TABLE orb_sessions ADD COLUMN asset_class TEXT NOT NULL DEFAULT 'equity';
ALTER TABLE orb_trades ADD COLUMN asset_class TEXT NOT NULL DEFAULT 'equity';
ALTER TABLE orb_trades ADD COLUMN conid TEXT;
