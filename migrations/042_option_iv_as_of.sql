-- Options-ORB: give option_iv_history a precise sample INSTANT alongside the day.
--
-- The IV-Rank as-of filter must exclude a same-day IV sample taken AFTER the
-- decision instant, or a pre-sample breakout (a short opening range) would consume
-- IV that hadn't been observed yet — lookahead. The `date` column is day-granular,
-- which loses that precision once the series is persisted. `as_of` carries the exact
-- sample time (e.g. 09:45 ET) so the filter (as_of <= asOf) is lookahead-safe, while
-- `date` stays the ET calendar day for one-row-per-day uniqueness (the PK).
ALTER TABLE option_iv_history ADD COLUMN IF NOT EXISTS as_of TIMESTAMPTZ;

-- Existing rows were sampled intraday (the default 09:45 ET ref minute) but stored
-- only the day. Anchor them at 09:45 ET — the SAMPLE time — not midnight: a midnight
-- as_of would be visible to a pre-09:45 opening-range breakout and rank on IV that
-- wasn't observable yet (lookahead). timezone('America/New_York', ...) builds the
-- instant in ET (session-timezone-independent) so it round-trips to the same ET day;
-- the write-through seeder re-derives any row whose as_of doesn't match the current
-- ref minute, so a wrong guess self-corrects on the next run.
UPDATE option_iv_history
   SET as_of = timezone('America/New_York', date::timestamp + interval '9 hours 45 minutes')
 WHERE as_of IS NULL;
