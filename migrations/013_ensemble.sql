-- 013_ensemble.sql
-- Phase 12: Ensemble Deliberation — adds per-analyst storage to analyses table.
-- All new columns are nullable for full backward compatibility.

ALTER TABLE analyses ADD COLUMN IF NOT EXISTS ensemble_results JSONB;
ALTER TABLE analyses ADD COLUMN IF NOT EXISTS arbiter_response JSONB;
ALTER TABLE analyses ADD COLUMN IF NOT EXISTS fast_pathed BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE analyses ADD COLUMN IF NOT EXISTS dissent_summary TEXT;

CREATE INDEX IF NOT EXISTS idx_analyses_fast_pathed ON analyses (fast_pathed) WHERE fast_pathed = TRUE;

-- Per-model performance view (Phase 12.6)
CREATE OR REPLACE VIEW model_performance_detail AS
WITH analyst_recs AS (
    SELECT
        a.id AS analysis_id,
        a.ticker,
        a.created_at,
        elem->>'provider'            AS provider,
        elem->>'model'               AS model,
        (elem->>'action')::signal_action AS action,
        (elem->>'confidence')::NUMERIC(4,3) AS confidence,
        a.fast_pathed
    FROM analyses a,
         jsonb_array_elements(a.ensemble_results) AS elem
    WHERE a.ensemble_results IS NOT NULL
      AND a.ensemble_results != 'null'::jsonb
      AND jsonb_array_length(a.ensemble_results) > 0
),
outcomes AS (
    SELECT unnest(s.analysis_ids) AS analysis_id, t.pnl_pct
    FROM signals s
    JOIN trades t ON t.signal_id = s.id
    WHERE t.pnl_pct IS NOT NULL
)
SELECT
    ar.provider,
    ar.model,
    COUNT(*)                                                              AS total_recommendations,
    AVG(ar.confidence)::NUMERIC(4,3)                                     AS avg_confidence,
    COUNT(o.pnl_pct)                                                     AS trade_count,
    AVG(o.pnl_pct)::NUMERIC(10,4)                                        AS avg_pnl_pct,
    COUNT(o.pnl_pct) FILTER (WHERE o.pnl_pct > 0)                        AS winning_trades,
    COUNT(o.pnl_pct) FILTER (WHERE o.pnl_pct <= 0)                       AS losing_trades,
    CASE WHEN COUNT(o.pnl_pct) > 0
         THEN COUNT(o.pnl_pct) FILTER (WHERE o.pnl_pct > 0)::FLOAT / COUNT(o.pnl_pct)
         ELSE NULL END                                                    AS win_rate
FROM analyst_recs ar
LEFT JOIN outcomes o ON o.analysis_id = ar.analysis_id
GROUP BY ar.provider, ar.model
ORDER BY trade_count DESC, avg_pnl_pct DESC NULLS LAST;

-- Fast-path vs arbitrated comparison view (Phase 12.6)
CREATE OR REPLACE VIEW ensemble_path_performance AS
SELECT
    a.fast_pathed,
    COUNT(t.id)                                              AS trade_count,
    AVG(t.pnl_pct)::NUMERIC(10,4)                           AS avg_pnl_pct,
    STDDEV(t.pnl_pct)::NUMERIC(10,4)                        AS pnl_stddev,
    COUNT(t.id) FILTER (WHERE t.pnl_pct > 0)                AS winning_trades,
    COUNT(t.id) FILTER (WHERE t.pnl_pct <= 0)               AS losing_trades,
    CASE WHEN COUNT(t.id) > 0
         THEN COUNT(t.id) FILTER (WHERE t.pnl_pct > 0)::FLOAT / COUNT(t.id)
         ELSE NULL END                                        AS win_rate
FROM analyses a
JOIN signals s ON a.id = ANY(s.analysis_ids)
JOIN trades t ON t.signal_id = s.id
WHERE t.pnl_pct IS NOT NULL
  AND a.ensemble_results IS NOT NULL
GROUP BY a.fast_pathed;
