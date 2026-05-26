-- 030_orb_close_status_and_filled_at.sql
--
-- Restores ORB exit/PnL recording, which was silently broken on two fronts:
--
--   1. CloseORBTrade writes status='closed', but the orb_trades CHECK constraint
--      only permitted ('pending','filled','cancelled','expired'). Every close
--      therefore violated the constraint and was logged-and-swallowed, so no
--      filled trade ever recorded an exit_price or pnl. This adds 'closed' to
--      the allowed set so the close write succeeds.
--
--   2. FillORBTradeByOrderID set closed_at=NOW() at *fill* time, overloading
--      closed_at to double as the entry-fill timestamp the reconciler reads.
--      A new filled_at column holds that timestamp so closed_at can mean what
--      it says — set only when the trade actually exits.
--
-- Single-file (no .down) by convention: the migration runner globs every *.sql
-- file, so a .down.sql would be applied forward. Every statement is idempotent.

-- 1. Allow the terminal 'closed' status. All existing rows already fall within
--    the prior set, so re-adding the (now wider) constraint validates cleanly.
ALTER TABLE orb_trades DROP CONSTRAINT IF EXISTS orb_trades_status_check;
ALTER TABLE orb_trades ADD CONSTRAINT orb_trades_status_check
    CHECK (status IN ('pending','filled','cancelled','expired','closed'));

-- 2. Dedicated entry-fill timestamp.
ALTER TABLE orb_trades ADD COLUMN IF NOT EXISTS filled_at TIMESTAMPTZ;

-- 3. Relocate the historically-overloaded closed_at on filled-but-open trades:
--    for these rows closed_at currently holds the entry-fill time, so move it to
--    filled_at and clear closed_at (an open trade has no exit). Guarded on
--    filled_at IS NULL so re-running is a no-op.
UPDATE orb_trades
SET filled_at = closed_at,
    closed_at = NULL
WHERE status = 'filled'
  AND filled_at IS NULL
  AND closed_at IS NOT NULL;
