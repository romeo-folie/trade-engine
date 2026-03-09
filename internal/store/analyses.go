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
type ModelPerformanceRow struct {
	Provider             string
	Model                string
	TotalRecommendations int
	AvgConfidence        float64
	TradeCount           int
	AvgPnLPct            *float64
	WinningTrades        int
	LosingTrades         int
	WinRate              *float64
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
