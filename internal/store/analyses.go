package store

import (
	"context"
	"errors"
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
	return InsertAnalysisWithScannerRun(ctx, ticker, strategy, resp, meta, "")
}

// InsertAnalysisWithScannerRun persists a completed analysis with optional
// prospective scanner lineage.
func InsertAnalysisWithScannerRun(
	ctx context.Context,
	ticker, strategy string,
	resp analysis.AnalysisResponse,
	meta analysis.ModelCallMeta,
	scannerRunID string,
) (string, error) {
	q := `
		INSERT INTO analyses (
			ticker, strategy, action, confidence, size_pct,
			stop_loss, take_profit, reasoning,
			raw_response, raw_prompt, prompt_hash,
			model_provider, model_name,
			token_count_input, token_count_output, latency_ms,
			indicators, scanner_run_id
		) VALUES (
			$1, $2, $3, $4, $5,
			$6, $7, $8,
			$9, $10, $11,
			$12, $13,
			$14, $15, $16,
			$17, $18
		) RETURNING id`
	var id string
	err := GetDB().QueryRow(ctx, q,
		ticker, strategy, string(resp.Action), resp.Confidence, resp.SizePct,
		nullableFloat(resp.StopLoss), nullableFloat(resp.TakeProfit), resp.Reasoning,
		meta.RawResponse, meta.RawPrompt, meta.PromptHash,
		meta.Provider, meta.Model,
		meta.InputTokens, meta.OutputTokens, meta.LatencyMs,
		meta.IndicatorJSON, nullableString(scannerRunID),
	).Scan(&id)
	return id, err
}

// EnsembleRecord carries Phase 12 ensemble-specific fields for storage.
type EnsembleRecord struct {
	EnsembleResults []byte // JSON-serialised []AnalystResult
	ArbiterResponse []byte // JSON-serialised ArbiterResult; nil if fast-pathed
	FastPathed      bool
	DissentSummary  string
	ScannerRunID    string
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
			ensemble_results, arbiter_response, fast_pathed, dissent_summary,
			scanner_run_id
		) VALUES (
			$1, $2, $3, $4, $5,
			$6, $7, $8,
			$9, $10, $11,
			$12, $13,
			$14, $15, $16,
			$17,
			$18, $19, $20, $21, $22
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
		nullableString(ensemble.DissentSummary), nullableString(ensemble.ScannerRunID),
	).Scan(&id)
	return id, err
}

// ModelPerformanceRow is a row from the model_performance_detail view.
// JSON tags use Provider/Model to preserve the shared /api/node/models contract consumed by
// both DrillDownPanel (llm_analysis, consensus tabs) and the System screen model-performance table.
type ModelPerformanceRow struct {
	Provider             *string  `json:"Provider"`
	Model                *string  `json:"Model"`
	TotalRecommendations int      `json:"TotalRecommendations"`
	AvgConfidence        *float64 `json:"AvgConfidence"`
	TradeCount           int      `json:"TradeCount"`
	AvgPnLPct            *float64 `json:"AvgPnLPct"`
	WinningTrades        int      `json:"WinningTrades"`
	LosingTrades         int      `json:"LosingTrades"`
	WinRate              *float64 `json:"WinRate"`
	// AvgLatencyMs and TokensPerCall come from migration 046 — the System
	// screen's table always rendered these columns but the view never
	// produced them (they showed a permanent "—").
	AvgLatencyMs  *float64 `json:"AvgLatencyMs"`
	TokensPerCall *float64 `json:"TokensPerCall"`
}

