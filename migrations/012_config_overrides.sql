-- 012_config_overrides.sql
-- Runtime config overrides stored in the database.
-- Overrides take precedence over YAML config but below CLI flags.

CREATE TABLE config_overrides (
    key        TEXT        PRIMARY KEY,
    value      TEXT        NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
