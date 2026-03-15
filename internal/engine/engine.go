// Package engine contains the core trading engine loop, scheduler, and pipeline
// integration. It orchestrates market data fetching, indicator computation,
// news retrieval, and (in later phases) analysis and order execution.
package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/romeo-folie/trade-engine/internal/analysis"
	"github.com/romeo-folie/trade-engine/internal/analysis/cache"
	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/cost"
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
	// maxTickIntervalSec is the upper bound for TickIntervalSec before the
	// time.Duration conversion overflows. Values above ~9.2×10⁹ seconds would
	// wrap to a negative duration.
	maxTickIntervalSec = math.MaxInt64 / int64(time.Second)

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

	// tickerState holds the last-seen indicator snapshot per ticker.
	// Used by the material-change gate (Epic 1 cost reduction).
	tickerState   map[string]*TickerState
	tickerStateMu sync.Mutex

	// batchFlusher is called at the end of each tick to flush the Telegram batch.
	batchFlusher BatchFlusher

	// costTracker accumulates per-model token spend and lifecycle counters.
	// Populated from AnalyzeFull results on every tick. Used for cost telemetry
	// and to feed the budget guardrail's period checks.
	costTracker *cost.Tracker

	// costGuardrail checks token spend against configured budgets and triggers
	// auto-degrade when thresholds are crossed. Check() is called once per tick.
	costGuardrail *cost.Guardrail

	// activeCostProfile tracks the profile most recently applied to e.cfg.Cost.
	// When the guardrail degrades the profile, this field drives the re-apply.
	activeCostProfile string

	modeMu sync.RWMutex
	mode   string // auto | paper | manual
}

// BatchFlusher is implemented by notify.TelegramBot when batch mode is enabled.
type BatchFlusher interface {
	FlushBatch()
}

