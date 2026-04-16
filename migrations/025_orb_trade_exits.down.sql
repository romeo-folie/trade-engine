ALTER TABLE orb_trades
    DROP COLUMN IF EXISTS tp_order_id,
    DROP COLUMN IF EXISTS sl_order_id,
    DROP COLUMN IF EXISTS exit_price,
    DROP COLUMN IF EXISTS exit_type;
