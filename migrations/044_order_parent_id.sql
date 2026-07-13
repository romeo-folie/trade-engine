-- 044: bracket-leg linkage for broker-resident stock exits (#435).
-- parent_order_id holds the broker order id of the bracket PARENT for child
-- exit legs (take-profit limit / stop-loss stop). A non-empty value marks the
-- row as an OCO-linked bracket child: the trailing manager must PATCH-replace
-- such a stop (never cancel+resubmit, which would orphan or kill the sibling
-- take-profit leg), and exit paths must cancel BOTH legs before selling the
-- shares they hold.
ALTER TABLE orders ADD COLUMN IF NOT EXISTS parent_order_id TEXT;
