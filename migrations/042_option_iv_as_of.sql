-- Options-ORB: give option_iv_history a precise sample INSTANT alongside the day.
--
-- The IV-Rank as-of filter must exclude a same-day IV sample taken AFTER the
-- decision instant, or a pre-sample breakout (a short opening range) would consume
-- IV that hadn't been observed yet — lookahead. The `date` column is day-granular,
-- which loses that precision once the series is persisted. `as_of` carries the exact
-- sample time (e.g. 09:45 ET) so the filter (as_of <= asOf) is lookahead-safe, while
-- `date` stays the ET calendar day for one-row-per-day uniqueness (the PK).
ALTER TABLE option_iv_history ADD COLUMN IF NOT EXISTS as_of TIMESTAMPTZ;

-- Existing rows have only day granularity; anchor them at ET midnight of their date.
-- Use timezone('America/New_York', date::timestamp) — NOT date::timestamptz, which
-- would interpret the date in the DB session zone (UTC) and land the instant in the
-- prior ET evening, so a later write-through read would bucket the row under the wrong
-- ET day (cache miss). ET-midnight round-trips back to the same ET calendar day.
UPDATE option_iv_history
   SET as_of = timezone('America/New_York', date::timestamp)
 WHERE as_of IS NULL;
