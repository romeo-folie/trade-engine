// Package engine contains the core trading engine loop, scheduler, and pipeline
// integration. It orchestrates market data fetching, indicator computation,
// news retrieval, and (in later phases) analysis and order execution.
package engine

import (
	"context"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/romeo-folie/trade-engine/internal/analysis"
	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/data"
	"github.com/romeo-folie/trade-engine/internal/embeddings"
	"github.com/romeo-folie/trade-engine/internal/events"
	"github.com/romeo-folie/trade-engine/internal/execution"
	"github.com/romeo-folie/trade-engine/internal/indicators"
	"github.com/romeo-folie/trade-engine/internal/knowledge"
	"github.com/romeo-folie/trade-engine/internal/logger"
	"github.com/romeo-folie/trade-engine/internal/portfolio"
	"github.com/romeo-folie/trade-engine/internal/risk"
	"github.com/romeo-folie/trade-engine/internal/store"
)

const (
	// historyCandles is the number of candles fetched per watchlist item per tick.
	// 200 daily bars covers ~10 months; enough for EMA200 and ADX warmup.
	historyCandles = 200

	// newsFetchInterval is the minimum time between news fetches for the same ticker.
	// News is expensive to fetch; 15 minutes between updates is the default cadence.
	newsFetchInterval = 15 * time.Minute

	// signalExpiryCheckInterval is how often the background goroutine checks for stale/expired signals.
	signalExpiryCheckInterval = time.Minute

	// retentionInterval is how often the retention cleanup runs (once per day).
	retentionInterval = 24 * time.Hour
)

// Engine is the core trading loop. It polls market data, computes indicators,
// fetches news, dispatches analysis, generates signals, and executes trades.
type Engine struct {
	cfg          *config.Config
	marketData   data.MarketDataProvider
	newsProvider data.NewsProvider
	analyzer     *analysis.Analyzer
	executor     execution.Executor
	portfolio    *portfolio.Tracker
	bus          *events.Bus
	enricher     *knowledge.Enricher  // nil if no LLM configured
	embedder     embeddings.Embedder  // nil if embeddings disabled

	// newsLastFetched tracks when news was last fetched per ticker, to avoid
	// hammering the news API on every 5-min tick.
	newsLastFetched map[string]time.Time
	newsMu          sync.Mutex

	modeMu sync.RWMutex
	mode   string // auto | paper | manual
}

// New creates a new Engine instance with the given providers.
// mode is the initial trading mode (manual | auto | paper).
// executor may be nil; if nil, the paper executor is used as fallback.
func New(cfg *config.Config, market data.MarketDataProvider, news data.NewsProvider, exec execution.Executor, mode string) *Engine {
	if exec == nil {
		exec = execution.NewPaperExecutor()
	}
	az := analysis.NewAnalyzer(cfg.Models)

	// Build an enricher that reuses the primary LLM for relationship extraction.
	// We piggyback on CustomSystemPrompt/CustomUserMessage + RawResponse so no
	// new interface methods are required.
	callFn := knowledge.ExtractionCallFn(func(ctx context.Context, system, user string) (string, error) {
		_, meta, err := az.Analyze(ctx, analysis.AnalysisRequest{
			Ticker:             "__extraction__",
			CustomSystemPrompt: system,
			CustomUserMessage:  user,
		})
		// meta.RawResponse is populated before the JSON-parse step.  The parse
		// step fails for extraction output (JSON array ≠ AnalysisResponse), which
		// is expected — we want the raw text, not the parsed struct.
		if meta.RawResponse != "" {
			return meta.RawResponse, nil
		}
		// No raw response means the HTTP/auth/rate-limit layer failed.
		if err != nil {
			return "", fmt.Errorf("enricher: model call failed: %w", err)
		}
		return "", fmt.Errorf("enricher: empty model response")
	})

	// Create embedder for similar-analysis search (best-effort; disabled if not configured).
	var emb embeddings.Embedder
	if cfg.Embeddings.Enabled && cfg.Embeddings.SimilarAnalysesEnabled {
		if e, err := embeddings.New(cfg.Embeddings); err == nil {
			emb = e
		} else {
			logger.Warn("embeddings disabled", "err", err.Error())
		}
	}

	return &Engine{
		cfg:             cfg,
		marketData:      market,
		analyzer:        az,
		newsProvider:    news,
		executor:        exec,
		portfolio:       portfolio.NewTracker(time.Hour),
		bus:             events.New(),
		enricher:        knowledge.NewEnricher(cfg, callFn),
		embedder:        emb,
		newsLastFetched: make(map[string]time.Time),
		mode:            mode,
	}
}

