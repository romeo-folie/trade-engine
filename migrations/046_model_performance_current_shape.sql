-- 046: model_performance_detail — read the CURRENT ensemble_results shape.
--
-- The view from 013_ensemble.sql extracted lowercase keys ('provider', 'action',
-- 'confidence') from ensemble_results elements, but the engine serializes Go
-- AnalystResult structs with capitalized keys and a nested Response:
--   {"Provider": "...", "Model": "...", "LatencyMs": 1234,
--    "Response": {"action": "buy", "confidence": 0.71, ...},
--    "Meta": {"InputTokens": 900, "OutputTokens": 400, ...}, "Err": null}
-- Every extract returned NULL, so the view collapsed all recommendations into a
-- single (NULL, NULL) group — the dashboard's Model Performance table showed one
-- meaningless "unknown" row. This rewrite reads the real keys, excludes errored
-- analyst calls (empty Response.action), and adds the latency / tokens-per-call
-- aggregates the System screen has always tried to render (AvgLatencyMs /
-- TokensPerCall were never produced by the old view).

DROP VIEW IF EXISTS model_performance_detail;

CREATE VIEW model_performance_detail AS
WITH analyst_recs AS (
    SELECT a.id AS analysis_id,
           a.ticker,
           a.created_at,
           elem.value ->> 'Provider' AS provider,
           elem.value ->> 'Model' AS model,
           (elem.value -> 'Response' ->> 'action')::signal_action AS action,
           (elem.value -> 'Response' ->> 'confidence')::numeric(4,3) AS confidence,
           NULLIF((elem.value ->> 'LatencyMs')::bigint, 0) AS latency_ms,
           NULLIF(
               COALESCE((elem.value -> 'Meta' ->> 'InputTokens')::bigint, 0)
             + COALESCE((elem.value -> 'Meta' ->> 'OutputTokens')::bigint, 0),
             0
           ) AS tokens,
           a.fast_pathed
    FROM analyses a,
         LATERAL jsonb_array_elements(a.ensemble_results) elem(value)
    WHERE a.ensemble_results IS NOT NULL
      AND a.ensemble_results <> 'null'::jsonb
      AND jsonb_array_length(a.ensemble_results) > 0
      -- Errored analyst calls persist with an empty Response — not recommendations.
      AND COALESCE(elem.value -> 'Response' ->> 'action', '') <> ''
), outcomes AS (
    SELECT unnest(s.analysis_ids) AS analysis_id,
           t.pnl_pct
    FROM signals s
    JOIN trades t ON t.signal_id = s.id
    WHERE t.pnl_pct IS NOT NULL
)
SELECT ar.provider,
       ar.model,
       count(*) AS total_recommendations,
       avg(ar.confidence)::numeric(4,3) AS avg_confidence,
       count(o.pnl_pct) AS trade_count,
       avg(o.pnl_pct)::numeric(10,4) AS avg_pnl_pct,
       count(o.pnl_pct) FILTER (WHERE o.pnl_pct > 0) AS winning_trades,
       count(o.pnl_pct) FILTER (WHERE o.pnl_pct <= 0) AS losing_trades,
       CASE WHEN count(o.pnl_pct) > 0
            THEN count(o.pnl_pct) FILTER (WHERE o.pnl_pct > 0)::double precision / count(o.pnl_pct)
       END AS win_rate,
       avg(ar.latency_ms)::double precision AS avg_latency_ms,
       avg(ar.tokens)::double precision AS tokens_per_call
FROM analyst_recs ar
LEFT JOIN outcomes o ON o.analysis_id = ar.analysis_id
GROUP BY ar.provider, ar.model;
