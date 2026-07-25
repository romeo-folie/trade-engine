ALTER TABLE trades
    ADD COLUMN IF NOT EXISTS realized_exit_qty NUMERIC(20,8) NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS realized_exit_proceeds NUMERIC(20,8) NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS realized_pnl NUMERIC(20,8) NOT NULL DEFAULT 0;

-- Preserve the accounting already published for historical closed trades.
-- Legacy order rows are not authoritative enough to rewrite financial
-- history automatically: some predate durable partial-fill allocation.
UPDATE trades
SET realized_exit_qty = quantity,
    realized_exit_proceeds = quantity * COALESCE(exit_price, entry_price),
    realized_pnl = COALESCE(pnl, 0)
WHERE status = 'closed'
  AND realized_exit_qty = 0
  AND quantity > 0;

-- Migration 050 made partial-exit allocation durable. Backfill any open or
-- exiting trade that received such a fill between migrations 050 and 052.
WITH applied AS (
    SELECT t.id,
           SUM(o.exit_fill_applied_qty) AS qty,
           SUM(o.exit_fill_applied_qty * COALESCE(o.filled_price, t.entry_price)) AS proceeds,
           SUM(
               CASE WHEN t.direction = 'short'
                    THEN (t.entry_price - COALESCE(o.filled_price, t.entry_price))
                    ELSE (COALESCE(o.filled_price, t.entry_price) - t.entry_price)
               END * o.exit_fill_applied_qty
           ) AS pnl
    FROM trades t
    JOIN orders o ON o.trade_id = t.id
    WHERE t.status IN ('open', 'exiting')
      AND o.exit_fill_applied_qty > 0
      AND ((t.direction = 'long' AND o.side = 'sell')
        OR (t.direction = 'short' AND o.side = 'buy'))
    GROUP BY t.id
)
UPDATE trades t
SET realized_exit_qty = applied.qty,
    realized_exit_proceeds = applied.proceeds,
    realized_pnl = applied.pnl
FROM applied
WHERE t.id = applied.id
  AND t.realized_exit_qty = 0;
