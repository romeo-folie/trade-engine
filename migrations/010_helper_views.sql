-- 010_helper_views.sql
-- Convenience views for common query patterns.

-- Current open positions with aggregated P&L context.
CREATE OR REPLACE VIEW open_positions AS
SELECT
    t.id,
    t.ticker,
    t.direction,
    t.strategy,
    t.entry_price,
    t.quantity,
    t.stop_loss,
    t.take_profit,
    t.model_provider,
    t.model_name,
    t.opened_at,
    -- Realised P&L placeholder: computed at query time by caller with current price.
    0::NUMERIC(20,8) AS unrealized_pnl,
    t.signal_id
FROM trades t
WHERE t.status = 'open';

-- Last 100 signals with status and ticker context.
CREATE OR REPLACE VIEW recent_signals AS
SELECT
    s.id,
    s.ticker,
    s.action,
    s.strategy,
    s.signal_price,
    s.confidence,
    s.status,
    s.block_reason,
    s.expires_at,
    s.created_at,
    s.updated_at
FROM signals s
ORDER BY s.created_at DESC
LIMIT 100;

-- Per-model accuracy tracking: win rate on trades triggered by each model.
CREATE OR REPLACE VIEW model_performance AS
SELECT
    t.model_provider,
    t.model_name,
    COUNT(*)                                                       AS total_trades,
    COUNT(*) FILTER (WHERE t.pnl > 0)                             AS wins,
    COUNT(*) FILTER (WHERE t.pnl < 0)                             AS losses,
    ROUND(
        100.0 * COUNT(*) FILTER (WHERE t.pnl > 0) /
        NULLIF(COUNT(*) FILTER (WHERE t.status = 'closed'), 0),
    2)                                                             AS win_rate_pct,
    ROUND(AVG(t.pnl_pct) FILTER (WHERE t.status = 'closed'), 4)   AS avg_pnl_pct
FROM trades t
WHERE t.model_provider IS NOT NULL
GROUP BY t.model_provider, t.model_name;

-- Per-strategy performance summary.
CREATE OR REPLACE VIEW strategy_performance AS
SELECT
    t.strategy,
    COUNT(*)                                                       AS total_trades,
    COUNT(*) FILTER (WHERE t.pnl > 0)                             AS wins,
    COUNT(*) FILTER (WHERE t.pnl < 0)                             AS losses,
    ROUND(
        100.0 * COUNT(*) FILTER (WHERE t.pnl > 0) /
        NULLIF(COUNT(*) FILTER (WHERE t.status = 'closed'), 0),
    2)                                                             AS win_rate_pct,
    ROUND(AVG(t.pnl_pct) FILTER (WHERE t.status = 'closed'), 4)   AS avg_pnl_pct,
    ROUND(SUM(t.pnl)      FILTER (WHERE t.status = 'closed'), 8)   AS total_pnl
FROM trades t
GROUP BY t.strategy;
