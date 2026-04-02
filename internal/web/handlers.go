package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/romeo-folie/trade-engine/internal/analysis/cache"
	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/logger"
	"github.com/romeo-folie/trade-engine/internal/store"
)

// processStart records the time this process was loaded, used for uptime.
var processStart = time.Now()

// registerAPIHandlers mounts the REST drill-down endpoints on mux.
func registerAPIHandlers(mux *http.ServeMux) {
	mux.HandleFunc("/api/node/risk", handleRisk)
	mux.HandleFunc("/api/node/portfolio", handlePortfolio)
	mux.HandleFunc("/api/node/ticker/", handleTicker) // /api/node/ticker/:ticker
	mux.HandleFunc("/api/node/watchlist", handleWatchlist)
	mux.HandleFunc("/api/node/models", handleModels)
	mux.HandleFunc("/api/signals/recent", handleRecentSignals)
	mux.HandleFunc("/api/trades/recent", handleRecentTrades)
	mux.HandleFunc("/api/analyses/recent", handleRecentAnalyses)
	mux.HandleFunc("/api/backtests/runs", handleBacktestRuns)
	mux.HandleFunc("/api/backtests/runs/", handleBacktestRunDetail)
	mux.HandleFunc("/api/portfolio/summaries", handlePortfolioSummaries)
	mux.HandleFunc("/api/portfolio/stats", handlePortfolioStats)
	mux.HandleFunc("/api/portfolio/positions", handlePortfolioPositions)
	mux.HandleFunc("/api/system/cache", handleSystemCache)
	mux.HandleFunc("/api/system/status", handleSystemStatus)
	mux.HandleFunc("/api/system/config", handleSystemConfig)
}

// ── Handlers ──────────────────────────────────────────────────────────────────

// GET /api/node/risk
func handleRisk(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	openTrades, err := store.GetOpenTrades(ctx)
	if err != nil {
		apiError(w, "risk query failed", err)
		return
	}
	jsonResponse(w, map[string]any{
		"open_trades":     openTrades,
		"open_trade_count": len(openTrades),
	})
}

// GET /api/node/portfolio
func handlePortfolio(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	snap, err := store.GetLatestPortfolioSnapshot(ctx)
	if err != nil {
		apiError(w, "portfolio query failed", err)
		return
	}
	jsonResponse(w, snap)
}

// GET /api/portfolio/summaries
func handlePortfolioSummaries(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	limit := queryInt(r, "limit", 30, 100)
	summaries, err := store.GetRecentDailySummaries(ctx, limit)
	if err != nil {
		apiError(w, "summaries query failed", err)
		return
	}
	if summaries == nil {
		summaries = []store.DailySummary{}
	}
	jsonResponse(w, map[string]any{"summaries": summaries})
}

// GET /api/portfolio/stats
func handlePortfolioStats(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	stats, err := store.GetPortfolioStats(ctx)
	if err != nil {
		apiError(w, "portfolio stats query failed", err)
		return
	}
	jsonResponse(w, stats)
}

// GET /api/portfolio/positions
func handlePortfolioPositions(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	trades, err := store.GetOpenTrades(ctx)
	if err != nil {
		apiError(w, "positions query failed", err)
		return
	}
	if trades == nil {
		trades = []store.Trade{}
	}
	jsonResponse(w, map[string]any{"positions": trades})
}

