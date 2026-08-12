-- 085: distinguish a broker-generated bracket child projection from a
-- caller-submitted intent. Children are reconciled by broker id, never replayed
-- as a new request on startup.
ALTER TABLE stock_order_intents
    ADD COLUMN IF NOT EXISTS purpose TEXT NOT NULL DEFAULT 'submit';

CREATE INDEX IF NOT EXISTS idx_stock_order_intents_broker_projection
    ON stock_order_intents (broker_order_id)
    WHERE purpose = 'broker_projection';
