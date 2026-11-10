-- no-transaction
-- 108_signal_action_short_cover.sql
--
-- Issue #599: equity short selling adds two actions distinct from SELL, which
-- only ever exits a long. SHORT opens a short on a flat position; COVER closes
-- a held short. ALTER TYPE ... ADD VALUE cannot run inside a transaction.
ALTER TYPE signal_action ADD VALUE IF NOT EXISTS 'SHORT';
ALTER TYPE signal_action ADD VALUE IF NOT EXISTS 'COVER';
