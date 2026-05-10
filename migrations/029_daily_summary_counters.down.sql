-- 029_daily_summary_counters.down.sql
-- Reverse Issue #325 daily-summary counter columns. Drops are idempotent.

ALTER TABLE daily_summaries DROP COLUMN IF EXISTS blocked_by_reason;
ALTER TABLE daily_summaries DROP COLUMN IF EXISTS executed_losing;
ALTER TABLE daily_summaries DROP COLUMN IF EXISTS executed_winning;
ALTER TABLE daily_summaries DROP COLUMN IF EXISTS signals_executed;
ALTER TABLE daily_summaries DROP COLUMN IF EXISTS signals_total;
