-- 023_wait_action.sql
-- Adds WAIT to the signal_action enum so analyses with WAIT action can be
-- persisted. ALTER TYPE ... ADD VALUE is transactional in Postgres 12+ and
-- does not require a table rewrite.
ALTER TYPE signal_action ADD VALUE IF NOT EXISTS 'WAIT';
