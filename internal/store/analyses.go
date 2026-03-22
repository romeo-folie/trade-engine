package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/romeo-folie/trade-engine/internal/analysis"
)

// StoredAnalysis is a row from the analyses table.
type StoredAnalysis struct {
	ID            string
	Ticker        string
	Strategy      string
	Action        string
	Confidence    float64
	SizePct       float64
	StopLoss      float64
	TakeProfit    float64
	Reasoning     string
	RawResponse   string
	RawPrompt     string
	PromptHash    string
	ModelProvider string
	ModelName     string
	InputTokens   int
	OutputTokens  int
	LatencyMs     int64
	Indicators    []byte // JSONB
	CreatedAt     time.Time
}

// InsertAnalysis persists a completed analysis record and returns its UUID.
func InsertAnalysis(ctx context.Context, ticker, strategy string, resp analysis.AnalysisResponse, meta analysis.ModelCallMeta) (string, error) {
	q := `
		INSERT INTO analyses (
			ticker, strategy, action, confidence, size_pct,
			stop_loss, take_profit, reasoning,
			raw_response, raw_prompt, prompt_hash,
			model_provider, model_name,
			token_count_input, token_count_output, latency_ms,
			indicators
		) VALUES (
			$1, $2, $3, $4, $5,
			$6, $7, $8,
			$9, $10, $11,
			$12, $13,
			$14, $15, $16,
			$17
		) RETURNING id`
	var id string
	err := GetDB().QueryRow(ctx, q,
		ticker, strategy, string(resp.Action), resp.Confidence, resp.SizePct,
		nullableFloat(resp.StopLoss), nullableFloat(resp.TakeProfit), resp.Reasoning,
		meta.RawResponse, meta.RawPrompt, meta.PromptHash,
		meta.Provider, meta.Model,
		meta.InputTokens, meta.OutputTokens, meta.LatencyMs,
		meta.IndicatorJSON,
	).Scan(&id)
	return id, err
}

// EnsembleRecord carries Phase 12 ensemble-specific fields for storage.
type EnsembleRecord struct {
	EnsembleResults []byte // JSON-serialised []AnalystResult
	ArbiterResponse []byte // JSON-serialised ArbiterResult; nil if fast-pathed
	FastPathed      bool
	DissentSummary  string
}

// InsertEnsembleAnalysis persists an ensemble analysis record (Phase 12).
func InsertEnsembleAnalysis(ctx context.Context, ticker, strategy string, resp analysis.AnalysisResponse, meta analysis.ModelCallMeta, ensemble EnsembleRecord) (string, error) {
	q := `
		INSERT INTO analyses (
			ticker, strategy, action, confidence, size_pct,
			stop_loss, take_profit, reasoning,
			raw_response, raw_prompt, prompt_hash,
			model_provider, model_name,
			token_count_input, token_count_output, latency_ms,
			indicators,
			ensemble_results, arbiter_response, fast_pathed, dissent_summary
		) VALUES (
			$1, $2, $3, $4, $5,
			$6, $7, $8,
			$9, $10, $11,
			$12, $13,
			$14, $15, $16,
			$17,
			$18, $19, $20, $21
		) RETURNING id`
	var id string
	err := GetDB().QueryRow(ctx, q,
		ticker, strategy, string(resp.Action), resp.Confidence, resp.SizePct,
		nullableFloat(resp.StopLoss), nullableFloat(resp.TakeProfit), resp.Reasoning,
		meta.RawResponse, meta.RawPrompt, meta.PromptHash,
		meta.Provider, meta.Model,
		meta.InputTokens, meta.OutputTokens, meta.LatencyMs,
		meta.IndicatorJSON,
		ensemble.EnsembleResults, ensemble.ArbiterResponse, ensemble.FastPathed,
		nullableString(ensemble.DissentSummary),
	).Scan(&id)
	return id, err
}

// ModelPerformanceRow is a row from the model_performance_detail view.
// JSON tags use Provider/Model to preserve the shared /api/node/models contract consumed by
// both DrillDownPanel (llm_analysis, consensus tabs) and the System screen model-performance table.
type ModelPerformanceRow struct {
	Provider             string   `json:"Provider"`
	Model                string   `json:"Model"`
	TotalRecommendations int      `json:"TotalRecommendations"`
	AvgConfidence        float64  `json:"AvgConfidence"`
	TradeCount           int      `json:"TradeCount"`
	AvgPnLPct            *float64 `json:"AvgPnLPct"`
	WinningTrades        int      `json:"WinningTrades"`
	LosingTrades         int      `json:"LosingTrades"`
	WinRate              *float64 `json:"WinRate"`
}

