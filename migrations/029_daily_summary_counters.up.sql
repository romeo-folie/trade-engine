-- 029_daily_summary_counters.up.sql
-- Issue #325: Daily summary counters for observability — extends the existing
-- daily_summaries rollup with per-day counts that let the dashboard answer
-- "what does the gating + execution pipeline look like today?" without ad-hoc
-- DB queries.
--
-- New columns are additive and idempotent so this migration applies cleanly
-- on top of an existing table or a fresh install. ALTER TABLE ... ADD COLUMN
-- IF NOT EXISTS is used throughout to allow safe re-runs.
--
--   signals_total      — total signals created today (any status)
--   signals_executed   — signals that reached status='executed'
--   blocked_by_reason  — JSONB: { "<block_reason>": <count>, ... }
--   executed_winning   — closed trades today with pnl > 0
--   executed_losing    — closed trades today with pnl < 0
--
-- signals_blocked + signals_generated already exist on the table; the rollup
-- writer keeps writing both for backwards compatibility and now also writes
-- signals_total (the strict daily total, including pending/expired/etc).

ALTER TABLE daily_summaries
    ADD COLUMN IF NOT EXISTS signals_total       INTEGER NOT NULL DEFAULT 0;

ALTER TABLE daily_summaries
    ADD COLUMN IF NOT EXISTS signals_executed    INTEGER NOT NULL DEFAULT 0;

ALTER TABLE daily_summaries
    ADD COLUMN IF NOT EXISTS executed_winning    INTEGER NOT NULL DEFAULT 0;

ALTER TABLE daily_summaries
    ADD COLUMN IF NOT EXISTS executed_losing     INTEGER NOT NULL DEFAULT 0;

ALTER TABLE daily_summaries
    ADD COLUMN IF NOT EXISTS blocked_by_reason   JSONB   NOT NULL DEFAULT '{}'::jsonb;
