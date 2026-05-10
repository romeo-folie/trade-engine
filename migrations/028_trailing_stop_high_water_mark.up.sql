-- Issue #322: Server-side trailing stop manager
--
-- Add high_water_mark column to trades. The trailing stop manager initializes
-- this on first tick to MAX(entry_price, current_price); thereafter it ratchets
-- upward only and is the basis for the trailing-stop formula:
--   new_stop = max(current_stop, high_water_mark - atr_multiple * ATR)
-- Existing open trades have NULL until the manager observes them.
ALTER TABLE trades
    ADD COLUMN IF NOT EXISTS high_water_mark NUMERIC(20,8) NULL;
