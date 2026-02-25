// Package engine contains the core trading engine loop, scheduler, and pipeline
// integration. It orchestrates market data fetching, indicator computation,
// news retrieval, and (in later phases) analysis and order execution.
package engine

import (
	"context"
	"fmt"
	"sync"
	"time"

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
// fetches news, and (in later phases) runs analysis and executes orders.
type Engine struct {
	cfg          *config.Config
	marketData   data.MarketDataProvider
	newsProvider data.NewsProvider

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
	primaryTF := data.Timeframe1Day
	for _, tf := range timeframes {
		if tf == string(data.Timeframe1Day) {
			primaryTF = data.Timeframe1Day
			break
		}
		primaryTF = data.Timeframe(timeframes[0])
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
