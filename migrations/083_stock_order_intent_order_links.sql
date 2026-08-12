-- 083: link durable stock intents to their projected order rows (#539).
--
-- Kept separate from 082 because 082 may already be applied on a running
-- database; migrations are immutable once recorded.
ALTER TABLE orders ADD COLUMN IF NOT EXISTS stock_intent_id UUID REFERENCES stock_order_intents(id);

ALTER TABLE trades ADD COLUMN IF NOT EXISTS stock_intent_id UUID REFERENCES stock_order_intents(id);

CREATE UNIQUE INDEX IF NOT EXISTS idx_orders_stock_intent_broker
    ON orders (stock_intent_id, broker_order_id) WHERE stock_intent_id IS NOT NULL;

CREATE UNIQUE INDEX IF NOT EXISTS idx_trades_stock_intent
    ON trades (stock_intent_id) WHERE stock_intent_id IS NOT NULL;
