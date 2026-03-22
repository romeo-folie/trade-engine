-- Remove prediction market tables and enum values
-- Phase 9 (prediction markets) has been moved to polymarket-geopolitical-agent

DROP TABLE IF EXISTS prediction_snapshots CASCADE;

-- Remove prediction_contract from entity_type enum.
-- PostgreSQL cannot remove enum values directly.
-- We rename the enum, create a new one without prediction_contract, migrate, drop old.
ALTER TYPE entity_type RENAME TO entity_type_old;

CREATE TYPE entity_type AS ENUM (
    'company', 'sector', 'person', 'country', 'commodity',
    'event', 'index'
);

ALTER TABLE entities
    ALTER COLUMN entity_type TYPE entity_type
    USING entity_type::text::entity_type;

DROP TYPE entity_type_old;