// New creates a new Engine instance with the given providers.
// mode is the initial trading mode (manual | auto | paper).
// executor may be nil; if nil, the paper executor is used as fallback.
func New(cfg *config.Config, market data.MarketDataProvider, news data.NewsProvider, exec execution.Executor, mode string) *Engine {
	if exec == nil {
		exec = execution.NewPaperExecutor()
	}
	az := analysis.NewAnalyzer(cfg.Models)

	// Build an enricher call that executes extraction against a single available
	// provider (instead of full ensemble mode).
	//
	// Provider resolution order:
	//   1. models.roles.news_enricher (registry path) — allows any model to be
	//      used for extraction independent of the analyst list. This is the
	//      preferred path when a registry is configured.
	//   2. pickExtractionProvider (legacy path) — scans analysts for a provider
	//      matching markets.news.extraction_provider / extraction_model. Used
	//      when no registry role is configured.
	//
	// Why raw response: extraction prompts return a JSON array, which does not
	// match AnalysisResponse. Provider analyzers may return a parse error while
	// still exposing the raw model text in meta.RawResponse. We treat that as
	// success and only fail when no raw output is returned.
	enricherProvider := resolveEnricherProvider(az, cfg)
	callFn := knowledge.ExtractionCallFn(func(ctx context.Context, system, user string) (string, error) {
		req := analysis.AnalysisRequest{
			Ticker:             "__extraction__",
			CustomSystemPrompt: system,
			CustomUserMessage:  user,
		}

		provider := enricherProvider
		if provider == nil || !provider.IsAvailable() {
			// Report the actual provider identity when available so logs reflect
			// what was tried. Fall back to legacy config fields when provider is nil.
			if provider != nil {
				return "", fmt.Errorf("enricher: extraction provider unavailable (provider=%q model=%q)",
					provider.ProviderName(), provider.ModelName())
			}
			return "", fmt.Errorf("enricher: extraction provider unavailable (provider=%q model=%q)",
				cfg.Markets.News.ExtractionProvider, cfg.Markets.News.ExtractionModel)
		}
		_, meta, err := provider.Analyze(ctx, req)
		if meta.RawResponse != "" {
			return meta.RawResponse, nil
		}
		if err != nil {
			return "", fmt.Errorf("enricher: %s/%s: %w", provider.ProviderName(), provider.ModelName(), err)
		}
		return "", fmt.Errorf("enricher: %s/%s: empty model response", provider.ProviderName(), provider.ModelName())
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

	tracker := cost.NewTracker()
	guardrail := cost.NewGuardrail(&cfg.Cost, tracker, nil) // notifier set later via SetCostNotifier

	return &Engine{
		cfg:               cfg,
		marketData:        market,
		analyzer:          az,
		newsProvider:      news,
		executor:          exec,
		portfolio:         portfolio.NewTracker(time.Hour),
		bus:               events.New(),
		enricher:          knowledge.NewEnricher(cfg, callFn),
		embedder:          emb,
		newsLastFetched:   make(map[string]time.Time),
		tickerState:       make(map[string]*TickerState),
		mode:              mode,
		costTracker:       tracker,
		costGuardrail:     guardrail,
		activeCostProfile: cfg.Cost.Profile,
	}
}

// Run starts the engine loop. It blocks until ctx is cancelled.
// On each tick it processes all active watchlist items in parallel.
// Background goroutines handle signal expiry and data retention.
func (e *Engine) Run(ctx context.Context) error {
	if e.cfg.Engine.TickIntervalSec <= 0 {
		return fmt.Errorf("engine: tick_interval_sec must be > 0, got %d", e.cfg.Engine.TickIntervalSec)
	}
	// Guard against time.Duration overflow: time.Duration is int64 nanoseconds.
	// MaxInt64 / 1e9 ≈ 9.2 × 10^9 seconds (≈292 years) — anything beyond that
	// wraps to a negative duration and breaks scheduling.
	if int64(e.cfg.Engine.TickIntervalSec) > maxTickIntervalSec {
		return fmt.Errorf("engine: tick_interval_sec %d overflows time.Duration (max %d)", e.cfg.Engine.TickIntervalSec, maxTickIntervalSec)
	}
	interval := time.Duration(e.cfg.Engine.TickIntervalSec) * time.Second

	// Background: signal expiry + staleness checks.
	go e.runSignalExpiryLoop(ctx)
	// Background: daily data retention cleanup.
	go e.runRetentionLoop(ctx)
	// Background: analysis cache sweep — deletes rows whose expires_at has passed.
	// The sweep interval is a fixed 15-minute cadence (how often the cleanup job
	// runs), NOT the cache TTL itself. Entry expiry is determined by the
	// expires_at column set at insert time. The fixed cadence is intentionally
	// independent of CacheTTLMin to avoid coupling sweep frequency to
	// profile-driven config changes.
	// Only start the cache sweeper when the database is connected.
	// cmd/te/run.go can start the engine without a DB (e.g. dry-run mode);
	// the sweeper calls store.DeleteExpiredCachedAnalyses which panics on
	// an uninitialised pool.
	if store.IsConnected() {
		const cacheSweepInterval = 15 * time.Minute
		go cache.NewSweeper(cacheSweepInterval).Run(ctx)
	}

	logger.Info("engine loop started", "interval_sec", e.cfg.Engine.TickIntervalSec, "mode", e.mode)

	// Startup tick: only run if the market is currently open.
	// Prior behaviour ran unconditionally, causing a full analysis cycle
	// on weekends, holidays, and outside trading hours the moment te run
	// was invoked.
	// Capture a single consistent timestamp for the startup market check and
	// for scheduling the first timer. Multiple time.Now() calls here could
	// flip across a session boundary (e.g. 15:59:59 → 16:00:00), causing
	// startedTick and the base time passed to nextEngineWake to disagree.
	// Capture the market-open decision and run (or skip) the startup tick.
	// startupNow is used only for the IsMarketOpen check; the timer base is
	// captured *after* the tick completes (see timerBase below) so that a
	// long-running startup tick doesn't produce a negative time.Until and
	// cause an immediate back-to-back fire.
	startupNow := time.Now()

	// Cost guardrail: roll any period boundaries that passed since last run
	// and check budgets before the first tick fires.
	e.costGuardrail.Check(ctx, startupNow)
	e.applyDegradedProfile(&interval)

	ranStartupTick := false
	if IsMarketOpen(startupNow) {
		ranStartupTick = true
		if err := e.tick(ctx); err != nil {
			logger.Warn("tick error", "err", err.Error())
		}
	} else {
		nextOpen := nextMarketOpen(startupNow)
		logger.Info("market closed on startup — skipping initial tick",
			"next_open_at", nextOpen.In(nyLoc).Format(time.RFC3339),
		)
	}

	// Use a one-shot timer instead of a fixed ticker so the engine sleeps
	// until the actual next trading time rather than waking every interval
	// all weekend to log "market closed" and immediately go back to sleep.
	//
	// nextEngineWake distinguishes two cases:
	//   ranTick=true  → interval from now (NextTickTime); routes to nextMarketOpen
	//                    if now+interval falls outside market hours.
	//   ranTick=false → nextMarketOpen(now) directly, so a pre-market or weekend
	//                    startup reliably wakes at the opening bell regardless of
	//                    how large the tick interval is.
	//
	// timerBase is captured after the startup work completes so the first
	// interval is measured from when the engine finished, not when it started.
	//
	// However, if we skipped the tick (ranStartupTick=false), we MUST use the
	// same startupNow timestamp for scheduling. If we used a fresh time.Now()
	// that crossed the 09:30 boundary, nextMarketOpen would jump to tomorrow,
	// missing the entire today's session.
	timerBase := time.Now()
	base := timerBase
	if !ranStartupTick {
		base = startupNow
	}
	nextTick := nextEngineWake(base, interval, ranStartupTick)
	e.logNextCycle(nextTick)
	timer := time.NewTimer(time.Until(nextTick))
	defer timer.Stop()

	for {
		select {
		case <-timer.C:
			// Capture a single timestamp for the open/closed decision.
			now := time.Now()

			// Cost guardrail: roll period boundaries BEFORE the tick so
			// counters are fresh. Run outside the IsMarketOpen gate so
			// period boundaries roll even on days when the market is closed.
			e.costGuardrail.Check(ctx, now)
			e.applyDegradedProfile(&interval)

			ranTick := false
			if IsMarketOpen(now) {
				ranTick = true
				if err := e.tick(ctx); err != nil {
					logger.Warn("tick error", "err", err.Error())
				}
			} else {
				// Defensive guard for clock skew / DST transitions.
				logger.Info("market closed — skipping tick (clock skew?)")
			}

			// Post-tick guardrail check: the tick just recorded token usage,
			// so re-check budgets BEFORE scheduling the next wake. This ensures
			// a profile degrade applies immediately to the next interval rather
			// than executing one extra tick at the higher-cost profile.
			postTickNow := time.Now()
			e.costGuardrail.Check(ctx, postTickNow)
			e.applyDegradedProfile(&interval)

			// Capture base after work completes to avoid negative time.Until.
			// Same boundary logic as startup: if we skipped, use the decision
			// timestamp 'now' to avoid skipping a whole session on a 1ms flip.
			loopBase := postTickNow
			base := loopBase
			if !ranTick {
				base = now
			}
			nextTick = nextEngineWake(base, interval, ranTick)
			e.logNextCycle(nextTick)
			// Safe to call Reset directly: timer.C was just consumed above.
			timer.Reset(time.Until(nextTick))
		case <-ctx.Done():
			logger.Info("engine loop stopping")
			return nil
		}
	}
}

// nextEngineWake returns when the engine loop should next wake up.
//
//   - ranTick=true:  engine ran a tick; next wake = now+interval (routed to
//     nextMarketOpen if that interval lands outside market hours).
//   - ranTick=false: engine skipped (pre-market, weekend, holiday); next wake =
//     nextMarketOpen(now) so the opening bell is never missed regardless of
//     how large the tick interval is.
func nextEngineWake(now time.Time, interval time.Duration, ranTick bool) time.Time {
	if ranTick {
		return NextTickTime(now, interval)
	}
	return nextMarketOpen(now)
}

// logNextCycle logs the next scheduled tick time and the seconds until it fires.
// Uses the actual computed next tick (which may be next market open) rather than
// the fixed tick interval, so logs are accurate over weekends and holidays.
// applyDegradedProfile checks if the guardrail has changed the active profile
// and, if so, applies the new profile config and recomputes the tick interval.
// interval is updated in-place via pointer. If the degraded profile produces an
// invalid TickIntervalSec, a warning is logged and the previous interval is kept.
func (e *Engine) applyDegradedProfile(interval *time.Duration) {
	active := e.costGuardrail.ActiveProfile()
	if active == e.activeCostProfile {
		return
	}
	e.cfg.Cost.Profile = active
	// Snapshot fields that ApplyProfile mutates but the engine cannot
	// re-wire at runtime (e.g. Telegram batch settings, which are
	// configured once at startup in cmd/te/run.go).
	savedBatchEnabled := e.cfg.Notifications.Telegram.BatchEnabled
	savedBatchWindowSec := e.cfg.Notifications.Telegram.BatchWindowSec
	savedMaxItemsPerBatch := e.cfg.Notifications.Telegram.MaxItemsPerBatch
	config.ApplyProfile(e.cfg)
	// Restore un-reactable fields to avoid cfg/runtime inconsistency.
	e.cfg.Notifications.Telegram.BatchEnabled = savedBatchEnabled
	e.cfg.Notifications.Telegram.BatchWindowSec = savedBatchWindowSec
	e.cfg.Notifications.Telegram.MaxItemsPerBatch = savedMaxItemsPerBatch
	e.activeCostProfile = active
	logger.Info("cost profile degraded — config updated", "profile", active)

	newTIS := e.cfg.Engine.TickIntervalSec
	if newTIS <= 0 || int64(newTIS) > maxTickIntervalSec {
		logger.Warn("degraded profile produced invalid tick_interval_sec — keeping previous interval",
			"tick_interval_sec", newTIS, "previous_interval_sec", int(interval.Seconds()))
	} else {
		*interval = time.Duration(newTIS) * time.Second
		logger.Info("tick interval updated after profile degrade", "interval_sec", newTIS)
	}
}

func (e *Engine) logNextCycle(nextTick time.Time) {
	wait := time.Until(nextTick)
	logger.Info("engine cycle complete",
		"next_cycle_in_sec", int(wait.Seconds()),
		"next_cycle_at", nextTick.In(nyLoc).Format(time.RFC3339),
	)
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
		go func(ticker string, timeframes, strategies []string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			e.processStock(ctx, ticker, timeframes, strategies)
		}(item.Ticker, item.Timeframes, item.Strategies)
	}

	wg.Wait()

	// Monitor open positions for stop/TP breaches after all tickers processed.
	e.monitorPositions(ctx)

	// Flush Telegram batch at the end of each tick (sends per-tick summary).
	// Runs in a goroutine so Telegram retries/sleeps don't block the engine loop.
	// Note: bus event delivery is asynchronous; events published late in the tick
	// may arrive after FlushBatch() runs and appear in the next batch. A
	// deterministic per-tick fence would require an explicit "tick done" event.
	if e.batchFlusher != nil {
		flusher := e.batchFlusher
		go flusher.FlushBatch()
	}

	return nil
}

// processStock fetches candles, computes indicators, and fetches news for a single stock.
// strategies is the ticker's configured strategy list (from the watchlist item).
func (e *Engine) processStock(ctx context.Context, ticker string, timeframes, strategies []string) {
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

	// Determine the ticker's primary strategy for prompt-context capping.
	// NOTE: HistoryWindowByStrategy caps how many candles reach the prompt
	// (applied in runAnalysis), NOT the DB fetch window. We always fetch 2 years
	// to ensure indicators like EMA200/ADX have sufficient warmup history.
	primaryStrategy := "trend_following"
	if len(strategies) > 0 {
		primaryStrategy = strategies[0]
	}

	historyFrom := time.Now().AddDate(-2, 0, 0)
	candles, err := store.GetCandles(ctx, ticker, primaryTF,
		historyFrom, time.Now())
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

	// Material-change gate: skip LLM analysis when nothing has changed.
	// Only query news when news_novelty_enabled — avoids a per-ticker DB round-trip
	// when the feature is disabled.
	var newsItems []data.NewsItem
	var newsHash string
	if e.cfg.Analysis.NewsNoveltyEnabled {
		var newsErr error
		newsItems, newsErr = store.GetRecentNews(ctx, ticker, time.Now().Add(-48*time.Hour))
		if newsErr != nil {
			// On error treat as "news unknown" — unique hash prevents wrongly skipping.
			logger.Warn("news query failed — treating as changed for material-change gate",
				"ticker", ticker, "err", newsErr.Error())
			newsHash = fmt.Sprintf("error:%d", time.Now().UnixNano())
		} else {
			headlines := make([]string, 0, len(newsItems))
			for _, n := range newsItems {
				headlines = append(headlines, n.Headline)
			}
			newsHash = NewsDigestHash(headlines)
		}
	}

	e.tickerStateMu.Lock()
	prev := e.tickerState[ticker]
	e.tickerStateMu.Unlock()

	latestClose := 0.0
	if len(candles) > 0 {
		latestClose = candles[len(candles)-1].Close
	}
	mcIn := MaterialChangeInput{
		RSI:      set.RSI,
		EMA20:    set.EMA20,
		Price:    latestClose,
		NewsHash: newsHash,
		Now:      time.Now(),
	}

	skip, skipReason, deltas := CheckMaterialChange(prev, mcIn, e.cfg.Analysis)
	if skip {
		msg := "analysis skipped — no material change"
		if skipReason == SkipReasonHoldCooldown {
			msg = "analysis skipped — HOLD cooldown active"
		}
		logger.Info(msg,
			"ticker", ticker,
			"reason", string(skipReason),
			"rsi_delta", fmt.Sprintf("%.2f", deltas["rsi_delta"]),
			"ema_gap_delta_pct", fmt.Sprintf("%.2f", deltas["ema_gap_delta_pct"]),
			"price_move_pct", fmt.Sprintf("%.2f", deltas["price_move_pct"]),
			"hold_elapsed_min", fmt.Sprintf("%.1f", deltas["hold_elapsed_min"]),
			"hold_cooldown_min", fmt.Sprintf("%.0f", deltas["hold_cooldown_min"]),
		)
		return
	}

	// Run LLM analysis — PRD §8. Errors are logged but non-fatal.
	e.runAnalysis(ctx, ticker, string(primaryTF), primaryStrategy, candles, set, newsHash)
}

// runAnalysis builds the analysis request, calls the configured model, stores
// the result, and logs the recommendation summary.
func (e *Engine) runAnalysis(ctx context.Context, ticker, timeframe, strategy string, candles []data.Candle, set *indicators.IndicatorSet, newsHash string) {
	if set == nil {
		return
	}
	if e.analyzer == nil || !e.analyzer.Primary().IsAvailable() {
		logger.Debug("analysis skipped — primary model not available", "ticker", ticker)
		return
	}

	if strategy == "" {
		strategy = "trend_following"
	}

	// Build candle snapshots (most-recent-first for prompt context).
	// Precedence: strategy window sets the cap (strategy-specific tuning);
	// MaxCandlesPerPrompt acts as an absolute hard upper bound.
	// The DB fetch always covers 2 years for indicator warmup; these caps
	// only limit what reaches the prompt to bound token usage.
	candleCap := e.cfg.Analysis.MaxCandlesPerPrompt
	if candleCap <= 0 {
		candleCap = 20
	}
	if windows := e.cfg.Analysis.HistoryWindowByStrategy; len(windows) > 0 {
		if n, ok := windows[strategy]; ok && n > 0 {
			candleCap = n // strategy window overrides; MaxCandlesPerPrompt clamps below
		}
	}
	if max := e.cfg.Analysis.MaxCandlesPerPrompt; max > 0 && candleCap > max {
		candleCap = max // hard upper bound
	}
	limit := len(candles)
	if limit > candleCap {
		limit = candleCap
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
	// Note: newsItems also fetched by processStock for the material-change gate;
	// this second query is a lightweight store read (no API call).
	recentNews, _ := store.GetRecentNews(ctx, ticker, time.Now().Add(-48*time.Hour))
	// Cap news articles to MaxNewsArticlesPerPrompt — newest first, excess dropped.
	newsCap := e.cfg.Analysis.MaxNewsArticlesPerPrompt
	if newsCap <= 0 {
		newsCap = 5
	}
	snippets := make([]analysis.NewsSnippet, 0, newsCap)
	for i, n := range recentNews {
		if i >= newsCap {
			break
		}
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
		// Use the same compact indicator format as the worker uses at index time
		// so search query and stored vectors occupy the same embedding space.
		searchText := embeddings.AnalysisEmbedText(ticker, set.RSI, set.EMA20, set.MACDLine, set.ADX)
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

	result, err := e.analyzer.AnalyzeFull(ctx, req)

	// Record token usage for cost tracking (#177) — BEFORE checking err.
	// AnalyzeFull can return a non-nil result alongside a non-nil error (e.g.
	// JSON extraction failures in single-analyst mode). Providers consume tokens
	// before erroring, so skipping recording on error would undercount spend and
	// delay guardrail enforcement.
	//
	// In ensemble mode each analyst result carries independent token counts;
	// in single-model mode the primary meta carries all tokens.
	// Guard: only record when provider is non-empty and at least one token count
	// is positive, to avoid creating noisy "/" model keys or zero-token entries.
	{
		// Prefer AnalystResults (covers both ensemble and single-analyst partial
		// results). Only fall back to result.Meta when AnalystResults is empty
		// (pure single-model path). This ensures a single partial ensemble result
		// (len==1, e.g. when only one analyst is available) still gets recorded
		// rather than falling into the Meta branch which is unpopulated in
		// ensemble mode.
		if len(result.AnalystResults) > 0 {
			for _, ar := range result.AnalystResults {
				if ar.Meta.Provider != "" && ar.Meta.Model != "" && (ar.Meta.InputTokens > 0 || ar.Meta.OutputTokens > 0) {
					e.costTracker.Record(ar.Meta.Provider, ar.Meta.Model, ar.Meta.InputTokens, ar.Meta.OutputTokens)
				}
			}
			if result.Arbiter != nil && result.Arbiter.Meta.Provider != "" && result.Arbiter.Meta.Model != "" &&
				(result.Arbiter.Meta.InputTokens > 0 || result.Arbiter.Meta.OutputTokens > 0) {
				e.costTracker.Record(result.Arbiter.Meta.Provider, result.Arbiter.Meta.Model,
					result.Arbiter.Meta.InputTokens, result.Arbiter.Meta.OutputTokens)
			}
		} else if result.Meta.Provider != "" && result.Meta.Model != "" && (result.Meta.InputTokens > 0 || result.Meta.OutputTokens > 0) {
			e.costTracker.Record(result.Meta.Provider, result.Meta.Model,
				result.Meta.InputTokens, result.Meta.OutputTokens)
		}
	}

	if err != nil {
		logger.Warn("analysis failed", "ticker", ticker, "err", err.Error())
		return
	}
	resp, meta := result.Response, result.Meta

	reasoningPreview := resp.Reasoning[:min(120, len(resp.Reasoning))]
	logger.Info("analysis complete",
		"ticker", ticker,
		"action", string(resp.Action),
		"confidence", fmt.Sprintf("%.2f", resp.Confidence),
		"analysts", len(result.AnalystResults),
		"fast_pathed", result.FastPathed,
		"reasoning_preview", reasoningPreview,
		"reasoning_len", len(resp.Reasoning),
		"reasoning_truncated", len(resp.Reasoning) > len(reasoningPreview),
	)
	e.bus.Publish(events.AnalysisCompleted, events.AnalysisCompletedPayload{
		Ticker: ticker, Action: string(resp.Action), Confidence: resp.Confidence,
		Reasoning: resp.Reasoning, Provider: meta.Provider, Model: meta.Model, LatencyMs: meta.LatencyMs,
	})

	// Persist — use ensemble variant when multiple analysts were queried.
	var analysisID string
	if len(result.AnalystResults) > 1 {
		ensembleJSON, _ := json.Marshal(result.AnalystResults)
		var arbJSON []byte
		var dissentSummary string
		if result.Arbiter != nil {
			arbJSON, _ = json.Marshal(result.Arbiter)
			dissentSummary = result.Arbiter.DissentSummary
		}
		analysisID, err = store.InsertEnsembleAnalysis(ctx, ticker, strategy, resp, meta, store.EnsembleRecord{
			EnsembleResults: ensembleJSON,
			ArbiterResponse: arbJSON,
			FastPathed:      result.FastPathed,
			DissentSummary:  dissentSummary,
		})
	} else {
		analysisID, err = store.InsertAnalysis(ctx, ticker, strategy, resp, meta)
	}
	if err != nil {
		logger.Warn("store analysis", "ticker", ticker, "err", err.Error())
	}

	// Phase 4: generate and potentially execute a signal from this analysis.
	e.generateSignal(ctx, ticker, strategy, candles, resp, analysisID)

	// Update material-change gate state for next tick.
	latestClose := 0.0
	if len(candles) > 0 {
		latestClose = candles[len(candles)-1].Close
	}
	e.tickerStateMu.Lock()
	e.tickerState[ticker] = &TickerState{
		LastRSI:        set.RSI,
		LastEMAGapPct:  emaGapPct(latestClose, set.EMA20),
		LastPrice:      latestClose,
		LastNewsHash:   newsHash,
		LastDecision:   string(resp.Action),
		LastAnalysedAt: time.Now(),
	}
	e.tickerStateMu.Unlock()
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// resolveEnricherProvider returns the ModelProvider to use for knowledge
// enrichment/extraction calls.
//
// Registry path (preferred): if models.roles.news_enricher is set, resolve it
// from the registry and build a standalone provider. This decouples extraction
// from the analyst list — any model can be used regardless of whether it is
// also configured as an analyst.
//
// Legacy fallback: scan analysts for a provider matching
// markets.news.extraction_provider / extraction_model.
func resolveEnricherProvider(az *analysis.Analyzer, cfg *config.Config) analysis.ModelProvider {
	if mc, err := cfg.Models.ResolveRole("news_enricher"); err == nil && mc.Provider != "" {
		p := analysis.NewProviderFromConfig(mc)
		if p.IsAvailable() {
			return p
		}
		logger.Warn("news_enricher registry role resolved but provider unavailable — falling back to analyst scan",
			"provider", mc.Provider, "model", mc.Model)
	}
	return pickExtractionProvider(az, cfg)
}

// pickExtractionProvider scans the analyst providers for one matching the
// legacy markets.news.extraction_provider / extraction_model fields.
// Used as a fallback when no models.roles.news_enricher is configured.
func pickExtractionProvider(az *analysis.Analyzer, cfg *config.Config) analysis.ModelProvider {
	providers := az.Providers()
	if len(providers) == 0 {
		return nil
	}
	wantProvider := cfg.Markets.News.ExtractionProvider
	wantModel := cfg.Markets.News.ExtractionModel
	for _, p := range providers {
		if p == nil {
			continue
		}
		if wantProvider != "" && p.ProviderName() != wantProvider {
			continue
		}
		if wantModel != "" && p.ModelName() != wantModel {
			continue
		}
		if p.IsAvailable() {
			return p
		}
	}

	// If extraction provider/model was explicitly configured, do NOT silently
	// fall back to another provider — that can reintroduce surprise cost spikes.
	if wantProvider != "" || wantModel != "" {
		return nil
	}

	for _, p := range providers {
		if p != nil && p.IsAvailable() {
			return p
		}
	}
	return nil
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

	if e.enricher == nil || !e.cfg.Markets.News.ExtractionEnabled {
		return
	}
	maxItems := e.cfg.Markets.News.ExtractionMaxArticlesPerTick
	if maxItems <= 0 || maxItems > len(items) {
		maxItems = len(items)
	}
	deferred := len(items) - maxItems
	if deferred > 0 {
		logger.Info("news extraction deferred", "ticker", ticker, "scheduled", maxItems, "deferred", deferred)
	}
	for i := 0; i < maxItems; i++ {
		item := items[i]
		e.enricher.EnrichNewsItem(ctx, "", item.Headline, item.Summary)
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

// SetBatchFlusher registers a BatchFlusher (e.g. TelegramBot) to be called
// at the end of each tick. Called from cmd/te/run.go after bot setup.
func (e *Engine) SetBatchFlusher(f BatchFlusher) {
	e.batchFlusher = f
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

// CostTracker returns the engine's token-spend tracker for external reads (TUI, reports).
func (e *Engine) CostTracker() *cost.Tracker { return e.costTracker }

// SetCostNotifier wires a DegradeNotifier into the cost guardrail so that
// auto-degrade alerts are delivered immediately (bypassing batch queues).
// Call this after the notifier is initialised — typically after TelegramBot.Start.
func (e *Engine) SetCostNotifier(n cost.DegradeNotifier) {
	e.costGuardrail.SetNotifier(n)
}

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

	// Cost tracking: a live signal was generated (#177).
	e.costTracker.RecordSignal()

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
		reason := fmt.Sprintf("zero quantity (cash=%.2f size_pct=%.2f signal_price=%.4f)", pState.Cash, sig.SizePct, sig.SignalPrice)
		if err := store.UpdateSignalStatus(ctx, sigID, "blocked", reason); err != nil {
			logger.Warn("update signal status", "id", sigID, "err", err)
		}
		logger.Warn("signal execution skipped — zero quantity",
			"ticker", sig.Ticker,
			"cash", pState.Cash,
			"size_pct", sig.SizePct,
			"signal_price", sig.SignalPrice,
		)
		e.bus.Publish(events.SignalBlocked, events.SignalBlockedPayload{
			Ticker: sig.Ticker, Action: sig.Action, Confidence: sig.Confidence,
			BlockReason: reason, FailedCheck: "position sizing",
		})
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

	// Cost tracking: a trade was executed (#177).
	e.costTracker.RecordTrade()

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
