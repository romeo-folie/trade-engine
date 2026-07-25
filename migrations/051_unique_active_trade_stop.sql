-- A trade may have only one active broker STOP owner. Both the entry path and
-- trailing manager can seed protection, and a concurrent "no row" observation
-- must resolve through the caller's broker-order rollback rather than leaving
-- two full-quantity stops live.
CREATE UNIQUE INDEX IF NOT EXISTS idx_orders_one_active_stop_per_trade
    ON orders (trade_id)
    WHERE trade_id IS NOT NULL
      AND is_stop_loss = TRUE
      AND order_type IN ('stop','stop_limit')
      AND status IN ('pending','partial');
