-- 038_watchlist_graph_context.sql
-- Persist WHY a scanner candidate was sourced (graph link / news-sympathy
-- trigger) on the watchlist item, so the engine's deep-analysis prompt can
-- carry the origin thesis (e.g. "NVDA news 2h ago: '<headline>' — NVDA
-- supplies_to this company") instead of losing it after the scanner pass.
ALTER TABLE watchlist ADD COLUMN IF NOT EXISTS graph_context TEXT;
