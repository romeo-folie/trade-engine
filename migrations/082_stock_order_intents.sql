-- 082: durable, replayable stock-broker order intents (#539).
--
-- These rows are deliberately separate from legacy orders: an intent exists
-- before a broker request, can survive without a broker id, and preserves the
-- immutable payload and client_order_id used to make replay idempotent.
CREATE TABLE IF NOT EXISTS stock_order_intents (
    id                  UUID PRIMARY KEY,
    idempotency_key     TEXT NOT NULL UNIQUE,
    client_order_id     TEXT NOT NULL UNIQUE,
    trade_id            UUID REFERENCES trades(id),
    parent_intent_id    UUID REFERENCES stock_order_intents(id),
    ticker              TEXT NOT NULL,
    side                order_side NOT NULL,
    order_type          order_type NOT NULL,
    requested_qty       NUMERIC(20,8) NOT NULL,
    request_payload     JSONB NOT NULL,
    payload_hash        TEXT NOT NULL,
    broker_order_id     TEXT,
    status              order_status NOT NULL DEFAULT 'pending',
    filled_price        NUMERIC(20,8),
    filled_qty          NUMERIC(20,8) NOT NULL DEFAULT 0,
    broker_error        TEXT,
    broker_request_id   TEXT,
    submitted_at        TIMESTAMPTZ,
    last_checked_at     TIMESTAMPTZ,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_stock_order_intents_nonterminal
    ON stock_order_intents (updated_at)
    WHERE status IN ('pending', 'partial');
CREATE INDEX IF NOT EXISTS idx_stock_order_intents_trade ON stock_order_intents (trade_id);
CREATE INDEX IF NOT EXISTS idx_stock_order_intents_ticker_nonterminal
    ON stock_order_intents (ticker)
    WHERE status IN ('pending', 'partial');

CREATE TABLE IF NOT EXISTS stock_order_attempts (
    id                  UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    intent_id           UUID NOT NULL REFERENCES stock_order_intents(id) ON DELETE CASCADE,
    operation           TEXT NOT NULL,
    broker_order_id     TEXT,
    broker_request_id   TEXT,
    outcome             TEXT NOT NULL,
    error               TEXT,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_stock_order_attempts_intent ON stock_order_attempts (intent_id, created_at);
