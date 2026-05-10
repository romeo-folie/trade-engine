-- Issue #322: revert trailing stop high water mark column.
ALTER TABLE trades DROP COLUMN IF EXISTS high_water_mark;
