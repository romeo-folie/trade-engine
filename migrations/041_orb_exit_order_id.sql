-- Options-ORB (review round 8): persist the in-flight exit sell's broker order id
-- on the trade row so restart recovery can poll it for the REAL fill price/reason,
-- instead of losing the in-memory pendingOptionExits marker across a restart and
-- healing later with an estimated price. NULL for non-options and before an exit
-- is submitted.
ALTER TABLE orb_trades ADD COLUMN IF NOT EXISTS exit_order_id TEXT;