// GetModelPerformance returns per-model accuracy stats (Phase 12.6).
func GetModelPerformance(ctx context.Context, minTrades int) ([]ModelPerformanceRow, error) {
	q := `
		SELECT COALESCE(provider, 'unknown'), COALESCE(model, 'unknown'),
		       total_recommendations, avg_confidence,
		       trade_count, avg_pnl_pct,
		       winning_trades, losing_trades, win_rate,
		       avg_latency_ms, tokens_per_call
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
			&r.TradeCount, &r.AvgPnLPct, &r.WinningTrades, &r.LosingTrades, &r.WinRate,
			&r.AvgLatencyMs, &r.TokensPerCall); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ── Entry performance (signal-provenance study) ──────────────────────────────
//
// These functions power the Performance screen: every closed equity trade
// joined back to the analysis that produced it (trades → signals.analysis_ids →
// analyses), then sliced by how the signal was made — confidence, decision mode
// (fast-path / arbitrated / consensus), strategy, direction and analyst count.
// ORB/forex live in orb_trades and are excluded by construction (they never
// carry a signal_id into `signals`).
//
// Win rate is defined once, everywhere: wins / all-closed × 100, with
// break-even (pnl = 0) trades kept in the denominator. The patched
// GetPortfolioStats uses the same definition so the two surfaces agree.

// entryBaseCTE is the shared join — exactly one row per closed trade. `mode`
// collapses the three provenance states (fast_pathed, arbiter_response present,
// neither) into one label; note that "arbitrated" is NOT simply the negation of
// "fast-path" — a signal can be neither (analysts agreed but not flagged
// fast-path).
//
// A signal's analysis_ids is an array (a signal can be produced by more than one
// analysis). We must attribute each trade to a single analysis or a multi-
// analysis signal would duplicate its trade across every count(*)/sum(pnl)/top-N
// below. The LATERAL … LIMIT 1 picks the most recent analysis per signal
// deterministically (created_at DESC, id tie-break), mirroring the DISTINCT ON
// (s.id) attribution used by the analyst batch path.
const entryBaseCTE = `
WITH base AS (
    SELECT t.ticker, t.direction::text AS dir, t.strategy,
           t.pnl::float8 AS pnl, t.pnl_pct::float8 AS pnl_pct, t.opened_at,
           a.confidence::float8 AS conf,
           CASE WHEN a.fast_pathed THEN 'fast_path'
                WHEN a.arbiter_response IS NOT NULL THEN 'arbitrated'
                ELSE 'consensus' END AS mode,
           jsonb_array_length(a.ensemble_results) AS n_analysts,
           a.ensemble_results AS ensemble
    FROM trades t
    JOIN signals s ON s.id = t.signal_id
    JOIN LATERAL (
        SELECT a.confidence, a.fast_pathed, a.arbiter_response, a.ensemble_results
        FROM unnest(s.analysis_ids) AS aid
        JOIN analyses a ON a.id = aid
        ORDER BY a.created_at DESC, a.id DESC
        LIMIT 1
    ) a ON true
    WHERE t.status = 'closed' AND t.pnl IS NOT NULL
)`

// EntryPerfOverall is the headline row across all closed equity entries.
type EntryPerfOverall struct {
	N            int      `json:"n"`
	Wins         int      `json:"wins"`
	Losses       int      `json:"losses"`
	Breakeven    int      `json:"breakeven"`
	WinRatePct   float64  `json:"win_rate_pct"`
	AvgPnLPct    *float64 `json:"avg_pnl_pct"`
	MedianPnLPct *float64 `json:"median_pnl_pct"`
	NetPnL       *float64 `json:"net_pnl"`
	AvgConf      *float64 `json:"avg_conf"`
	WindowStart  *string  `json:"window_start"`
	WindowEnd    *string  `json:"window_end"`
}

// EntryPerfBucket is one row of any breakdown (confidence band, decision mode,
// strategy, direction, analyst count). `Label` carries the group key.
type EntryPerfBucket struct {
	Label      string   `json:"label"`
	N          int      `json:"n"`
	WinRatePct float64  `json:"win_rate_pct"`
	AvgPnLPct  *float64 `json:"avg_pnl_pct"`
	NetPnL     *float64 `json:"net_pnl"`
	AvgConf    *float64 `json:"avg_conf"`
}

// EntryPerfConcentration surfaces how top-heavy the P&L is — at small samples a
// single trade can carry the whole book, so the UI must warn rather than imply
// broad edge.
type EntryPerfConcentration struct {
	NetPnL      *float64 `json:"net_pnl"`
	Top3PnL     *float64 `json:"top3_pnl"`
	TopTicker   *string  `json:"top_ticker"`
	TopStrategy *string  `json:"top_strategy"`
	TopPnL      *float64 `json:"top_pnl"`
	TopPnLPct   *float64 `json:"top_pnl_pct"`
	TopConf     *float64 `json:"top_conf"`
}

// EntryPerformance is the full payload for the Performance screen. ConfidenceGate
// is filled by the handler from live config (risk.confidence_threshold).
type EntryPerformance struct {
	Overall        EntryPerfOverall       `json:"overall"`
	ByConfidence   []EntryPerfBucket      `json:"by_confidence"`
	ByMode         []EntryPerfBucket      `json:"by_mode"`
	ByStrategy     []EntryPerfBucket      `json:"by_strategy"`
	ByDirection    []EntryPerfBucket      `json:"by_direction"`
	ByAnalysts     []EntryPerfBucket      `json:"by_analysts"`
	ByRole         []EntryPerfBucket      `json:"by_role"`
	Concentration  EntryPerfConcentration `json:"concentration"`
	ConfidenceGate float64                `json:"confidence_gate"`
}

// entryBuckets runs one grouped breakdown over the shared base CTE. labelExpr is
// the SQL projected as the group label; groupOrder is the GROUP BY … ORDER BY …
// tail. Callers pass trusted, constant SQL fragments only (never user input).
func entryBuckets(ctx context.Context, labelExpr, groupOrder string) ([]EntryPerfBucket, error) {
	q := entryBaseCTE + `
		SELECT ` + labelExpr + ` AS label,
		       count(*)::int,
		       COALESCE(round(100.0 * count(*) FILTER (WHERE pnl > 0)::numeric / NULLIF(count(*), 0), 1), 0)::float8,
		       round(avg(pnl_pct)::numeric, 2)::float8,
		       round(sum(pnl)::numeric, 2)::float8,
		       round(avg(conf)::numeric, 3)::float8
		FROM base ` + groupOrder
	rows, err := GetDB().Query(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []EntryPerfBucket{}
	for rows.Next() {
		var b EntryPerfBucket
		if err := rows.Scan(&b.Label, &b.N, &b.WinRatePct, &b.AvgPnLPct, &b.NetPnL, &b.AvgConf); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// entryRoleAccuracy measures each specialist review angle by whether its
// directional call matched what actually happened — not the win rate of trades
// it touched (in specialist mode all roles vote on every trade, so that would be
// identical across roles). A role vote is "correct" when it backed the taken
// side and the trade won, or dissented from the taken side and the trade did not
// win. HOLD/WAIT abstentions are excluded. Empty until specialist mode has run:
// the "Role" key only exists in ensemble_results written after that feature is
// enabled (rows without it are filtered out here).
func entryRoleAccuracy(ctx context.Context) ([]EntryPerfBucket, error) {
	// Net and avg-P&L are deliberately NULL for role rows. Summing the executed
	// trade's realized P&L against a role would misattribute it: a role that
	// dissented from the taken side (said SELL, engine went long) would book the
	// long's profit as if it were the role's own, so a wrong dissent could show
	// positive "net". The role cut measures hit rate, not executed profitability
	// (Codex #390). n + hit rate + avg confidence are the honest columns.
	q := entryBaseCTE + `
		SELECT v.role,
		       count(*)::int,
		       round(100.0 * avg(v.correct::int)::numeric, 1)::float8,
		       NULL::float8,
		       NULL::float8,
		       round(avg(v.conf)::numeric, 3)::float8
		FROM (
		    SELECT elem->>'Role' AS role,
		           (elem->'Response'->>'confidence')::float8 AS conf,
		           CASE
		               WHEN (CASE upper(elem->'Response'->>'action')
		                          WHEN 'BUY' THEN 'long' WHEN 'SELL' THEN 'short' END) = b.dir
		               THEN (b.pnl > 0)      -- role backed the taken side → right iff it worked
		               ELSE (b.pnl <= 0)     -- role dissented → right iff it didn't work
		           END AS correct
		    FROM base b
		    CROSS JOIN LATERAL jsonb_array_elements(b.ensemble) AS elem
		    WHERE COALESCE(elem->>'Role', '') <> ''
		      AND upper(elem->'Response'->>'action') IN ('BUY', 'SELL')
		) v
		GROUP BY 1 ORDER BY 3 DESC`
	rows, err := GetDB().Query(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []EntryPerfBucket{}
	for rows.Next() {
		var b EntryPerfBucket
		if err := rows.Scan(&b.Label, &b.N, &b.WinRatePct, &b.AvgPnLPct, &b.NetPnL, &b.AvgConf); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// GetEntryPerformance assembles the full signal-provenance breakdown. Each cut
// re-runs the (cheap, ~100-row) base join; correctness beats a single monster
// query here.
func GetEntryPerformance(ctx context.Context) (*EntryPerformance, error) {
	var ep EntryPerformance

	// Overall headline.
	overallQ := entryBaseCTE + `
		SELECT count(*)::int,
		       count(*) FILTER (WHERE pnl > 0)::int,
		       count(*) FILTER (WHERE pnl < 0)::int,
		       count(*) FILTER (WHERE pnl = 0)::int,
		       COALESCE(round(100.0 * count(*) FILTER (WHERE pnl > 0)::numeric / NULLIF(count(*), 0), 1), 0)::float8,
		       round(avg(pnl_pct)::numeric, 2)::float8,
		       round((percentile_cont(0.5) WITHIN GROUP (ORDER BY pnl_pct))::numeric, 2)::float8,
		       round(sum(pnl)::numeric, 2)::float8,
		       round(avg(conf)::numeric, 3)::float8,
		       min(opened_at)::date::text,
		       max(opened_at)::date::text
		FROM base`
	o := &ep.Overall
	if err := GetDB().QueryRow(ctx, overallQ).Scan(
		&o.N, &o.Wins, &o.Losses, &o.Breakeven, &o.WinRatePct,
		&o.AvgPnLPct, &o.MedianPnLPct, &o.NetPnL, &o.AvgConf,
		&o.WindowStart, &o.WindowEnd,
	); err != nil {
		return nil, err
	}

	var err error
	confLabel := `CASE WHEN conf < 0.60 THEN '<0.60'
	                   WHEN conf < 0.68 THEN '0.60-0.68'
	                   WHEN conf < 0.75 THEN '0.68-0.75'
	                   WHEN conf < 0.85 THEN '0.75-0.85'
	                   ELSE '>=0.85' END`
	if ep.ByConfidence, err = entryBuckets(ctx, confLabel, `GROUP BY 1 ORDER BY min(conf)`); err != nil {
		return nil, err
	}
	// ORDER BY 3 = the win-rate column (3rd in the SELECT list); ordering by an
	// output alias isn't available since only `label` is named.
	if ep.ByMode, err = entryBuckets(ctx, `mode`, `GROUP BY 1 ORDER BY 3 DESC`); err != nil {
		return nil, err
	}
	if ep.ByStrategy, err = entryBuckets(ctx, `strategy`, `GROUP BY 1 ORDER BY 3 DESC`); err != nil {
		return nil, err
	}
	if ep.ByDirection, err = entryBuckets(ctx, `dir`, `GROUP BY 1 ORDER BY 1`); err != nil {
		return nil, err
	}
	if ep.ByAnalysts, err = entryBuckets(ctx, `n_analysts::text`, `GROUP BY 1 ORDER BY min(n_analysts)`); err != nil {
		return nil, err
	}
	if ep.ByRole, err = entryRoleAccuracy(ctx); err != nil {
		return nil, err
	}

	// Concentration: net, top-3 sum, and the single largest winner.
	concQ := entryBaseCTE + `
		SELECT (SELECT round(sum(pnl)::numeric, 2)::float8 FROM base),
		       (SELECT round(sum(pnl)::numeric, 2)::float8 FROM (SELECT pnl FROM base ORDER BY pnl DESC LIMIT 3) x),
		       top.ticker, top.strategy,
		       round(top.pnl::numeric, 2)::float8, round(top.pnl_pct::numeric, 2)::float8, round(top.conf::numeric, 3)::float8
		FROM (SELECT ticker, strategy, pnl, pnl_pct, conf FROM base ORDER BY pnl DESC LIMIT 1) top`
	c := &ep.Concentration
	if err := GetDB().QueryRow(ctx, concQ).Scan(
		&c.NetPnL, &c.Top3PnL, &c.TopTicker, &c.TopStrategy, &c.TopPnL, &c.TopPnLPct, &c.TopConf,
	); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}

	return &ep, nil
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

// GetLatestAnalysisTime returns the most recent created_at from the analyses table.
// Returns (nil, nil) when the table is empty or the DB is not connected.
func GetLatestAnalysisTime(ctx context.Context) (*time.Time, error) {
	if !IsConnected() {
		return nil, nil
	}
	var t *time.Time
	err := GetDB().QueryRow(ctx, `SELECT MAX(created_at) FROM analyses`).Scan(&t)
	return t, err
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
	CompanyName   string    `json:"CompanyName,omitempty"`
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

	// Raw bytes for handler-side mapping; not serialized to JSON.
	EnsembleResultsRaw []byte `json:"-"`
	ArbiterResponseRaw []byte `json:"-"`
	FastPathed         bool   `json:"-"`
	DissentSummaryRaw  string `json:"-"`
}

// GetRecentAnalysesSummary retrieves summary fields for the most recent analyses.
// It does not fetch raw_response, raw_prompt, prompt_hash, or indicators.
// GetRecentAnalysesSummary returns the most recent N analyses for the list view.
// Deliberately OMITS the heavy JSONB columns (ensemble_results, arbiter_response):
// they average 13 KB and 3.4 KB per row respectively and the list cards don't
// render the per-analyst reasoning or the arbiter reasoning — only AnalysisModal
// does, and it now fetches GetAnalysisDetail on open. Trimming these here cut the
// /api/analyses/recent payload from ~1.6 MB to ~150 KB (limit=100).
func GetRecentAnalysesSummary(ctx context.Context, limit int) ([]AnalysisSummary, error) {
	q := `
		SELECT a.id, a.ticker, COALESCE(e.name, ''), a.strategy, a.action, a.confidence, a.size_pct,
		       COALESCE(a.stop_loss, 0), COALESCE(a.take_profit, 0), COALESCE(a.reasoning, ''),
		       a.model_provider, a.model_name,
		       COALESCE(a.token_count_input, 0), COALESCE(a.token_count_output, 0), COALESCE(a.latency_ms, 0),
		       a.created_at,
		       COALESCE(a.fast_pathed, false), COALESCE(a.dissent_summary, '')
		FROM analyses a
		LEFT JOIN LATERAL (
			SELECT name FROM entities
			WHERE UPPER(ticker) = UPPER(a.ticker)
			ORDER BY (entity_type = 'company') DESC, name
			LIMIT 1
		) e ON TRUE
		ORDER BY a.created_at DESC
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
			&a.ID, &a.Ticker, &a.CompanyName, &a.Strategy, &a.Action, &a.Confidence, &a.SizePct,
			&a.StopLoss, &a.TakeProfit, &a.Reasoning,
			&a.ModelProvider, &a.ModelName,
			&a.InputTokens, &a.OutputTokens, &a.LatencyMs,
			&a.CreatedAt,
			&a.FastPathed, &a.DissentSummaryRaw,
		); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// GetAnalysisDetail returns a single analysis with the full JSONB payload — used
