package web

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/romeo-folie/trade-engine/internal/logger"
	"github.com/romeo-folie/trade-engine/internal/store"
)

// registerAPIHandlers mounts the REST drill-down endpoints on mux.
func registerAPIHandlers(mux *http.ServeMux) {
	mux.HandleFunc("/api/node/risk", handleRisk)
	mux.HandleFunc("/api/node/portfolio", handlePortfolio)
	mux.HandleFunc("/api/node/ticker/", handleTicker) // /api/node/ticker/:ticker
	mux.HandleFunc("/api/node/models", handleModels)
	mux.HandleFunc("/api/signals/recent", handleRecentSignals)
	mux.HandleFunc("/api/trades/recent", handleRecentTrades)
	mux.HandleFunc("/api/analyses/recent", handleRecentAnalyses)
	mux.HandleFunc("/api/backtests/recent", handleRecentBacktests)
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

// GET /api/signals/recent?limit=N
func handleRecentSignals(w http.ResponseWriter, r *http.Request) {
	limit := queryInt(r, "limit", 50)
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	sigs, err := store.GetRecentSignals(ctx, limit, true)
	if err != nil {
		apiError(w, "signals query failed", err)
		return
	}
	jsonResponse(w, map[string]any{"signals": sigs})
}

// GET /api/trades/recent?limit=N
func handleRecentTrades(w http.ResponseWriter, r *http.Request) {
	limit := queryInt(r, "limit", 50)
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	trades, err := store.GetRecentTrades(ctx, limit)
	if err != nil {
		apiError(w, "trades query failed", err)
		return
	}
	jsonResponse(w, map[string]any{"trades": trades})
}

// GET /api/analyses/recent?limit=N
func handleRecentAnalyses(w http.ResponseWriter, r *http.Request) {
	limit := queryInt(r, "limit", 50)
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	analyses, err := store.GetRecentAnalyses(ctx, limit)
	if err != nil {
		apiError(w, "analyses query failed", err)
		return
	}
	jsonResponse(w, map[string]any{"analyses": analyses})
}

// GET /api/backtests/recent?limit=N
func handleRecentBacktests(w http.ResponseWriter, r *http.Request) {
	jsonResponse(w, map[string]any{"backtests": []any{}})
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

func queryInt(r *http.Request, key string, def int) int {
	v := r.URL.Query().Get(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return def
	}
	return n
}
