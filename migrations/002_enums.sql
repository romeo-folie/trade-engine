-- 002_enums.sql
-- Domain enumerations used across the schema.

CREATE TYPE engine_mode       AS ENUM ('manual', 'auto', 'paper');
CREATE TYPE signal_action     AS ENUM ('BUY', 'SELL', 'HOLD');
CREATE TYPE signal_status     AS ENUM ('pending', 'approved', 'rejected', 'expired', 'stale', 'blocked', 'executed');
CREATE TYPE trade_direction   AS ENUM ('long', 'short');
CREATE TYPE trade_status      AS ENUM ('open', 'closed', 'cancelled');
CREATE TYPE order_side        AS ENUM ('buy', 'sell');
CREATE TYPE order_type        AS ENUM ('market', 'limit', 'stop', 'stop_limit');
CREATE TYPE order_status      AS ENUM ('pending', 'filled', 'partial', 'cancelled', 'rejected');
CREATE TYPE entity_type       AS ENUM (
    'company', 'sector', 'person', 'country', 'commodity',
    'event', 'prediction_contract', 'index'
);
CREATE TYPE relationship_type AS ENUM (
    'supplies_to', 'supplied_by', 'competes_with', 'subsidiary_of', 'parent_of',
    'partner_of', 'sector_member', 'manufactures_for', 'manufactured_by',
    'regulates', 'regulated_by', 'affects_contract', 'affected_by_event'
);
CREATE TYPE timeframe         AS ENUM ('1m', '5m', '15m', '1h', '4h', '1d', '1w');
