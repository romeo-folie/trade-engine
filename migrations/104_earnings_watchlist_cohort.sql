-- Preserve the original ranked top-five across retries after partial admission.
ALTER TABLE earnings_watchlist_runs
    ADD COLUMN IF NOT EXISTS selected_candidates JSONB;