// Run starts the engine loop. It blocks until ctx is cancelled.
// On each tick it processes all active watchlist items in parallel.
// Background goroutines handle signal expiry and data retention.
func (e *Engine) Run(ctx context.Context) error {
	interval := time.Duration(e.cfg.Engine.TickIntervalSec) * time.Second
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Background: signal expiry + staleness checks.
	go e.runSignalExpiryLoop(ctx)
	// Background: daily data retention cleanup.
	go e.runRetentionLoop(ctx)

	logger.Info("engine loop started", "interval_sec", e.cfg.Engine.TickIntervalSec, "mode", e.mode)

	// Run immediately on start, then on every tick.
	if err := e.tick(ctx); err != nil {
		logger.Warn("tick error", "err", err.Error())
	}

	for {
		select {
		case <-ticker.C:
			if !IsMarketOpen(time.Now()) {
				logger.Info("market closed — skipping tick")
				continue
			}
			if err := e.tick(ctx); err != nil {
				logger.Warn("tick error", "err", err.Error())
			}
		case <-ctx.Done():
			logger.Info("engine loop stopping")
			return nil
		}
	}
}

// tick processes all active watchlist items for a single engine cycle.
// Items are processed in parallel; errors per item are logged but do not abort the tick.
func (e *Engine) tick(ctx context.Context) error {
	items, err := store.GetWatchlistItems(ctx, true)
	if err != nil {
		return fmt.Errorf("get watchlist: %w", err)
	}
	if len(items) == 0 {
		logger.Info("watchlist is empty — nothing to process")
		return nil
	}

	logger.Info("processing watchlist", "count", len(items))

	var wg sync.WaitGroup
	// Semaphore: limit to 5 concurrent goroutines to avoid API rate limit bursts.
	sem := make(chan struct{}, 5)

	for _, item := range items {
		if item.Platform != "stock" {
			// Non-stock platforms handled in later phases.
			continue
		}
		wg.Add(1)
		go func(ticker string, timeframes []string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			e.processStock(ctx, ticker, timeframes)
		}(item.Ticker, item.Timeframes)
	}

	wg.Wait()

	// Monitor open positions for stop/TP breaches after all tickers processed.
	e.monitorPositions(ctx)

	return nil
}

// processStock fetches candles, computes indicators, and fetches news for a single stock.
func (e *Engine) processStock(ctx context.Context, ticker string, timeframes []string) {
	if len(timeframes) == 0 {
		timeframes = []string{string(data.Timeframe1Day)}
	}

	for _, tfStr := range timeframes {
		tf := data.Timeframe(tfStr)
		e.fetchAndStoreCandles(ctx, ticker, tf)
	}

	// Compute indicators on the primary timeframe (prefer "1d", fall back to first).
	primaryTF := data.Timeframe(timeframes[0])
	for _, tf := range timeframes {
		if tf == string(data.Timeframe1Day) {
			primaryTF = data.Timeframe1Day
			break
		}
	}

	candles, err := store.GetCandles(ctx, ticker, primaryTF,
		time.Now().AddDate(-2, 0, 0), time.Now())
	if err != nil {
		logger.Warn("get candles for indicators", "ticker", ticker, "err", err.Error())
		return
	}

	if len(candles) < 20 {
		logger.Info("insufficient candles for indicators", "ticker", ticker, "count", len(candles))
		return
	}

	set, err := indicators.Compute(candles)
	if err != nil {
		logger.Warn("compute indicators", "ticker", ticker, "err", err.Error())
		return
	}

	logger.Info("indicators computed",
		"ticker", ticker,
		"rsi", fmt.Sprintf("%.2f", set.RSI),
		"ema20", fmt.Sprintf("%.2f", set.EMA20),
		"macd", fmt.Sprintf("%.4f", set.MACDLine),
		"adx", fmt.Sprintf("%.2f", set.ADX),
		"atr", fmt.Sprintf("%.4f", set.ATR),
		"obv", fmt.Sprintf("%.0f", set.OBV),
	)
	e.bus.Publish(events.IndicatorsComputed, events.IndicatorsComputedPayload{
		Ticker: ticker, RSI: set.RSI, EMA20: set.EMA20, MACD: set.MACDLine, ADX: set.ADX,
	})

	// Fetch news (rate-limited to newsFetchInterval cadence).
	e.fetchAndStoreNews(ctx, ticker)

	// Run LLM analysis — PRD §8. Errors are logged but non-fatal.
	e.runAnalysis(ctx, ticker, string(primaryTF), candles, set)
}