// GET /api/node/ticker/:ticker
func handleTicker(w http.ResponseWriter, r *http.Request) {
	ticker := r.URL.Path[len("/api/node/ticker/"):]
	if ticker == "" {
		http.Error(w, "ticker required", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	analyses, err := store.GetAnalysesByTicker(ctx, ticker, 1)
	if err != nil {
		apiError(w, "analysis query failed", err)
		return
	}

	signals, err := store.GetSignalsByTicker(ctx, ticker, 10)
	if err != nil {
		apiError(w, "signals query failed", err)
		return
	}

	var latestAnalysis any
	if len(analyses) > 0 {
		latestAnalysis = analyses[0]
	}

	jsonResponse(w, map[string]any{
		"ticker":   ticker,
		"analysis": latestAnalysis,
		"signals":  signals,
	})
}

// GET /api/node/watchlist
func handleWatchlist(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	items, err := store.GetWatchlistItems(ctx, true)
	if err != nil {
		apiError(w, "watchlist query failed", err)
		return
	}
	type watchlistEntry struct {
		Ticker     string   `json:"ticker"`
		Strategies []string `json:"strategies"`
		Timeframes []string `json:"timeframes"`
	}
	out := make([]watchlistEntry, 0, len(items))
	for _, it := range items {
		out = append(out, watchlistEntry{
			Ticker:     it.Ticker,
			Strategies: it.Strategies,
			Timeframes: it.Timeframes,
		})
	}
	jsonResponse(w, map[string]any{"tickers": out})
}

// GET /api/node/models
func handleModels(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	// minTrades=5: only show models with enough samples to be meaningful.
	perf, err := store.GetModelPerformance(ctx, 5)
	if err != nil {
		apiError(w, "model performance query failed", err)
		return
	}
	jsonResponse(w, map[string]any{"model_performance": perf})
}

// signalWithAnalysts enriches a Signal with analyst routing data.
type signalWithAnalysts struct {
	store.Signal
	Analysts []APIAnalystResult `json:"Analysts,omitempty"`
	Ensemble *APIEnsembleResult `json:"Ensemble,omitempty"`
}

// GET /api/signals/recent?limit=N
func handleRecentSignals(w http.ResponseWriter, r *http.Request) {
	limit := queryInt(r, "limit", 50, 200)
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	sigs, err := store.GetRecentSignals(ctx, limit)
	if err != nil {
		apiError(w, "signals query failed", err)
		return
	}

	// Batch-fetch all analyst results in one query (avoids N+1).
	analystBatch, err := store.GetAnalystResultsBatchForSignals(ctx, sigs)
	if err != nil {
		logger.Warn("web: batch analyst results lookup failed", "err", err.Error())
		// Non-fatal — enrich what we can (map will be nil/empty)
	}

	enriched := make([]signalWithAnalysts, len(sigs))
	for i, sig := range sigs {
		enriched[i].Signal = sig
		if analystBatch != nil {
			if raw, ok := analystBatch[i]; ok {
				enriched[i].Analysts, enriched[i].Ensemble = mapAnalystResults(raw)
			}
		}
	}
	jsonResponse(w, map[string]any{"signals": enriched})
}

// tradeWithAnalysts enriches a Trade with analyst routing data.
type tradeWithAnalysts struct {
	store.Trade
	Analysts []APIAnalystResult `json:"Analysts,omitempty"`
	Ensemble *APIEnsembleResult `json:"Ensemble,omitempty"`
}

// GET /api/trades/recent?limit=N
func handleRecentTrades(w http.ResponseWriter, r *http.Request) {
	limit := queryInt(r, "limit", 50, 200)
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	trades, err := store.GetRecentTrades(ctx, limit)
	if err != nil {
		apiError(w, "trades query failed", err)
		return
	}

	// Batch-fetch all analyst results in one query (avoids N+1).
	tradeBatch, err := store.GetAnalystResultsBatchForTrades(ctx, trades)
	if err != nil {
		logger.Warn("web: batch analyst results lookup failed for trades", "err", err.Error())
	}

	enriched := make([]tradeWithAnalysts, len(trades))
	for i, t := range trades {
		enriched[i].Trade = t
		if tradeBatch != nil {
			if raw, ok := tradeBatch[i]; ok {
				enriched[i].Analysts, enriched[i].Ensemble = mapAnalystResults(raw)
			}
		}
	}
	jsonResponse(w, map[string]any{"trades": enriched})
}

// GET /api/analyses/recent?limit=N
func handleRecentAnalyses(w http.ResponseWriter, r *http.Request) {
	limit := queryInt(r, "limit", 50, 200)
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	analyses, err := store.GetRecentAnalysesSummary(ctx, limit)
	if err != nil {
		apiError(w, "analyses query failed", err)
		return
	}
	if analyses == nil {
		analyses = []store.AnalysisSummary{}
	}
	jsonResponse(w, map[string]any{"analyses": analyses})
}

// GET /api/backtests/runs?limit=N
// backteststRunSummary is a list-safe projection of BacktestRun that omits
// ConfigSnapshot (which may contain API keys from the persisted config).
type backteststRunSummary struct {
	ID                  string   `json:"ID"`
	Label               string   `json:"Label"`
	Ticker              string   `json:"Ticker"`
	Strategy            string   `json:"Strategy"`
	ModelProvider       string   `json:"ModelProvider"`
	ModelName           string   `json:"ModelName"`
	FromDate            string   `json:"FromDate"`
	ToDate              string   `json:"ToDate"`
	TotalReturnPct      *float64 `json:"TotalReturnPct"`
	AnnualizedReturnPct *float64 `json:"AnnualizedReturnPct"`
	SharpeRatio         *float64 `json:"SharpeRatio"`
	SortinoRatio        *float64 `json:"SortinoRatio"`
	CalmarRatio         *float64 `json:"CalmarRatio"`
	MaxDrawdownPct      *float64 `json:"MaxDrawdownPct"`
	WinRatePct          *float64 `json:"WinRatePct"`
	ProfitFactor        *float64 `json:"ProfitFactor"`
	Expectancy          *float64 `json:"Expectancy"`
	TotalTrades         int      `json:"TotalTrades"`
	AvgHoldHours        *float64 `json:"AvgHoldHours"`
	BestTradePct        *float64 `json:"BestTradePct"`
	WorstTradePct       *float64 `json:"WorstTradePct"`
	CacheHitRate        *float64 `json:"CacheHitRate"`
	CreatedAt           string   `json:"CreatedAt"`
}

func toRunSummary(r store.BacktestRun) backteststRunSummary {
	return backteststRunSummary{
		ID:                  r.ID,
		Label:               r.Label,
		Ticker:              r.Ticker,
		Strategy:            r.Strategy,
		ModelProvider:       r.ModelProvider,
		ModelName:           r.ModelName,
		FromDate:            r.FromDate.Format("2006-01-02"),
		ToDate:              r.ToDate.Format("2006-01-02"),
		TotalReturnPct:      r.TotalReturnPct,
		AnnualizedReturnPct: r.AnnualizedReturnPct,
		SharpeRatio:         r.SharpeRatio,
		SortinoRatio:        r.SortinoRatio,
		CalmarRatio:         r.CalmarRatio,
		MaxDrawdownPct:      r.MaxDrawdownPct,
		WinRatePct:          r.WinRatePct,
		ProfitFactor:        r.ProfitFactor,
		Expectancy:          r.Expectancy,
		TotalTrades:         r.TotalTrades,
		AvgHoldHours:        r.AvgHoldHours,
		BestTradePct:        r.BestTradePct,
		WorstTradePct:       r.WorstTradePct,
		CacheHitRate:        r.CacheHitRate,
		CreatedAt:           r.CreatedAt.Format(time.RFC3339),
	}
}

// GET /api/backtests/runs?limit=N
// Returns runs without ConfigSnapshot to avoid leaking API keys.
func handleBacktestRuns(w http.ResponseWriter, r *http.Request) {
	limit := queryInt(r, "limit", 20, 200)
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	runs, err := store.ListRuns(ctx, limit)
	if err != nil {
		apiError(w, "backtest runs query failed", err)
		return
	}
	summaries := make([]backteststRunSummary, 0, len(runs))
	for _, r := range runs {
		summaries = append(summaries, toRunSummary(r))
	}
	jsonResponse(w, map[string]any{"runs": summaries})
}

// GET /api/backtests/runs/:id         → run metadata including ConfigSnapshot.initial_cash
// GET /api/backtests/runs/:id/trades  → trade list for equity curve
func handleBacktestRunDetail(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path[len("/api/backtests/runs/"):]

	if strings.HasSuffix(path, "/trades") {
		// Trade list sub-resource
		id := strings.TrimSuffix(path, "/trades")
		if id == "" {
			http.Error(w, "expected /api/backtests/runs/{id}/trades", http.StatusBadRequest)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		trades, err := store.ListTradesForRun(ctx, id)
		if err != nil {
			apiError(w, "backtest trades query failed", err)
			return
		}
		if trades == nil {
			trades = []store.BacktestTrade{}
		}
		jsonResponse(w, map[string]any{"trades": trades})
		return
	}

	// Run metadata — includes ConfigSnapshot so the frontend can read initial_cash.
	// Only the summary fields + ConfigSnapshot are returned (no raw_prompt/raw_response).
	id := path
	if id == "" {
		http.Error(w, "expected /api/backtests/runs/{id}", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	run, err := store.GetRun(ctx, id)
	if err != nil {
		if isNotFound(err) {
			apiNotFound(w, "backtest run not found")
		} else {
			apiError(w, "backtest run query failed", err)
		}
		return
	}
	// Return summary + extracted InitialCash only.
	// ConfigSnapshot contains live env-resolved secrets (APIKey, Secret, BotToken)
	// so we never send it raw to the browser — we parse out only the fields we need.
	type runDetail struct {
		backteststRunSummary
		InitialCash float64 `json:"InitialCash"`
	}
	var initialCash float64
	if len(run.ConfigSnapshot) > 0 {
		var snap struct {
			Backtest struct {
				InitialCash float64 `json:"initial_cash"`
			} `json:"backtest"`
		}
		if err := json.Unmarshal(run.ConfigSnapshot, &snap); err == nil && snap.Backtest.InitialCash > 0 {
			initialCash = snap.Backtest.InitialCash
		}
	}
	jsonResponse(w, runDetail{
		backteststRunSummary: toRunSummary(run),
		InitialCash:          initialCash,
	})
}

// GET /api/system/cache
func handleSystemCache(w http.ResponseWriter, _ *http.Request) {
	s := cache.TakeSnapshot()
	jsonResponse(w, map[string]any{
		"hits":      s.Hits,
		"misses":    s.Misses,
		"evictions": s.Evictions,
		"errors":    s.Errors,
	})
}

// GET /api/system/status
func handleSystemStatus(w http.ResponseWriter, _ *http.Request) {
	cfg := config.Get()
	uptimeSec := int(time.Since(processStart).Seconds())

	jsonResponse(w, map[string]any{
		"version":           "0.1.0",
		"mode":              strings.ToUpper(cfg.Engine.Mode),
		"tick_interval_sec": cfg.Engine.TickIntervalSec,
		"tick_interval_min": float64(cfg.Engine.TickIntervalSec) / 60.0,
		"uptime_seconds":    uptimeSec,
	})
}

// GET /api/system/config
// POST /api/system/config  — mutation is restricted to loopback (127.x / ::1) callers.
func handleSystemConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		jsonResponse(w, config.Get())
	case http.MethodPost:
		// Only loopback callers may mutate engine config.
		if !isLoopback(r) {
			http.Error(w, "forbidden: config mutation requires local connection", http.StatusForbidden)
			return
		}
		var updates map[string]any
		if err := json.NewDecoder(r.Body).Decode(&updates); err != nil {
			http.Error(w, "invalid json", http.StatusBadRequest)
			return
		}

		// Field mapping for flat keys from frontend to nested config paths.
		mapping := map[string]string{
			"mode":                 "engine.mode",
			"tick_interval_sec":    "engine.tick_interval_sec",
			"initial_capital":      "engine.initial_capital",
			"dry_run":              "engine.dry_run",
			"confidence_threshold": "risk.confidence_threshold",
			"max_position_pct":     "risk.max_position_pct",
			"max_open_positions":   "risk.max_open_positions",
			"daily_loss_limit_pct": "risk.daily_loss_limit_pct",
			"max_drawdown_pct":     "risk.max_drawdown_pct",
			"cooldown_after_loss":  "risk.cooldown_after_loss_min",
			"signal_expiry":        "risk.signal_expiry_min",
			"cost_profile":         "cost.profile",
			"daily_token_budget":   "cost.daily_token_budget",
			"weekly_token_budget":  "cost.weekly_token_budget",
			"auto_degrade_budget":  "cost.auto_degrade_on_budget_hit",
			"telegram_enabled":     "notifications.telegram.enabled",
			// Analyst roles
			"analyst_cheap":    "models.roles.analysts.0",
			"analyst_mid":      "models.roles.analysts.1",
			"analyst_frontier": "models.roles.analysts.2",
			"arbiter":          "models.roles.arbiter",
			"classifier":       "models.roles.classifier",
		}

		for k, v := range updates {
			dotPath, ok := mapping[k]
			if !ok {
				// Also allow direct dot-path updates if provided.
				dotPath = k
			}
			valStr := fmt.Sprintf("%v", v)
			if err := config.SetField(dotPath, valStr); err != nil {
				apiError(w, "config update failed", err)
				return
			}
		}

		// Reload live config into memory
		if _, err := config.Load(); err != nil {
			apiError(w, "reload failed", err)
			return
		}

		jsonResponse(w, map[string]string{"status": "ok"})
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func jsonResponse(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		logger.Warn("web: json encode error", "err", err.Error())
	}
}

func apiError(w http.ResponseWriter, msg string, err error) {
	logger.Warn("web: api error", "msg", msg, "err", err.Error())
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusInternalServerError)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// apiNotFound writes a 404 JSON response. Used when a resource is missing
// rather than when a query fails — not-found is not a 500.
func apiNotFound(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotFound)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// isNotFound returns true when err signals a missing row rather than a
// database/network failure. Store functions return a sentinel message
// ("... not found: <id>") for the no-row case.
func isNotFound(err error) bool {
	return err != nil && strings.Contains(err.Error(), "not found")
}

// queryInt parses an integer query parameter, falling back to def when absent
// or invalid. If max > 0 the returned value is capped at max.
func queryInt(r *http.Request, key string, def, max int) int {
	v := r.URL.Query().Get(key)
	if v == "" {
		if max > 0 && def > max {
			return max
		}
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		if max > 0 && def > max {
			return max
		}
		return def
	}
	if max > 0 && n > max {
		return max
	}
	return n
}

// isLoopback returns true when the request originates from a loopback address (127.x or ::1).
// Used to restrict mutating endpoints to local callers only.
func isLoopback(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
