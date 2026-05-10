package web

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/romeo-folie/trade-engine/internal/analysis/cache"
	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/logger"
	"github.com/romeo-folie/trade-engine/internal/store"
)

// processStart records the time this process was loaded, used for uptime.
var processStart = time.Now()

// restartSignalFn sends SIGTERM to the current process. Overridden in tests.
var restartSignalFn = func() error {
	proc, err := os.FindProcess(os.Getpid())
	if err != nil {
		return err
	}
	return proc.Signal(syscall.SIGTERM)
}

// registerAPIHandlers mounts the REST drill-down endpoints on mux.
func (s *Server) registerAPIHandlers(mux *http.ServeMux) {
	mux.HandleFunc("/api/node/risk", s.handleRisk)
	mux.HandleFunc("/api/node/portfolio", s.handlePortfolio)
	mux.HandleFunc("/api/node/ticker/", s.handleTicker) // /api/node/ticker/:ticker
	mux.HandleFunc("/api/node/watchlist", s.handleWatchlist)
	mux.HandleFunc("/api/node/models", s.handleModels)
	mux.HandleFunc("/api/signals/recent", s.handleRecentSignals)
	mux.HandleFunc("/api/trades/recent", s.handleRecentTrades)
	mux.HandleFunc("/api/analyses/recent", s.handleRecentAnalyses)
	mux.HandleFunc("/api/backtests/runs", s.handleBacktestRuns)
	mux.HandleFunc("/api/backtests/runs/", s.handleBacktestRunDetail)
	mux.HandleFunc("/api/portfolio/history", s.handlePortfolioHistory)
	mux.HandleFunc("/api/portfolio/performance", s.handlePortfolioPerformance)
	mux.HandleFunc("/api/portfolio/summaries", s.handlePortfolioSummaries)
	mux.HandleFunc("/api/portfolio/stats", s.handlePortfolioStats)
	mux.HandleFunc("/api/portfolio/daily-counters", s.handlePortfolioDailyCounters)
	mux.HandleFunc("/api/portfolio/positions", s.handlePortfolioPositions)
	mux.HandleFunc("/api/system/cache", s.handleSystemCache)
	mux.HandleFunc("/api/system/status", s.handleSystemStatus)
	mux.HandleFunc("/api/system/config", s.handleSystemConfig)
	mux.HandleFunc("/api/system/restart", s.handleSystemRestart)
	mux.HandleFunc("/api/orb/sessions", s.handleORBSessions)
	mux.HandleFunc("/api/orb/session/today", s.handleORBSessionToday)
	mux.HandleFunc("/api/orb/trades/recent", s.handleORBRecentTrades)
	mux.HandleFunc("/api/orb/trades", s.handleORBTrades)
	mux.HandleFunc("/api/orb/performance", s.handleORBPerformance)
	mux.HandleFunc("/api/orb/flags", s.handleORBFlags)
	mux.HandleFunc("/api/orb/flags/", s.handleORBFlagResolve) // POST /api/orb/flags/:id/resolve
}

// ── Handlers ──────────────────────────────────────────────────────────────────

