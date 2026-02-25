// Package engine contains the core trading engine loop, scheduler, and pipeline
// integration. It orchestrates market data fetching, indicator computation,
// news retrieval, and (in later phases) analysis and order execution.
package engine

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/romeo-folie/trade-engine/internal/analysis"
	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/data"
	"github.com/romeo-folie/trade-engine/internal/indicators"
	"github.com/romeo-folie/trade-engine/internal/logger"
	"github.com/romeo-folie/trade-engine/internal/store"
)

const (
	// historyCandles is the number of candles fetched per watchlist item per tick.
	// 200 daily bars covers ~10 months; enough for EMA200 and ADX warmup.
	historyCandles = 200

	// newsFetchInterval is the minimum time between news fetches for the same ticker.
	// News is expensive to fetch; 15 minutes between updates is the default cadence.
	newsFetchInterval = 15 * time.Minute
)

// Engine is the core trading loop. It polls market data, computes indicators,
// fetches news, and dispatches to the analysis engine on each tick.
type Engine struct {
	cfg          *config.Config
	marketData   data.MarketDataProvider
	newsProvider data.NewsProvider
	analyzer     *analysis.Analyzer

	// newsLastFetched tracks when news was last fetched per ticker, to avoid
	// hammering the news API on every 5-min tick.
	newsLastFetched map[string]time.Time
	newsMu          sync.Mutex

	mode string
}

// New creates a new Engine instance with the given providers.
// mode is the initial trading mode (manual | auto | paper).
func New(cfg *config.Config, market data.MarketDataProvider, news data.NewsProvider, mode string) *Engine {
	return &Engine{
		cfg:             cfg,
		marketData:      market,
		analyzer:        analysis.NewAnalyzer(cfg.Models),
		newsProvider:    news,
		newsLastFetched: make(map[string]time.Time),
		mode:            mode,
	}
}

// Run starts the engine loop. It blocks until ctx is cancelled.
// On each tick it processes all active watchlist items in parallel.
func (e *Engine) Run(ctx context.Context) error {
	interval := time.Duration(e.cfg.Engine.TickIntervalSec) * time.Second
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

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

	req := analysis.AnalysisRequest{
		Ticker:        ticker,
		Strategy:      strategy,
		Timeframe:     timeframe,
		RecentCandles: snaps,
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

	if err := store.InsertAnalysis(ctx, ticker, strategy, resp, meta); err != nil {
		logger.Warn("store analysis", "ticker", ticker, "err", err.Error())
	}
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
	e.mode = mode
	logger.Info("engine mode updated", "mode", mode)
}
