-- no-transaction
-- 027_consolidated_trade_status.sql
--
-- Adds the schema scaffolding required to collapse stale trade rows into a
-- single canonical row without losing history. Used initially by the JMIA
-- 8 → 1 collapse (issue #311) and reusable for any future consolidation.
--
-- This migration must run with the "-- no-transaction" directive because
-- ALTER TYPE ... ADD VALUE cannot run inside a transaction block in
-- PostgreSQL. Each statement is independently idempotent (IF NOT EXISTS),
-- so re-running on partial failure is safe.
--
-- Reversibility (operator-applied, not auto-rolled-back):
--   UPDATE trades SET status='open', consolidated_into=NULL
--   WHERE consolidated_into = '<canonical_id>';
--
-- The schema columns/index can stay in place after a rollback — they are
-- additive and inert when no rows reference them.

-- 1. New enum value used by the data migration to mark redundant rows.
ALTER TYPE trade_status ADD VALUE IF NOT EXISTS 'consolidated';

-- 2. Pointer back to the canonical surviving trade row. Self-referential
--    FK with ON DELETE SET NULL so dropping the canonical row cleans up
--    dangling pointers rather than cascading the delete.
ALTER TABLE trades
    ADD COLUMN IF NOT EXISTS consolidated_into UUID
    REFERENCES trades(id) ON DELETE SET NULL;

-- 3. Index supports lookups like "show me every row collapsed into X" and
--    keeps the FK validation cheap.
CREATE INDEX IF NOT EXISTS idx_trades_consolidated_into
    ON trades(consolidated_into);