// GET /api/node/risk
//
// Returns two distinct counts so the canvas can flag drift between them:
//   - open_positions: distinct (ticker, direction) pairs — the canonical
//     "open position" notion used by the risk gate (see PR #330).
//   - open_trade_count: count of DB rows with status='open' (kept for
//     backwards compat). Equals open_positions in steady state; divergence
//     points to pending consolidation work or a bug.
func (s *Server) handleRisk(w http.ResponseWriter, r *http.Request) {
	if !store.IsConnected() {
		apiError(w, "database not connected", nil)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	openTrades, err := store.GetOpenTrades(ctx)
	if err != nil {
		apiError(w, "risk query failed", err)
		return
	}
	// open_trade_count is the authoritative trade-row count and always succeeds
	// here because it's just len() over the loaded rows. Surface it under both
	// keys so the canvas card (which reads `open_trade_rows` everywhere) can
	// detect divergence on /api/node/risk too.
	rowCount := len(openTrades)
	jsonResponse(w, map[string]any{
		"open_trades":      openTrades,
		"open_trade_count": rowCount,
		"open_trade_rows":  &rowCount,
		"open_positions":   countDistinctOpenPositions(openTrades),
	})
}

// countDistinctOpenPositions returns the number of distinct (ticker, direction)
// pairs in the supplied trade rows. Mirrors the canonical pattern from
// internal/engine/engine.go (PR #330) so all consumers — risk gate, portfolio
// card, canvas — agree on the "open position" denominator.
func countDistinctOpenPositions(trades []store.Trade) int {
	seen := make(map[string]struct{}, len(trades))
	for _, t := range trades {
		seen[t.Ticker+"|"+t.Direction] = struct{}{}
	}
	return len(seen)
}

// GET /api/node/portfolio
// Broker-first: reads live account state from the broker when configured,
// falls back to the in-process tracker, then to the latest DB snapshot.
// All responses include source ("broker"|"tracker"|"db") and stale_at (non-null on fallback).
//
// All three paths emit two position counts:
//   - OpenPositions: distinct ticker (or ticker+direction) count — the canonical
//     denominator used by the risk gate (see PR #330). For the broker path this
//     is len(positions) since the broker already aggregates per ticker.
//   - OpenTradeRows: count of DB rows with status='open'. Equals OpenPositions
//     in steady state; divergence is rendered in the canvas card to surface
//     pending consolidation work or drift.
func (s *Server) handlePortfolio(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	// Trade-row count from DB — used by every path so the response shape is
	// uniform. Best-effort: when the DB is disconnected or the count query
	// fails, leave the pointer nil so the JSON serializes to null and the
	// frontend (which already treats null as "hide divergence") doesn't
	// mistake an unknown value for a real zero.
	var openTradeRows *int
	if store.IsConnected() {
		if n, dbErr := store.CountOpenTrades(ctx); dbErr == nil {
			openTradeRows = &n
		} else {
			logger.Warn("CountOpenTrades failed; omitting open_trade_rows", "err", dbErr)
		}
	}

	// ── 1. Broker-direct path ────────────────────────────────────────────────
	if s.brokerView != nil {
		acct, err := s.brokerView.AccountSnapshot(ctx)
		if err == nil {
			invested := acct.Equity - acct.Cash

			// Aggregate unrealized PnL from positions — the account endpoint
			// doesn't expose it directly.
			var unrealizedPnL float64
			var openPositions int
			if positions, pErr := s.brokerView.PositionsSnapshot(ctx); pErr == nil {
				openPositions = len(positions)
				for _, p := range positions {
					unrealizedPnL += p.UnrealizedPL
				}
			}

			jsonResponse(w, map[string]any{
				"TotalValue":       acct.Equity,
				"Cash":             acct.Cash,
				"Invested":         invested,
				"UnrealizedPnL":    unrealizedPnL,
				// IBKR does not expose prior-day close; guard to avoid showing
				// full equity as DayPnL when LastEquity is zero.
				"DayPnL": func() float64 {
					if acct.LastEquity > 0 {
						return acct.Equity - acct.LastEquity
					}
					return 0.0
				}(),
				"RealizedPnLToday": 0.0,
				"OpenPositions":    openPositions,
				"OpenTradeRows":    openTradeRows,
				"BuyingPower":      acct.BuyingPower,
				"source":           "broker",
				"stale_at":         nil,
			})
			return
		}
		logger.Warn("broker AccountSnapshot failed, falling back", "err", err)
	}

	// ── 2. In-process tracker ────────────────────────────────────────────────
	if s.tracker != nil {
		state := s.tracker.CurrentState()
		jsonResponse(w, map[string]any{
			"TotalValue":       state.TotalValue,
			"Cash":             state.Cash,
			"Invested":         state.Invested,
			"UnrealizedPnL":    state.UnrealizedPnL,
			"DayPnL":           state.DayPnL,
			"RealizedPnLToday": state.RealizedPnLDay,
			"OpenPositions":    state.OpenPositions,
			"OpenTradeRows":    openTradeRows,
			"source":           "tracker",
			"stale_at":         nil,
		})
		return
	}

	// ── 3. DB fallback ────────────────────────────────────────────────────────
	if !store.IsConnected() {
		apiError(w, "portfolio tracker not initialized and database not connected", nil)
		return
	}
	snap, err := store.GetLatestPortfolioSnapshot(ctx)
	if err != nil {
		apiError(w, "portfolio snapshot query failed", err)
		return
	}
	if snap == nil {
		jsonResponse(w, map[string]any{
			"TotalValue": 0.0, "Cash": 0.0, "Invested": 0.0,
			"UnrealizedPnL": 0.0, "DayPnL": 0.0, "RealizedPnLToday": 0.0,
			"OpenPositions": 0, "OpenTradeRows": openTradeRows,
			"source": "db", "stale_at": nil,
		})
		return
	}
	jsonResponse(w, map[string]any{
		"TotalValue":       snap.TotalValue,
		"Cash":             snap.Cash,
		"Invested":         snap.Invested,
		"UnrealizedPnL":    snap.UnrealizedPnL,
		"DayPnL":           0.0,
		"RealizedPnLToday": snap.RealizedPnLToday,
		"OpenPositions":    snap.OpenPositions,
		"OpenTradeRows":    openTradeRows,
		"source":           "db",
		"stale_at":         snap.CreatedAt.UTC().Format(time.RFC3339),
	})
}

// GET /api/portfolio/history?days=N&period=1M&timeframe=1D&mode=all
// Returns two series for the overlay chart:
//   - alpaca: broker equity curve (from BrokerView.EquityCurve, or nil on unavailability)
//   - trade_engine: our snapshotted equity from daily_summaries
//
// The `period` and `timeframe` query params follow Alpaca conventions (e.g.
// period=1M&timeframe=1D). When omitted, defaults mirror the `days` param.
func (s *Server) handlePortfolioHistory(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	period := r.URL.Query().Get("period")
	timeframe := r.URL.Query().Get("timeframe")
	if period == "" {
		period = "1M"
	}
	if timeframe == "" {
		timeframe = "1D"
	}
	days := queryInt(r, "days", 30, 365)
	mode := r.URL.Query().Get("mode") // "live" | "paper" | "all" (default "all")

	type equityPoint struct {
		T string  `json:"t"`
		V float64 `json:"v"`
	}

	// ── Broker series (Alpaca) ───────────────────────────────────────────────
	var alpacaSeries []equityPoint
	source := "db"
	if s.brokerView != nil {
		if hist, err := s.brokerView.EquityCurve(ctx, period, timeframe); err == nil {
			source = "broker"
			alpacaSeries = make([]equityPoint, 0, len(hist.Points))
			for _, p := range hist.Points {
				alpacaSeries = append(alpacaSeries, equityPoint{
					T: p.Time.Format(time.RFC3339),
					V: p.Equity,
				})
			}
		}
		// ErrBrokerUnavailable (IBKR) is silent — alpacaSeries stays nil.
	}

	// ── TradeEngine series (DB) ──────────────────────────────────────────────
	// When mode is "all" (or empty), use daily_summaries (one row/day, fast).
	// When mode is "live" or "paper", query portfolio_snapshots filtered by mode
	// (tagged since migration 021) and collapse to one point per calendar day.
	var teSeries []equityPoint
	if store.IsConnected() {
		from := time.Now().AddDate(0, 0, -days)
		to := time.Now()
		if mode == "" || mode == "all" {
			summaries, err := store.GetDailySummaries(ctx, from, to)
			if err == nil {
				teSeries = make([]equityPoint, 0, len(summaries))
				for _, s := range summaries {
					teSeries = append(teSeries, equityPoint{
						T: s.Date.Format("2006-01-02"),
						V: s.EndingValue,
					})
				}
			}
		} else {
			snaps, err := store.GetPortfolioSnapshotsByMode(ctx, from, to, mode)
			if err == nil {
				// Collapse to one point per calendar day (last snapshot of each day).
				byDay := map[string]float64{}
				dayOrder := []string{}
				for _, sn := range snaps {
					key := sn.CreatedAt.UTC().Format("2006-01-02")
					if _, seen := byDay[key]; !seen {
						dayOrder = append(dayOrder, key)
					}
					byDay[key] = sn.TotalValue // last row wins (snaps ordered ASC)
				}
				teSeries = make([]equityPoint, 0, len(dayOrder))
				for _, k := range dayOrder {
					teSeries = append(teSeries, equityPoint{T: k, V: byDay[k]})
				}
			}
		}
	}

	jsonResponse(w, map[string]any{
		"alpaca":       alpacaSeries,
		"trade_engine": teSeries,
		"source":       source,
	})
}

// GET /api/portfolio/performance
// Broker-first: derives period P&L from broker account (day) and DB summaries
// (week/month). Falls back to DB-only when no broker is configured.
func (s *Server) handlePortfolioPerformance(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	now := time.Now().UTC()
	today := now.Truncate(24 * time.Hour)
	week := today.AddDate(0, 0, -7)
	month := today.AddDate(0, 0, -30)

	// Try broker for live current value + today P&L.
	var currentValue float64
	var dayPnL *float64
	source := "db"

	if s.brokerView != nil {
		if acct, err := s.brokerView.AccountSnapshot(ctx); err == nil {
			source = "broker"
			currentValue = acct.Equity
			dpnl := acct.Equity - acct.LastEquity
			dayPnL = &dpnl
		}
	}

	// Fall back to DB for current value when broker unavailable.
	var staleAt *string
	if currentValue == 0 && store.IsConnected() {
		if snap, err := store.GetLatestPortfolioSnapshot(ctx); err == nil && snap != nil {
			currentValue = snap.TotalValue
			s := snap.CreatedAt.UTC().Format(time.RFC3339)
			staleAt = &s
		}
	}

	if currentValue == 0 {
		jsonResponse(w, map[string]any{"source": source})
		return
	}

	performance := map[string]any{
		"current_value": currentValue,
		"source":        source,
		"stale_at":      staleAt,
	}

	// Today P&L from broker when available — set unconditionally (does not
	// require DB). pnl_pct is relative to prior-day close (currentValue - pnl).
	if dayPnL != nil {
		pnl := *dayPnL
		performance["today"] = map[string]any{"pnl": pnl, "pnl_pct": safeDiv(pnl, currentValue-pnl) * 100}
	}

	// Week/month P&L always derived from DB summaries.
	if store.IsConnected() {
		summaries, err := store.GetDailySummaries(ctx, month, today)
		if err == nil {
			findStart := func(target time.Time) float64 {
				for _, s := range summaries {
					if s.Date.Equal(target) || s.Date.After(target) {
						return s.StartingValue
					}
				}
				return 0
			}

			if dayPnL == nil {
				// No broker — derive today from summaries too.
				if todayStart := findStart(today); todayStart > 0 {
					pnl := currentValue - todayStart
					performance["today"] = map[string]any{"pnl": pnl, "pnl_pct": safeDiv(pnl, todayStart) * 100}
				}
			}
			if weekStart := findStart(week); weekStart > 0 {
				pnl := currentValue - weekStart
				performance["week"] = map[string]any{"pnl": pnl, "pnl_pct": safeDiv(pnl, weekStart) * 100}
			}
			if monthStart := findStart(month); monthStart > 0 {
				pnl := currentValue - monthStart
				performance["month"] = map[string]any{"pnl": pnl, "pnl_pct": safeDiv(pnl, monthStart) * 100}
			}
		}
	}

	jsonResponse(w, performance)
}

// safeDiv returns a/b, or 0 when b is zero (avoids division-by-zero panics).
func safeDiv(a, b float64) float64 {
	if b == 0 {
		return 0
	}
	return a / b
}

// GET /api/portfolio/summaries
func (s *Server) handlePortfolioSummaries(w http.ResponseWriter, r *http.Request) {
	if !store.IsConnected() {
		apiError(w, "database not connected", nil)
		return
	}
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

// GET /api/portfolio/daily-counters?days=N
//
// Returns the last N days of observability rollups (Issue #325) — one row per
// calendar day with signals_total / signals_blocked / signals_executed,
// blocked_by_reason histogram, and executed_winning / executed_losing trade
// counts. Default 30 days, max 90.
//
// Today's row is computed live from signals + trades when there's no
// daily_summaries row yet, so the dashboard never reads a "0 signals" row on
// engine startup before the rollup writer fires.
func (s *Server) handlePortfolioDailyCounters(w http.ResponseWriter, r *http.Request) {
	if !store.IsConnected() {
		apiError(w, "database not connected", nil)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	days := queryInt(r, "days", 30, 90)
	now := time.Now().UTC()
	from := now.AddDate(0, 0, -days+1).Truncate(24 * time.Hour)
	to := now.Truncate(24 * time.Hour)

	summaries, err := store.GetDailySummaries(ctx, from, to)
	if err != nil {
		apiError(w, "daily counters query failed", err)
		return
	}
	if summaries == nil {
		summaries = []store.DailySummary{}
	}

	// If today's row hasn't been upserted yet, synthesize it from the live
	// aggregate so the dashboard isn't permanently waiting for the next 5-
	// minute rollup tick.
	todayStart := now.Truncate(24 * time.Hour)
	hasToday := false
	for _, s := range summaries {
		if s.Date.UTC().Truncate(24 * time.Hour).Equal(todayStart) {
			hasToday = true
			break
		}
	}

	type counterDTO struct {
		Date            string         `json:"date"`
		SignalsTotal    int            `json:"signals_total"`
		SignalsBlocked  int            `json:"signals_blocked"`
		SignalsExecuted int            `json:"signals_executed"`
		ExecutedWinning int            `json:"executed_winning"`
		ExecutedLosing  int            `json:"executed_losing"`
		BlockedByReason map[string]int `json:"blocked_by_reason"`
	}
	out := make([]counterDTO, 0, len(summaries)+1)
	for _, s := range summaries {
		out = append(out, counterDTO{
			Date:            s.Date.UTC().Format("2006-01-02"),
			SignalsTotal:    s.SignalsTotal,
			SignalsBlocked:  s.SignalsBlocked,
			SignalsExecuted: s.SignalsExecuted,
			ExecutedWinning: s.ExecutedWinning,
			ExecutedLosing:  s.ExecutedLosing,
			BlockedByReason: s.BlockedByReason,
		})
	}
	if !hasToday {
		if dc, err := store.AggregateDailyCounters(ctx, todayStart); err == nil {
			out = append(out, counterDTO{
				Date:            todayStart.Format("2006-01-02"),
				SignalsTotal:    dc.SignalsTotal,
				SignalsBlocked:  dc.SignalsBlocked,
				SignalsExecuted: dc.SignalsExecuted,
				ExecutedWinning: dc.ExecutedWinning,
				ExecutedLosing:  dc.ExecutedLosing,
				BlockedByReason: dc.BlockedByReason,
			})
		}
	}

	jsonResponse(w, map[string]any{"counters": out})
}

// GET /api/portfolio/stats
func (s *Server) handlePortfolioStats(w http.ResponseWriter, r *http.Request) {
	if !store.IsConnected() {
		apiError(w, "database not connected", nil)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	stats, err := store.GetPortfolioStats(ctx)
	if err != nil {
		apiError(w, "portfolio stats query failed", err)
		return
	}
	jsonResponse(w, stats)
}

// positionDTO is the wire shape for a single position in /api/portfolio/positions.
type positionDTO struct {
	Ticker       string  `json:"Ticker"`
	Direction    string  `json:"Direction"`
	Strategy     string  `json:"Strategy"`
	EntryPrice   float64 `json:"EntryPrice"`
	CurrentPrice float64 `json:"CurrentPrice"`
	Quantity     float64 `json:"Quantity"`
	PnL          float64 `json:"PnL"`
	PnLPct       float64 `json:"PnLPct"`
}

// GET /api/portfolio/positions
// Broker-first: fetches live positions from broker, enriches with trade metadata
// from the DB, and falls back to tracker → DB when broker is unavailable.
func (s *Server) handlePortfolioPositions(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	// Build trade-metadata lookup once (used by all paths that have DB).
	tradeByTicker := map[string]store.Trade{}
	if store.IsConnected() {
		if openTrades, err := store.GetOpenTrades(ctx); err == nil {
			for _, t := range openTrades {
				tradeByTicker[t.Ticker] = t
			}
		}
	}

	// ── 1. Broker-direct ─────────────────────────────────────────────────────
	if s.brokerView != nil {
		brokerPositions, err := s.brokerView.PositionsSnapshot(ctx)
		if err == nil {
			dtos := make([]positionDTO, 0, len(brokerPositions))
			for _, p := range brokerPositions {
				dto := positionDTO{
					Ticker:       p.Symbol,
					EntryPrice:   p.AvgEntryPrice,
					CurrentPrice: p.CurrentPrice,
					Quantity:     p.Quantity,
					PnL:          p.UnrealizedPL,
					Direction:    p.Side,
				}
				// Use absolute quantity so short positions (negative qty) don't
				// produce a negative cost basis and a nonsensical PnLPct.
				qtyAbs := p.Quantity
				if qtyAbs < 0 {
					qtyAbs = -qtyAbs
				}
				costBasis := p.AvgEntryPrice * qtyAbs
				if costBasis > 0 {
					dto.PnLPct = safeDiv(p.UnrealizedPL, costBasis) * 100
				}
				if t, ok := tradeByTicker[p.Symbol]; ok {
					if dto.Direction == "" {
						dto.Direction = t.Direction
					}
					dto.Strategy = t.Strategy
				}
				dtos = append(dtos, dto)
			}
			jsonResponse(w, map[string]any{"positions": dtos, "source": "broker", "stale_at": nil})
			return
		}
		logger.Warn("broker PositionsSnapshot failed, falling back", "err", err)
	}

	// ── 2. In-process tracker ─────────────────────────────────────────────────
	if s.tracker != nil {
		positions := s.tracker.Positions()
		dtos := make([]positionDTO, 0, len(positions))
		for _, p := range positions {
			dto := positionDTO{
				Ticker:     p.Ticker,
				EntryPrice: p.AvgCostBasis,
				Quantity:   p.Quantity,
				PnL:        p.UnrealizedPL,
			}
			if p.CurrentPrice > 0 {
				dto.CurrentPrice = p.CurrentPrice
			} else if p.Quantity > 0 {
				dto.CurrentPrice = safeDiv(p.CurrentValue, p.Quantity)
			}
			costBasis := p.AvgCostBasis * p.Quantity
			if costBasis > 0 {
				dto.PnLPct = safeDiv(p.UnrealizedPL, costBasis) * 100
			}
			if p.Side != "" {
				dto.Direction = p.Side
			}
			if t, ok := tradeByTicker[p.Ticker]; ok {
				if dto.Direction == "" {
					dto.Direction = t.Direction
				}
				dto.Strategy = t.Strategy
			}
			dtos = append(dtos, dto)
		}
		jsonResponse(w, map[string]any{"positions": dtos, "source": "tracker", "stale_at": nil})
		return
	}

	// ── 3. DB fallback ────────────────────────────────────────────────────────
	if !store.IsConnected() {
		apiError(w, "portfolio tracker not initialized and database not connected", nil)
		return
	}
	trades, err := store.GetOpenTrades(ctx)
	if err != nil {
		apiError(w, "positions query failed", err)
		return
	}
	if trades == nil {
		trades = []store.Trade{}
	}
	jsonResponse(w, map[string]any{"positions": trades, "source": "db", "stale_at": nil})
}

// GET /api/node/ticker/:ticker
func (s *Server) handleTicker(w http.ResponseWriter, r *http.Request) {
	if !store.IsConnected() {
		apiError(w, "database not connected", nil)
		return
	}
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
func (s *Server) handleWatchlist(w http.ResponseWriter, r *http.Request) {
	if !store.IsConnected() {
		apiError(w, "database not connected", nil)
		return
	}
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
func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	if !store.IsConnected() {
		apiError(w, "database not connected", nil)
		return
	}
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

// validSignalStatuses is the set of accepted values for the status filter.
var validSignalStatuses = map[string]bool{
	"pending": true, "approved": true, "executed": true,
	"blocked": true, "expired": true, "stale": true,
}

// GET /api/signals/recent?limit=N&ticker=T&status=S&before=<created_at_rfc3339nano>,<id>
func (s *Server) handleRecentSignals(w http.ResponseWriter, r *http.Request) {
	limit := queryInt(r, "limit", 50, 200)
	ticker := strings.TrimSpace(r.URL.Query().Get("ticker"))
	status := strings.TrimSpace(r.URL.Query().Get("status"))
	beforeRaw := strings.TrimSpace(r.URL.Query().Get("before"))

	if status != "" && !validSignalStatuses[status] {
		http.Error(w, `{"error":"invalid status value"}`, http.StatusBadRequest)
		return
	}

	opts := store.ListSignalsOpts{Limit: limit, Ticker: ticker, Status: status}

	if beforeRaw != "" {
		idx := strings.LastIndex(beforeRaw, ",")
		if idx < 1 || idx == len(beforeRaw)-1 {
			http.Error(w, `{"error":"malformed before cursor"}`, http.StatusBadRequest)
			return
		}
		ts, err := time.Parse(time.RFC3339Nano, beforeRaw[:idx])
		if err != nil {
			http.Error(w, `{"error":"malformed before cursor"}`, http.StatusBadRequest)
			return
		}
		opts.BeforeTime = ts
		opts.BeforeID = beforeRaw[idx+1:]
	}

	if !store.IsConnected() {
		apiError(w, "database not connected", nil)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	sigs, err := store.ListSignals(ctx, opts)
	if err != nil {
		apiError(w, "signals query failed", err)
		return
	}

	// Batch-fetch all analyst results in one query (avoids N+1).
	analystBatch, err := store.GetAnalystResultsBatchForSignals(ctx, sigs)
	if err != nil {
		logger.Warn("web: batch analyst results lookup failed", "err", err.Error())
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

	var nextCursor *string
	if len(sigs) == limit {
		last := sigs[len(sigs)-1]
		c := last.CreatedAt.UTC().Format(time.RFC3339Nano) + "," + last.ID
		nextCursor = &c
	}

	jsonResponse(w, map[string]any{"signals": enriched, "next_cursor": nextCursor})
}

// tradeWithAnalysts enriches a Trade with analyst routing data.
type tradeWithAnalysts struct {
	store.Trade
	Analysts []APIAnalystResult `json:"Analysts,omitempty"`
	Ensemble *APIEnsembleResult `json:"Ensemble,omitempty"`
}

// GET /api/trades/recent?limit=N
func (s *Server) handleRecentTrades(w http.ResponseWriter, r *http.Request) {
	if !store.IsConnected() {
		apiError(w, "database not connected", nil)
		return
	}
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
func (s *Server) handleRecentAnalyses(w http.ResponseWriter, r *http.Request) {
	if !store.IsConnected() {
		apiError(w, "database not connected", nil)
		return
	}
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
func (s *Server) handleBacktestRuns(w http.ResponseWriter, r *http.Request) {
	if !store.IsConnected() {
		apiError(w, "database not connected", nil)
		return
	}
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
func (s *Server) handleBacktestRunDetail(w http.ResponseWriter, r *http.Request) {
	if !store.IsConnected() {
		apiError(w, "database not connected", nil)
		return
	}
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
func (s *Server) handleSystemCache(w http.ResponseWriter, _ *http.Request) {
	snap := cache.TakeSnapshot()
	jsonResponse(w, map[string]any{
		"hits":      snap.Hits,
		"misses":    snap.Misses,
		"evictions": snap.Evictions,
		"errors":    snap.Errors,
	})
}

// GET /api/system/status
func (s *Server) handleSystemStatus(w http.ResponseWriter, r *http.Request) {
	cfg := config.Get()
	uptimeSec := int(time.Since(processStart).Seconds())

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	var peak float64
	var drawdown float64
	if store.IsConnected() {
		_ = store.GetDB().QueryRow(ctx, "SELECT COALESCE(MAX(total_value),0) FROM portfolio_snapshots").Scan(&peak)

		snap, _ := store.GetLatestPortfolioSnapshot(ctx)
		if peak > 0 && snap != nil && snap.TotalValue < peak {
			drawdown = (peak - snap.TotalValue) / peak
		}
	}

	jsonResponse(w, map[string]any{
		"version":           "2.1.0",
		"mode":              strings.ToUpper(cfg.Engine.Mode),
		"tick_interval_sec": cfg.Engine.TickIntervalSec,
		"tick_interval_min": float64(cfg.Engine.TickIntervalSec) / 60.0,
		"uptime_seconds":    uptimeSec,
		"drawdown":          drawdown,
	})
}

// POST /api/system/restart — restricted to loopback callers.
func (s *Server) handleSystemRestart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if !isLoopback(r) {
		http.Error(w, "forbidden: restart requires local connection", http.StatusForbidden)
		return
	}

	jsonResponse(w, map[string]string{"status": "restarting"})

	go func() {
		time.Sleep(100 * time.Millisecond)
		if err := restartSignalFn(); err != nil {
			logger.Warn("web: restart signal failed", "err", err.Error())
		}
	}()
}

// GET /api/system/config
// POST /api/system/config  — mutation is restricted to loopback (127.x / ::1) callers.
func (s *Server) handleSystemConfig(w http.ResponseWriter, r *http.Request) {
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
		dec := json.NewDecoder(r.Body)
		dec.UseNumber()
		if err := dec.Decode(&updates); err != nil {
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
			if err := config.SetFieldAny(dotPath, v); err != nil {
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

// ── ORB handlers ──────────────────────────────────────────────────────────────

// GET /api/orb/sessions?limit=10
func (s *Server) handleORBSessions(w http.ResponseWriter, r *http.Request) {
	if !store.IsConnected() {
		apiError(w, "database not connected", nil)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	limit := 10
	if l := r.URL.Query().Get("limit"); l != "" {
		if parsed, err := strconv.Atoi(l); err == nil && parsed > 0 {
			limit = parsed
		}
	}

	sessions, err := store.GetRecentORBSessions(ctx, store.GetDB(), limit)
	if err != nil {
		apiError(w, "orb sessions query failed", err)
		return
	}
	if sessions == nil {
		sessions = []store.ORBSession{}
	}
	jsonResponse(w, map[string]any{"sessions": sessions})
}

// GET /api/orb/session/today
func (s *Server) handleORBSessionToday(w http.ResponseWriter, r *http.Request) {
	if !store.IsConnected() {
		apiError(w, "database not connected", nil)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	now := time.Now().UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	sessionName := r.URL.Query().Get("session_name")
	session, err := store.GetORBSessionByDate(ctx, store.GetDB(), today, sessionName)
	if err != nil {
		apiError(w, "orb session query failed", err)
		return
	}
	if session == nil {
		w.WriteHeader(http.StatusNotFound)
		jsonResponse(w, map[string]any{"error": "no ORB session for today"})
		return
	}
	jsonResponse(w, map[string]any{"session": session})
}

// GET /api/orb/trades?session_id=<uuid>
func (s *Server) handleORBTrades(w http.ResponseWriter, r *http.Request) {
	if !store.IsConnected() {
		apiError(w, "database not connected", nil)
		return
	}
	sessionID := r.URL.Query().Get("session_id")
	if sessionID == "" {
		apiError(w, "session_id required", nil)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	trades, err := store.GetORBTrades(ctx, store.GetDB(), sessionID)
	if err != nil {
		apiError(w, "orb trades query failed", err)
		return
	}
	if trades == nil {
		trades = []store.ORBTrade{}
	}
	jsonResponse(w, map[string]any{"trades": trades})
}

// GET /api/orb/trades/recent?limit=20
func (s *Server) handleORBRecentTrades(w http.ResponseWriter, r *http.Request) {
	if !store.IsConnected() {
		apiError(w, "database not connected", nil)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	limit := 20
	if l := r.URL.Query().Get("limit"); l != "" {
		if parsed, err := strconv.Atoi(l); err == nil && parsed > 0 {
			limit = parsed
		}
	}

	trades, err := store.GetRecentORBTrades(ctx, store.GetDB(), limit)
	if err != nil {
		apiError(w, "orb recent trades query failed", err)
		return
	}
	if trades == nil {
		trades = []store.ORBTrade{}
	}
	jsonResponse(w, map[string]any{"trades": trades})
}

// GET /api/orb/performance?window=30d
func (s *Server) handleORBPerformance(w http.ResponseWriter, r *http.Request) {
	if !store.IsConnected() {
		apiError(w, "database not connected", nil)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	window := r.URL.Query().Get("window")
	if window == "" {
		window = "30d"
	}

	now := time.Now().UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)

	var insights []store.FeedbackInsight
	for daysBack := 0; daysBack < 7; daysBack++ {
		insightDate := today.AddDate(0, 0, -daysBack)
		var err2 error
		insights, err2 = store.GetLatestFeedbackInsights(ctx, "orb_session", window, insightDate)
		if err2 != nil {
			apiError(w, "orb performance query failed", err2)
			return
		}
		if len(insights) > 0 {
			break
		}
	}

	// Group by scope_key and extract metrics per instrument
	type instrumentPerf struct {
		Ticker     string  `json:"ticker"`
		WinRate    float64 `json:"win_rate"`
		AvgPnLPct  float64 `json:"avg_pnl_pct"`
		TradeCount int     `json:"trade_count"`
	}
	perfByScopeKey := map[string]*instrumentPerf{}
	for _, ins := range insights {
		p, ok := perfByScopeKey[ins.ScopeKey]
		if !ok {
			p = &instrumentPerf{Ticker: ins.ScopeKey}
			perfByScopeKey[ins.ScopeKey] = p
		}
		switch ins.Metric {
		case "win_rate":
			p.WinRate = ins.Value
		case "avg_pnl_pct":
			p.AvgPnLPct = ins.Value
		case "trade_count":
			p.TradeCount = ins.TradeCount
		}
	}

	instruments := make([]instrumentPerf, 0, len(perfByScopeKey))
	var totalTrades int
	var weightedWinRate, weightedPnL float64
	for _, p := range perfByScopeKey {
		instruments = append(instruments, *p)
		totalTrades += p.TradeCount
		weightedWinRate += p.WinRate * float64(p.TradeCount)
		weightedPnL += p.AvgPnLPct * float64(p.TradeCount)
	}

	overallWinRate := 0.0
	overallAvgPnL := 0.0
	if totalTrades > 0 {
		overallWinRate = weightedWinRate / float64(totalTrades)
		overallAvgPnL = weightedPnL / float64(totalTrades)
	}

	jsonResponse(w, map[string]any{
		"instruments": instruments,
		"overall": map[string]any{
			"win_rate":    overallWinRate,
			"avg_pnl_pct": overallAvgPnL,
			"trade_count": totalTrades,
			"window":      window,
		},
	})
}

// GET /api/orb/flags?limit=20
func (s *Server) handleORBFlags(w http.ResponseWriter, r *http.Request) {
	if !store.IsConnected() {
		apiError(w, "database not connected", nil)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	limit := 20
	if l := r.URL.Query().Get("limit"); l != "" {
		if parsed, err := strconv.Atoi(l); err == nil && parsed > 0 {
			limit = parsed
		}
	}

	flags, err := store.GetActiveFlags(ctx, limit)
	if err != nil {
		apiError(w, "orb flags query failed", err)
		return
	}
	if flags == nil {
		flags = []store.FeedbackFlag{}
	}

	var orbFlags []store.FeedbackFlag
	for _, f := range flags {
		if f.Scope == "orb_session" || f.Scope == "instrument" {
			orbFlags = append(orbFlags, f)
		}
	}
	if orbFlags == nil {
		orbFlags = []store.FeedbackFlag{}
	}
	jsonResponse(w, map[string]any{"flags": orbFlags})
}

// POST /api/orb/flags/:id/resolve
func (s *Server) handleORBFlagResolve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !store.IsConnected() {
		apiError(w, "database not connected", nil)
		return
	}

	// Extract flag ID: path is /api/orb/flags/<id>/resolve
	path := strings.TrimPrefix(r.URL.Path, "/api/orb/flags/")
	id := strings.TrimSuffix(path, "/resolve")
	if id == "" || id == path {
		http.Error(w, "expected /api/orb/flags/{id}/resolve", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	if err := store.ResolveFlag(ctx, id); err != nil {
		apiError(w, "resolve flag failed", err)
		return
	}
	jsonResponse(w, map[string]string{"status": "ok"})
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func jsonResponse(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		logger.Warn("web: json encode error", "err", err.Error())
	}
}

func apiError(w http.ResponseWriter, msg string, err error) {
	errStr := ""
	if err != nil {
		errStr = err.Error()
	}
	logger.Warn("web: api error", "msg", msg, "err", errStr)
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