// GetModelPerformance returns per-model accuracy stats (Phase 12.6).
func GetModelPerformance(ctx context.Context, minTrades int) ([]ModelPerformanceRow, error) {
	q := `
		SELECT provider, model,
		       total_recommendations, avg_confidence,
		       trade_count, avg_pnl_pct,
		       winning_trades, losing_trades, win_rate
		FROM model_performance_detail
		WHERE trade_count >= $1
		ORDER BY win_rate DESC NULLS LAST, avg_pnl_pct DESC NULLS LAST`
	rows, err := GetDB().Query(ctx, q, minTrades)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ModelPerformanceRow
	for rows.Next() {
		var r ModelPerformanceRow
		if err := rows.Scan(&r.Provider, &r.Model, &r.TotalRecommendations, &r.AvgConfidence,
			&r.TradeCount, &r.AvgPnLPct, &r.WinningTrades, &r.LosingTrades, &r.WinRate); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// GetAnalysesByTicker retrieves the most recent `limit` analyses for a ticker.
func GetAnalysesByTicker(ctx context.Context, ticker string, limit int) ([]StoredAnalysis, error) {
	q := `
		SELECT id, ticker, strategy, action, confidence, size_pct,
		       COALESCE(stop_loss, 0), COALESCE(take_profit, 0), COALESCE(reasoning, ''),
		       COALESCE(raw_response, ''), COALESCE(raw_prompt, ''), prompt_hash,
		       model_provider, model_name,
		       COALESCE(token_count_input, 0), COALESCE(token_count_output, 0), COALESCE(latency_ms, 0),
		       COALESCE(indicators, '{}'), created_at
		FROM analyses
		WHERE ticker = $1
		ORDER BY created_at DESC
		LIMIT $2`
	rows, err := GetDB().Query(ctx, q, ticker, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanAnalyses(rows)
}

// GetAnalysisByID retrieves a single analysis by its UUID.
func GetAnalysisByID(ctx context.Context, id string) (*StoredAnalysis, error) {
	q := `
		SELECT id, ticker, strategy, action, confidence, size_pct,
		       COALESCE(stop_loss, 0), COALESCE(take_profit, 0), COALESCE(reasoning, ''),
		       COALESCE(raw_response, ''), COALESCE(raw_prompt, ''), prompt_hash,
		       model_provider, model_name,
		       COALESCE(token_count_input, 0), COALESCE(token_count_output, 0), COALESCE(latency_ms, 0),
		       COALESCE(indicators, '{}'), created_at
		FROM analyses
		WHERE id = $1`
	rows, err := GetDB().Query(ctx, q, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	results, err := scanAnalyses(rows)
	if err != nil || len(results) == 0 {
		return nil, err
	}
	return &results[0], nil
}

// GetRecentAnalyses retrieves the most recent limit analyses across all tickers.
func GetRecentAnalyses(ctx context.Context, limit int) ([]StoredAnalysis, error) {
	q := `
		SELECT id, ticker, strategy, action, confidence, size_pct,
		       COALESCE(stop_loss, 0), COALESCE(take_profit, 0), COALESCE(reasoning, ''),
		       COALESCE(raw_response, ''), COALESCE(raw_prompt, ''), prompt_hash,
		       model_provider, model_name,
		       COALESCE(token_count_input, 0), COALESCE(token_count_output, 0), COALESCE(latency_ms, 0),
		       COALESCE(indicators, '{}'), created_at
		FROM analyses
		ORDER BY created_at DESC
		LIMIT $1`
	rows, err := GetDB().Query(ctx, q, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanAnalyses(rows)
}

func scanAnalyses(rows pgx.Rows) ([]StoredAnalysis, error) {
	var out []StoredAnalysis
	for rows.Next() {
		var a StoredAnalysis
		if err := rows.Scan(
			&a.ID, &a.Ticker, &a.Strategy, &a.Action, &a.Confidence, &a.SizePct,
			&a.StopLoss, &a.TakeProfit, &a.Reasoning,
			&a.RawResponse, &a.RawPrompt, &a.PromptHash,
			&a.ModelProvider, &a.ModelName,
			&a.InputTokens, &a.OutputTokens, &a.LatencyMs,
			&a.Indicators, &a.CreatedAt,
		); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// nullableFloat returns nil for zero values (stop_loss / take_profit on HOLD).
func nullableFloat(f float64) any {
	if f == 0 {
		return nil
	}
	return f
}

// AnalysisSummary is a lightweight projection of StoredAnalysis for list views.
// It omits raw_response, raw_prompt, prompt_hash, and indicators to keep
// the /api/analyses/recent payload small.
// AnalysisSummary uses PascalCase JSON keys to match the TypeScript AnalysisRecord interface
// defined in web/frontend/src/screens/Analysis.tsx.
type AnalysisSummary struct {
	ID            string    `json:"ID"`
	Ticker        string    `json:"Ticker"`
	Strategy      string    `json:"Strategy"`
	Action        string    `json:"Action"`
	Confidence    float64   `json:"Confidence"`
	SizePct       float64   `json:"SizePct"`
	StopLoss      float64   `json:"StopLoss"`
	TakeProfit    float64   `json:"TakeProfit"`
	Reasoning     string    `json:"Reasoning"`
	ModelProvider string    `json:"ModelProvider"`
	ModelName     string    `json:"ModelName"`
	InputTokens   int       `json:"InputTokens"`
	OutputTokens  int       `json:"OutputTokens"`
	LatencyMs     int       `json:"LatencyMs"`
	CreatedAt     time.Time `json:"CreatedAt"`
}

// GetRecentAnalysesSummary retrieves summary fields for the most recent analyses.
// It does not fetch raw_response, raw_prompt, prompt_hash, or indicators.
func GetRecentAnalysesSummary(ctx context.Context, limit int) ([]AnalysisSummary, error) {
	q := `
		SELECT id, ticker, strategy, action, confidence, size_pct,
		       COALESCE(stop_loss, 0), COALESCE(take_profit, 0), COALESCE(reasoning, ''),
		       model_provider, model_name,
		       COALESCE(token_count_input, 0), COALESCE(token_count_output, 0), COALESCE(latency_ms, 0),
		       created_at
		FROM analyses
		ORDER BY created_at DESC
		LIMIT $1`
	rows, err := GetDB().Query(ctx, q, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []AnalysisSummary
	for rows.Next() {
		var a AnalysisSummary
		if err := rows.Scan(
			&a.ID, &a.Ticker, &a.Strategy, &a.Action, &a.Confidence, &a.SizePct,
			&a.StopLoss, &a.TakeProfit, &a.Reasoning,
			&a.ModelProvider, &a.ModelName,
			&a.InputTokens, &a.OutputTokens, &a.LatencyMs,
			&a.CreatedAt,
		); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// GetAnalystResultsBatchForSignals fetches ensemble_results for a batch of signals
// in a single query (one roundtrip). Returns a map from signal array-index to
// ensemble_results JSON. Signals with no linked analysis are absent from the map.
func GetAnalystResultsBatchForSignals(ctx context.Context, sigs []Signal) (map[int][]byte, error) {
	// Collect the first analysis ID per signal; remember original index.
	type entry struct {
		sigIdx     int
		analysisID string
	}
	var entries []entry
	for i, sig := range sigs {
		if len(sig.AnalysisIDs) > 0 && sig.AnalysisIDs[0] != "" {
			entries = append(entries, entry{i, sig.AnalysisIDs[0]})
		}
	}
	if len(entries) == 0 {
		return nil, nil
	}

	ids := make([]string, len(entries))
	for i, e := range entries {
		ids[i] = e.analysisID
	}

	rows, err := GetDB().Query(ctx, `
		SELECT id, ensemble_results
		FROM analyses
		WHERE id = ANY($1)
	`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	// Build analysis-id → JSON map
	byAnalysisID := make(map[string][]byte, len(entries))
	for rows.Next() {
		var id string
		var raw []byte
		if err := rows.Scan(&id, &raw); err != nil {
			return nil, err
		}
		byAnalysisID[id] = raw
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Map back to signal index
	out := make(map[int][]byte, len(entries))
	for _, e := range entries {
		if raw, ok := byAnalysisID[e.analysisID]; ok {
			out[e.sigIdx] = raw
		}
	}
	return out, nil
}

// GetAnalystResultsBatchForTrades fetches ensemble_results for a batch of trades
// in a single query (one roundtrip). Returns a map from trade array-index to
// ensemble_results JSON. Trades with no linked signal/analysis are absent from the map.
func GetAnalystResultsBatchForTrades(ctx context.Context, trades []Trade) (map[int][]byte, error) {
	type entry struct {
		tradeIdx int
		signalID string
	}
	var entries []entry
	for i, t := range trades {
		if t.SignalID != "" {
			entries = append(entries, entry{i, t.SignalID})
		}
	}
	if len(entries) == 0 {
		return nil, nil
	}

	signalIDs := make([]string, len(entries))
	for i, e := range entries {
		signalIDs[i] = e.signalID
	}

	rows, err := GetDB().Query(ctx, `
		SELECT s.id AS signal_id, a.ensemble_results
		FROM signals s
		JOIN analyses a ON a.id = ANY(s.analysis_ids)
		WHERE s.id = ANY($1)
	`, signalIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	bySignalID := make(map[string][]byte, len(entries))
	for rows.Next() {
		var signalID string
		var raw []byte
		if err := rows.Scan(&signalID, &raw); err != nil {
			return nil, err
		}
		// Keep first result per signal (same as the old single-row query)
		if _, seen := bySignalID[signalID]; !seen {
			bySignalID[signalID] = raw
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make(map[int][]byte, len(entries))
	for _, e := range entries {
		if raw, ok := bySignalID[e.signalID]; ok {
			out[e.tradeIdx] = raw
		}
	}
	return out, nil
}
