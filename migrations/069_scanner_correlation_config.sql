-- Persist correlation-aware promotion policy with each scanner run.
ALTER TABLE scanner_runs
    ADD COLUMN promotion_correlation_enabled BOOLEAN NOT NULL DEFAULT false,
    ADD COLUMN promotion_max_abs_correlation DOUBLE PRECISION NOT NULL DEFAULT 0
        CHECK (promotion_max_abs_correlation BETWEEN 0 AND 1),
    ADD COLUMN promotion_correlation_lookback_days INTEGER NOT NULL DEFAULT 60
        CHECK (promotion_correlation_lookback_days > 0),
    ADD COLUMN promotion_correlation_min_overlap INTEGER NOT NULL DEFAULT 30
        CHECK (promotion_correlation_min_overlap > 0
               AND promotion_correlation_min_overlap <= promotion_correlation_lookback_days);