// runAnalysis builds the analysis request, calls the configured model, stores
// the result, and logs the recommendation summary.
func (e *Engine) runAnalysis(ctx context.Context, ticker, timeframe string, candles []data.Candle, set *indicators.IndicatorSet) {
	if set == nil {
		return
	}
	if e.analyzer == nil || !e.analyzer.Primary().IsAvailable() {
		logger.Debug("analysis skipped — primary model not available", "ticker", ticker)
		return
	}

	strategy := "default"
	if e.cfg.Engine.Mode != "" {
		strategy = e.cfg.Engine.Mode
	}

	// Build candle snapshots (most-recent-first for prompt context).
	limit := len(candles)
	if limit > 10 {
		limit = 10
	}
	snaps := make([]analysis.CandleSnapshot, 0, limit)
	for i := len(candles) - 1; i >= 0 && len(snaps) < limit; i-- {
		c := candles[i]
		snaps = append(snaps, analysis.CandleSnapshot{
			Timestamp: c.Timestamp,
			Open:      c.Open,
			High:      c.High,
			Low:       c.Low,
			Close:     c.Close,
			Volume:    c.Volume,
			VWAP:      c.VWAP,
		})
	}

	// Load recent news from the store for prompt inclusion.
	newsItems, _ := store.GetRecentNews(ctx, ticker, time.Now().Add(-48*time.Hour))
	snippets := make([]analysis.NewsSnippet, 0, len(newsItems))
	for _, n := range newsItems {
		snippets = append(snippets, analysis.NewsSnippet{
			Headline:    n.Headline,
			Source:      n.Source,
			PublishedAt: n.PublishedAt,
			Sentiment:   sentimentLabel(n.SentimentScore),
		})
	}

	// Build knowledge graph context for this ticker (best-effort; non-fatal).
	var knowledgeCtx string
	if gc, err := knowledge.BuildContext(ctx, ticker, e.portfolio.PositionMap()); err == nil {
		knowledgeCtx = gc.Format()
	}

	// Semantic search for similar past setups (best-effort).
	var similarSetups []analysis.SimilarSetup
	if e.embedder != nil && e.cfg.Embeddings.SimilarAnalysesEnabled {
		// Embed a compact description of current conditions for similarity search.
		searchText := fmt.Sprintf("%s RSI:%.1f EMA20:%.2f MACD:%.4f ADX:%.1f",
			ticker, set.RSI, set.EMA20, set.MACDLine, set.ADX)
		if queryVec, err := e.embedder.Embed(ctx, searchText); err == nil {
			threshold := e.cfg.Embeddings.SimilarAnalysesThreshold
			limit := e.cfg.Embeddings.SimilarAnalysesLimit
			if past, err := store.FindSimilarAnalyses(ctx, queryVec, threshold, limit); err == nil {
				for _, p := range past {
					similarSetups = append(similarSetups, analysis.SimilarSetup{
						Ticker:          p.Ticker,
						Action:          p.Action,
						Confidence:      p.Confidence,
						Reasoning:       p.Reasoning,
						CreatedAt:       p.CreatedAt,
						TradeOutcomePct: p.TradeOutcomePct,
					})
				}
			}
		}
	}

	req := analysis.AnalysisRequest{
		Ticker:           ticker,
		Strategy:         strategy,
		Timeframe:        timeframe,
		RecentCandles:    snaps,
		KnowledgeContext: knowledgeCtx,
		SimilarAnalyses:  similarSetups,
		Indicators: analysis.IndicatorSnapshot{
			EMA20: set.EMA20, EMA50: set.EMA50, EMA200: set.EMA200,
			SMA20: set.SMA20, SMA50: set.SMA50, SMA200: set.SMA200,
			RSI:           set.RSI,
			MACDLine:      set.MACDLine,
			MACDSignal:    set.MACDSignal,
			MACDHistogram: set.MACDHistogram,
			BBUpper:       set.BBUpper,
			BBMiddle:      set.BBMiddle,
			BBLower:       set.BBLower,
			ADX:           set.ADX, ATR: set.ATR, OBV: set.OBV,
			StochK: set.StochK, StochD: set.StochD,
			VWAP:     set.VWAP,
			VolSMA20: set.VolumeSMA20,
			VolRatio: volRatio(candles, set.VolumeSMA20),
		},
		News:      snippets,
		Portfolio: analysis.PortfolioContext{}, // Phase 4
	}

	resp, meta, err := e.analyzer.Analyze(ctx, req)
	if err != nil {
		logger.Warn("analysis failed", "ticker", ticker, "err", err.Error())
		return
	}

	logger.Info("analysis complete",
		"ticker", ticker,
		"action", string(resp.Action),
		"confidence", fmt.Sprintf("%.2f", resp.Confidence),
		"size_pct", fmt.Sprintf("%.1f%%", resp.SizePct),
		"reasoning", resp.Reasoning[:min(120, len(resp.Reasoning))],
	)
	e.bus.Publish(events.AnalysisCompleted, events.AnalysisCompletedPayload{
		Ticker: ticker, Action: string(resp.Action), Confidence: resp.Confidence,
		Reasoning: resp.Reasoning, Provider: meta.Provider, Model: meta.Model, LatencyMs: meta.LatencyMs,
	})

	analysisID, err := store.InsertAnalysis(ctx, ticker, strategy, resp, meta)
	if err != nil {
		logger.Warn("store analysis", "ticker", ticker, "err", err.Error())
	}

	// Phase 4: generate and potentially execute a signal from this analysis.
	e.generateSignal(ctx, ticker, strategy, candles, resp, analysisID)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// volRatio returns current volume / VolSMA20, or 1.0 if data is unavailable.
func volRatio(candles []data.Candle, volSMA float64) float64 {
	if len(candles) == 0 || volSMA == 0 {
		return 1.0
	}
	latest := candles[len(candles)-1]
	return float64(latest.Volume) / volSMA
}

// sentimentLabel converts a numeric sentiment score (0.0–1.0 bullish) to a label.
func sentimentLabel(score float64) string {
	switch {
	case score >= 0.55:
		return "bullish"
	case score <= 0.45:
		return "bearish"
	default:
		return "neutral"
	}
}

// fetchAndStoreCandles fetches recent candles from the market data provider and
// upserts them into the database. Uses FetchHistory for the live loop (recent bars only).
func (e *Engine) fetchAndStoreCandles(ctx context.Context, ticker string, tf data.Timeframe) {
	if e.marketData == nil {
		return
	}
	candles, err := e.marketData.FetchHistory(ctx, ticker, tf, historyCandles)
	if err != nil {
		logger.Warn("fetch candles", "ticker", ticker, "tf", string(tf), "err", err.Error())
		return
	}
	if err := store.UpsertCandles(ctx, candles); err != nil {
		logger.Warn("upsert candles", "ticker", ticker, "err", err.Error())
		return
	}
	logger.Info("candles upserted", "ticker", ticker, "tf", string(tf), "count", len(candles))
	e.bus.Publish(events.DataFetched, events.DataFetchedPayload{
		Ticker: ticker, Timeframe: string(tf), Count: len(candles),
	})
}

// fetchAndStoreNews fetches recent news for a ticker (respecting the cadence limit)
// and upserts articles into the database.
func (e *Engine) fetchAndStoreNews(ctx context.Context, ticker string) {
	if e.newsProvider == nil {
		return
	}

	e.newsMu.Lock()
	last := e.newsLastFetched[ticker]
	if time.Since(last) < newsFetchInterval {
		e.newsMu.Unlock()
		return
	}
	e.newsLastFetched[ticker] = time.Now()
	e.newsMu.Unlock()

	since := time.Now().Add(-48 * time.Hour)
	items, err := e.newsProvider.FetchNews(ctx, ticker, since)
	if err != nil {
		logger.Warn("fetch news", "ticker", ticker, "err", err.Error())
		return
	}
	if err := store.UpsertNewsItems(ctx, items); err != nil {
		logger.Warn("upsert news", "ticker", ticker, "err", err.Error())
		return
	}
	logger.Info("news fetched", "ticker", ticker, "count", len(items))

	// Kick off relationship extraction for each article in the background.
	// EnrichNewsItem is semaphore-bounded and non-blocking from the caller's
	// perspective (blocks only if all extraction slots are full).
	// newsItemID is passed as "" because we don't have the DB UUID here;
	// entity-mention recording is skipped when no UUID is provided.
	if e.enricher != nil {
		for _, item := range items {
			e.enricher.EnrichNewsItem(ctx, "", item.Headline, item.Summary)
		}
	}
}

// BackfillHistory fetches and stores 2 years of daily + 30 days of hourly candles
// for a newly added ticker. Called from the watchlist add command.
func (e *Engine) BackfillHistory(ctx context.Context, ticker string) error {
	if e.marketData == nil {
		return fmt.Errorf("no market data provider configured")
	}

	logger.Info("backfilling history", "ticker", ticker)

	now := time.Now()
	twoYearsAgo := now.AddDate(-2, 0, 0)
	thirtyDaysAgo := now.AddDate(0, 0, -30)

	// 2 years of daily candles.
	daily, err := e.marketData.FetchCandles(ctx, ticker, data.Timeframe1Day, twoYearsAgo, now)
	if err != nil {
		return fmt.Errorf("fetch daily history: %w", err)
	}
	if err := store.UpsertCandles(ctx, daily); err != nil {
		return fmt.Errorf("store daily history: %w", err)
	}
	logger.Info("daily backfill complete", "ticker", ticker, "candles", len(daily))

	// 30 days of hourly candles.
	hourly, err := e.marketData.FetchCandles(ctx, ticker, data.Timeframe1Hour, thirtyDaysAgo, now)
	if err != nil {
		return fmt.Errorf("fetch hourly history: %w", err)
	}
	if err := store.UpsertCandles(ctx, hourly); err != nil {
		return fmt.Errorf("store hourly history: %w", err)
	}
	logger.Info("hourly backfill complete", "ticker", ticker, "candles", len(hourly))

	return nil
}

// SetMode updates the engine's active trading mode at runtime.
func (e *Engine) SetMode(mode string) {
	e.modeMu.Lock()
	e.mode = mode
	e.modeMu.Unlock()
	logger.Info("engine mode updated", "mode", mode)
}

// Mode returns the current trading mode.
func (e *Engine) Mode() string {
	e.modeMu.RLock()
	defer e.modeMu.RUnlock()
	return e.mode
}

// Portfolio returns the portfolio tracker for external reads (e.g. CLI status).
func (e *Engine) Portfolio() *portfolio.Tracker { return e.portfolio }

// Bus returns the event bus for external subscribers (TUI, Telegram).
func (e *Engine) Bus() *events.Bus { return e.bus }

// ─── Phase 4: Signal generation ───────────────────────────────────────────────

// generateSignal creates a signal from a BUY/SELL analysis, runs risk checks,
// and routes execution according to the current mode.
func (e *Engine) generateSignal(ctx context.Context, ticker, strategy string, candles []data.Candle, resp analysis.AnalysisResponse, analysisID string) {
	if resp.Action == analysis.ActionHold {
		return
	}

	// Current price = latest candle close.
	var signalPrice float64
	if len(candles) > 0 {
		signalPrice = candles[len(candles)-1].Close
	}

	expiryMin := e.cfg.Risk.SignalExpiryMin
	if expiryMin == 0 {
		expiryMin = 30
	}
	stalePct := e.cfg.Risk.StalePriceMovePct
	if stalePct == 0 {
		stalePct = 2.0
	}

	sig := store.Signal{
		Ticker:            ticker,
		Action:            string(resp.Action),
		Strategy:          strategy,
		SignalPrice:       signalPrice,
		StopLoss:          resp.StopLoss,
		TakeProfit:        resp.TakeProfit,
		Confidence:        resp.Confidence,
		SizePct:           resp.SizePct,
		Status:            "pending",
		ExpiresAt:         time.Now().Add(time.Duration(expiryMin) * time.Minute),
		StalePriceMovePct: stalePct,
	}

	// Run risk checks.
	pState := e.portfolio.CurrentState()
	openTrades, _ := store.GetOpenTrades(ctx)

	riskIn := risk.CheckInput{
		Cfg:            e.cfg.Risk,
		Action:         string(resp.Action),
		Confidence:     resp.Confidence,
		SizePct:        resp.SizePct,
		StopLoss:       resp.StopLoss,
		TakeProfit:     resp.TakeProfit,
		SignalPrice:    signalPrice,
		PortfolioValue: pState.TotalValue,
		Cash:           pState.Cash,
		OpenPositions:  len(openTrades),
		DailyPnL:       pState.RealizedPnLDay,
		PeakValue:      pState.PeakValue,
		LastLossAt:     pState.LastLossAt,
	}

	results, passed := risk.CheckAll(riskIn)
	for _, r := range results {
		level := "INFO"
		if !r.Passed {
			level = "WARN"
		}
		logger.Info("risk check", "check", r.Name, "level", level, "passed", r.Passed, "reason", r.Reason)
	}

	if !passed {
		sig.Status = "blocked"
		sig.BlockReason = risk.BlockReason(results)
		if _, err := store.InsertSignal(ctx, sig); err != nil {
			logger.Warn("store blocked signal", "ticker", ticker, "err", err)
		}
		logger.Warn("signal blocked", "ticker", ticker, "reason", sig.BlockReason)
		e.bus.Publish(events.SignalBlocked, events.SignalBlockedPayload{
			Ticker: ticker, Action: string(resp.Action), Confidence: resp.Confidence,
			BlockReason: sig.BlockReason, FailedCheck: risk.BlockReason(results),
		})
		return
	}

	sigID, err := store.InsertSignal(ctx, sig)
	if err != nil {
		logger.Warn("store signal", "ticker", ticker, "err", err)
		return
	}

	logger.Info("signal created",
		"id", sigID, "ticker", ticker,
		"action", string(resp.Action), "confidence", resp.Confidence,
		"mode", e.Mode(),
	)

	e.bus.Publish(events.SignalGenerated, events.SignalGeneratedPayload{
		SignalID: sigID, Ticker: ticker, Action: string(resp.Action),
		Confidence: resp.Confidence, SignalPrice: signalPrice,
		StopLoss: resp.StopLoss, TakeProfit: resp.TakeProfit, Mode: e.Mode(),
	})

	// Route based on mode.
	mode := e.Mode()
	switch mode {
	case "auto", "paper":
		e.executeSignal(ctx, sigID, sig)
	case "manual":
		// Signal stays pending — user approves via CLI or Telegram.
		logger.Info("signal queued for manual approval", "id", sigID, "ticker", ticker)
		e.bus.Publish(events.SignalQueued, events.SignalQueuedPayload{
			SignalID: sigID, Ticker: ticker, Action: string(resp.Action),
			Confidence: resp.Confidence, ExpiresIn: time.Until(sig.ExpiresAt),
		})
	default:
		logger.Warn("unknown mode, defaulting to manual", "mode", mode)
	}
}

// executeSignal places the order via the executor, creates a trade record, and
// records the entry order.
func (e *Engine) executeSignal(ctx context.Context, sigID string, sig store.Signal) {
	side := execution.OrderSideBuy
	direction := "buy"
	if sig.Action == string(analysis.ActionSell) {
		side = execution.OrderSideSell
		direction = "sell"
	}

	// Size: use signal's size_pct of portfolio cash.
	pState := e.portfolio.CurrentState()
	positionValue := pState.Cash * (sig.SizePct / 100)
	qty := positionValue / sig.SignalPrice
	if qty <= 0 {
		logger.Warn("signal execution skipped — zero quantity", "ticker", sig.Ticker)
		return
	}

	orderReq := execution.OrderRequest{
		Ticker:     sig.Ticker,
		Side:       side,
		Type:       execution.OrderTypeMarket,
		Quantity:   qty,
		LimitPrice: sig.SignalPrice, // paper executor fills at this price
	}

	result, err := e.executor.PlaceOrder(ctx, orderReq)
	if err != nil {
		logger.Warn("order placement failed", "ticker", sig.Ticker, "err", err)
		return
	}

	// Mark signal as executed.
	if err := store.UpdateSignalStatus(ctx, sigID, "executed", ""); err != nil {
		logger.Warn("update signal status", "id", sigID, "err", err)
	}

	// Create trade record.
	trade := store.Trade{
		SignalID:   sigID,
		Ticker:     sig.Ticker,
		Direction:  direction,
		Strategy:   sig.Strategy,
		EntryPrice: result.FilledPrice,
		Quantity:   result.FilledQty,
		StopLoss:   sig.StopLoss,
		TakeProfit: sig.TakeProfit,
		Status:     "open",
	}
	tradeID, err := store.InsertTrade(ctx, trade)
	if err != nil {
		logger.Warn("insert trade", "ticker", sig.Ticker, "err", err)
		return
	}

	// Record the entry order.
	entryOrder := store.Order{
		TradeID:       tradeID,
		BrokerOrderID: result.BrokerOrderID,
		Ticker:        sig.Ticker,
		Side:          string(side),
		OrderType:     string(execution.OrderTypeMarket),
		Quantity:      result.FilledQty,
		FilledPrice:   result.FilledPrice,
		FilledQty:     result.FilledQty,
		Status:        result.Status,
	}
	if _, err := store.InsertOrder(ctx, entryOrder); err != nil {
		logger.Warn("insert entry order", "tradeID", tradeID, "err", err)
	}

	logger.Info("trade opened",
		"trade_id", tradeID,
		"ticker", sig.Ticker,
		"direction", direction,
		"entry_price", result.FilledPrice,
		"qty", result.FilledQty,
	)
	e.bus.Publish(events.TradeExecuted, events.TradeExecutedPayload{
		TradeID: tradeID, SignalID: sigID, Ticker: sig.Ticker,
		Direction: direction, EntryPrice: result.FilledPrice,
		Quantity: result.FilledQty, Strategy: sig.Strategy,
	})
}

// ApproveSignal approves a pending signal and executes it. Called from CLI / Telegram.
func (e *Engine) ApproveSignal(ctx context.Context, sigID string) error {
	sig, err := store.GetSignalByID(ctx, sigID)
	if err != nil {
		return fmt.Errorf("get signal: %w", err)
	}
	if sig == nil {
		return fmt.Errorf("signal %s not found", sigID)
	}
	if sig.Status != "pending" && sig.Status != "approved" {
		return fmt.Errorf("signal %s has status %q — cannot approve", sigID, sig.Status)
	}
	e.executeSignal(ctx, sigID, *sig)
	return nil
}

// RejectSignal marks a pending signal as rejected.
func (e *Engine) RejectSignal(ctx context.Context, sigID string) error {
	return store.UpdateSignalStatus(ctx, sigID, "rejected", "manually rejected")
}

// ─── Phase 4: Portfolio refresh ───────────────────────────────────────────────

// refreshPortfolio fetches current positions from the executor and updates the
// portfolio tracker. This ensures risk checks and signal sizing use live data.
func (e *Engine) refreshPortfolio(ctx context.Context) {
	positions, err := e.executor.GetPositions(ctx)
	if err != nil {
		logger.Warn("get positions for portfolio refresh", "err", err)
		return
	}

	eps := make([]portfolio.ExecPosition, 0, len(positions))
	for _, p := range positions {
		eps = append(eps, portfolio.ExecPosition{
			Ticker:       p.Ticker,
			CurrentValue: p.CurrentValue,
			UnrealizedPL: p.UnrealizedPL,
		})
	}

	// Compute realized P&L today from closed trades.
	var realizedToday float64
	closedTrades, _ := store.GetRecentTrades(ctx, 100)
	today := time.Now().UTC().Truncate(24 * time.Hour)
	for _, t := range closedTrades {
		if t.Status == "closed" && t.ClosedAt.After(today) {
			realizedToday += t.PnL
		}
	}

	// Cash = initial capital - invested. For paper trading without a real broker
	// account, estimate from config or use a default.
	cash := e.cfg.Engine.InitialCapital
	for _, p := range positions {
		cash -= p.CurrentValue - p.UnrealizedPL // subtract cost basis
	}
	if cash < 0 {
		cash = 0
	}

	e.portfolio.Update(eps, cash, realizedToday)
}

// ─── Phase 4: Position monitoring (4.7) ───────────────────────────────────────

// monitorPositions checks all open trades for stop-loss and take-profit breaches.
func (e *Engine) monitorPositions(ctx context.Context) {
	// Refresh portfolio state from executor positions.
	e.refreshPortfolio(ctx)

	trades, err := store.GetOpenTrades(ctx)
	if err != nil {
		logger.Warn("get open trades for monitoring", "err", err)
		return
	}
	if len(trades) == 0 {
		return
	}

	for _, t := range trades {
		// Try intraday candles first for more responsive SL/TP detection,
		// then fall back to daily.
		var currentPrice float64
		for _, tf := range []data.Timeframe{data.Timeframe5Min, data.Timeframe15Min, data.Timeframe1Hour, data.Timeframe1Day} {
			latest, err := store.GetLatestCandle(ctx, t.Ticker, tf)
			if err == nil && latest != nil {
				currentPrice = latest.Close
				break
			}
		}
		if currentPrice == 0 {
			continue
		}

		var shouldClose bool
		var exitReason string

		if t.StopLoss > 0 {
			if (t.Direction == "buy" && currentPrice <= t.StopLoss) ||
				(t.Direction == "sell" && currentPrice >= t.StopLoss) {
				shouldClose = true
				exitReason = "stop-loss hit"
			}
		}

		if !shouldClose && t.TakeProfit > 0 {
			if (t.Direction == "buy" && currentPrice >= t.TakeProfit) ||
				(t.Direction == "sell" && currentPrice <= t.TakeProfit) {
				shouldClose = true
				exitReason = "take-profit hit"
			}
		}

		if !shouldClose {
			continue
		}

		// Place exit order.
		exitSide := execution.OrderSideSell
		if t.Direction == "sell" {
			exitSide = execution.OrderSideBuy
		}
		result, err := e.executor.PlaceOrder(ctx, execution.OrderRequest{
			Ticker:     t.Ticker,
			Side:       exitSide,
			Type:       execution.OrderTypeMarket,
			Quantity:   t.Quantity,
			LimitPrice: currentPrice,
			TradeID:    t.ID,
		})
		if err != nil {
			logger.Warn("exit order failed", "trade_id", t.ID, "reason", exitReason, "err", err)
			continue
		}

		// Calculate P&L.
		pnl := (result.FilledPrice - t.EntryPrice) * t.Quantity
		if t.Direction == "sell" {
			pnl = (t.EntryPrice - result.FilledPrice) * t.Quantity
		}
		pnlPct := 0.0
		if t.EntryPrice > 0 {
			pnlPct = (pnl / (t.EntryPrice * t.Quantity)) * 100
		}

		if err := store.CloseTrade(ctx, t.ID, result.FilledPrice, pnl, pnlPct); err != nil {
			logger.Warn("close trade", "trade_id", t.ID, "err", err)
		}

		// Record exit order.
		exitOrder := store.Order{
			TradeID:       t.ID,
			BrokerOrderID: result.BrokerOrderID,
			Ticker:        t.Ticker,
			Side:          string(exitSide),
			OrderType:     string(execution.OrderTypeMarket),
			Quantity:      result.FilledQty,
			FilledPrice:   result.FilledPrice,
			FilledQty:     result.FilledQty,
			Status:        result.Status,
			IsStopLoss:    exitReason == "stop-loss hit",
			IsTakeProfit:  exitReason == "take-profit hit",
		}
		if _, err := store.InsertOrder(ctx, exitOrder); err != nil {
			logger.Warn("insert exit order", "trade_id", t.ID, "err", err)
		}

		if pnl < 0 {
			e.portfolio.RecordLoss()
		}

		logger.Info("trade closed",
			"trade_id", t.ID,
			"ticker", t.Ticker,
			"reason", exitReason,
			"pnl", fmt.Sprintf("%.2f", pnl),
			"pnl_pct", fmt.Sprintf("%.2f%%", pnlPct),
		)
		e.bus.Publish(events.TradeClosed, events.TradeClosedPayload{
			TradeID: t.ID, Ticker: t.Ticker, Direction: t.Direction,
			EntryPrice: t.EntryPrice, ExitPrice: result.FilledPrice,
			PnL: pnl, PnLPct: pnlPct, Reason: exitReason,
		})
	}

	// Update portfolio state after monitoring.
	e.portfolio.MaybeSnapshot(ctx)
}

// ─── Phase 4: Signal expiry (4.8) ────────────────────────────────────────────

// runSignalExpiryLoop checks pending signals for time/price-based expiry.
func (e *Engine) runSignalExpiryLoop(ctx context.Context) {
	ticker := time.NewTicker(signalExpiryCheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			e.expireStaleSignals(ctx)
		case <-ctx.Done():
			return
		}
	}
}

