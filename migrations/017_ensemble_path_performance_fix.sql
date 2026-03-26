-- 017_ensemble_path_performance_fix.sql
-- Tighten ensemble_path_performance view to require jsonb_array_length > 1.
--
-- Context: engine.go now calls InsertEnsembleAnalysis for all runs (>= 1
-- analyst) so that ensemble_results is always populated and the frontend
-- analyst panel has data. Single-analyst records are legitimate in that table,
-- but they should NOT be counted in the "fast-path vs arbitrated" comparison
-- view because that comparison is only meaningful when at least two analysts
-- participated (fast-path = unanimous, arbitrated = arbiter intervened).
--
-- The old filter (ensemble_results IS NOT NULL) now admits single-analyst rows,
-- which have no fast-path / arbiter distinction and would distort both cohorts.
-- The new filter (jsonb_array_length > 1) restricts the view to true ensemble
-- runs, preserving the intended semantics.

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
  AND jsonb_array_length(a.ensemble_results) > 1
GROUP BY a.fast_pathed;
