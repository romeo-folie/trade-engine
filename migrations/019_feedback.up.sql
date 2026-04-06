-- Phase 17: Feedback pipeline — performance metrics and alert flags.
CREATE TABLE feedback_insights (
    id          UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    date        DATE NOT NULL,
    scope       TEXT NOT NULL,
    scope_key   TEXT NOT NULL,
    metric      TEXT NOT NULL,
    value       NUMERIC NOT NULL,
    time_window TEXT NOT NULL,
    trade_count INT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE(date, scope, scope_key, metric, time_window)
);

CREATE INDEX idx_feedback_insights_lookup
    ON feedback_insights (scope, scope_key, date DESC);

CREATE TABLE feedback_flags (
    id            UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    date          DATE NOT NULL,
    scope         TEXT NOT NULL,
    scope_key     TEXT NOT NULL,
    severity      TEXT NOT NULL DEFAULT 'warning',
    message       TEXT NOT NULL,
    metric        TEXT NOT NULL,
    metric_value  NUMERIC NOT NULL,
    threshold     NUMERIC NOT NULL,
    resolved      BOOLEAN NOT NULL DEFAULT FALSE,
    resolved_at   TIMESTAMPTZ,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_feedback_flags_active
    ON feedback_flags (resolved, created_at DESC);