func (e *Engine) expireStaleSignals(ctx context.Context) {
	signals, err := store.GetPendingSignals(ctx)
	if err != nil {
		logger.Warn("get pending signals for expiry", "err", err)
		return
	}

	now := time.Now()
	for _, sig := range signals {
		// Time-based expiry.
		if !sig.ExpiresAt.IsZero() && now.After(sig.ExpiresAt) {
			if err := store.UpdateSignalStatus(ctx, sig.ID, "expired", "signal expired (time limit)"); err != nil {
				logger.Warn("expire signal", "id", sig.ID, "err", err)
			}
			logger.Info("signal expired", "id", sig.ID, "ticker", sig.Ticker)
			continue
		}

		// Price-based staleness.
		if sig.StalePriceMovePct > 0 && sig.SignalPrice > 0 {
			latest, err := store.GetLatestCandle(ctx, sig.Ticker, data.Timeframe1Day)
			if err != nil || latest == nil {
				continue
			}
			priceDelta := math.Abs(latest.Close-sig.SignalPrice) / sig.SignalPrice * 100
			if priceDelta > sig.StalePriceMovePct {
				reason := fmt.Sprintf("price moved %.1f%% from signal (threshold %.1f%%)", priceDelta, sig.StalePriceMovePct)
				if err := store.UpdateSignalStatus(ctx, sig.ID, "stale", reason); err != nil {
					logger.Warn("stale signal", "id", sig.ID, "err", err)
				}
				logger.Info("signal stale", "id", sig.ID, "ticker", sig.Ticker, "move_pct", priceDelta)
			}
		}
	}
}

// ─── Phase 4: Data retention (4.11) ──────────────────────────────────────────

func (e *Engine) runRetentionLoop(ctx context.Context) {
	// Run once on start, then daily.
	e.runRetention(ctx)

	ticker := time.NewTicker(retentionInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			e.runRetention(ctx)
		case <-ctx.Done():
			return
		}
	}
}

func (e *Engine) runRetention(ctx context.Context) {
	n1, err1 := store.CleanupIntradayCandles(ctx)
	n2, err2 := store.CleanupPromptCache(ctx)
	if err1 != nil {
		logger.Warn("retention: intraday cleanup failed", "err", err1)
	}
	if err2 != nil {
		logger.Warn("retention: prompt cache cleanup failed", "err", err2)
	}
	if n1 > 0 || n2 > 0 {
		logger.Info("retention complete", "intraday_candles", n1, "prompt_cache", n2)
	}
}
