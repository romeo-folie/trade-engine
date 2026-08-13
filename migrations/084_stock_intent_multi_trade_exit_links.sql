-- 084: a single aggregate standard-stock exit may project to one order row
-- per owned trade. Keep each row intent-linked without treating those rows as
-- duplicates.
DROP INDEX IF EXISTS idx_orders_stock_intent_broker;

CREATE UNIQUE INDEX IF NOT EXISTS idx_orders_stock_intent_broker_trade
    ON orders (stock_intent_id, broker_order_id, COALESCE(trade_id::text,''))
    WHERE stock_intent_id IS NOT NULL;