// by AnalysisModal which renders per-analyst reasoning (ensemble_results) and
// the arbiter synthesis (arbiter_response). Returns (nil, nil) when not found
// so callers can render a 404 cleanly.
func GetAnalysisDetail(ctx context.Context, id string) (*AnalysisSummary, error) {
	q := `
		SELECT a.id, a.ticker, COALESCE(e.name, ''), a.strategy, a.action, a.confidence, a.size_pct,
		       COALESCE(a.stop_loss, 0), COALESCE(a.take_profit, 0), COALESCE(a.reasoning, ''),
		       a.model_provider, a.model_name,
		       COALESCE(a.token_count_input, 0), COALESCE(a.token_count_output, 0), COALESCE(a.latency_ms, 0),
		       a.created_at,
		       a.ensemble_results, a.arbiter_response,
		       COALESCE(a.fast_pathed, false), COALESCE(a.dissent_summary, '')
		FROM analyses a
		LEFT JOIN LATERAL (
			SELECT name FROM entities
			WHERE UPPER(ticker) = UPPER(a.ticker)
			ORDER BY (entity_type = 'company') DESC, name
			LIMIT 1
		) e ON TRUE
		WHERE a.id = $1`
	var a AnalysisSummary
	err := GetDB().QueryRow(ctx, q, id).Scan(
		&a.ID, &a.Ticker, &a.CompanyName, &a.Strategy, &a.Action, &a.Confidence, &a.SizePct,
		&a.StopLoss, &a.TakeProfit, &a.Reasoning,
		&a.ModelProvider, &a.ModelName,
		&a.InputTokens, &a.OutputTokens, &a.LatencyMs,
		&a.CreatedAt,
		&a.EnsembleResultsRaw, &a.ArbiterResponseRaw,
		&a.FastPathed, &a.DissentSummaryRaw,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &a, nil
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

	// DISTINCT ON (s.id) ensures one row per signal even when analysis_ids contains
	// multiple entries — picks the first analysis deterministically and avoids the
	// "keep first" dedup in Go. Ordered by s.id so DISTINCT ON is well-defined.
	rows, err := GetDB().Query(ctx, `
		SELECT DISTINCT ON (s.id) s.id AS signal_id, a.ensemble_results
		FROM signals s
		JOIN analyses a ON a.id = ANY(s.analysis_ids)
		WHERE s.id = ANY($1)
		ORDER BY s.id
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
