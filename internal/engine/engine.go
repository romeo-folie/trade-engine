// Package engine contains the core trading engine loop, scheduler, and pipeline
// integration. It orchestrates market data fetching, indicator computation,
// news retrieval, and (in later phases) analysis and order execution.
package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/romeo-folie/trade-engine/internal/analysis"
	"github.com/romeo-folie/trade-engine/internal/analysis/cache"
	"github.com/romeo-folie/trade-engine/internal/analysis/depth"
	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/cost"
	"github.com/romeo-folie/trade-engine/internal/data"
	"github.com/romeo-folie/trade-engine/internal/embeddings"
	"github.com/romeo-folie/trade-engine/internal/events"
	"github.com/romeo-folie/trade-engine/internal/execution"
	"github.com/romeo-folie/trade-engine/internal/feedback"
	"github.com/romeo-folie/trade-engine/internal/indicators"
	"github.com/romeo-folie/trade-engine/internal/knowledge"
	"github.com/romeo-folie/trade-engine/internal/logger"
	"github.com/romeo-folie/trade-engine/internal/portfolio"
	"github.com/romeo-folie/trade-engine/internal/risk"
	"github.com/romeo-folie/trade-engine/internal/store"
	"github.com/romeo-folie/trade-engine/internal/strategy/pullback"
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

	// Stored candle fallbacks may be minutes or hours old; pullback execution
	// requires a current two-sided quote for stop-width and R:R validation.
	pullbackQuoteMaxAge          = 15 * time.Second
	pullbackQuoteFutureTolerance = 10 * time.Second

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
	enricher     *knowledge.Enricher // nil if no LLM configured
	embedder     embeddings.Embedder // nil if embeddings disabled

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

	// lastTickAt is the previous tick's start; sessionFirstTick is true while
	// processing the first tick of an ET trading day (or the first after a
	// restart). Both are written by tick() before the per-ticker goroutines
	// fan out and are read-only afterwards, so no lock is needed.
	lastTickAt       time.Time
	sessionFirstTick bool

	modeMu sync.RWMutex
	mode   string // auto | paper | manual

	// trailingManagerAvailable is set by command wiring only when every
	// dependency needed to run the trailing worker is present. Config alone is
	// insufficient: uncapping without a running worker removes the winning exit.
	trailingManagerAvailable bool

	// todayStartingEquity tracks the equity at 00:00 UTC for the current day.
	todayStartingEquity float64
	todaySummaryDate    string

	// lastDailySummaryUpdate tracks when updateDailySummary last ran to throttle
	// the expensive DB query + upsert cycle.
	lastDailySummaryUpdate time.Time

	// lastExitReconcile throttles the exit-fill reconciler sweep the same way.
	// exitReconcileInFlight guards against overlapping background sweeps when
	// a sweep outlives the throttle interval (slow broker API + backlog).
	lastExitReconcile     time.Time
	exitReconcileInFlight atomic.Bool

	feedbackReviewer *feedback.Reviewer

	// multiTFCandleFetcher is the on-demand fetch path used by the borderline
	// re-arbitration alt pass (Issue #321 PR #336). Watchlist-driven refresh
	// only fetches the timeframes listed on the item, so an alt-tf candle
	// store would otherwise go stale and the second pass would re-arbitrate
	// over outdated bars. This field lets the alt pass proactively refresh
	// the alt timeframe before reading. Defaults to fetchAndStoreCandles;
	// tests override it to avoid network/DB.
	multiTFCandleFetcher func(ctx context.Context, ticker string, tf data.Timeframe)
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
	bus := events.New()
	az := analysis.NewAnalyzer(cfg.Models, bus)

	// Issue #323: wire specialist sub-config when the feature flag is on so
	// AnalyzeSpecialist can resolve role→registry-key mappings at runtime.
	if cfg.Analysis.SpecialistModeEnabled {
		az.SetSpecialistConfig(&cfg.Analysis)
	}

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

	e := &Engine{
		cfg:               cfg,
		marketData:        market,
		analyzer:          az,
		newsProvider:      news,
		executor:          exec,
		portfolio:         portfolio.NewTracker(5 * time.Minute),
		bus:               bus, // use the same bus as az
		enricher:          knowledge.NewEnricher(cfg, callFn),
		embedder:          emb,
		newsLastFetched:   make(map[string]time.Time),
		tickerState:       make(map[string]*TickerState),
		mode:              mode,
		costTracker:       tracker,
		costGuardrail:     guardrail,
		activeCostProfile: cfg.Cost.Profile,
	}

	// Default the multi-tf alt-pass fetcher to the production candle fetch path.
	// Tests override this field directly to avoid network/DB. See Issue #321
	// PR #336 fix for stale alt-tf candles.
	e.multiTFCandleFetcher = e.fetchAndStoreCandles

	if cfg.Feedback.Enabled && store.IsConnected() {
		e.feedbackReviewer = feedback.NewReviewer(cfg.Feedback, bus, store.GetDB())
	}

	return e
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

	// Load display timezone for log formatting (internal scheduling remains in nyLoc).
	displayTZ := e.cfg.Engine.LogTimezone
	if displayTZ == "" {
		displayTZ = "UTC"
	}
	displayLoc, err := time.LoadLocation(displayTZ)
	if err != nil {
		displayLoc = time.UTC
		logger.Warn("invalid log_timezone, falling back to UTC", "log_timezone", displayTZ, "err", err)
	}

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

	if e.feedbackReviewer != nil {
		go func() {
			if err := e.feedbackReviewer.Run(ctx); err != nil && err != context.Canceled {
				fmt.Printf("feedback reviewer: %v\n", err)
			}
		}()
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
	var skippedLastAnalysisAt *time.Time // set when we skip due to recent analysis
	if IsMarketOpen(startupNow) {
		skipStartup := false
		if e.cfg.Engine.ShouldSkipRecentTick() {
			lastAnalysis, err := store.GetLatestAnalysisTime(ctx)
			if err == nil {
				skip, _ := startupSkipDecision(startupNow, lastAnalysis, interval)
				if skip {
					skipStartup = true
					skippedLastAnalysisAt = lastAnalysis
					logger.Info("skipping startup tick — last analysis recent",
						"last_analysis_age", startupNow.Sub(*lastAnalysis).Round(time.Second).String(),
						"interval", interval.String(),
					)
				}
			}
		}
		if !skipStartup {
			ranStartupTick = true
			if err := e.tick(ctx); err != nil {
				logger.Warn("tick error", "err", err.Error())
			}
		}
	} else {
		nextOpen := nextMarketOpen(startupNow)
		logger.Info("market closed on startup — skipping initial tick",
			"next_open_at", nextOpen.In(displayLoc).Format(time.RFC3339),
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
	//
	// Special case: if we skipped because a recent analysis exists, schedule
	// the next tick from the last analysis time so we fire at
	// lastAnalysis + interval (the remaining time) rather than waiting a full
	// interval or jumping to tomorrow's open.
	timerBase := time.Now()
	var nextTick time.Time
	if skippedLastAnalysisAt != nil {
		_, nextTick = startupSkipDecision(startupNow, skippedLastAnalysisAt, interval)
	} else {
		base := timerBase
		if !ranStartupTick {
			base = startupNow
		}
		nextTick = nextEngineWake(base, interval, ranStartupTick)
	}
	e.logNextCycle(ctx, nextTick, displayLoc)
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
			e.logNextCycle(ctx, nextTick, displayLoc)
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

func (e *Engine) logNextCycle(ctx context.Context, nextTick time.Time, displayLoc *time.Location) {
	wait := time.Until(nextTick)
	logger.Info("engine cycle complete",
		"next_cycle_in_sec", int(wait.Seconds()),
		"next_cycle_at", nextTick.In(displayLoc).Format(time.RFC3339),
	)

	// Publish the next-cycle time to a shared store so cross-process readers
	// (the in-process web server and the out-of-process TUI) can surface it.
	// Best-effort: a failure here must never disrupt scheduling.
	if store.IsConnected() {
		wctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		if err := store.SetEngineState(wctx, store.EngineStateNextCycleAt, nextTick.UTC().Format(time.RFC3339)); err != nil {
			logger.Warn("failed to persist next_cycle_at", "err", err.Error())
		}
	}
}

// tick processes all active watchlist items for a single engine cycle.
// Items are processed in parallel; errors per item are logged but do not abort the tick.
func (e *Engine) tick(ctx context.Context) error {
	// Signal the start of a new tick cycle. The frontend uses this as a boundary
	// to reset pipeline node card state (event counts, verdicts) before new data
	// arrives. Must fire before any other events in the tick.
	e.bus.Publish(events.TickStarted, events.TickStartedPayload{})

	// Refresh portfolio state before processing any signals so that executeSignal
	// has an accurate cash balance from the very first tick (not the zero-value
	// default from NewTracker).
	e.refreshPortfolio(ctx)

	// Entry gate: first-tick deferral (entry_gates.go). Mark whether this is
	// the ET session's first tick (or the first after a restart) BEFORE the
	// per-ticker goroutines fan out — they read the flag when routing BUYs.
	// Then give BUYs deferred by the previous tick their price-hold
	// confirmation before fresh analysis competes for the same capital.
	tickNow := time.Now()
	e.sessionFirstTick = e.lastTickAt.IsZero() || !sameMarketDay(e.lastTickAt, tickNow, nyLoc)
	e.lastTickAt = tickNow
	e.processDeferredSignals(ctx)

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
		go func(item store.WatchlistItem) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			e.processStock(ctx, item.Ticker, item.Timeframes, item.Strategies, item.Source, item.MinConfidence, item.GraphContext)
		}(item)
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
// scannerConviction carries the scanner's conviction score (0.0–1.0) for
// scanner-sourced tickers; 0 for manual/config sources. Used by the
// scanner-only RSI-delta loosen (#317).
// graphContext, when non-empty, is the persisted origin thesis for a
// scanner-sourced candidate (graph link / news-sympathy trigger, migration
// 038); it is prepended to the knowledge-graph prompt section so the trading
// analysts see WHY the ticker is on the watchlist.
func (e *Engine) processStock(ctx context.Context, ticker string, timeframes, strategies []string, source string, scannerConviction float64, graphContext string) {
	requestedTimeframes := stockTimeframesForEntry(timeframes, false)
	// The complete pullback setup is defined on daily history even when a
	// scanner candidate uses 1H/4H for its analyst prompt. Fetch 1D explicitly
	// so the execution gate never silently fails open because of watchlist
	// timeframe selection.
	entryGates := e.cfg.Risk.EntryGates
	needsDailyEvidence := entryGates.PullbackStrengthEnabled ||
		entryGates.TrendAlignmentEnabled
	fetchTimeframes := stockTimeframesForEntry(timeframes, needsDailyEvidence)

	for _, tfStr := range fetchTimeframes {
		tf := data.Timeframe(tfStr)
		e.fetchAndStoreCandles(ctx, ticker, tf)
	}

	// Compute indicators on the primary timeframe (prefer "1d", fall back to first).
	// Only requested timeframes participate: an evidence-only daily fetch must
	// not silently change a scanner item's 1H/4H analyst prompt to daily.
	primaryTF := primaryStockTimeframe(requestedTimeframes)

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

	mcCfg := applyScannerOverrides(e.cfg.Analysis, source, scannerConviction)
	skip, skipReason, deltas := CheckMaterialChange(prev, mcIn, mcCfg)
	// Scanner tickers that previously returned WAIT must be re-evaluated next
	// cycle regardless of indicator movement — WAIT means "check again soon".
	if skip && skipReason == SkipReasonNoChange && source == "scanner" && prev != nil && prev.LastDecision == "WAIT" {
		skip = false
	}
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
	e.runAnalysis(ctx, ticker, string(primaryTF), primaryStrategy, candles, set, newsHash, source, graphContext)
}

// buildPortfolioContext maps live portfolio state into the analysis request so
// analysts see cash and open positions (closes the "Phase 4" gap: 24% of all
// blocked signals were SELLs on tickers we did not hold, and another 27% hit
// per-ticker/pyramid/max-position limits — all invisible to a portfolio-blind
// analyst). Pure function for testability. Downstream, toPromptPortfolio pins
// the analyzed ticker + caps the list, and the prompt renderer buckets the
// volatile figures so analysis_cache stays effective.
//
// openTrades is the store fallback for when the tracker has no positions: the
// paper executor's GetPositions returns nil (Codex #392 P1), so in paper mode
// the tracker is empty while real open trades sit in the trades table — and
// the PortfolioRules SELL guard would then suppress valid analyst exits.
// Trades aggregate per (ticker, direction) with a weighted-average entry (the
// same "distinct position" notion as the risk counter, PR #330); they carry no
// mark-to-market, so CurrentValue/UnrealizedPL stay zero and the renderer
// omits the P&L bucket rather than fabricate ≈+0%.
//
// CashKnown distinguishes a real balance (including fully-deployed $0) from an
// uninitialized tracker: equity > 0 means the tracker received a real update.
func buildPortfolioContext(state portfolio.State, positions []portfolio.ExecPosition, openTrades []store.Trade) analysis.PortfolioContext {
	out := analysis.PortfolioContext{
		CashAvailable: state.Cash,
		CashKnown:     state.Cash > 0 || state.TotalValue > 0,
	}
	for _, p := range positions {
		if p.Quantity == 0 {
			continue
		}
		out.OpenPositions = append(out.OpenPositions, analysis.PositionSummary{
			Ticker:       p.Ticker,
			Side:         p.Side,
			Quantity:     p.Quantity,
			AvgCostBasis: p.AvgCostBasis,
			CurrentValue: p.CurrentValue,
			UnrealizedPL: p.UnrealizedPL,
		})
	}
	if len(out.OpenPositions) > 0 {
		return out
	}

	// Tracker empty — aggregate open trades per (ticker, direction).
	type key struct{ ticker, dir string }
	agg := make(map[key]*analysis.PositionSummary)
	order := []key{}
	for _, t := range openTrades {
		if t.Quantity == 0 {
			continue
		}
		k := key{t.Ticker, t.Direction}
		ps, ok := agg[k]
		if !ok {
			ps = &analysis.PositionSummary{Ticker: t.Ticker, Side: t.Direction}
			agg[k] = ps
			order = append(order, k)
		}
		// Weighted-average entry across pyramided rows.
		totalCost := ps.AvgCostBasis*ps.Quantity + t.EntryPrice*t.Quantity
		ps.Quantity += t.Quantity
		if ps.Quantity != 0 {
			ps.AvgCostBasis = totalCost / ps.Quantity
		}
	}
	for _, k := range order {
		out.OpenPositions = append(out.OpenPositions, *agg[k])
	}
	return out
}

// portfolioContextForPrompt assembles the portfolio context for an analysis
// request, loading the store's open trades as fallback only when the tracker
// holds no positions (the paper executor reports none — Codex #392 P1). The
// store query is skipped entirely in the common live-broker case.
func (e *Engine) portfolioContextForPrompt(ctx context.Context) analysis.PortfolioContext {
	state := e.portfolio.CurrentState()
	positions := e.portfolio.Positions()
	var openTrades []store.Trade
	if len(positions) == 0 && store.IsConnected() {
		if trades, err := store.GetOpenTrades(ctx); err == nil {
			openTrades = trades
		} else {
			logger.Warn("portfolio context: open-trades fallback failed", "err", err)
		}
	}
	return buildPortfolioContext(state, positions, openTrades)
}

// runAnalysis builds the analysis request, calls the configured model, stores
// the result, and logs the recommendation summary.
func (e *Engine) runAnalysis(ctx context.Context, ticker, timeframe, strategy string, candles []data.Candle, set *indicators.IndicatorSet, newsHash string, source string, graphContext string) {
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

	// Pre-LLM SELL ownership short-circuit (#319). For strategies whose primary
	// action is SELL (close/exit-only), there is no point asking the analyst
	// what to do on a ticker we don't hold long: the only valid recommendation
	// is SELL, which the post-LLM position-ownership gate would then block.
	// Skipping here saves the full LLM round-trip — input prompt, model output,
	// arbiter pass — for every such cycle.
	//
	// The canonical SELL-only strategy list is defined in risk.IsSellOnlyStrategy
	// (sell_only / exit_signal / close_position today). For strategies that emit
	// BUY *or* SELL depending on context (the default trend_following/
	// mean_reversion/momentum/news_sentiment set), we still need the LLM to
	// pick a direction, so the short-circuit does not fire.
	//
	// Load openTrades here ONLY when the ownership probe is needed: a SELL-only
	// strategy with a live DB. For dual-direction strategies and HOLD/WAIT
	// outcomes the prefetch would be wasted work — generateSignal already
	// fetches on-demand for the post-LLM ownership check (Copilot perf
	// regression on PR #337 round-2). When the prefetch is skipped we leave
	// prefetchedOpenTrades nil so generateSignal re-issues the query through
	// its existing degraded-mode handling.
	//
	// store.IsConnected gating prevents store.GetOpenTrades from panicking
	// (GetDB panics if the pool is nil). When a SELL-only strategy is gated
	// out of the prefetch because the DB is not connected, we cannot verify
	// ownership, so we fall through to the LLM and let generateSignal's
	// post-LLM ownership gate handle the no-DB path the same way it does for
	// any other SELL.
	var prefetchedOpenTrades []store.Trade
	if shouldPrefetchOpenTrades(strategy, store.IsConnected()) {
		if openTrades, err := store.GetOpenTrades(ctx); err != nil {
			// On a DB read failure, fall through to the LLM rather than silently
			// skipping; the post-LLM gate will still block any SELL we cannot
			// verify. This mirrors how generateSignal handles GetOpenTrades errors
			// for SELL signals. Leave prefetchedOpenTrades nil so generateSignal
			// re-tries the query and applies its own degraded-mode handling.
			logger.Warn("sell-strategy ownership probe failed — falling through to LLM",
				"ticker", ticker, "strategy", strategy, "err", err.Error())
		} else {
			prefetchedOpenTrades = openTrades
			holdsLong := tickerHasOpenLongIn(openTrades, ticker)
			if shouldSkipSellStrategy(strategy, holdsLong) {
				reason := risk.NoPositionBlockReason(ticker)
				logger.Info("analysis skipped — no position",
					"ticker", ticker,
					"strategy", strategy,
					"timeframe", timeframe,
					"reason", reason,
				)
				e.emitOwnershipShortCircuitBlock(ctx, ticker, strategy, candles, reason)
				return
			}
		}
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
	e.bus.Publish(events.NewsEnriched, events.NewsEnrichedPayload{
		Ticker: ticker,
		Count:  len(snippets),
	})

	// Build knowledge graph context for this ticker (best-effort; non-fatal).
	var knowledgeCtx string
	if gc, err := knowledge.BuildContext(ctx, ticker, e.portfolio.PositionMap()); err == nil {
		knowledgeCtx = gc.Format()
	}
	// Prepend the candidate's persisted origin thesis (graph link / sympathy
	// trigger) so the analysts know WHY this ticker is on the watchlist — e.g.
	// "SYMPATHY TRIGGER — NVDA news 2h ago (...)". Stable per watchlist item,
	// so it does not churn the prompt cache.
	if graphContext != "" {
		knowledgeCtx = "CANDIDATE ORIGIN: " + graphContext + "\n\n" + knowledgeCtx
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

	var performanceCtx string
	if e.cfg.Feedback.Enabled && e.cfg.Feedback.PromptEnrichment {
		if pc, err := feedback.GetPerformanceContext(ctx, ticker); err == nil {
			performanceCtx = pc
		}
	}

	req := analysis.AnalysisRequest{
		Ticker:             ticker,
		Strategy:           strategy,
		Timeframe:          timeframe,
		MomentumPlay:       source == "scanner",
		RecentCandles:      snaps,
		KnowledgeContext:   knowledgeCtx,
		SimilarAnalyses:    similarSetups,
		PerformanceContext: performanceCtx,
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
		Portfolio: e.portfolioContextForPrompt(ctx),
	}

	impact := e.calculateImpactScore(ticker, snippets, candles)
	// Issue #323: when specialist mode is enabled, route to the 4-role
	// specialist arbiter (fundamental / technical / sentiment / risk_manager)
	// instead of the duplicate-prompt ensemble. Both paths return AnalyzeResult
	// so all downstream cost-tracking, persistence and UI code is unchanged.
	var result analysis.AnalyzeResult
	var err error
	if e.cfg.Analysis.SpecialistModeEnabled {
		result, err = e.analyzer.AnalyzeSpecialist(ctx, req, impact)
	} else {
		result, err = e.analyzer.AnalyzeFull(ctx, req, impact)
	}

	// Record token usage for cost tracking (#177) — BEFORE checking err and
	// BEFORE the optional second-pass below so first-pass spend is always
	// captured even if a second pass overwrites `result`. Issue #321's
	// alt-timeframe pass records its own tokens via recordMultiTFCost into a
	// distinct "<model>#mtf2" bucket; the re-arbiter cost is recorded after
	// re-arbitration completes.
	//
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

	// Issue #321 — Multi-timeframe arbiter for borderline WAITs.
	// If the first pass resolved to WAIT inside the configured borderline band,
	// run a second analyst pass on the alternate timeframe and re-arbitrate
	// over the combined set. The recursion guard is enforced by passing the
	// secondPass=false flag here; runSecondTimeframePass never re-invokes this
	// branch, so a third pass cannot fire.
	if err == nil {
		if altTF, run := shouldRunSecondPass(e.cfg.Analysis, result.Response, timeframe, false); run {
			logger.Info("multi-tf: borderline WAIT — running alt-timeframe pass",
				"ticker", ticker, "primary_tf", timeframe, "alt_tf", altTF,
				"confidence", fmt.Sprintf("%.3f", result.Response.Confidence))
			if altResult, ok := e.runSecondTimeframePass(ctx, req, altTF, ticker, impact); ok {
				// Charge the alt-pass tokens to a distinct bucket
				// ("<model>#mtf2") so we can measure marginal cost.
				e.recordMultiTFCost(altResult)
				combined := combineForArbiter(timeframe, altTF, result.AnalystResults, altResult.AnalystResults)
				combinedReq := req
				combinedReq.Timeframe = fmt.Sprintf("%s+%s", timeframe, altTF)
				if reArb, rerr := reArbitrate(ctx, e.analyzer, combinedReq, combined); rerr == nil {
					// Charge re-arbiter tokens to the multi-tf bucket too —
					// it would not have run absent the second pass.
					if reArb.Arbiter != nil && reArb.Arbiter.Meta.Provider != "" && reArb.Arbiter.Meta.Model != "" &&
						(reArb.Arbiter.Meta.InputTokens > 0 || reArb.Arbiter.Meta.OutputTokens > 0) {
						e.costTracker.Record(reArb.Arbiter.Meta.Provider, reArb.Arbiter.Meta.Model+multiTFCostSuffix,
							reArb.Arbiter.Meta.InputTokens, reArb.Arbiter.Meta.OutputTokens)
					}
					logger.Info("multi-tf: re-arbitration complete",
						"ticker", ticker,
						"original_action", string(result.Response.Action),
						"final_action", string(reArb.Response.Action),
						"final_confidence", fmt.Sprintf("%.3f", reArb.Response.Confidence))
					result = reArb
				} else {
					logger.Warn("multi-tf: re-arbitration failed — keeping original WAIT",
						"ticker", ticker, "err", rerr.Error())
				}
			} else {
				logger.Debug("multi-tf: alt-timeframe pass unavailable — keeping original WAIT",
					"ticker", ticker, "alt_tf", altTF)
			}
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
	// Broadcast per-analyst votes so the frontend can render individual cards.
	{
		var votes []events.AnalystVotePayload
		if len(result.AnalystResults) > 0 {
			for _, ar := range result.AnalystResults {
				skipped := ar.Err != nil
				action := ""
				conf := 0.0
				if !skipped {
					action = string(ar.Response.Action)
					conf = ar.Response.Confidence
				}
				votes = append(votes, events.AnalystVotePayload{
					Provider:   ar.Provider,
					Model:      ar.Model,
					Action:     action,
					Confidence: conf,
					Skipped:    skipped,
					LatencyMs:  ar.LatencyMs,
				})
			}
		} else {
			// Single-model path — synthesize a one-entry slice.
			votes = []events.AnalystVotePayload{{
				Provider:   meta.Provider,
				Model:      meta.Model,
				Action:     string(resp.Action),
				Confidence: resp.Confidence,
				Skipped:    false,
				LatencyMs:  meta.LatencyMs,
			}}
		}
		e.bus.Publish(events.AnalystVotesPublished, events.AnalystVotesPublishedPayload{
			Ticker: ticker,
			Votes:  votes,
		})
	}

	e.bus.Publish(events.AnalysisCompleted, events.AnalysisCompletedPayload{
		Ticker: ticker, Action: string(resp.Action), Confidence: resp.Confidence,
		Reasoning: resp.Reasoning, Provider: meta.Provider, Model: meta.Model, LatencyMs: meta.LatencyMs,
		FastPathed: result.FastPathed,
	})
	if !result.FastPathed && result.Arbiter != nil {
		e.bus.Publish(events.ArbiterInvoked, events.ArbiterInvokedPayload{
			Ticker:         ticker,
			Action:         string(resp.Action),
			Confidence:     resp.Confidence,
			DissentSummary: result.Arbiter.DissentSummary,
		})
	}

	// Persist — use ensemble variant when any analyst results are available so
	// that ensemble_results is always stored and the frontend analyst panel is
	// populated even when only a single analyst is configured.
	var analysisID string
	if len(result.AnalystResults) >= 1 {
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
	// Hand the pre-fetched openTrades (loaded by the SELL ownership short-circuit
	// probe above) into generateSignal so we don't fire a second identical
	// GetOpenTrades query for SELL-only strategies. Nil is the common case —
	// dual-direction strategies and no-DB / probe-failed cycles all leave the
	// prefetch unset, and generateSignal then re-fetches on-demand and applies
	// its degraded-mode handling.
	e.generateSignal(
		ctx, ticker, timeframe, strategy, candles, resp, analysisID,
		prefetchedOpenTrades,
	)

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

// shouldSkipSellStrategy is the pure decision predicate for the pre-LLM
// SELL ownership short-circuit (#319). Returns true only when the strategy
// is canonically SELL-only AND the portfolio holds no open long. Extracted
// from runAnalysis so the decision can be unit-tested without a DB.
func shouldSkipSellStrategy(strategy string, holdsLong bool) bool {
	return risk.IsSellOnlyStrategy(strategy) && !holdsLong
}

// shouldPrefetchOpenTrades reports whether runAnalysis should issue an
// up-front store.GetOpenTrades query to support the SELL-only ownership
// short-circuit (#319). Returns true only when (a) the strategy is canonically
// SELL-only — dual-direction strategies and HOLD/WAIT cycles get the prefetch
// skipped to avoid a wasted DB roundtrip every tick (Copilot perf review on
// PR #337 round-2) — AND (b) the store is connected, so GetOpenTrades won't
// panic via GetDB on a nil pool. When this returns false the SELL ownership
// gate falls through to generateSignal's on-demand fetch for the post-LLM
// ownership check.
func shouldPrefetchOpenTrades(strategy string, dbConnected bool) bool {
	return risk.IsSellOnlyStrategy(strategy) && dbConnected
}

// tickerHasOpenLongIn reports whether the supplied openTrades slice contains
// any open long position in the given ticker. Used by the SELL ownership
// pre-LLM short-circuit (#319) so we can skip the analyst call entirely when
// a SELL-only strategy is scheduled against a ticker we don't hold.
//
// Slice-based (rather than fetching itself) so the caller can reuse the same
// GetOpenTrades result for the post-LLM risk-check pass — avoiding a duplicate
// DB query per analysis cycle.
func tickerHasOpenLongIn(openTrades []store.Trade, ticker string) bool {
	for _, t := range openTrades {
		if t.Ticker == ticker && t.Direction == "long" {
			return true
		}
	}
	return false
}

func tickerHasExitingLongIn(trades []store.Trade, ticker string) bool {
	for _, t := range trades {
		if t.Ticker == ticker && t.Direction == "long" && t.Status == "exiting" {
			return true
		}
	}
	return false
}

// buildOwnershipShortCircuitBlock returns the synthetic blocked SELL signal
// and the two event payloads that the SELL-only pre-LLM short-circuit emits
// so the cycle is still visible in the signals table and on the dashboard
// rather than disappearing silently (Codex/Copilot review on PR #337).
//
// Pure (no DB / no event bus) so it can be unit-tested without spinning up a
// Postgres test database. emitOwnershipShortCircuitBlock wires the result to
// store.InsertSignal and the engine event bus.
func buildOwnershipShortCircuitBlock(cfg config.RiskConfig, ticker, strategy string, candles []data.Candle, reason string, now time.Time) (store.Signal, events.RiskCheckCompletedPayload, events.SignalBlockedPayload) {
	var signalPrice float64
	if len(candles) > 0 {
		signalPrice = candles[len(candles)-1].Close
	}
	expiryMin := cfg.SignalExpiryMin
	if expiryMin == 0 {
		expiryMin = 30
	}
	stalePct := cfg.StalePriceMovePct
	if stalePct == 0 {
		stalePct = 2.0
	}
	sig := store.Signal{
		Ticker:            ticker,
		Action:            "SELL",
		Strategy:          strategy,
		SignalPrice:       signalPrice,
		Status:            "blocked",
		BlockReason:       reason,
		ExpiresAt:         now.Add(time.Duration(expiryMin) * time.Minute),
		StalePriceMovePct: stalePct,
	}
	riskEv := events.RiskCheckCompletedPayload{
		Ticker: ticker,
		Passed: false,
		Checks: []events.RiskCheckResult{{
			Name:   "position_ownership",
			Passed: false,
			Reason: reason,
		}},
	}
	blockEv := events.SignalBlockedPayload{
		Ticker:      ticker,
		Action:      "SELL",
		BlockReason: reason,
		FailedCheck: "position_ownership",
	}
	return sig, riskEv, blockEv
}

// emitOwnershipShortCircuitBlock persists the synthetic blocked signal and
// publishes the matching RiskCheckCompleted + SignalBlocked events so the
// downstream surface (Signals table, TUI, Telegram, dashboard) sees a SELL
// short-circuit identically to a normal post-LLM risk-check rejection.
//
// When the store is not connected (degraded/no-DB mode) the persistent insert
// is skipped — store.InsertSignal calls store.GetDB which panics if the pool
// is nil — but we still publish the in-process events so the TUI/Telegram/
// dashboard surfaces continue to see the block. Only the persistent record is
// lost (Copilot crash review on PR #337 round-2).
func (e *Engine) emitOwnershipShortCircuitBlock(ctx context.Context, ticker, strategy string, candles []data.Candle, reason string) {
	sig, riskEv, blockEv := buildOwnershipShortCircuitBlock(e.cfg.Risk, ticker, strategy, candles, reason, time.Now())
	if store.IsConnected() {
		if _, err := store.InsertSignal(ctx, sig); err != nil {
			logger.Warn("store ownership short-circuit blocked signal",
				"ticker", ticker, "err", err)
		}
	} else {
		logger.Info("signal blocked (no-DB; not persisted)",
			"ticker", ticker, "strategy", strategy, "reason", reason)
	}
	e.bus.Publish(events.RiskCheckCompleted, riskEv)
	e.bus.Publish(events.SignalBlocked, blockEv)
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
	_, _ = e.fetchAndStoreCandlesObserved(ctx, ticker, tf)
}

// fetchAndStoreCandlesObserved is the checked variant used when callers need
// the exact boundary at which the returned provider snapshot became observable.
func (e *Engine) fetchAndStoreCandlesObserved(
	ctx context.Context, ticker string, tf data.Timeframe,
) (time.Time, error) {
	if e.marketData == nil {
		return time.Time{}, fmt.Errorf("market data unavailable")
	}
	candles, err := e.marketData.FetchHistory(ctx, ticker, tf, historyCandles)
	if err != nil {
		logger.Warn("fetch candles", "ticker", ticker, "tf", string(tf), "err", err.Error())
		return time.Time{}, err
	}
	if len(candles) == 0 {
		err := fmt.Errorf("provider returned no candles")
		logger.Warn("fetch candles", "ticker", ticker, "tf", string(tf), "err", err.Error())
		return time.Time{}, err
	}
	// Exact post-setup invalidation starts when this provider snapshot became
	// observable. Capture the boundary immediately after FetchHistory returns,
	// before DB persistence or any subsequent qualification work.
	observedAt := time.Now()
	if err := store.UpsertCandles(ctx, candles); err != nil {
		logger.Warn("upsert candles", "ticker", ticker, "err", err.Error())
		return time.Time{}, err
	}
	logger.Info("candles upserted", "ticker", ticker, "tf", string(tf), "count", len(candles))
	e.bus.Publish(events.DataFetched, events.DataFetchedPayload{
		Ticker: ticker, Timeframe: string(tf), Count: len(candles),
	})
	return observedAt, nil
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
	newsIDs, err := store.UpsertNewsItems(ctx, items)
	if err != nil {
		logger.Warn("upsert news", "ticker", ticker, "err", err.Error())
		return
	}
	logger.Info("news fetched", "ticker", ticker, "count", len(items))
	e.bus.Publish(events.NewsFetched, events.NewsFetchedPayload{
		Ticker: ticker, Count: len(items),
	})

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
		// Pass the stored row's UUID so the enricher records entity/relationship
		// mentions — the corroboration count that gates graph-neighbor scanner
		// eligibility. An empty ID (article without external_id) skips mention
		// recording, matching prior behavior for those rows.
		e.enricher.EnrichNewsItem(ctx, newsIDs[item.ExternalID], item.Headline, item.Summary)
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

// SetTrailingManagerAvailable records whether command wiring actually started
// the trailing worker. Uncapped entries fail safe to their baseline TP unless
// this runtime capability is present.
func (e *Engine) SetTrailingManagerAvailable(available bool) {
	e.trailingManagerAvailable = available
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

// pyramidTightenedStop returns the stop the new lot of a pyramid add should
// carry. Inputs:
//
//   - analystStop is the StopLoss the analyst returned (resp.StopLoss).
//   - existingMaxStop is the highest stop across open long lots in the
//     ticker (canonical existing protective stop).
//   - avgEntry is the quantity-weighted average entry across those lots.
//   - signalPrice is the current price the new lot would be opened at.
//
// The new lot's stop is the larger of (existingMaxStop, avgEntry) — the
// former honours stops the operator (or a trailing manager) has already
// ratcheted up; the latter guarantees a stop-out is at worst breakeven on
// the original lot. The analyst stop is kept if it's already tighter than
// either. The result is also capped strictly below signalPrice so a
// degenerate input can never produce an immediately-out-of-range long stop;
// callers should additionally route through checkPyramidPolicy which
// rejects pyramid attempts where the existing stop has drifted at or above
// market (#324 review).
func pyramidTightenedStop(analystStop, existingMaxStop, avgEntry, signalPrice float64) float64 {
	tightened := existingMaxStop
	if avgEntry > tightened {
		tightened = avgEntry
	}
	if tightened <= analystStop {
		return analystStop
	}
	// Defence in depth: never return a stop that would be at or above the
	// current signal price for a long position. checkPyramidPolicy already
	// rejects this case, so in practice we won't reach here — but keep the
	// invariant local so a future refactor can't accidentally violate it.
	if signalPrice > 0 && tightened >= signalPrice {
		return analystStop
	}
	return tightened
}

// generateSignal creates a signal from a BUY/SELL analysis, runs risk checks,
// and routes execution according to the current mode.
//
// prefetchedOpenTrades, when non-nil, is reused for the position-ownership and
// portfolio-shape risk inputs instead of issuing another GetOpenTrades query.
// runAnalysis loads openTrades up front for the SELL-only short-circuit probe
// (#319) and threads the same slice through here so a typical analysis cycle
// only hits the trades store once. Pass nil to force a fresh query.
func (e *Engine) generateSignal(ctx context.Context, ticker, timeframe, strategy string, candles []data.Candle, resp analysis.AnalysisResponse, analysisID string, prefetchedOpenTrades []store.Trade) {
	if resp.Action == analysis.ActionHold || resp.Action == analysis.ActionWait {
		// WAIT: real catalyst but poor entry timing — analysis is recorded,
		// no signal fires. No cooldown is applied so the ticker is re-evaluated
		// on the next normal tick.
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

	analysisIDs := []string{}
	if analysisID != "" {
		analysisIDs = []string{analysisID}
	}
	createdAt := time.Now()
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
		ExpiresAt:         createdAt.Add(time.Duration(expiryMin) * time.Minute),
		StalePriceMovePct: stalePct,
		AnalysisIDs:       analysisIDs,
		CreatedAt:         createdAt,
	}

	// Entry-quality gates (entry_gates.go): daily trend alignment, news
	// tape-confirmation, and momentum anti-chase. Checked before the generic
	// risk gates so the block reason names the specific losing cohort. Legacy
	// cohort gates fail open on missing evidence; the complete pullback setup
	// fails closed because every component is required.
	eg := e.cfg.Risk.EntryGates
	if resp.Action == "BUY" {
		var entryGateReason string
		entryGateFailedCheck := "entry_gates"
		entryDailyCandles := candles
		var pullbackObservedAt time.Time
		needsDailyEvidence := eg.PullbackStrengthEnabled ||
			eg.TrendAlignmentEnabled
		if eg.PullbackStrengthEnabled {
			// The analyst may take long enough for the session low to change.
			// Refresh the partial daily bar at qualification time, then scan
			// every exact trade print from the returned snapshot boundary.
			var err error
			pullbackObservedAt, err = e.fetchAndStoreCandlesObserved(
				ctx, ticker, data.Timeframe1Day,
			)
			if err != nil {
				entryGateReason = "pullback strength: unable to refresh daily setup evidence"
				entryGateFailedCheck = "pullback_history"
			}
		}
		if needsDailyEvidence &&
			(data.Timeframe(timeframe) != data.Timeframe1Day || eg.PullbackStrengthEnabled) {
			var err error
			entryDailyCandles, err = store.GetCandles(
				ctx, ticker, data.Timeframe1Day,
				time.Now().AddDate(-2, 0, 0), time.Now(),
			)
			if err != nil {
				logger.Warn("entry gates: load daily evidence",
					"ticker", ticker, "err", err)
				entryDailyCandles = nil
			}
		}
		if eg.PullbackStrengthEnabled && entryGateReason == "" {
			// Use the refreshed daily snapshot's close for reclaim, stop width,
			// signal persistence, and the initial risk pass. The pre-analysis
			// primary-timeframe close may be minutes old by this point.
			if len(entryDailyCandles) > 0 {
				signalPrice = entryDailyCandles[len(entryDailyCandles)-1].Close
				sig.SignalPrice = signalPrice
			}
			setup := pullback.Evaluate(entryDailyCandles, signalPrice, pullback.Config{
				MinMomentumReturnPct:  eg.PullbackMinMomentumReturnPct,
				ConsolidationSessions: eg.PullbackConsolidationSessions,
				SupportTolerancePct:   eg.PullbackSupportTolerancePct,
				MaxStopWidthPct:       eg.PullbackMaxStopWidthPct,
			})
			if !setup.Eligible {
				entryGateReason = setup.Reason
				entryGateFailedCheck = "pullback_" + setup.FailedCheck
			} else {
				// The research entry is inseparable from its tight low-of-day
				// invalidation. Replace the analyst's generic stop before risk
				// sizing, persistence, and bracket submission.
				sig.StopLoss = setup.StopLoss
				sig.PullbackStrength = true
				// CreatedAt is the durable exact-print boundary for pullback
				// signals. It begins at the refreshed evidence snapshot rather
				// than after qualification, risk checks, and persistence.
				sig.CreatedAt = pullbackObservedAt
				sig.ExpiresAt = pullbackObservedAt.Add(
					time.Duration(expiryMin) * time.Minute,
				)
				logger.Info("pullback strength entry confirmed",
					"ticker", ticker,
					"return_1m_pct", fmt.Sprintf("%.2f", setup.Return1M),
					"return_3m_pct", fmt.Sprintf("%.2f", setup.Return3M),
					"return_6m_pct", fmt.Sprintf("%.2f", setup.Return6M),
					"ema9", fmt.Sprintf("%.2f", setup.EMA9),
					"ema21", fmt.Sprintf("%.2f", setup.EMA21),
					"avwap_high", fmt.Sprintf("%.2f", setup.AVWAPHigh),
					"avwap_low", fmt.Sprintf("%.2f", setup.AVWAPLow),
					"support", fmt.Sprintf("%.2f", setup.Support),
					"stop_loss", fmt.Sprintf("%.2f", setup.StopLoss),
					"stop_width_pct", fmt.Sprintf("%.2f", setup.StopWidthPct),
				)
			}
		} else if eg.TrendAlignmentEnabled {
			var evaluated bool
			entryGateReason, evaluated = evaluateTrendAlignmentGate(
				string(resp.Action), signalPrice, data.Timeframe1Day,
				entryDailyCandles, time.Now(), eg,
			)
			if !evaluated {
				logger.Warn("entry gates: daily trend evidence unavailable — trend gate fails open",
					"ticker", ticker, "timeframe", timeframe, "candles", len(candles))
			}
		}
		if entryGateReason == "" &&
			(eg.NewsMinRunupPct > 0 || eg.MomentumMaxRunupPct > 0) {
			nowET := time.Now().In(nyLoc)
			dayStartET := time.Date(nowET.Year(), nowET.Month(), nowET.Day(), 0, 0, 0, 0, nyLoc)
			prevClose, pcErr := store.GetPrevDailyClose(ctx, ticker, dayStartET)
			if pcErr != nil || prevClose <= 0 {
				logger.Warn("entry gates: prior daily close unavailable — run-up gates fail open",
					"ticker", ticker, "err", pcErr)
			}
			entryGateReason = evaluateRunupGates(
				strategy, string(resp.Action), signalPrice, prevClose, eg,
			)
		}
		if entryGateReason != "" {
			sig.Status = "blocked"
			sig.BlockReason = entryGateReason
			if _, err := store.InsertSignal(ctx, sig); err != nil {
				logger.Warn("store blocked signal", "ticker", ticker, "err", err)
			}
			logger.Warn("signal blocked", "ticker", ticker, "reason", entryGateReason)
			e.bus.Publish(events.SignalBlocked, events.SignalBlockedPayload{
				Ticker: ticker, Action: string(resp.Action), Confidence: resp.Confidence,
				BlockReason: entryGateReason, FailedCheck: entryGateFailedCheck,
			})
			return
		}
	}

	// Run risk checks.
	pState := e.portfolio.CurrentState()
	var openTrades []store.Trade
	var openTradesErr error
	if prefetchedOpenTrades != nil {
		// Reuse the slice already loaded by runAnalysis's SELL-ownership probe
		// so a normal analysis cycle costs one GetOpenTrades query, not two.
		openTrades = prefetchedOpenTrades
	} else if resp.Action == "BUY" {
		// A durable in-flight exit remains a real broker position until
		// reconciliation completes, so it must count against entry limits and
		// duplicate/pyramid policy.
		openTrades, openTradesErr = store.GetOwnedPositionTrades(ctx)
	} else {
		openTrades, openTradesErr = store.GetOpenTrades(ctx)
	}
	if openTradesErr != nil {
		logger.Warn("get open trades for risk checks", "ticker", ticker, "err", openTradesErr)
		// For SELL signals we cannot verify position ownership — block rather than
		// risk a naked sell. BUY signals can proceed with a degraded position count.
		if resp.Action == "SELL" {
			sig.Status = "blocked"
			sig.BlockReason = "unable to verify open positions (DB error)"
			if _, err := store.InsertSignal(ctx, sig); err != nil {
				logger.Warn("store blocked signal", "ticker", ticker, "err", err)
			}
			logger.Warn("signal blocked", "ticker", ticker, "reason", sig.BlockReason)
			return
		}
	}
	if resp.Action == "BUY" && tickerHasExitingLongIn(openTrades, ticker) {
		sig.Status = "blocked"
		sig.BlockReason = "prior long exit is still reconciling"
		if _, err := store.InsertSignal(ctx, sig); err != nil {
			logger.Warn("store blocked signal", "ticker", ticker, "err", err)
		}
		e.bus.Publish(events.SignalBlocked, events.SignalBlockedPayload{
			Ticker: ticker, Action: string(resp.Action), Confidence: resp.Confidence,
			BlockReason: sig.BlockReason, FailedCheck: "exit_ownership",
		})
		return
	}

	// Pyramiding (#324): the new lot's stop will be tightened to
	// max(canonical existing stop, weighted avg entry) so the combined cost
	// basis is at worst breakeven on the original lot when the new stop
	// trips. The mutation is deferred until AFTER risk checks pass so a
	// blocked signal still records the unmodified analyst stop and the
	// SignalGenerated payload below matches what is stored. checkPyramidPolicy
	// rejects the attempt outright when the existing stop already sits at or
	// above the current signal price.
	riskIn := buildRiskInput(e.cfg.Risk, string(resp.Action), ticker,
		resp.Confidence, resp.SizePct, sig.StopLoss, sig.TakeProfit, signalPrice,
		pState, openTrades)
	holdsLong := riskIn.HoldsLong
	longAvgEntry := riskIn.LongAvgEntryPrice
	longMaxStop := riskIn.LongMaxStop

	results, passed := risk.CheckAll(riskIn)
	for _, r := range results {
		level := "INFO"
		if !r.Passed {
			level = "WARN"
		}
		logger.Info("risk check", "check", r.Name, "level", level, "passed", r.Passed, "reason", r.Reason)
	}
	// Broadcast all check results so the pipeline UI can show live PASS/FAIL.
	riskEvChecks := make([]events.RiskCheckResult, len(results))
	for i, r := range results {
		riskEvChecks[i] = events.RiskCheckResult{Name: r.Name, Passed: r.Passed, Reason: r.Reason}
	}
	e.bus.Publish(events.RiskCheckCompleted, events.RiskCheckCompletedPayload{
		Ticker: ticker, Passed: passed, Checks: riskEvChecks,
	})

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

		// Issue #325: counterfactual logging. Re-evaluates the same signal
		// against a relaxed gate set (R:R 1.30, conf 0.55, no tiers) so we can
		// measure empirically whether the strict gates are over- or under-tuned.
		// Observability-only — never affects which signals execute. Skipped when
		// the failing check is a state machine (max_open_positions, cooldown,
		// drawdown, …) since loosening thresholds wouldn't change the outcome.
		var failedCheck string
		for _, r := range results {
			if !r.Passed {
				failedCheck = r.Name
				break
			}
		}
		if risk.IsCalibratable(failedCheck) {
			cf := risk.EvaluateCounterfactual(riskIn, results)
			flipped := cf.FlippedCheck
			if !cf.PassedLoose {
				flipped = "none"
			}
			logger.Info("signal blocked counterfactual",
				"ticker", ticker,
				"passed_strict", false,
				"passed_loose", cf.PassedLoose,
				"flipped_check", flipped,
			)
		}
		return
	}

	// Pyramiding (#324): risk checks have cleared, including
	// checkPyramidPolicy's gain-met / stop-below-price gates. Tighten the new
	// lot's stop so the combined cost basis is at worst breakeven on the
	// original lot when the new stop trips. Performed here — after the gate,
	// before InsertSignal — so blocked signals keep the unmodified analyst
	// stop and the SignalGenerated payload below reflects the actual stored
	// value.
	if resp.Action == "BUY" && holdsLong && e.cfg.Risk.PyramidEnabled {
		sig.StopLoss = pyramidTightenedStop(sig.StopLoss, longMaxStop, longAvgEntry, signalPrice)
	}

	// First-tick deferral decision is made BEFORE insert so the row carries
	// the deferred marker (migration 043): the deferred-execution sweep only
	// ever touches rows stamped here, never manual-approval pendings left
	// over from a mode switch or restart.
	mode := e.Mode()
	deferBuy := e.cfg.Risk.EntryGates.FirstTickDefer && e.sessionFirstTick &&
		resp.Action == "BUY" && (mode == "auto" || mode == "paper")
	sig.Deferred = deferBuy

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

	// Publish the actual stored signal parameters — sig.StopLoss/sig.TakeProfit
	// reflect any pyramid stop tightening above; using resp.* would diverge
	// from what downstream order placement consumes (#324 review).
	e.bus.Publish(events.SignalGenerated, events.SignalGeneratedPayload{
		SignalID: sigID, Ticker: ticker, Action: string(resp.Action),
		Confidence: resp.Confidence, SignalPrice: signalPrice,
		StopLoss: sig.StopLoss, TakeProfit: sig.TakeProfit, Mode: e.Mode(),
	})

	// Route based on mode.
	switch mode {
	case "auto", "paper":
		// First-tick deferral (entry_gates.go): BUYs born on the session's
		// first tick stay pending; the next tick executes them only if the
		// price held (processDeferredSignals). While pending they inherit the
		// normal expiry + stale-price protections — which also puts the
		// staleness guard on the auto path for the first time.
		if deferBuy {
			logger.Info("signal deferred for first-tick confirmation",
				"id", sigID, "ticker", ticker, "signal_price", signalPrice)
			e.bus.Publish(events.SignalQueued, events.SignalQueuedPayload{
				SignalID: sigID, Ticker: ticker, Action: string(resp.Action),
				Confidence: resp.Confidence, ExpiresIn: time.Until(sig.ExpiresAt),
			})
			return
		}
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

// resolveFallbackCandle selects the freshest available candle for a signal
// price fallback, enforcing timeframe-specific recency windows.
func resolveFallbackCandle(now time.Time, lookup func(tf data.Timeframe) (*data.Candle, error)) *data.Candle {
	for _, tf := range []data.Timeframe{data.Timeframe5Min, data.Timeframe15Min, data.Timeframe1Hour, data.Timeframe1Day} {
		latest, err := lookup(tf)
		if err != nil || latest == nil {
			continue
		}
		if latest.Close <= 0 {
			continue
		}
		if !isFreshCandle(tf, latest.Timestamp, now) {
			continue
		}
		return latest
	}
	return nil
}

func isFreshCandle(tf data.Timeframe, ts time.Time, now time.Time) bool {
	maxAge := maxFallbackCandleAge(tf)
	if maxAge <= 0 {
		return false
	}
	age := now.Sub(ts)
	return age >= 0 && age <= maxAge
}

func maxFallbackCandleAge(tf data.Timeframe) time.Duration {
	switch tf {
	case data.Timeframe5Min:
		return 30 * time.Minute
	case data.Timeframe15Min:
		return 2 * time.Hour
	case data.Timeframe1Hour:
		return 6 * time.Hour
	case data.Timeframe1Day:
		return 48 * time.Hour
	default:
		return 0
	}
}

// executeSignal places the order via the executor, creates a trade record, and
// records the entry order.
func (e *Engine) executeSignal(ctx context.Context, sigID string, sig store.Signal) {
	// Market-hours guard (#437): a tick that STARTS before the close can
	// spend 20+ minutes in analysis and reach execution after 16:00 ET —
	// 17% of recorded entries landed in the 16:00 hour. After-hours market
	// orders never fill in-session (day orders expire; GTC bracket entries
	// queue overnight, far past the fill-poll window) and were recorded as
	// qty-0 "open" trades that clogged the position budget. Block; the
	// ticker re-evaluates next session. Applies to SELL exits too — their
	// market orders equally cannot fill until the open.
	if !IsMarketOpen(time.Now()) {
		reason := "market closed — order would not fill this session"
		if err := store.UpdateSignalStatus(ctx, sigID, "blocked", reason); err != nil {
			logger.Warn("update signal status", "id", sigID, "err", err)
		}
		logger.Warn("signal blocked", "ticker", sig.Ticker, "reason", reason)
		e.bus.Publish(events.SignalBlocked, events.SignalBlockedPayload{
			Ticker: sig.Ticker, Action: sig.Action, Confidence: sig.Confidence,
			BlockReason: reason, FailedCheck: "market_hours",
		})
		return
	}

	side := execution.OrderSideBuy
	direction := "long"

	// Ensure signal price is populated before sizing/execution.
	if sig.SignalPrice <= 0 {
		if candle := resolveFallbackCandle(time.Now(), func(tf data.Timeframe) (*data.Candle, error) {
			return store.GetLatestCandle(ctx, sig.Ticker, tf)
		}); candle != nil {
			sig.SignalPrice = candle.Close
			if err := store.UpdateSignalPrice(ctx, sigID, sig.SignalPrice); err != nil {
				logger.Warn("update signal price", "id", sigID, "err", err)
			}
		}
	}
	if sig.SignalPrice <= 0 {
		reason := "signal price unavailable (no recent candles)"
		if err := store.UpdateSignalStatus(ctx, sigID, "blocked", reason); err != nil {
			logger.Warn("update signal status", "id", sigID, "err", err)
		}
		logger.Warn("signal execution skipped — missing signal price",
			"ticker", sig.Ticker,
		)
		e.bus.Publish(events.SignalBlocked, events.SignalBlockedPayload{
			Ticker: sig.Ticker, Action: sig.Action, Confidence: sig.Confidence,
			BlockReason: reason, FailedCheck: "signal price",
		})
		return
	}

	// SELL = close the ticker's open long (sell_exit.go, #436). Routed
	// AFTER the signal-price validation above so a SELL can never execute
	// (or worse, close lots) against a zero price — the paper executor
	// fills market orders at LimitPrice, so a zero would become a permanent
	// fabricated ~-100%% exit that no reconciler heals. The old branch here
	// placed a cash-sized market sell and booked a brand-new SHORT trade
	// row, leaving the long rows open forever — phantom long+short pairs.
	// Genuine shorting was never supported: the ownership gate (#306) only
	// lets SELL through when an open long exists.
	if sig.Action == string(analysis.ActionSell) {
		e.executeSellExit(ctx, sigID, sig)
		return
	}

	// A pullback entry's edge and risk are anchored to the signal-session low.
	// Revalidate against a fresh mark immediately before sizing/order placement:
	// LLM latency, manual approval, or a restart may otherwise leave an already
	// breached/inverted stop attached to a new position.
	if sig.PullbackStrength {
		now := time.Now()
		evidence, err := e.pullbackExecutionEvidence(
			ctx, sig.Ticker, sig.CreatedAt, now,
		)
		if err != nil {
			reason := "pullback strength: unable to verify fresh execution price and post-signal path"
			_ = store.UpdateSignalStatus(ctx, sigID, "blocked", reason)
			logger.Warn("pullback execution blocked",
				"ticker", sig.Ticker, "reason", reason, "err", err)
			e.bus.Publish(events.SignalBlocked, events.SignalBlockedPayload{
				Ticker: sig.Ticker, Action: sig.Action, Confidence: sig.Confidence,
				BlockReason: reason, FailedCheck: "pullback_execution_evidence",
			})
			return
		}
		mark := evidence.Mark
		if reason := pullback.ValidateExecutionMark(
			mark, sig.StopLoss,
			e.cfg.Risk.EntryGates.PullbackMaxStopWidthPct,
		); reason != "" {
			_ = store.UpdateSignalStatus(ctx, sigID, "blocked", reason)
			logger.Warn("pullback execution blocked",
				"ticker", sig.Ticker, "mark", mark,
				"stop_loss", sig.StopLoss, "reason", reason)
			e.bus.Publish(events.SignalBlocked, events.SignalBlockedPayload{
				Ticker: sig.Ticker, Action: sig.Action, Confidence: sig.Confidence,
				BlockReason: reason, FailedCheck: "pullback_execution_stop",
			})
			return
		}
		minTraded := evidence.MinimumTraded
		if minTraded <= sig.StopLoss {
			reason := fmt.Sprintf(
				"pullback strength: setup invalidated before execution (minimum traded %.2f at/below low-of-day stop %.2f)",
				minTraded, sig.StopLoss,
			)
			_ = store.UpdateSignalStatus(ctx, sigID, "blocked", reason)
			logger.Warn("pullback execution blocked",
				"ticker", sig.Ticker, "mark", mark, "minimum_traded", minTraded,
				"stop_loss", sig.StopLoss, "reason", reason)
			e.bus.Publish(events.SignalBlocked, events.SignalBlockedPayload{
				Ticker: sig.Ticker, Action: sig.Action, Confidence: sig.Confidence,
				BlockReason: reason, FailedCheck: "pullback_execution_path",
			})
			return
		}
		sig.SignalPrice = mark
		if err := store.UpdateSignalPrice(ctx, sigID, mark); err != nil {
			logger.Warn("update pullback execution price", "id", sigID, "err", err)
		}

		openTrades, err := store.GetOwnedPositionTrades(ctx)
		if err != nil {
			reason := "pullback execution risk re-check failed: unable to verify open positions"
			_ = store.UpdateSignalStatus(ctx, sigID, "blocked", reason)
			logger.Warn("pullback execution blocked",
				"ticker", sig.Ticker, "reason", reason, "err", err)
			return
		}
		riskIn := buildRiskInput(e.cfg.Risk, sig.Action, sig.Ticker,
			sig.Confidence, sig.SizePct, sig.StopLoss, sig.TakeProfit, mark,
			e.portfolio.CurrentState(), openTrades)
		if results, passed := risk.CheckAll(riskIn); !passed {
			reason := "pullback execution risk re-check failed: " + risk.BlockReason(results)
			_ = store.UpdateSignalStatus(ctx, sigID, "blocked", reason)
			logger.Warn("pullback execution blocked",
				"ticker", sig.Ticker, "mark", mark, "reason", reason)
			e.bus.Publish(events.SignalBlocked, events.SignalBlockedPayload{
				Ticker: sig.Ticker, Action: sig.Action, Confidence: sig.Confidence,
				BlockReason: reason, FailedCheck: risk.BlockReason(results),
			})
			return
		}
	}

	// An `exiting` trade still owns broker quantity until reconciliation
	// proves the sell terminal. Never layer a fresh BUY onto that in-flight
	// state: it defeats per-trade order attribution and can make a late exit
	// fill sell shares from the new position.
	if store.IsConnected() {
		owned, err := store.GetOwnedPositionTrades(ctx)
		if err != nil {
			reason := "unable to verify in-flight exits (DB error)"
			_ = store.UpdateSignalStatus(ctx, sigID, "blocked", reason)
			logger.Warn("signal execution blocked", "ticker", sig.Ticker, "reason", reason, "err", err)
			return
		}
		if tickerHasExitingLongIn(owned, sig.Ticker) {
			reason := "prior long exit is still reconciling"
			_ = store.UpdateSignalStatus(ctx, sigID, "blocked", reason)
			logger.Warn("signal execution blocked", "ticker", sig.Ticker, "reason", reason)
			e.bus.Publish(events.SignalBlocked, events.SignalBlockedPayload{
				Ticker: sig.Ticker, Action: sig.Action, Confidence: sig.Confidence,
				BlockReason: reason, FailedCheck: "exit_ownership",
			})
			return
		}
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

	// The uncapped exit policy is active only with the trailing manager: this
	// guarantees the position retains a broker hard stop while the selected
	// trail owns the winning exit. Keeping the manager requirement here makes
	// an accidentally-partial config fail safe to the existing fixed TP.
	uncapRequested := side == execution.OrderSideBuy && e.cfg.Risk.UncapTakeProfit
	uncapTakeProfit, uncappedQty := resolveUncappedEntry(
		e.cfg.Risk, side, qty, sig.SignalPrice, sig.StopLoss,
		e.trailingManagerAvailable,
	)
	if uncapTakeProfit {
		orderReq.Quantity = uncappedQty
	} else if uncapRequested {
		logger.Warn("exit policy: uncapped entry lacks a running trail, whole-share sizing, or a coherent hard stop; keeping baseline fixed take-profit",
			"ticker", sig.Ticker,
			"qty", qty,
			"signal_price", sig.SignalPrice,
			"stop_loss", sig.StopLoss,
		)
	}

	// Broker-resident bracket exits (#435, see bracket_exits.go). BUYs with a
	// coherent stop/target become one atomic bracket: entry + TP limit + SL
	// stop, GTC so the legs survive the session close. Whole shares only —
	// Alpaca rejects fractional complex orders — so sub-one-share sizings
	// fall back to the plain market entry with app-managed exits.
	bracketed := false
	if stockBracketEligible(e.cfg.Risk, side, sig.SignalPrice, sig.StopLoss, sig.TakeProfit, uncapTakeProfit) {
		if whole := math.Floor(qty); whole >= 1 {
			orderReq.Quantity = whole
			orderReq.OrderClass = "bracket"
			orderReq.TakeProfitPrice = sig.TakeProfit
			orderReq.StopLossPrice = sig.StopLoss
			orderReq.TimeInForce = "gtc"
			bracketed = true
		} else {
			logger.Warn("bracket exits: sized under one whole share — falling back to app-managed exits",
				"ticker", sig.Ticker, "qty", qty)
		}
	}
	if uncapTakeProfit {
		logger.Info("exit policy: fixed take-profit uncapped; trailing manager owns winning exit",
			"ticker", sig.Ticker,
			"trailing_rule", e.cfg.Risk.TrailingStop.Rule,
		)
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
	// The software position monitor reads trades.take_profit. Clearing the
	// persisted value is load-bearing: merely omitting the broker bracket
	// would still let the hourly monitor cap the winner at the model TP.
	tradeTakeProfit := persistedTakeProfit(sig.TakeProfit, uncapTakeProfit)
	trade := store.Trade{
		SignalID:   sigID,
		Ticker:     sig.Ticker,
		Direction:  direction,
		Strategy:   sig.Strategy,
		EntryPrice: result.FilledPrice,
		Quantity:   result.FilledQty,
		StopLoss:   sig.StopLoss,
		TakeProfit: tradeTakeProfit,
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
		Status:        execution.NormalizeOrderStatus(result.Status),
	}
	if _, err := store.InsertOrder(ctx, entryOrder); err != nil {
		logger.Warn("insert entry order", "tradeID", tradeID, "err", err)
	}
	if result.FilledQty == 0 && execution.IsTerminalOrderStatus(result.Status) {
		// A synchronously rejected/cancelled entry never enters the fill poll.
		// Remove its zero-quantity trade immediately so it cannot occupy a
		// position slot forever.
		if cancelled, err := store.CancelUnfilledTrade(ctx, tradeID); err != nil {
			logger.Warn("cancel synchronous zero-fill trade", "trade_id", tradeID, "err", err)
		} else if cancelled && result.BrokerOrderID != "" {
			if err := store.MarkOrderStatus(
				ctx, result.BrokerOrderID, execution.NormalizeOrderStatus(result.Status),
			); err != nil {
				logger.Warn("mark synchronous zero-fill order", "trade_id", tradeID, "err", err)
			}
		}
		return
	}

	initialProtectedQty := 0.0
	if uncapTakeProfit && result.FilledQty > 0 {
		if err := e.ensureUncappedProtectiveStop(
			ctx, tradeID, sig.Ticker, result.FilledQty, sig.StopLoss,
		); err != nil {
			logger.Error("uncapped entry filled without immediate broker stop",
				"trade_id", tradeID,
				"ticker", sig.Ticker,
				"qty", result.FilledQty,
				"stop_loss", sig.StopLoss,
				"err", err,
			)
		} else {
			initialProtectedQty = result.FilledQty
		}
	}

	// Persist the bracket's child exit legs so the monitor knows the trade
	// is broker-managed (reconcileBracketTrade) and the trailing manager
	// finds the SL leg to PATCH-ratchet. ChildOrderIDs is [TP, SL]. A broker
	// that returned no legs (e.g. the in-memory paper executor) leaves the
	// trade app-managed — graceful degradation, loudly logged.
	if bracketed {
		if len(result.ChildOrderIDs) >= 2 {
			legRows := []store.Order{
				{
					TradeID: tradeID, BrokerOrderID: result.ChildOrderIDs[0],
					Ticker: sig.Ticker, Side: string(execution.OrderSideSell),
					OrderType: string(execution.OrderTypeLimit), Quantity: orderReq.Quantity,
					LimitPrice: sig.TakeProfit, Status: "pending",
					IsTakeProfit: true, ParentOrderID: result.BrokerOrderID,
				},
				{
					TradeID: tradeID, BrokerOrderID: result.ChildOrderIDs[1],
					Ticker: sig.Ticker, Side: string(execution.OrderSideSell),
					OrderType: string(execution.OrderTypeStop), Quantity: orderReq.Quantity,
					StopPrice: sig.StopLoss, Status: "pending",
					IsStopLoss: true, ParentOrderID: result.BrokerOrderID,
				},
			}
			for _, leg := range legRows {
				if _, err := store.InsertOrder(ctx, leg); err != nil {
					logger.Warn("insert bracket leg order", "tradeID", tradeID,
						"broker_order_id", leg.BrokerOrderID, "err", err)
				}
			}
			logger.Info("bracket exit legs resident at broker",
				"trade_id", tradeID, "ticker", sig.Ticker,
				"take_profit", sig.TakeProfit, "stop_loss", sig.StopLoss)
		} else {
			logger.Warn("bracket requested but broker returned no child legs — exits remain app-managed",
				"trade_id", tradeID, "ticker", sig.Ticker)
		}
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

	// Poll every nonterminal broker order and backfill until it reaches a final
	// state. A nonzero price does not imply completion: Alpaca can return an
	// immediate partial fill, then continue filling the remaining quantity.
	// Stopping at that first partial freezes the smaller quantity as trade truth
	// and leaves later shares untracked and under-protected.
	if shouldPollEntryFill(result) {
		brokerOrderID := result.BrokerOrderID
		executor := e.executor
		go func() {
			// 40 × 3s = 2 min: fractional market orders can fill in several
			// parts; the old 24s window was part of how a partial got frozen.
			const maxRetries = 40
			const retryDelay = 3 * time.Second
			backfill := func(status execution.OrderResult) {
				bfCtx, bfCancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer bfCancel()
				if err := store.ApplyEntryFill(
					bfCtx, tradeID, brokerOrderID,
					status.FilledPrice, status.FilledQty,
					execution.NormalizeOrderStatus(status.Status),
				); err != nil {
					logger.Warn("fill poll: atomically apply entry fill",
						"trade_id", tradeID, "broker_order_id", brokerOrderID, "err", err)
				}
			}
			protect := func(status execution.OrderResult) error {
				bfCtx, bfCancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer bfCancel()
				if err := e.ensureUncappedProtectiveStop(
					bfCtx, tradeID, sig.Ticker, status.FilledQty, sig.StopLoss,
				); err != nil {
					logger.Error("fill poll: uncapped position lacks full broker-stop coverage",
						"trade_id", tradeID,
						"ticker", sig.Ticker,
						"filled_qty", status.FilledQty,
						"stop_loss", sig.StopLoss,
						"err", err,
					)
					return err
				}
				return nil
			}
			lastRecordedQty := result.FilledQty
			lastProtectedQty := initialProtectedQty
			for i := 0; i < maxRetries; i++ {
				time.Sleep(retryDelay)
				pollCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				status, err := executor.GetOrderStatus(pollCtx, brokerOrderID)
				cancel()
				if err != nil {
					logger.Warn("fill poll: GetOrderStatus error",
						"broker_order_id", brokerOrderID, "attempt", i+1, "err", err)
					continue
				}
				if status.FilledPrice > 0 && status.FilledQty != lastRecordedQty {
					backfill(status)
					lastRecordedQty = status.FilledQty
				}
				exitInFlight := false
				stateCtx, stateCancel := context.WithTimeout(context.Background(), 5*time.Second)
				trade, getErr := store.GetTradeByID(stateCtx, tradeID)
				stateCancel()
				if getErr == nil &&
					trade != nil && trade.Status == "exiting" {
					exitInFlight = true
					// A protective stop or another exit worker has started
					// selling this partial entry. Stop the still-working BUY
					// from adding new, unprotected shares behind that exit.
					if !execution.IsTerminalOrderStatus(status.Status) {
						cancelCtx, cancelEntry := context.WithTimeout(context.Background(), 5*time.Second)
						if cancelErr := executor.CancelOrder(
							cancelCtx, brokerOrderID,
						); cancelErr != nil {
							logger.Warn("fill poll: cancel entry after exit ownership transfer",
								"trade_id", tradeID,
								"broker_order_id", brokerOrderID,
								"err", cancelErr)
						}
						cancelEntry()
					}
				}
				if uncapTakeProfit && !exitInFlight &&
					status.FilledQty > lastProtectedQty {
					// Protection progress is deliberately independent from
					// fill persistence. A transient resize error leaves this
					// quantity behind so the next identical poll retries it.
					nextProtectedQty, err := advanceProtectedQuantity(
						lastProtectedQty, status.FilledQty,
						func() error { return protect(status) },
					)
					if err == nil {
						lastProtectedQty = nextProtectedQty
					}
				}
				if execution.IsTerminalOrderStatus(status.Status) {
					if status.FilledQty == 0 && lastRecordedQty == 0 {
						// Terminal with ZERO fill (expired / cancelled /
						// rejected): the entry never happened. Without this,
						// the qty-0 row stayed "open" forever — the IREN/MU/
						// TSLA phantom rows that inflated the position count.
						e.cancelUnfilledEntry(tradeID, brokerOrderID, sig.Ticker)
						return
					}
					if uncapTakeProfit && status.FilledQty > lastProtectedQty {
						logger.Warn("fill poll: terminal fill still lacks full broker-stop coverage; retrying",
							"trade_id", tradeID,
							"ticker", sig.Ticker,
							"filled_qty", status.FilledQty,
							"protected_qty", lastProtectedQty,
						)
						continue
					}
					logger.Info("fill poll: order terminal",
						"trade_id", tradeID,
						"ticker", sig.Ticker,
						"status", status.Status,
						"entry_price", status.FilledPrice,
						"qty", status.FilledQty,
					)
					return
				}
				logger.Debug("fill poll: order not terminal yet, retrying",
					"broker_order_id", brokerOrderID, "attempt", i+1, "status", status.Status, "filled_qty", status.FilledQty)
			}
			if lastRecordedQty == 0 {
				// Two minutes of polling an in-hours market order with zero
				// fill: something is wrong. Cancel rather than leave a qty-0
				// "open" trade the position counter treats as real.
				e.cancelUnfilledEntry(tradeID, brokerOrderID, sig.Ticker)
				return
			}
			logger.Warn("fill poll: order not terminal after max retries — recorded last known fill; reconciler will surface any residue",
				"trade_id", tradeID,
				"broker_order_id", brokerOrderID,
				"ticker", sig.Ticker,
				"last_filled_qty", lastRecordedQty,
				"protected_qty", lastProtectedQty,
			)
		}()
	}
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

	// Re-check position ownership at execution time: the long may have been
	// closed between signal generation and manual approval.
	if sig.Action == "SELL" {
		openTrades, _ := store.GetOpenTrades(ctx)
		var holdsLong bool
		for _, t := range openTrades {
			if t.Ticker == sig.Ticker && t.Direction == "long" {
				holdsLong = true
				break
			}
		}
		if !holdsLong {
			if updateErr := store.UpdateSignalStatus(ctx, sigID, "blocked",
				"no open long position in "+sig.Ticker+" — cannot sell (position closed since approval)"); updateErr != nil {
				logger.Warn("update signal status", "err", updateErr)
			}
			return fmt.Errorf("signal %s blocked: no open long position in %s", sigID, sig.Ticker)
		}
	}

	e.executeSignal(ctx, sigID, *sig)

	// executeSignal reports blocks by updating the signal row, not by
	// returning an error — re-fetch so a manual approver (CLI/Telegram)
	// gets the real outcome instead of "approved and executing" while the
	// signal sits blocked (e.g. the market-hours guard on an after-hours
	// approval, review catch on #437).
	if after, err := store.GetSignalByID(ctx, sigID); err == nil && after != nil && after.Status == "blocked" {
		return fmt.Errorf("signal %s blocked at execution: %s", sigID, after.BlockReason)
	}
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
			Quantity:     p.Quantity,
			AvgCostBasis: p.AvgCostBasis,
			CurrentPrice: p.CurrentPrice,
			CurrentValue: p.CurrentValue,
			UnrealizedPL: p.UnrealizedPL,
			Side:         p.Side,
		})
	}

	// Prefer broker daily P&L (realized + unrealized today) as the source of truth.
	// Fall back to local realized P&L calculation for PaperExecutor or if broker
	// does not support day_pnl reporting.
	var realizedToday float64
	var cash float64
	var totalEquity float64
	var dayPnL float64

	if acct, err := e.executor.GetAccount(ctx); err != nil {
		logger.Warn("get broker account for portfolio refresh — falling back to local derivation", "err", err)
	} else {
		cash = acct.Cash
		totalEquity = acct.PortfolioEquity
		dayPnL = acct.DayPnL
	}

	if dayPnL == 0 {
		// Local fallback: sum realized P&L from trades closed today (only when
		// the broker does not provide it to avoid unnecessary DB load).
		closedTrades, _ := store.GetRecentTrades(ctx, 100)
		today := time.Now().UTC().Truncate(24 * time.Hour)
		for _, t := range closedTrades {
			if t.Status == "closed" && t.ClosedAt.After(today) {
				realizedToday += t.PnL
			}
		}
	}

	if cash == 0 {
		// Local fallback: initial_capital minus cost basis of open positions.
		cash = e.cfg.Engine.InitialCapital
		for _, p := range positions {
			cash -= p.CurrentValue - p.UnrealizedPL
		}
		if cash < 0 {
			cash = 0
		}
	}

	if totalEquity == 0 {
		totalEquity = cash
		for _, p := range positions {
			totalEquity += p.CurrentValue
		}
	}

	e.portfolio.Update(eps, cash, totalEquity, dayPnL, realizedToday)
}

// ─── Phase 4: Position monitoring (4.7) ───────────────────────────────────────

// maxMonitorCandleAge bounds how old a candle may be and still drive SL/TP
// decisions in monitorPositions. Daily bars are stamped at the session date
// (04:00 UTC), so for a daily-only ticker the freshest bar is already ~5.5
// days old at the Tuesday open after a Monday holiday (Thu bar → Tue 13:30
// UTC). 7 days tolerates every ordinary holiday-weekend shape without
// suspending stop monitoring, while still rejecting genuinely dead data
// (LAC's freshest 1h candle was 31 days old when it triggered a phantom
// take-profit).
const maxMonitorCandleAge = 7 * 24 * time.Hour

// pollExitFill polls the broker for a short window waiting for an exit order
// to fill, returning the confirmed OrderResult. Market exits typically fill
// within seconds; blocking the monitor loop briefly is preferable to
// recording a guessed exit price. Returns ok=false if the fill hasn't
// confirmed inside the window — the caller records the row as pending and the
// exit-fill reconciler corrects it later.
func (e *Engine) pollExitFill(ctx context.Context, brokerOrderID string) (execution.OrderResult, bool) {
	const attempts = 5
	const delay = 2 * time.Second
	for i := 0; i < attempts; i++ {
		select {
		case <-ctx.Done():
			return execution.OrderResult{}, false
		case <-time.After(delay):
		}
		status, err := e.executor.GetOrderStatus(ctx, brokerOrderID)
		if err != nil {
			logger.Warn("exit fill poll: GetOrderStatus",
				"broker_order_id", brokerOrderID, "attempt", i+1, "err", err)
			continue
		}
		if execution.NormalizeOrderStatus(status.Status) == "filled" && status.FilledPrice > 0 {
			return status, true
		}
	}
	return execution.OrderResult{}, false
}

// exitReconcileInterval throttles the exit-fill reconciler sweep inside the
// monitor loop.
const exitReconcileInterval = 10 * time.Minute

// monitorPositions checks all open trades for stop-loss and take-profit breaches.
// checkExitResidue verifies, shortly after an exit, that the broker holds no
// more of `ticker` than the DB's remaining open trades account for. Fires the
// comparison after a settle delay so async exit fills are reflected in the
// broker's position feed. Detection-only: residue is logged at ERROR (and
// surfaced by the startup reconciler / adopt_broker_orphans on next boot).
func (e *Engine) checkExitResidue(ticker string) {
	executor := e.executor
	go func() {
		time.Sleep(15 * time.Second)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		positions, err := executor.GetPositions(ctx)
		if err != nil || positions == nil {
			return // paper executor reports none; broker errors are non-fatal here
		}
		var brokerQty float64
		for _, p := range positions {
			if strings.EqualFold(p.Ticker, ticker) {
				brokerQty += math.Abs(p.Quantity)
			}
		}
		if brokerQty == 0 {
			return
		}
		open, err := store.GetOpenTrades(ctx)
		if err != nil {
			return
		}
		var dbQty float64
		for _, t := range open {
			if strings.EqualFold(t.Ticker, ticker) {
				dbQty += math.Abs(t.Quantity)
			}
		}
		if residue := brokerQty - dbQty; residue > DefaultBrokerDriftToleranceShares {
			logger.Error("exit residue: broker still holds untracked shares after exit — orphaned position",
				"ticker", ticker,
				"broker_qty", brokerQty,
				"db_open_qty", dbQty,
				"residue", residue,
			)
		}
	}()
}

func (e *Engine) monitorPositions(ctx context.Context) {
	// Refresh portfolio state from executor positions.
	e.refreshPortfolio(ctx)

	// Sweep exit orders whose broker fills were never confirmed and align the
	// closed trades' recorded exits with broker truth. Runs in the background:
	// a backlog is up to DefaultExitReconcileBatch GetOrderStatus calls, and a
	// slow broker API must never sit between the tick and the safety-critical
	// SL/TP checks below. The CAS guard prevents overlapping sweeps; the
	// detached context keeps a sweep from being cancelled when the tick
	// returns, bounded by its own timeout.
	if time.Since(e.lastExitReconcile) >= exitReconcileInterval && e.exitReconcileInFlight.CompareAndSwap(false, true) {
		e.lastExitReconcile = time.Now()
		go func() {
			defer e.exitReconcileInFlight.Store(false)
			sweepCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			ReconcileExitOrderFillsWithStoreAndLoss(
				sweepCtx, e.executor, DefaultExitReconcileBatch, e.portfolio.RecordLoss,
			)
		}()
	}

	trades, err := store.GetOpenTrades(ctx)
	if err != nil {
		logger.Warn("get open trades for monitoring", "err", err)
		return
	}
	if len(trades) == 0 {
		return
	}

	for _, t := range trades {
		// Broker-resident exits (#435): a trade with working bracket legs, or
		// an uncapped trade with a standalone hard stop, is protected at the
		// broker — reconcile fills into the trades table and NEVER run the
		// software price-compare exit on top of live legs (double-sell).
		// Deliberately NOT gated on
		// bracket_exits_enabled: the flag only decides whether NEW entries
		// become brackets — an existing bracket trade must stay
		// broker-managed even after the flag is flipped off, or this loop
		// would market-sell shares the live legs still hold. Cheap no-op
		// (one indexed query) for trades without legs. Checked before the
		// candle logic so a leg fill is booked even when candles are stale.
		if handled := e.reconcileBracketTrade(ctx, t); handled {
			continue
		}
		// Use the FRESHEST candle across all timeframes, not the finest
		// available one. Data collection can stop on a timeframe (ticker
		// dropped off the watchlist, backfill gap) while coarser timeframes
		// stay current — a month-old 1h candle must not beat a same-day 4h
		// candle. A stale 1h close of 5.50 did exactly that for LAC on
		// 2026-07-02: it crossed the 4.60 take-profit and closed a position
		// the market (at 3.82) never justified closing.
		var currentPrice float64
		var priceTS time.Time
		for _, tf := range []data.Timeframe{data.Timeframe5Min, data.Timeframe15Min, data.Timeframe1Hour, data.Timeframe4Hour, data.Timeframe1Day} {
			latest, err := store.GetLatestCandle(ctx, t.Ticker, tf)
			if err == nil && latest != nil && latest.Close > 0 && latest.Timestamp.After(priceTS) {
				currentPrice = latest.Close
				priceTS = latest.Timestamp
			}
		}
		if currentPrice == 0 {
			continue
		}
		// Even the freshest candle can be dangerously old (data collection
		// dead for the ticker entirely). Acting on it risks phantom SL/TP
		// triggers, so skip — loudly, because it also means real stops are
		// NOT being monitored for this position. The cap allows a long
		// weekend + holiday gap on daily candles.
		if age := time.Since(priceTS); age > maxMonitorCandleAge {
			logger.Warn("position monitor: all candle data stale — SL/TP not being checked",
				"ticker", t.Ticker,
				"trade_id", t.ID,
				"latest_candle", priceTS.Format(time.RFC3339),
				"age", age.Round(time.Minute).String(),
			)
			continue
		}

		var shouldClose bool
		var exitReason string

		if t.StopLoss > 0 {
			if (t.Direction == "long" && currentPrice <= t.StopLoss) ||
				(t.Direction == "short" && currentPrice >= t.StopLoss) {
				shouldClose = true
				exitReason = "stop-loss hit"
			}
		}

		if !shouldClose && t.TakeProfit > 0 {
			if (t.Direction == "long" && currentPrice >= t.TakeProfit) ||
				(t.Direction == "short" && currentPrice <= t.TakeProfit) {
				shouldClose = true
				exitReason = "take-profit hit"
			}
		}

		if !shouldClose {
			continue
		}

		claimed, err := store.ClaimTradeExit(ctx, t.ID)
		if err != nil {
			logger.Warn("claim trade exit", "trade_id", t.ID, "err", err)
			continue
		}
		if !claimed {
			// Another worker (EMA manager, analyst SELL, or this monitor on
			// an overlapping sweep) already owns the broker exit.
			continue
		}

		// A capped ATR trade can carry a standalone broker stop while its
		// winning take-profit remains software-managed. Cancel and confirm
		// every such leg after winning exit ownership but before submitting
		// the market order; otherwise the TP sell races a still-live stop and
		// can flip the account short.
		remainingByTrade, remainingQty, clear, releaseSafe :=
			e.cancelClaimedExitLegs(ctx, []store.Trade{t}, t.Ticker)
		if !clear {
			if releaseSafe {
				_ = store.ReleaseTradeExitClaim(ctx, t.ID)
			}
			logger.Warn("position monitor: exit legs could not be confirmed cancelled",
				"trade_id", t.ID, "ticker", t.Ticker, "reason", exitReason)
			continue
		}
		if remaining := remainingByTrade[t.ID]; remaining > 0 {
			t.Quantity = remaining
		} else {
			t.Quantity = remainingQty
		}

		// Place exit order.
		exitSide := execution.OrderSideSell
		if t.Direction == "short" {
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
			// A POST can fail after broker acceptance. Retain ownership and
			// let aged orderless-exit recovery inspect broker truth.
			logger.Warn("exit order placement ambiguous — claim retained",
				"trade_id", t.ID, "reason", exitReason, "err", err)
			continue
		}

		if result.FilledPrice == 0 && result.BrokerOrderID != "" {
			if filled, ok := e.pollExitFill(ctx, result.BrokerOrderID); ok {
				result = filled
			}
		}

		exitOrder := store.Order{
			TradeID:       t.ID,
			BrokerOrderID: result.BrokerOrderID,
			Ticker:        t.Ticker,
			Side:          string(exitSide),
			OrderType:     string(execution.OrderTypeMarket),
			Quantity:      t.Quantity,
			FilledQty:     result.FilledQty,
			Status:        execution.NormalizeOrderStatus(result.Status),
			IsStopLoss:    exitReason == "stop-loss hit",
			IsTakeProfit:  exitReason == "take-profit hit",
		}
		exitOrder.FilledPrice = result.FilledPrice
		if _, err := store.InsertOrderWithRetry(ctx, exitOrder); err != nil {
			// Keep the durable claim: releasing it after broker acceptance can
			// submit a duplicate sell. Startup reconciliation still counts the
			// exiting quantity and surfaces the missing order for repair.
			logger.Error("insert accepted exit order — trade remains exiting",
				"trade_id", t.ID, "broker_order_id", result.BrokerOrderID, "err", err)
			continue
		}

		status := execution.NormalizeOrderStatus(result.Status)
		if (status == "cancelled" || status == "rejected") && result.FilledQty == 0 {
			_ = store.ReleaseTradeExitClaim(ctx, t.ID)
			continue
		}
		fullFill := status == "filled" && result.FilledPrice > 0 &&
			result.FilledQty >= t.Quantity-1e-9
		if !fullFill {
			// Reconciler finalizes a full fill or reopens only the unsold
			// remainder after a terminal partial/cancel.
			continue
		}

		exitPrice := result.FilledPrice
		realized, err := store.CloseTradeRealized(ctx, t.ID, exitPrice)
		if err != nil {
			logger.Warn("close confirmed trade", "trade_id", t.ID, "err", err)
			continue
		}
		if realized.PnL < 0 {
			e.portfolio.RecordLoss()
		}

		logger.Info("trade closed",
			"trade_id", t.ID,
			"ticker", t.Ticker,
			"reason", exitReason,
			"pnl", fmt.Sprintf("%.2f", realized.PnL),
			"pnl_pct", fmt.Sprintf("%.2f%%", realized.PnLPct),
		)
		e.bus.Publish(events.TradeClosed, events.TradeClosedPayload{
			TradeID: t.ID, Ticker: t.Ticker, Direction: t.Direction,
			EntryPrice: t.EntryPrice, ExitPrice: realized.ExitPrice,
			PnL: realized.PnL, PnLPct: realized.PnLPct, Reason: exitReason,
		})

		// Residue check: after the exit settles, the broker must hold no more
		// of this ticker than the DB still tracks. This is how the NOK orphan
		// stayed invisible — exits sold the recorded quantity and nobody
		// compared against the broker's remaining position. Async: the exit
		// fill needs a moment to settle before positions reflect it.
		e.checkExitResidue(t.Ticker)
	}

	// Update portfolio state after monitoring.
	snap := e.portfolio.MaybeSnapshot(ctx)
	if snap != nil {
		// Best-effort trade-row count so live ticks carry the divergence
		// signal alongside OpenPositions. Nil on error/disconnected DB —
		// the frontend treats null as "hide divergence", matching the REST
		// shape from /api/node/portfolio.
		var openTradeRows *int
		if store.IsConnected() {
			if n, dbErr := store.CountOpenTrades(ctx); dbErr == nil {
				openTradeRows = &n
			} else {
				logger.Warn("CountOpenTrades failed; portfolio snapshot omits open_trade_rows", "err", dbErr)
			}
		}
		e.bus.Publish(events.PortfolioSnapshotTaken, events.PortfolioSnapshotPayload{
			TotalValue:       snap.TotalValue,
			Cash:             snap.Cash,
			Invested:         snap.Invested,
			UnrealizedPnL:    snap.UnrealizedPnL,
			RealizedPnLToday: snap.RealizedPnLDay,
			DayPnL:           snap.DayPnL,
			OpenPositions:    snap.OpenPositions,
			OpenTradeRows:    openTradeRows,
		})
	}

	// Throttle daily summary updates: run at most every 5 minutes to avoid
	// excessive DB query + upsert load on every monitorPositions invocation.
	const dailySummaryUpdateInterval = 5 * time.Minute
	if time.Since(e.lastDailySummaryUpdate) >= dailySummaryUpdateInterval {
		e.updateDailySummary(ctx)
		e.lastDailySummaryUpdate = time.Now()
	}
}

// updateDailySummary upserts a daily_summaries row for the current date.
func (e *Engine) updateDailySummary(ctx context.Context) {
	now := time.Now().UTC()
	today := now.Format("2006-01-02")
	if e.todaySummaryDate != today {
		// New day or engine startup — determine the true beginning-of-day equity.
		// Priority: (1) existing DB row preserves its starting_value; (2) last
		// snapshot written before today (previous-day close); (3) first snapshot
		// of today (engine started mid-session); (4) live tracker state.
		e.todaySummaryDate = today
		todayStart := now.Truncate(24 * time.Hour)
		if s, _ := store.GetDailySummaryByDate(ctx, todayStart); s != nil {
			e.todayStartingEquity = s.StartingValue
		} else if snap, _ := store.GetLastSnapshotBeforeDay(ctx, todayStart); snap != nil {
			e.todayStartingEquity = snap.TotalValue
		} else if snap, _ := store.GetFirstSnapshotOfDay(ctx, todayStart); snap != nil {
			e.todayStartingEquity = snap.TotalValue
		} else {
			e.todayStartingEquity = e.portfolio.CurrentState().TotalValue
		}
	}

	// Fetch current day's trade stats from store (simplest way to ensure accuracy).
	start := now.Truncate(24 * time.Hour)
	end := start.Add(24 * time.Hour)
	trades, _ := store.GetClosedTradesByDateRange(ctx, start, end)

	var wins, losses int
	for _, t := range trades {
		if t.PnL > 0 {
			wins++
		} else if t.PnL < 0 {
			losses++
		}
	}

	// Token usage from cost tracker.
	tokStats := e.costTracker.TakeSnapshot()

	// Issue #325: aggregate observability counters (signals_total /
	// signals_executed / executed_winning|losing / blocked_by_reason). On DB
	// error we MUST skip the upsert entirely — otherwise the ON CONFLICT path
	// would overwrite the previously-populated row's signals_total /
	// signals_executed / blocked_by_reason columns with zeros, silently
	// erasing today's accumulated counters. updateDailySummary runs on a
	// throttled loop, so the next tick will retry; there's no risk of a
	// permanently missing row.
	counters, err := store.AggregateDailyCounters(ctx, start)
	if err != nil {
		logger.Warn("aggregate daily counters; skipping upsert to preserve prior row", "err", err)
		return
	}

	// Perform the upsert. Prefer the aggregated signal counts over the
	// historical zero-value placeholders so the legacy signals_generated /
	// signals_blocked columns now reflect reality too.
	e.portfolio.MaybeDailySummary(ctx,
		e.todayStartingEquity,
		0,           // tradesOpened (placeholder, could track in engine if needed)
		len(trades), // tradesClosed
		wins,
		losses,
		counters.SignalsTotal,   // signalGenerated — total daily signals
		counters.SignalsBlocked, // signalBlocked — daily blocked subset
		tokStats.DailyTokens,    // total tokens as input for now
		0,                       // tokens output
		counters,
	)
}

// processDeferredSignals executes BUY signals deferred by the first-tick
// gate once their price has held, and marks the rest stale. Runs at the
// start of every tick, auto/paper mode only. Only rows stamped
// deferred=TRUE at generation are swept (GetDeferredSignals) — manual-mode
// approval pendings are structurally out of reach even after a mode switch
// or restart. Before executing, the portfolio-level risk gates are re-run
// against CURRENT state: cash, open-position count, daily loss, and
// cooldown may all have changed in the tick since the signal was generated.
func (e *Engine) processDeferredSignals(ctx context.Context) {
	if !e.cfg.Risk.EntryGates.FirstTickDefer {
		return
	}
	mode := e.Mode()
	if mode != "auto" && mode != "paper" {
		return
	}
	deferred, err := store.GetDeferredSignals(ctx)
	if err != nil {
		logger.Warn("get deferred signals", "err", err)
		return
	}
	now := time.Now()
	for _, sig := range deferred {
		if sig.Action != "BUY" {
			continue // defensive: only BUYs are ever deferred
		}
		if !sig.ExpiresAt.IsZero() && now.After(sig.ExpiresAt) {
			continue // the expiry loop owns time-outs
		}
		// Confirm against a LIVE quote, not the candle store: this sweep runs
		// before the tick's candle refresh, so the freshest stored candle is
		// usually the same bar that priced the signal on the previous tick —
		// "confirming" against it would observe no new information at all
		// (Codex P1 on #434). Stored candles are only a fallback.
		confirmPrice := e.freshStockMark(ctx, sig.Ticker, now)
		if confirmPrice <= 0 {
			// No mark to confirm against — keep pending; either a later tick
			// confirms or the expiry loop cleans up.
			logger.Warn("deferred signal: no price to confirm against — keeping pending",
				"id", sig.ID, "ticker", sig.Ticker)
			continue
		}
		tol := e.cfg.Risk.EntryGates.FirstTickHoldTolerancePct
		if !deferredPriceHeld(sig.SignalPrice, confirmPrice, tol) {
			reason := fmt.Sprintf("first-tick confirmation failed: price %.4f below signal %.4f beyond tolerance", confirmPrice, sig.SignalPrice)
			if err := store.UpdateSignalStatus(ctx, sig.ID, "stale", reason); err != nil {
				logger.Warn("mark deferred signal stale", "id", sig.ID, "err", err)
			}
			logger.Info("deferred signal dropped", "id", sig.ID, "ticker", sig.Ticker, "reason", reason)
			continue
		}

		// Price held — re-price the signal to the confirmation mark so risk
		// checks (R:R, dollar-risk), position sizing, and the paper fill all
		// see what the broker would actually fill at, not the stale
		// first-tick price (Codex P2 on #434). A gap up that wrecks the R:R
		// is rejected organically by the re-check below.
		sig.SignalPrice = confirmPrice

		// Re-validate the portfolio-level gates before firing. Fetched per
		// signal (not per sweep) so an execution earlier in this loop counts
		// against the next signal's position budget.
		openTrades, otErr := store.GetOwnedPositionTrades(ctx)
		if otErr != nil {
			logger.Warn("deferred signal: open trades unavailable — retrying next tick",
				"id", sig.ID, "ticker", sig.Ticker, "err", otErr)
			continue
		}
		riskIn := buildRiskInput(e.cfg.Risk, sig.Action, sig.Ticker,
			sig.Confidence, sig.SizePct, sig.StopLoss, sig.TakeProfit, sig.SignalPrice,
			e.portfolio.CurrentState(), openTrades)
		if results, passed := risk.CheckAll(riskIn); !passed {
			reason := "deferred re-check failed: " + risk.BlockReason(results)
			if err := store.UpdateSignalStatus(ctx, sig.ID, "stale", reason); err != nil {
				logger.Warn("mark deferred signal stale", "id", sig.ID, "err", err)
			}
			logger.Info("deferred signal dropped", "id", sig.ID, "ticker", sig.Ticker, "reason", reason)
			continue
		}

		// Persist the re-priced signal so the stored row matches what was
		// risk-checked and executed. Best-effort: an execution at the right
		// price with a stale stored row beats no execution.
		if err := store.UpdateSignalPrice(ctx, sig.ID, confirmPrice); err != nil {
			logger.Warn("update deferred signal price", "id", sig.ID, "err", err)
		}

		logger.Info("deferred signal confirmed — executing",
			"id", sig.ID, "ticker", sig.Ticker, "confirm_price", confirmPrice)
		e.executeSignal(ctx, sig.ID, sig)
	}
}

// freshStockMark returns the freshest available stock mark: a live quote mid
// when the provider answers with a
// two-sided quote, else the freshest stored candle within its recency
// window, else 0 (no mark). One-sided quotes (pre-market / thin books) are
// rejected — FetchQuote's mid is (bid+ask)/2, so a missing side halves the
// price and would wrongly stale a legitimate signal (same guard as the ORB
// spread gate).
func (e *Engine) freshStockMark(ctx context.Context, ticker string, now time.Time) float64 {
	if e.marketData == nil {
		return 0
	}
	if q, err := e.marketData.FetchQuote(ctx, ticker); err != nil {
		logger.Warn("fresh stock mark: quote fetch failed — falling back to stored candles",
			"ticker", ticker, "err", err)
	} else if q != nil && q.Bid > 0 && q.Ask > 0 {
		return q.Last
	}
	latest := resolveFallbackCandle(now, func(tf data.Timeframe) (*data.Candle, error) {
		return store.GetLatestCandle(ctx, ticker, tf)
	})
	if latest == nil {
		return 0
	}
	return latest.Close
}

type pullbackExecutionEvidence struct {
	Mark          float64
	MinimumTraded float64
}

func validatePullbackExecutionQuote(
	quote *data.Quote, since, now time.Time,
) error {
	if quote == nil || quote.Bid <= 0 || quote.Ask <= 0 || quote.Last <= 0 {
		return fmt.Errorf("current two-sided execution quote unavailable")
	}
	quoteAge := now.Sub(quote.Timestamp)
	if quote.Timestamp.IsZero() || quoteAge > pullbackQuoteMaxAge ||
		quoteAge < -pullbackQuoteFutureTolerance {
		return fmt.Errorf(
			"execution quote timestamp is not fresh (age %s)", quoteAge,
		)
	}
	if quote.Timestamp.Before(since) {
		return fmt.Errorf("latest execution quote predates pullback evidence snapshot")
	}
	return nil
}

// pullbackExecutionEvidence requires a current two-sided quote and exact trade
// prints from the qualification-time daily evidence snapshot. Minute OHLC
// cannot distinguish a qualifying pre-snapshot flush from a genuine later stop
// breach inside the same bucket, so providers without the high-resolution
// trade-path capability fail closed for pullback execution.
func (e *Engine) pullbackExecutionEvidence(
	ctx context.Context, ticker string, since, now time.Time,
) (pullbackExecutionEvidence, error) {
	if e.marketData == nil {
		return pullbackExecutionEvidence{}, fmt.Errorf("market data unavailable")
	}
	if since.IsZero() || now.Before(since) {
		return pullbackExecutionEvidence{}, fmt.Errorf(
			"invalid pullback evidence observation time",
		)
	}

	quote, err := e.marketData.FetchQuote(ctx, ticker)
	if err != nil {
		return pullbackExecutionEvidence{}, fmt.Errorf("fetch execution quote: %w", err)
	}
	if err := validatePullbackExecutionQuote(quote, since, now); err != nil {
		return pullbackExecutionEvidence{}, err
	}

	evidence := pullbackExecutionEvidence{
		// A BUY market order crosses toward the ask. Validate and paper-fill at
		// that conservative executable side rather than the quote midpoint.
		Mark:          quote.Ask,
		MinimumTraded: quote.Ask,
	}
	tradeProvider, ok := e.marketData.(data.TradePathProvider)
	if !ok {
		return pullbackExecutionEvidence{}, fmt.Errorf(
			"market data provider cannot verify exact post-signal trades",
		)
	}
	scanTrades := func(from, to time.Time) error {
		trades, err := tradeProvider.FetchTrades(ctx, ticker, from, to)
		if err != nil {
			return err
		}
		for _, trade := range trades {
			if trade.Timestamp.Before(from) || trade.Timestamp.After(to) || trade.Price <= 0 {
				continue
			}
			if trade.Price < evidence.MinimumTraded {
				evidence.MinimumTraded = trade.Price
			}
		}
		return nil
	}

	// Scan through the first quote, then refresh the quote and extend the exact
	// trade-print scan through that newer boundary. This closes the otherwise
	// unobserved interval while the first (possibly several-seconds-old) quote
	// and paginated path were being fetched.
	if err := scanTrades(since, quote.Timestamp); err != nil {
		return pullbackExecutionEvidence{}, fmt.Errorf("fetch exact execution path: %w", err)
	}
	refreshed, err := e.marketData.FetchQuote(ctx, ticker)
	if err != nil {
		return pullbackExecutionEvidence{}, fmt.Errorf("refresh execution quote: %w", err)
	}
	// The exact path can require several paginated requests. Judge the refreshed
	// quote against the clock after that work, rather than incorrectly rejecting
	// a legitimate timestamp that advanced beyond the initial `now` snapshot.
	if err := validatePullbackExecutionQuote(refreshed, since, time.Now()); err != nil {
		return pullbackExecutionEvidence{}, err
	}
	if refreshed.Timestamp.Before(quote.Timestamp) {
		return pullbackExecutionEvidence{}, fmt.Errorf(
			"refreshed execution quote moved backwards",
		)
	}
	if refreshed.Ask < evidence.MinimumTraded {
		evidence.MinimumTraded = refreshed.Ask
	}
	if refreshed.Timestamp.After(quote.Timestamp) {
		if err := scanTrades(quote.Timestamp, refreshed.Timestamp); err != nil {
			return pullbackExecutionEvidence{}, fmt.Errorf(
				"extend exact execution path: %w", err,
			)
		}
	}
	evidence.Mark = refreshed.Ask
	return evidence, nil
}

// buildRiskInput assembles risk.CheckInput for a signal against a portfolio
// snapshot and open trades. Shared by generateSignal (signal birth) and
// processDeferredSignals (re-validation at deferred execution) so both
// paths enforce identical portfolio-level gates. Counts distinct
// (ticker, direction) pairs so consolidated rows don't inflate the
// open-positions budget (#312), and builds the per-ticker count restricted
// to the signal's direction for the per-ticker cap.
func buildRiskInput(cfg config.RiskConfig, action, ticker string, confidence, sizePct, stopLoss, takeProfit, signalPrice float64, pState portfolio.State, openTrades []store.Trade) risk.CheckInput {
	var holdsLong bool
	var longQuantity float64
	var longCostBasis float64 // sum of qty * entry_price across open longs
	var longMaxStop float64   // canonical (highest) existing stop across open longs
	positions := make(map[string]struct{}, len(openTrades))
	byTicker := make(map[string]int, len(openTrades))
	for _, t := range openTrades {
		key := t.Ticker + "|" + t.Direction
		positions[key] = struct{}{}
		if (action == "BUY" && t.Direction == "long") ||
			(action == "SELL" && t.Direction == "short") {
			byTicker[t.Ticker]++
		}
		if t.Ticker == ticker && t.Direction == "long" {
			holdsLong = true
			longQuantity += t.Quantity
			longCostBasis += t.Quantity * t.EntryPrice
			if t.StopLoss > longMaxStop {
				longMaxStop = t.StopLoss
			}
		}
	}
	var longAvgEntry float64
	if longQuantity > 0 {
		longAvgEntry = longCostBasis / longQuantity
	}
	return risk.CheckInput{
		Cfg:               cfg,
		Action:            action,
		Ticker:            ticker,
		Confidence:        confidence,
		SizePct:           sizePct,
		StopLoss:          stopLoss,
		TakeProfit:        takeProfit,
		SignalPrice:       signalPrice,
		PortfolioValue:    pState.TotalValue,
		Cash:              pState.Cash,
		OpenPositions:     len(positions),
		OpenByTicker:      byTicker,
		DailyPnL:          pState.RealizedPnLDay,
		PeakValue:         pState.PeakValue,
		LastLossAt:        pState.LastLossAt,
		HoldsLong:         holdsLong,
		LongQuantity:      longQuantity,
		LongAvgEntryPrice: longAvgEntry,
		LongMaxStop:       longMaxStop,
	}
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
	if !store.IsConnected() {
		return
	}
	n1, err1 := store.CleanupIntradayCandles(ctx)
	n2, err2 := store.CleanupPromptCache(ctx)
	n3, err3 := store.CleanupPortfolioSnapshots(ctx)
	n4, err4 := store.CleanupNewsItems(ctx)
	n5, err5 := store.CleanupConfigHistory(ctx)
	n6, err6 := store.CleanupFeedbackFlags(ctx)
	if err1 != nil {
		logger.Warn("retention: intraday cleanup failed", "err", err1)
	}
	if err2 != nil {
		logger.Warn("retention: prompt cache cleanup failed", "err", err2)
	}
	if err3 != nil {
		logger.Warn("retention: portfolio snapshots cleanup failed", "err", err3)
	}
	if err4 != nil {
		logger.Warn("retention: news items cleanup failed", "err", err4)
	}
	if err5 != nil {
		logger.Warn("retention: config history cleanup failed", "err", err5)
	}
	if err6 != nil {
		logger.Warn("retention: feedback flags cleanup failed", "err", err6)
	}
	if n1+n2+n3+n4+n5+n6 > 0 {
		logger.Info("retention complete",
			"intraday_candles", n1, "prompt_cache", n2,
			"portfolio_snapshots", n3, "news_items", n4,
			"config_history", n5, "feedback_flags", n6,
		)
	}
}
func (e *Engine) calculateImpactScore(ticker string, snippets []analysis.NewsSnippet, candles []data.Candle) depth.ImpactScore {
	if !e.cfg.Analysis.AdaptiveDepth.Enabled {
		return depth.ImpactScore{Score: 1.0, Depth: 3} // default to full depth
	}

	// 1. ATRPct
	var atrPct float64
	if len(candles) > 0 {
		_ = candles[len(candles)-1]
		atrPct = 2.0 // placeholder for 2%
	}

	// 2. NewsChanged
	newsChanged := false
	if len(snippets) > 0 {
		// hash comparison logic here...
		newsChanged = true
	}

	// 3. RiskProximity
	riskProximity := 0.0
	// ...

	// 4. ADX
	adx := 25.0 // placeholder

	tc := depth.TickContext{
		ATRPct:           atrPct,
		NewsChanged:      newsChanged,
		RiskProximityPct: riskProximity,
		ADX:              adx,
	}

	return depth.Score(tc, e.cfg.Analysis.AdaptiveDepth)
}
