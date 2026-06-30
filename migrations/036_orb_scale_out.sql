-- 036_orb_scale_out.sql
--
-- Scale-out (partial take-profit) accounting for ORB forex trades.
--
-- When an instrument configures a partial take-profit (partial_take_profit_r +
-- partial_fraction), the engine enters the breakout as TWO independent OCO brackets
-- sharing one stop: a PARTIAL bracket (partial_fraction of size, take-profit at the
-- nearer partial_take_profit_r level) and a RUNNER bracket (the remainder, take-profit
-- at the farther runner_take_profit_r level). Each bracket is a normal orb_trades row
-- reconciled by the existing per-row machinery; the two rows are linked so reporting and
-- position counting treat them as ONE logical trade.
--
--   scale_out_role     'runner' | 'partial' | NULL (NULL = ordinary single-bracket trade)
--   scale_out_parent_id  on the PARTIAL row, the id of its RUNNER row (the primary leg);
--                        NULL on runner rows and ordinary trades. Self-FK, same pattern as
--                        trades.consolidated_into (migration 027).
--
-- A partial index supports folding children into their parent for stats / distinct-ticker
-- counting without scanning the whole table.
--
-- Single-file (no .down) by convention: the migration runner globs every *.sql file, so a
-- .down.sql would be applied forward. Every statement is idempotent.

ALTER TABLE orb_trades ADD COLUMN IF NOT EXISTS scale_out_role TEXT;

ALTER TABLE orb_trades ADD COLUMN IF NOT EXISTS scale_out_parent_id UUID REFERENCES orb_trades(id);

CREATE INDEX IF NOT EXISTS idx_orb_trades_scale_out_parent
    ON orb_trades(scale_out_parent_id)
    WHERE scale_out_parent_id IS NOT NULL;
