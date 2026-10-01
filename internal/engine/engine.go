// Package engine contains the core trading engine loop, scheduler, and pipeline
// integration. It orchestrates market data fetching, indicator computation,
// news retrieval, and (in later phases) analysis and order execution.
package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/romeo-folie/trade-engine/internal/analysis"
	"github.com/romeo-folie/trade-engine/internal/analysis/cache"
	"github.com/romeo-folie/trade-engine/internal/analysis/depth"
	"github.com/romeo-folie/trade-engine/internal/analysis/prompts"
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
	"github.com/romeo-folie/trade-engine/internal/strategy/antichase"
	"github.com/romeo-folie/trade-engine/internal/strategy/pullback"
	"github.com/romeo-folie/trade-engine/internal/tradeprofile"
)

const (
	// maxTickIntervalSec is the upper bound for TickIntervalSec before the
	// time.Duration conversion overflows. Values above ~9.2×10⁹ seconds would
	// wrap to a negative duration.
	maxTickIntervalSec = math.MaxInt64 / int64(time.Second)

	// historyCandles is the standard number of candles fetched per watchlist item
	// per tick. The anti-chase policy requests a larger daily-only history through
	// historyCandleLimit so it can establish a true prior 52-week high.
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

	// newsLastFetched tracks each durable refresh's timestamp and source window.
	// A short prompt-news refresh is not sufficient evidence for the 30-day
	// stale-catalyst timing rule.
	newsLastFetched map[string]newsRefresh
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
	exitStartupMu          sync.Mutex
	exitStartupPrepared    bool
	positionExitMu         sync.Mutex // serializes protective reconciliation with software position exits
	// stockEntryAdmissionMu serializes only standard-stock BUY admission and
	// immediate durable submission. Risk-reducing SELL/protection paths never
	// take it.
	stockEntryAdmissionMu sync.Mutex
	stockAccountPeakMu    sync.Mutex
	stockAccountPeak      float64
	stockAccountPeakAt    time.Time
	stockAccountPeakID    string

	feedbackReviewer *feedback.Reviewer

	// recoveredEntryEffects keeps asynchronous polling and coordinator startup
	// recovery from publishing the same in-process entry event twice.
	recoveredEntryEffects sync.Map
}

type newsRefresh struct {
	at       time.Time
	lookback time.Duration
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
		newsLastFetched:   make(map[string]newsRefresh),
		tickerState:       make(map[string]*TickerState),
		mode:              mode,
		costTracker:       tracker,
		costGuardrail:     guardrail,
		activeCostProfile: cfg.Cost.Profile,
	}

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
		// Establish exit truth before the first trading tick. A stale exiting
		// row must be finalized (and announced) before ownership gates evaluate
		// new signals from the startup cycle.
		if err := requireStartupExitReconciliation(ctx, 5*time.Second, e.ReconcileExitsNow); err != nil {
			return err
		}
		const cacheSweepInterval = 15 * time.Minute
		go cache.NewSweeper(cacheSweepInterval).Run(ctx)
		go e.runExitReconcileLoop(ctx)
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

			e.processStockWithProfile(
				ctx, item.Ticker, item.Timeframes, item.Strategies, item.Source,
				item.MinConfidence, item.GraphContext, item.ScannerRunID,
				item.EffectiveDiscoveryContext(), item.ExecutionProfile,
			)
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
func (e *Engine) processStock(
	ctx context.Context,
	ticker string,
	timeframes, strategies []string,
	source string,
	scannerConviction float64,
	graphContext, scannerRunID string,
) {
	e.processStockWithProfile(
		ctx, ticker, timeframes, strategies, source, scannerConviction, graphContext, scannerRunID,
		nil, nil,
	)
}

// processStockWithProfile carries the scanner's immutable provenance and
// bounded lens profile to deep analysis. The legacy processStock wrapper is
// intentionally kept for test/tool callers that have no persisted profile.
func (e *Engine) processStockWithProfile(
	ctx context.Context,
	ticker string,
	timeframes, strategies []string,
	source string,
	scannerConviction float64,
	graphContext, scannerRunID string,
	discoveryContext *tradeprofile.DiscoveryContext,
	executionProfile *tradeprofile.ExecutionProfile,
) {
	workflow := resolveTimeframeWorkflow(timeframes, executionProfile)
	primaryTF := workflow.PrimaryTimeframe
	// The complete pullback setup is defined on daily history even when a
	// scanner candidate uses 1H/4H for its analyst prompt. Fetch 1D explicitly
	// so the execution gate never silently fails open because of watchlist
	// timeframe selection.
	entryGates := e.cfg.Risk.EntryGates
	pullbackCandidate := entryGates.PullbackStrengthEnabled &&
		hasEntryStrategy(strategies, store.EntrySetupPullbackStrength)
	pullbackPromptCandidate := pullbackLensAdmissible(strategies, executionProfile)
	needsDailyEvidence := pullbackCandidate || pullbackPromptCandidate ||
		entryGates.TrendAlignmentEnabled || entryGates.AntiChase.Enabled
	fetchTimeframes := workflow.FetchTimeframes
	if needsDailyEvidence && workflow.PrimaryTimeframe != data.Timeframe1Day {
		fetchTimeframes = append(fetchTimeframes, string(data.Timeframe1Day))
	}

	var primaryCandleObservation candleFetchObservation
	// Supplemental timeframe evidence is only valid when this tick refreshed
	// it successfully. Do not fall back to cached rows after a provider error:
	// unified analysis must not turn a stale entry-timing close into a trade.
	timeframeRefreshErrors := make(map[data.Timeframe]error)
	for _, tfStr := range fetchTimeframes {
		tf := data.Timeframe(strings.ToLower(strings.TrimSpace(tfStr)))
		observation, err := e.fetchAndStoreCandlesObserved(ctx, ticker, tf)
		if err != nil {
			timeframeRefreshErrors[tf] = err
			continue
		}
		if tf == primaryTF {
			primaryCandleObservation = observation
		}
	}

	// Compute indicators on the primary timeframe (prefer "1d", fall back to first).
	// Only requested timeframes participate: an evidence-only daily fetch must
	// not silently change a scanner item's 1H/4H analyst prompt to daily.
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
		e.recordScannerEngineOutcome(ctx, scannerRunID, ticker, "candles_unavailable", err.Error(), "")
		return
	}

	if len(candles) < 20 {
		logger.Info("insufficient candles for indicators", "ticker", ticker, "count", len(candles))
		e.recordScannerEngineOutcome(
			ctx, scannerRunID, ticker, "insufficient_candles",
			fmt.Sprintf("need 20 candles; got %d", len(candles)), "",
		)
		return
	}

	set, err := indicators.Compute(candles)
	if err != nil {
		logger.Warn("compute indicators", "ticker", ticker, "err", err.Error())
		e.recordScannerEngineOutcome(ctx, scannerRunID, ticker, "indicators_failed", err.Error(), "")
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

	// Fetch news (rate-limited to newsFetchInterval cadence). The returned
	// result is timing evidence: a stale-catalyst BUY rule cannot treat an
	// empty database window as complete after its provider refresh failed.
	newsRefreshErr := e.fetchAndStoreNews(ctx, ticker)

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
	contractDigest := DiscoveryExecutionContractDigest(discoveryContext, executionProfile)
	mcIn := MaterialChangeInput{
		RSI:            set.RSI,
		EMA20:          set.EMA20,
		Price:          latestClose,
		NewsHash:       newsHash,
		ContractDigest: contractDigest,
		Now:            time.Now(),
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
		e.recordScannerEngineOutcome(
			ctx, scannerRunID, ticker, "material_change_skip", string(skipReason), "",
		)
		return
	}

	// Evaluate the daily pullback facts once for the prompt whenever the
	// scanner's bounded menu admits the lens. This does not expand the additive
	// execution path: generateSignal still uses pullbackCandidate and rechecks
	// the setup against a refreshed daily snapshot immediately before entry.
	var pullbackContext *pullback.PromptContext
	if pullbackPromptCandidate {
		dailyCandles := candles
		if primaryTF != data.Timeframe1Day {
			var dailyErr error
			dailyCandles, dailyErr = store.GetCandles(ctx, ticker, data.Timeframe1Day, historyFrom, time.Now())
			if dailyErr != nil {
				logger.Warn("pullback strength: load daily prompt evidence", "ticker", ticker, "err", dailyErr)
				dailyCandles = nil
			}
		}
		if len(dailyCandles) > 0 {
			signalPrice := dailyCandles[len(dailyCandles)-1].Close
			setup := pullback.Evaluate(dailyCandles, signalPrice, pullback.Config{
				MinMomentumReturnPct:  entryGates.PullbackMinMomentumReturnPct,
				ConsolidationSessions: entryGates.PullbackConsolidationSessions,
				SupportTolerancePct:   entryGates.PullbackSupportTolerancePct,
				MaxStopWidthPct:       entryGates.PullbackMaxStopWidthPct,
			})
			context := setup.PromptContext()
			pullbackContext = &context
		}
	}

	// Run LLM analysis — PRD §8. Errors are logged but non-fatal.
	e.runAnalysisWithProfile(
		ctx, ticker, string(primaryTF), primaryStrategy, strategies, candles, set,
		newsHash, source, graphContext, scannerRunID, pullbackCandidate,
		discoveryContext, executionProfile, pullbackContext, newsRefreshErr, primaryCandleObservation, workflow, timeframeRefreshErrors,
	)
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
		CashAvailable:   state.Cash,
		PortfolioEquity: state.TotalValue,
		CashKnown:       state.Cash > 0 || state.TotalValue > 0,
	}
	entryCounts := make(map[string]int, len(openTrades))
	for _, trade := range openTrades {
		if trade.Quantity != 0 {
			entryCounts[strings.ToUpper(trade.Ticker)+"|"+strings.ToLower(trade.Direction)]++
		}
	}
	for _, p := range positions {
		if p.Quantity == 0 {
			continue
		}
		side := p.Side
		if side == "" {
			side = "long"
		}
		count := entryCounts[strings.ToUpper(p.Ticker)+"|"+strings.ToLower(side)]
		entryCountKnown := count > 0
		if count == 0 {
			count = 1 // broker-only holding: one entry is known, exact local lot count is not
		}
		out.OpenPositions = append(out.OpenPositions, analysis.PositionSummary{
			Ticker:          p.Ticker,
			Side:            p.Side,
			Quantity:        p.Quantity,
			AvgCostBasis:    p.AvgCostBasis,
			CurrentValue:    p.CurrentValue,
			UnrealizedPL:    p.UnrealizedPL,
			EntryCount:      count,
			EntryCountKnown: entryCountKnown,
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
			ps = &analysis.PositionSummary{Ticker: t.Ticker, Side: t.Direction, EntryCountKnown: true}
			agg[k] = ps
			order = append(order, k)
		}
		// Weighted-average entry across pyramided rows.
		totalCost := ps.AvgCostBasis*ps.Quantity + t.EntryPrice*t.Quantity
		ps.Quantity += t.Quantity
		ps.EntryCount++
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
// request. When the per-ticker entry cap is enabled, load local trade rows even
// if the broker tracker has positions so the model sees the actual pyramid lot
// count. Otherwise the store is only needed when the tracker is empty (paper
// mode — Codex #392 P1).
func (e *Engine) portfolioContextForPrompt(ctx context.Context) analysis.PortfolioContext {
	state := e.portfolio.CurrentState()
	positions := e.portfolio.Positions()
	var openTrades []store.Trade
	if (len(positions) == 0 || e.cfg.Risk.MaxPerTicker > 0) && store.IsConnected() {
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
func (e *Engine) runAnalysis(
	ctx context.Context,
	ticker, timeframe, strategy string,
	strategies []string,
	candles []data.Candle,
	set *indicators.IndicatorSet,
	newsHash, source, graphContext, scannerRunID string,
	pullbackCandidate bool,
) {
	e.runAnalysisWithProfile(
		ctx, ticker, timeframe, strategy, strategies, candles, set,
		newsHash, source, graphContext, scannerRunID, pullbackCandidate, nil, nil,
		nil, nil, candleFetchObservation{}, resolveTimeframeWorkflow([]string{timeframe}, nil), nil,
	)
}

// runAnalysisWithProfile is the scanner-aware analysis path. It does not
// alter timeframe scheduling or entry gates; PR 1 only carries the profile
// into prompts and analysis audit storage.
func (e *Engine) runAnalysisWithProfile(
	ctx context.Context,
	ticker, timeframe, strategy string,
	strategies []string,
	candles []data.Candle,
	set *indicators.IndicatorSet,
	newsHash, source, graphContext, scannerRunID string,
	pullbackCandidate bool,
	discoveryContext *tradeprofile.DiscoveryContext,
	executionProfile *tradeprofile.ExecutionProfile,
	pullbackContext *pullback.PromptContext,
	newsRefreshErr error,
	primaryCandleObservation candleFetchObservation,
	workflow timeframeWorkflow,
	timeframeRefreshErrors map[data.Timeframe]error,
) {
	if set == nil {
		e.recordScannerEngineOutcome(ctx, scannerRunID, ticker, "indicators_missing", "", "")
		return
	}
	if e.analyzer == nil || !e.analyzer.Primary().IsAvailable() {
		logger.Debug("analysis skipped — primary model not available", "ticker", ticker)
		e.recordScannerEngineOutcome(ctx, scannerRunID, ticker, "model_unavailable", "", "")
		return
	}

	if strategy == "" {
		strategy = "trend_following"
	}
	// A valid scanner profile supersedes legacy Strategies for the analyst's
	// candidate menu, so it must also determine the prompt history budget.
	// Otherwise an offered long-window lens can be starved by a short legacy
	// strategy window before the model has a chance to select it.
	promptLenses := analysis.AnalysisRequest{
		Strategy: strategy, Strategies: strategies, ExecutionProfile: executionProfile,
	}.LensSet()

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
				e.emitOwnershipShortCircuitBlock(
					ctx, ticker, strategy, candles, reason, scannerRunID,
				)
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
	// The analyst may now select ANY admissible lens, so cap at the WIDEST
	// configured window across the set — capping at the primary's window would
	// starve a lens that needs more history (momentum 14 vs trend_following 20)
	// of the bars required to confirm or reject it.
	if windows := e.cfg.Analysis.HistoryWindowByStrategy; len(windows) > 0 {
		if n := widestHistoryWindow(windows, strategy, promptLenses); n > 0 {
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
	// Keep a longer timestamped source window for the deterministic timing
	// snapshot. The prompt still receives only its configured newest subset.
	timingNow := time.Now()
	recentNews, recentNewsErr := store.GetRecentNews(ctx, ticker, timingNow.Add(-antiChaseNewsLookback))
	if recentNewsErr == nil && newsRefreshErr != nil {
		recentNewsErr = newsRefreshErr
	}
	// Cap news articles to MaxNewsArticlesPerPrompt — newest first, excess dropped.
	newsCap := e.cfg.Analysis.MaxNewsArticlesPerPrompt
	if newsCap <= 0 {
		newsCap = 5
	}
	timingNews := make([]analysis.NewsSnippet, 0, len(recentNews))
	for _, n := range recentNews {
		item := analysis.NewsSnippet{
			Headline:    n.Headline,
			Source:      n.Source,
			PublishedAt: n.PublishedAt,
			Sentiment:   sentimentLabel(n.SentimentScore),
		}
		timingNews = append(timingNews, item)
	}
	snippets := promptNewsSnippets(timingNews, timingNow, newsCap)
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

	// Build the deterministic timing snapshot from full daily history. The
	// prompt remains capped to its short strategy window; this separate block is
	// the only source of long-history/high-proximity facts available to models.
	var timingSnapshot *antichase.Snapshot
	if e.cfg.Risk.EntryGates.AntiChase.Enabled {
		timingDaily := candles
		if data.Timeframe(timeframe) != data.Timeframe1Day {
			var dailyErr error
			timingDaily, dailyErr = store.GetCandles(ctx, ticker, data.Timeframe1Day,
				time.Now().AddDate(-2, 0, 0), time.Now())
			if dailyErr != nil {
				logger.Warn("anti-chase: load daily timing evidence", "ticker", ticker, "err", dailyErr)
				timingDaily = nil
			}
		}
		mark, markSource := analysisAntiChaseMark(
			ticker, candles, data.Timeframe(timeframe), timingNow, primaryCandleObservation,
		)
		timingSnapshot = buildAnalysisAntiChaseSnapshot(
			ticker, timingDaily, mark, markSource, timingNews, recentNewsErr,
			e.cfg.Risk.EntryGates.AntiChase.StaleCatalystRunupEnabled, timingNow,
		)
	}

	req := analysis.AnalysisRequest{
		Ticker:             ticker,
		RiskPolicy:         promptRiskPolicy(e.cfg.Risk, brokerName(e.executor) == "alpaca"),
		Strategy:           strategy,
		Strategies:         strategies,
		DiscoveryContext:   discoveryContext,
		ExecutionProfile:   executionProfile,
		PullbackContext:    pullbackContext,
		Timeframe:          timeframe,
		MomentumPlay:       profileMomentumPlay(timeframe, source, executionProfile),
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
			VWAP:           set.VWAP,
			VolSMA20:       set.VolumeSMA20,
			VolRatio:       volRatio(candles, set.VolumeSMA20),
			TimingSnapshot: timingSnapshot,
		},
		News:      snippets,
		Portfolio: e.portfolioContextForPrompt(ctx),
	}
	contextEvidence, timeframeCandles, contextErr := e.unifiedTimeframeEvidence(
		ctx, ticker, workflow, req, time.Now().AddDate(-2, 0, 0), timeframeRefreshErrors,
	)
	// Missing supplemental evidence must never support a new entry, but a
	// primary-timeframe analysis can still return a risk-reducing SELL for an
	// existing position. Run that degraded analysis and suppress BUY below.
	if contextErr != nil {
		logger.Warn("supplemental timeframe evidence unavailable — retaining exit analysis", "ticker", ticker, "err", contextErr)
	}
	req.TimeframeContexts = contextEvidence

	impact := e.calculateImpactScore(ticker, snippets, candles)
	routingSource := "legacy"
	if workflow.ProfileOwned {
		routingSource = "profile"
	}
	workflowAudit := store.TimeframeWorkflowAudit{
		Version: 2, RoutingSource: routingSource, Mode: "unified",
		Primary:  store.TimeframePassAudit{Timeframe: timeframe, Status: "primary"},
		Contexts: timeframeAuditContexts(timeframe, workflow, req.TimeframeContexts, contextErr),
	}
	signalCandles := candles
	signalTimeframe := timeframe
	if workflow.ProfileOwned && workflow.EntryTimingTimeframe != "" {
		entryTF := string(workflow.EntryTimingTimeframe)
		if entryCandles := timeframeCandles[entryTF]; len(entryCandles) > 0 {
			signalCandles, signalTimeframe = entryCandles, entryTF
		}
	}
	// Specialist mode runs three directional roles followed by a mandatory
	// Risk Specialist instead of the generalist ensemble. Both paths return
	// AnalyzeResult so downstream cost tracking and persistence stay shared.
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

	var primaryAudit analysis.LensSelectionAudit
	if err == nil {
		beforePrimaryLensValidation := result.Response.Action
		primaryResp := result.Response
		primaryAudit = analysis.ValidateFinalLensSelection(&primaryResp, req, len(result.AnalystResults) > 1)
		if primaryResp.Action != beforePrimaryLensValidation {
			primaryResp.DecisiveReason = finalLensSuppressionReason(primaryAudit)
		}
		result.Response = primaryResp
		populatePrimaryWorkflowAudit(&workflowAudit, primaryResp, primaryAudit)
	}

	if err != nil {
		logger.Warn("analysis failed", "ticker", ticker, "err", err.Error())
		e.recordScannerEngineOutcome(ctx, scannerRunID, ticker, "analysis_failed", err.Error(), "")
		return
	}
	resp, meta := result.Response, result.Meta
	if contextErr != nil && resp.Action == analysis.ActionBuy {
		resp.Action = analysis.ActionWait
		resp.SizePct, resp.StopLoss, resp.TakeProfit = 0, 0, 0
		resp.DecisiveReason = "Entry blocked because required supplemental timeframe evidence was unavailable this cycle."
		resp.Reasoning = resp.Reasoning + "\n\nEntry blocked: required supplemental timeframe evidence was unavailable."
	}
	// Validate the final aggregate, rather than individual analyst responses:
	// an execution profile bounds what may be persisted as the selected lens.
	// Invalid model output remains in meta.RawResponse, but is cleared from the
	// live response before the existing downstream compatibility paths see it.
	lensAudit := analysis.ValidateFinalLensSelection(&resp, req, len(result.AnalystResults) > 1)
	if result.DecisionPolicy != nil && result.DecisionPolicy.Applied && result.DecisionPolicy.OriginalAction != result.DecisionPolicy.ResolvedAction {
		resp.DecisiveReason = result.DecisionPolicy.Reason
	}
	workflowAudit.FinalAction = string(resp.Action)
	workflowAudit.DecisiveReason = decisiveReason(resp)
	if lensAudit.SelectionStatus == analysis.LensSelectionInvalid {
		logger.Warn("invalid profile-bound lens selection",
			"ticker", ticker, "selected", lensAudit.SelectedLens,
			"admissible", strings.Join(req.LensSet(), ","))
	}

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
	// Lens selection (issue #509), logged as its own event so
	// configured-vs-selected divergence is greppable rather than requiring a
	// regex over analyses.raw_response. A ticker whose analyst systematically
	// selects something other than its configured strategy — or "none", the
	// regime-mismatch answer — is a mislabelled watchlist row.
	//
	// Emitted only when the analyst actually reported a selection: older
	// models and truncation-recovered responses leave it empty, and logging
	// those as divergent=false would poison the very measurement this exists
	// for. This is the phase-1 evidence that decides whether persisting
	// selected_strategy is worth a migration.
	if resp.SelectedStrategy != "" {
		configuredLens, admissibleLenses := lensSelectionTelemetry(strategy, strategies, executionProfile)
		logger.Info("lens selection",
			"ticker", ticker,
			"configured", configuredLens,
			"selected", resp.SelectedStrategy,
			"regime", resp.Regime,
			"divergent", resp.SelectedStrategy != configuredLens,
			"admissible", strings.Join(admissibleLenses, ","),
			"rejected_count", len(resp.RejectedStrategies),
		)
	}
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
					Role:       ar.Role,
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
		FastPathed: result.FastPathed, SourceNode: e.analysisSourceNode(result),
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
			ScannerRunID:    scannerRunID,
			Audit: store.AnalysisAudit{
				SelectedLens:      lensAudit.SelectedLens,
				RejectedLenses:    lensAudit.RejectedLenses,
				Regime:            lensAudit.Regime,
				SelectionStatus:   lensAudit.SelectionStatus,
				TimeframeWorkflow: &workflowAudit,
				TimingSnapshot:    timingSnapshot,
				DecisionPolicy:    result.DecisionPolicy,
			},
		})
	} else {
		analysisID, err = store.InsertAnalysisWithScannerRunAudit(
			ctx, ticker, strategy, resp, meta, scannerRunID, store.AnalysisAudit{
				SelectedLens:      lensAudit.SelectedLens,
				RejectedLenses:    lensAudit.RejectedLenses,
				Regime:            lensAudit.Regime,
				SelectionStatus:   lensAudit.SelectionStatus,
				TimeframeWorkflow: &workflowAudit,
				TimingSnapshot:    timingSnapshot,
				DecisionPolicy:    result.DecisionPolicy,
			},
		)
	}
	if err != nil {
		logger.Warn("store analysis", "ticker", ticker, "err", err.Error())
		e.recordScannerEngineOutcome(
			ctx, scannerRunID, ticker, "analysis_persistence_failed", err.Error(), "",
		)
	}

	// Phase 4: generate and potentially execute a signal from this analysis.
	// Hand the pre-fetched openTrades (loaded by the SELL ownership short-circuit
	// probe above) into generateSignal so we don't fire a second identical
	// GetOpenTrades query for SELL-only strategies. Nil is the common case —
	// dual-direction strategies and no-DB / probe-failed cycles all leave the
	// prefetch unset, and generateSignal then re-fetches on-demand and applies
	// its degraded-mode handling.
	e.generateSignal(
		ctx, ticker, signalTimeframe, strategy, strategies, signalCandles, resp, timingSnapshot, analysisID,
		prefetchedOpenTrades, e.analysisSourceNode(result), scannerRunID,
		pullbackCandidate, req.LensSet(),
	)

	// Update material-change gate state for next tick.
	latestClose := 0.0
	if len(candles) > 0 {
		latestClose = candles[len(candles)-1].Close
	}
	e.tickerStateMu.Lock()
	e.tickerState[ticker] = &TickerState{
		LastRSI:            set.RSI,
		LastEMAGapPct:      emaGapPct(latestClose, set.EMA20),
		LastPrice:          latestClose,
		LastNewsHash:       newsHash,
		LastContractDigest: DiscoveryExecutionContractDigest(discoveryContext, executionProfile),
		LastDecision:       string(resp.Action),
		LastAnalysedAt:     time.Now(),
	}
	e.tickerStateMu.Unlock()
}

func promptRiskPolicy(cfg config.RiskConfig, brokerAuthoritative bool) prompts.RiskPolicy {
	basis := prompts.SizeBasisCash
	if brokerAuthoritative {
		basis = prompts.SizeBasisEquity
	}
	return prompts.RiskPolicy{
		MaxTickerExposurePct: cfg.MaxPositionPct,
		MaxPerTicker:         cfg.MaxPerTicker,
		MinRiskReward:        cfg.MinRiskReward,
		SizeBasis:            basis,
		Configured:           true,
	}
}

// lensSelectionTelemetry returns the scanner-owned recommendation and bounded
// menu that a profile-bound analyst was asked to choose from. Legacy rows
// retain the historical strategy-derived telemetry. Canonicalizing here keeps
// log fields consistent with the durable execution-profile contract even when
// callers constructed the profile with non-canonical casing or whitespace.
func lensSelectionTelemetry(
	strategy string,
	strategies []string,
	executionProfile *tradeprofile.ExecutionProfile,
) (configured string, admissible []string) {
	if executionProfile != nil {
		if canonical, err := executionProfile.Canonicalize(); err == nil {
			return canonical.RecommendedLens, canonical.AdmissibleLenses
		}
	}
	return strategy, prompts.Lenses(strategies)
}

// analysisSourceNode identifies the configured analysis stage that produced the
// final verdict. Web clients use this semantic ID to route live events across
// the same nodes advertised by the topology snapshot.
func (e *Engine) analysisSourceNode(result analysis.AnalyzeResult) string {
	if e.cfg.Analysis.SpecialistModeEnabled {
		return "specialist-risk-manager"
	}
	if result.Arbiter != nil && !result.FastPathed {
		return "arbiter_node"
	}
	if e.cfg.Models.Consensus.Enabled && len(result.AnalystResults) > 1 {
		return "consensus"
	}
	return e.cfg.Models.AnalystNodeID(0)
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
func buildOwnershipShortCircuitBlock(
	cfg config.RiskConfig,
	ticker, strategy string,
	candles []data.Candle,
	reason string,
	now time.Time,
	scannerRunIDs ...string,
) (store.Signal, events.RiskCheckCompletedPayload, events.SignalBlockedPayload) {
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
	if len(scannerRunIDs) > 0 {
		sig.ScannerRunID = scannerRunIDs[0]
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
func (e *Engine) emitOwnershipShortCircuitBlock(
	ctx context.Context,
	ticker, strategy string,
	candles []data.Candle,
	reason string,
	scannerRunIDs ...string,
) {
	var scannerRunID string
	if len(scannerRunIDs) > 0 {
		scannerRunID = scannerRunIDs[0]
	}
	sig, riskEv, blockEv := buildOwnershipShortCircuitBlock(
		e.cfg.Risk, ticker, strategy, candles, reason, time.Now(), scannerRunID,
	)
	var signalID string
	if store.IsConnected() {
		var err error
		signalID, err = store.InsertSignal(ctx, sig)
		if err != nil {
			logger.Warn("store ownership short-circuit blocked signal",
				"ticker", ticker, "err", err)
		}
	} else {
		logger.Info("signal blocked (no-DB; not persisted)",
			"ticker", ticker, "strategy", strategy, "reason", reason)
	}
	e.recordScannerEngineOutcome(
		ctx, scannerRunID, ticker, "ownership_short_circuit", reason, signalID,
	)
	e.bus.Publish(events.RiskCheckCompleted, riskEv)
	e.bus.Publish(events.SignalBlocked, blockEv)
}

// recordScannerEngineOutcome durably marks scanner-originated engine attempts
// that intentionally produce no analysis row. Observability stays fail-open:
// a snapshot write can never change signal generation or execution.
func (e *Engine) recordScannerEngineOutcome(
	ctx context.Context,
	scannerRunID, ticker, stage, reason, signalID string,
) {
	if scannerRunID == "" || !store.IsConnected() {
		return
	}
	if err := store.InsertScannerEngineOutcome(ctx, store.ScannerEngineOutcome{
		ScannerRunID: scannerRunID,
		Ticker:       ticker,
		Stage:        stage,
		Reason:       reason,
		SignalID:     signalID,
	}); err != nil {
		logger.Warn("store scanner engine outcome",
			"ticker", ticker, "stage", stage, "err", err)
	}
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

// sentimentLabel converts the signed [-1, 1] sentiment scale stored by Finnhub
// (bullish percent minus bearish percent) into the prompt/gate labels.
func sentimentLabel(score float64) string {
	switch {
	case score > signedBullishSentimentMin:
		return "bullish"
	case score < signedBearishSentimentMax:
		return "bearish"
	default:
		return "neutral"
	}
}

// promptNewsSnippets preserves the prompt's short, recent-news contract even
// when the timing policy has loaded a longer catalyst-evidence window.
func promptNewsSnippets(items []analysis.NewsSnippet, now time.Time, limit int) []analysis.NewsSnippet {
	if limit <= 0 {
		return nil
	}
	cutoff := now.Add(-defaultNewsLookback)
	prompt := make([]analysis.NewsSnippet, 0, limit)
	for _, item := range items {
		if item.PublishedAt.Before(cutoff) {
			continue
		}
		prompt = append(prompt, item)
		if len(prompt) == limit {
			break
		}
	}
	return prompt
}

// fetchAndStoreCandles fetches recent candles from the market data provider and
// upserts them into the database. Uses FetchHistory for the live loop (recent bars only).
func (e *Engine) fetchAndStoreCandles(ctx context.Context, ticker string, tf data.Timeframe) {
	_, _ = e.fetchAndStoreCandlesObserved(ctx, ticker, tf)
}

// candleFetchObservation identifies the freshest candle that the provider
// returned in a specific fetch cycle. Live prompt timing must use this value,
// not merely a same-session candle that happened to already be cached.
type candleFetchObservation struct {
	ObservedAt    time.Time
	Ticker        string
	Timeframe     data.Timeframe
	LatestFetched *data.Candle
}

// fetchAndStoreCandlesObserved is the checked variant used when callers need
// the exact boundary at which the returned provider snapshot became observable.
func (e *Engine) fetchAndStoreCandlesObserved(
	ctx context.Context, ticker string, tf data.Timeframe,
) (candleFetchObservation, error) {
	if e.marketData == nil {
		return candleFetchObservation{}, fmt.Errorf("market data unavailable")
	}
	candles, err := e.marketData.FetchHistory(ctx, ticker, tf, e.historyCandleLimit(tf))
	if err != nil {
		logger.Warn("fetch candles", "ticker", ticker, "tf", string(tf), "err", err.Error())
		return candleFetchObservation{}, err
	}
	if len(candles) == 0 {
		err := fmt.Errorf("provider returned no candles")
		logger.Warn("fetch candles", "ticker", ticker, "tf", string(tf), "err", err.Error())
		return candleFetchObservation{}, err
	}
	// Exact post-setup invalidation starts when this provider snapshot became
	// observable. Capture the boundary immediately after FetchHistory returns,
	// before DB persistence or any subsequent qualification work.
	observedAt := time.Now()
	survivors, err := store.UpsertCandlesObserved(ctx, candles)
	if err != nil {
		logger.Warn("upsert candles", "ticker", ticker, "err", err.Error())
		return candleFetchObservation{}, err
	}
	var latest *data.Candle
	for _, candle := range survivors {
		if candle.Ticker != ticker || candle.Timeframe != tf {
			continue
		}
		if latest == nil || candle.Timestamp.After(latest.Timestamp) {
			candidate := candle
			latest = &candidate
		}
	}
	if latest == nil {
		err := fmt.Errorf("provider returned no admissible candles for %s/%s", ticker, tf)
		logger.Warn("fetch candles", "ticker", ticker, "tf", string(tf), "err", err.Error())
		return candleFetchObservation{}, err
	}
	logger.Info("candles upserted", "ticker", ticker, "tf", string(tf), "count", len(candles))
	e.bus.Publish(events.DataFetched, events.DataFetchedPayload{
		Ticker: ticker, Timeframe: string(tf), Count: len(candles),
	})
	return candleFetchObservation{
		ObservedAt: observedAt, Ticker: ticker, Timeframe: tf, LatestFetched: latest,
	}, nil
}

// fetchAndStoreNews fetches recent news for a ticker (respecting the cadence
// limit) and upserts articles into the database. A nil result means the
// requested window was either durably refreshed or a prior successful refresh
// is still within cadence; callers may safely use an empty result as evidence.
func (e *Engine) fetchAndStoreNews(ctx context.Context, ticker string) error {
	if e.newsProvider == nil {
		if e.cfg.Risk.EntryGates.AntiChase.Enabled && e.cfg.Risk.EntryGates.AntiChase.StaleCatalystRunupEnabled {
			return errors.New("anti-chase: timestamped catalyst provider is unavailable")
		}
		return nil
	}

	now := time.Now()
	lookback := newsLookback(
		e.cfg.Risk.EntryGates.AntiChase.Enabled,
		e.cfg.Risk.EntryGates.AntiChase.StaleCatalystRunupEnabled,
	)
	e.newsMu.Lock()
	last := e.newsLastFetched[ticker]
	if newsRefreshCovers(last, now, lookback) {
		e.newsMu.Unlock()
		return nil
	}
	e.newsMu.Unlock()

	since := now.Add(-lookback)
	items, err := e.newsProvider.FetchNews(ctx, ticker, since)
	if err != nil {
		logger.Warn("fetch news", "ticker", ticker, "err", err.Error())
		return fmt.Errorf("fetch timestamped news: %w", err)
	}
	newsIDs, err := store.UpsertNewsItems(ctx, items)
	if err != nil {
		logger.Warn("upsert news", "ticker", ticker, "err", err.Error())
		return fmt.Errorf("persist timestamped news: %w", err)
	}
	e.newsMu.Lock()
	e.newsLastFetched[ticker] = newsRefresh{at: time.Now(), lookback: lookback}
	e.newsMu.Unlock()
	logger.Info("news fetched", "ticker", ticker, "count", len(items))
	e.bus.Publish(events.NewsFetched, events.NewsFetchedPayload{
		Ticker: ticker, Count: len(items),
	})

	if e.enricher == nil || !e.cfg.Markets.News.ExtractionEnabled {
		return nil
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
	return nil
}

func newsRefreshCovers(last newsRefresh, now time.Time, requiredLookback time.Duration) bool {
	return !last.at.IsZero() && now.Sub(last.at) < newsFetchInterval && last.lookback >= requiredLookback
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
func (e *Engine) generateSignal(
	ctx context.Context,
	ticker, timeframe, strategy string,
	strategies []string,
	candles []data.Candle,
	resp analysis.AnalysisResponse,
	timingSnapshot *antichase.Snapshot,
	analysisID string,
	prefetchedOpenTrades []store.Trade,
	analysisSourceNode string,
	scannerRunID string,
	pullbackCandidate bool,
	admissibleLensSets ...[]string,
) {
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
	admissibleLenses := strategies
	if len(admissibleLensSets) > 0 {
		admissibleLenses = admissibleLensSets[0]
	}
	signalStrategy := strategy
	if resp.Action == analysis.ActionBuy {
		signalStrategy = effectiveGateStrategy(strategy, resp.SelectedStrategy, admissibleLenses)
	}
	sig := store.Signal{
		Ticker:            ticker,
		Action:            string(resp.Action),
		Strategy:          signalStrategy,
		SignalPrice:       signalPrice,
		StopLoss:          resp.StopLoss,
		TakeProfit:        resp.TakeProfit,
		Confidence:        resp.Confidence,
		SizePct:           resp.SizePct,
		Status:            "pending",
		ExpiresAt:         createdAt.Add(time.Duration(expiryMin) * time.Minute),
		StalePriceMovePct: stalePct,
		EntrySetup:        store.EntrySetupStandard,
		ScannerRunID:      scannerRunID,
		AnalysisIDs:       analysisIDs,
		CreatedAt:         createdAt,
	}
	gateDecisions := make([]store.EntryGateDecision, 0, 5)

	// Entry-quality gates (entry_gates.go): daily trend alignment, news
	// tape-confirmation, and momentum anti-chase. Checked before the generic
	// risk gates so the block reason names the specific losing cohort. The
	// pullback setup is an additive path: universe-qualified candidates that
	// complete it receive its low-of-day stop and execution revalidation;
	// candidates that do not complete it continue through the standard gates.
	eg := e.cfg.Risk.EntryGates
	if resp.Action == "BUY" {
		var entryGateReason string
		entryGateFailedCheck := "entry_gates"
		entryDailyCandles := candles
		var pullbackObservedAt time.Time
		pullbackCandidate = eg.PullbackStrengthEnabled && pullbackCandidate
		// A lens made available by the scanner profile cannot bypass its own
		// deterministic qualification. Legacy qualified candidates retain the
		// additive path below even when an older model omits selected_strategy.
		pullbackSelected := selectedPullbackLens(strategy, resp.SelectedStrategy, admissibleLenses)
		// A scanner profile may outlive a temporary gate disable. In that case a
		// BUY reasoned under pullback_strength cannot silently fall back to a
		// standard entry: qualification is integral to that lens, and disabled
		// qualification makes the recommendation non-executable.
		if pullbackSelectionDisabled(eg.PullbackStrengthEnabled, pullbackSelected) {
			entryGateReason = "pullback strength: selected lens is disabled"
			entryGateFailedCheck = "pullback_disabled"
		}
		pullbackEvaluationRequired := pullbackCandidate ||
			(pullbackSelected && eg.PullbackStrengthEnabled)
		pullbackConfirmed := false
		pullbackFailureReason := ""
		var pullbackQualificationDetails []byte
		needsDailyEvidence := pullbackEvaluationRequired ||
			eg.TrendAlignmentEnabled || eg.AntiChase.Enabled
		if pullbackEvaluationRequired {
			// The analyst may take long enough for the session low to change.
			// Refresh the partial daily bar at qualification time, then scan
			// every exact trade print from the returned snapshot boundary.
			observation, err := e.fetchAndStoreCandlesObserved(
				ctx, ticker, data.Timeframe1Day,
			)
			if err != nil {
				logger.Warn("pullback strength: unable to refresh setup evidence; using standard entry path",
					"ticker", ticker, "err", err)
			} else {
				pullbackObservedAt = observation.ObservedAt
			}
		}
		if needsDailyEvidence &&
			(data.Timeframe(timeframe) != data.Timeframe1Day || pullbackEvaluationRequired) {
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
		if eg.AntiChase.Enabled {
			policy := antiChasePolicy(eg.AntiChase)
			reason := "anti-chase: deterministic timing evidence was not recorded"
			decision := antichase.Decision{Status: "unavailable_block", Reason: reason}
			if timingSnapshot != nil {
				reason, decision = antiChaseEntryGate(timingSnapshot, policy)
			}
			details, _ := json.Marshal(struct {
				Snapshot *antichase.Snapshot `json:"snapshot,omitempty"`
				Decision antichase.Decision  `json:"decision"`
			}{timingSnapshot, decision})
			status := "passed"
			if reason != "" {
				status = "blocked"
				entryGateReason = reason
				entryGateFailedCheck = "universal_anti_chase"
			}
			gateDecisions = append(gateDecisions, store.EntryGateDecision{
				Name: "universal_anti_chase", Status: status, Reason: reason, Details: details,
			})
		} else {
			gateDecisions = append(gateDecisions, store.EntryGateDecision{Name: "universal_anti_chase", Status: "disabled"})
		}
		if pullbackEvaluationRequired && !pullbackObservedAt.IsZero() {
			// Use the refreshed daily snapshot's close for reclaim, stop width,
			// signal persistence, and the initial risk pass. The pre-analysis
			// primary-timeframe close may be minutes old by this point.
			pullbackSignalPrice := signalPrice
			if len(entryDailyCandles) > 0 {
				pullbackSignalPrice = entryDailyCandles[len(entryDailyCandles)-1].Close
			}
			setup := pullback.Evaluate(entryDailyCandles, pullbackSignalPrice, pullback.Config{
				MinMomentumReturnPct:  eg.PullbackMinMomentumReturnPct,
				ConsolidationSessions: eg.PullbackConsolidationSessions,
				SupportTolerancePct:   eg.PullbackSupportTolerancePct,
				MaxStopWidthPct:       eg.PullbackMaxStopWidthPct,
			})
			pullbackQualificationDetails, _ = json.Marshal(setup.PromptContext())
			entryDecision := resolveAdditiveEntry(setup)
			if entryDecision.PullbackConfirmed {
				// The research entry is inseparable from its tight low-of-day
				// invalidation. Replace the analyst's generic stop before risk
				// sizing, persistence, and bracket submission.
				sig.StopLoss = setup.StopLoss
				signalPrice = pullbackSignalPrice
				sig.SignalPrice = pullbackSignalPrice
				sig.PullbackStrength = true
				sig.EntrySetup = entryDecision.EntrySetup
				sig.EntrySetupEvidence = entryDecision.Evidence
				pullbackConfirmed = true
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
			} else {
				pullbackFailureReason = setup.Reason
				entryGateFailedCheck = setup.FailedCheck
				logger.Info("pullback strength not confirmed; using standard entry path",
					"ticker", ticker,
					"failed_check", setup.FailedCheck,
					"reason", setup.Reason,
				)
			}
		}
		if pullbackSelected && !pullbackConfirmed && entryGateReason == "" {
			if pullbackFailureReason == "" {
				pullbackFailureReason = "daily setup evidence was unavailable for deterministic qualification"
				entryGateFailedCheck = "pullback_evidence"
			}
			entryGateReason = "pullback strength: deterministic qualification required for the selected lens: " + pullbackFailureReason
		}
		if pullbackConfirmed || pullbackSelected {
			status, reason := "passed", "deterministic pullback qualification passed"
			if !pullbackConfirmed {
				status, reason = "blocked", entryGateReason
			}
			gateDecisions = append(gateDecisions, store.EntryGateDecision{
				Name: "pullback_qualification", Status: status, Reason: reason,
				Details: pullbackQualificationDetails,
			})
		} else {
			gateDecisions = append(gateDecisions, store.EntryGateDecision{Name: "pullback_qualification", Status: "not_applicable"})
		}
		if entryGateReason == "" && !pullbackConfirmed && eg.TrendAlignmentEnabled {
			var evaluated bool
			entryGateReason, evaluated = evaluateTrendAlignmentGate(
				string(resp.Action), signalPrice, data.Timeframe1Day,
				entryDailyCandles, time.Now(), eg,
			)
			if !evaluated {
				logger.Warn("entry gates: daily trend evidence unavailable — trend gate fails open",
					"ticker", ticker, "timeframe", timeframe, "candles", len(candles))
			}
			status := entryGateAuditStatus(evaluated, entryGateReason)
			gateDecisions = append(gateDecisions, store.EntryGateDecision{Name: "trend_alignment", Status: status, Reason: entryGateReason})
		} else if !eg.TrendAlignmentEnabled {
			gateDecisions = append(gateDecisions, store.EntryGateDecision{Name: "trend_alignment", Status: "disabled"})
		}
		if !pullbackConfirmed && entryGateReason == "" &&
			(eg.NewsMinRunupPct > 0 || eg.MomentumMaxRunupPct > 0) {
			nowET := time.Now().In(nyLoc)
			dayStartET := time.Date(nowET.Year(), nowET.Month(), nowET.Day(), 0, 0, 0, 0, nyLoc)
			prevClose, pcErr := store.GetPrevDailyClose(ctx, ticker, dayStartET)
			evaluated := pcErr == nil && prevClose > 0
			if !evaluated {
				logger.Warn("entry gates: prior daily close unavailable — run-up gates fail open",
					"ticker", ticker, "err", pcErr)
			}
			entryGateReason = evaluateRunupGates(
				effectiveGateStrategy(strategy, resp.SelectedStrategy, admissibleLenses),
				string(resp.Action), signalPrice, prevClose, eg,
			)
			status := entryGateAuditStatus(evaluated, entryGateReason)
			gateDecisions = append(gateDecisions, store.EntryGateDecision{Name: "runup_anti_chase", Status: status, Reason: entryGateReason})
		}
		if entryGateReason != "" {
			sig.EntryGateDecisions, _ = json.Marshal(gateDecisions)
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
		sig.EntryGateDecisions, _ = json.Marshal(gateDecisions)
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
	if resp.Action == "BUY" && openTradesErr == nil {
		openTrades, openTradesErr = withWorkingStockEntryReservations(ctx, openTrades)
		if openTradesErr != nil {
			sig.Status = "blocked"
			sig.BlockReason = "unable to verify working stock entries (DB error)"
			if _, err := store.InsertSignal(ctx, sig); err != nil {
				logger.Warn("store blocked signal", "ticker", ticker, "err", err)
			}
			logger.Warn("signal blocked", "ticker", ticker, "reason", sig.BlockReason, "err", openTradesErr)
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
		SourceNode: analysisSourceNode,
		EntrySetup: sig.EntrySetup, SetupEvidence: sig.EntrySetupEvidence,
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
				EntrySetup: sig.EntrySetup, SetupEvidence: sig.EntrySetupEvidence,
			})
			return
		}
		// Straight off risk approval — no queue involved.
		e.executeSignal(ctx, sigID, sig, false)
	case "manual":
		// Signal stays pending — user approves via CLI or Telegram.
		logger.Info("signal queued for manual approval", "id", sigID, "ticker", ticker)
		e.bus.Publish(events.SignalQueued, events.SignalQueuedPayload{
			SignalID: sigID, Ticker: ticker, Action: string(resp.Action),
			Confidence: resp.Confidence, ExpiresIn: time.Until(sig.ExpiresAt),
			EntrySetup: sig.EntrySetup, SetupEvidence: sig.EntrySetupEvidence,
		})
	default:
		logger.Warn("unknown mode, defaulting to manual", "mode", mode)
	}
}

func entryGateAuditStatus(evaluated bool, blockReason string) string {
	if !evaluated {
		return "unavailable_fail_open"
	}
	if blockReason != "" {
		return "blocked"
	}
	return "passed"
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

// executionSourceNode names the observability node an execution leaves from.
// Empty keeps the default risk-controls attribution; queue-released executions
// name the pending queue so the dashboard lights the path actually taken.
func executionSourceNode(fromQueue bool) string {
	if fromQueue {
		return "approvals"
	}
	return ""
}

// executeSignal places the order via the executor, creates a trade record, and
// records the entry order.
//
// fromQueue marks an execution released from the pending queue — a manual
// approval, or a first-tick deferral picked up by processDeferredSignals — as
// opposed to executing straight off risk approval. It only rides along on the
// emitted event so the dashboard can animate the path the signal actually took;
// it has no effect on execution itself.
func (e *Engine) executeSignal(ctx context.Context, sigID string, sig store.Signal, fromQueue bool) {
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
			if err := store.SetInitialSignalReferencePrice(ctx, sigID, sig.SignalPrice); err != nil {
				logger.Warn("set initial signal reference price", "id", sigID, "err", err)
			}
			sig.AcceptedReferencePrice = sig.SignalPrice
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
	// Preserve the immutable accepted signal mark before a pullback
	// revalidation or broker admission can deliberately reprice the execution.
	// SignalPrice is mutable execution state; AcceptedReferencePrice is the
	// stale-price baseline across deferred and broker-admission retries.
	signalReferencePrice := sig.AcceptedReferencePrice
	if signalReferencePrice <= 0 {
		signalReferencePrice = sig.SignalPrice
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
	if sig.PullbackStrength && !fromQueue {
		sessionLow, err := pullbackSignalSessionLow(sig)
		if err != nil {
			reason := "pullback strength: unable to recover original signal-session low"
			_ = store.UpdateSignalStatus(ctx, sigID, "blocked", reason)
			logger.Warn("pullback execution blocked", "ticker", sig.Ticker, "reason", reason, "err", err)
			e.bus.Publish(events.SignalBlocked, events.SignalBlockedPayload{
				Ticker: sig.Ticker, Action: sig.Action, Confidence: sig.Confidence,
				BlockReason: reason, FailedCheck: "pullback_execution_session_low",
			})
			return
		}
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
		if _, err := e.fetchAndStoreCandlesObserved(ctx, sig.Ticker, data.Timeframe1Day); err != nil {
			reason := "pullback strength: unable to refresh daily setup before execution"
			_ = store.UpdateSignalStatus(ctx, sigID, "blocked", reason)
			logger.Warn("pullback execution blocked", "ticker", sig.Ticker, "reason", reason, "err", err)
			e.bus.Publish(events.SignalBlocked, events.SignalBlockedPayload{
				Ticker: sig.Ticker, Action: sig.Action, Confidence: sig.Confidence,
				BlockReason: reason, FailedCheck: "pullback_execution_refresh",
			})
			return
		}
		freshDailyCandles, err := store.GetCandles(
			ctx, sig.Ticker, data.Timeframe1Day,
			time.Now().AddDate(-2, 0, 0), time.Now(),
		)
		if err != nil {
			reason := "pullback strength: unable to load refreshed daily setup before execution"
			_ = store.UpdateSignalStatus(ctx, sigID, "blocked", reason)
			logger.Warn("pullback execution blocked", "ticker", sig.Ticker, "reason", reason, "err", err)
			e.bus.Publish(events.SignalBlocked, events.SignalBlockedPayload{
				Ticker: sig.Ticker, Action: sig.Action, Confidence: sig.Confidence,
				BlockReason: reason, FailedCheck: "pullback_execution_daily_evidence",
			})
			return
		}
		freshSetup := pullback.Evaluate(freshDailyCandles, mark, pullback.Config{
			MinMomentumReturnPct:  e.cfg.Risk.EntryGates.PullbackMinMomentumReturnPct,
			ConsolidationSessions: e.cfg.Risk.EntryGates.PullbackConsolidationSessions,
			SupportTolerancePct:   e.cfg.Risk.EntryGates.PullbackSupportTolerancePct,
			MaxStopWidthPct:       e.cfg.Risk.EntryGates.PullbackMaxStopWidthPct,
		})
		if reason := pullback.ValidateExecutionSetup(
			freshSetup, sessionLow, sig.StopLoss, mark,
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
		if minTraded <= sessionLow {
			reason := fmt.Sprintf(
				"pullback strength: setup invalidated before execution (minimum traded %.2f at/below low-of-day stop %.2f)",
				minTraded, sessionLow,
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
		openTrades, err = withWorkingStockEntryReservations(ctx, openTrades)
		if err != nil {
			reason := "pullback execution risk re-check failed: unable to verify working stock entries"
			_ = store.UpdateSignalStatus(ctx, sigID, "blocked", reason)
			logger.Warn("pullback execution blocked", "ticker", sig.Ticker, "reason", reason, "err", err)
			return
		}
		riskIn := buildRiskInput(e.cfg.Risk, sig.Action, sig.Ticker,
			sig.Confidence, sig.SizePct, sig.StopLoss, sig.TakeProfit, mark,
			e.portfolio.CurrentState(), openTrades)
		if results, passed := risk.CheckEntryQuality(riskIn); !passed {
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
		owned, err = withWorkingStockEntryReservations(ctx, owned)
		if err != nil {
			reason := "unable to verify working stock entries (DB error)"
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

	intentKey := "entry:" + sigID
	admission, existingRequest, effectiveIntentKey, releaseAdmission, err := e.beginStockEntryAdmission(
		ctx, intentKey, sig.Ticker, sig.SizePct,
	)
	if err != nil {
		if errors.Is(err, errStockEntryIntentComplete) {
			return
		}
		if fromQueue && existingRequest != nil {
			if queueErr := store.QueueSignalForDeferredExecution(ctx, sigID); queueErr != nil {
				logger.Warn("keep prepared stock entry queued after temporary admission failure", "id", sigID, "err", queueErr)
			}
			logger.Warn("prepared stock entry admission unavailable — retrying on a later tick", "ticker", sig.Ticker, "err", err)
			return
		}
		reason := "unable to verify durable entry replay"
		_ = store.UpdateSignalStatus(ctx, sigID, "blocked", reason)
		logger.Warn("signal execution blocked", "ticker", sig.Ticker, "reason", reason, "err", err)
		return
	}
	defer releaseAdmission()
	intentKey = effectiveIntentKey

	pState := e.portfolio.CurrentState()
	qty := admission.Quantity
	if admission.BrokerAuthoritative && !admission.BrokerHeldReplay {
		// The broker admission ask is the authoritative execution price. Check it
		// against the accepted thesis price before any optional anti-chase work so
		// stale-price protection is never disabled with the anti-chase policy.
		if e.rejectStaleSignalExecution(ctx, sigID, sig, signalReferencePrice, admission.Ask) {
			return
		}
		sig.SignalPrice = admission.Ask
		if err := store.UpdateSignalPrice(ctx, sigID, sig.SignalPrice); err != nil {
			logger.Warn("update signal with fresh broker ask", "id", sigID, "err", err)
		}
	}
	var finalExecutionQuote *data.Quote
	var pullbackVerifiedThrough time.Time
	// Every non-broker release that acquired a current execution quote must
	// prove it is still fresh at submission. Anti-chase and stale-only paths are
	// mutually exclusive, so this never triggers an additional provider fetch.
	finalExecutionQuoteRequired := requiresFinalExecutionQuote(
		admission, e.cfg.Risk.EntryGates.AntiChase.Enabled, sig.StalePriceMovePct,
	)
	if needsFreshStaleSignalQuote(admission, e.cfg.Risk.EntryGates.AntiChase.Enabled, sig.StalePriceMovePct) {
		// Without anti-chase evidence there is no later quote acquisition. Still
		// obtain one validated two-sided quote when stale-price protection is
		// configured, rather than allowing a delayed approval to chase a move.
		quote, finalAsk := e.freshAntiChaseQuote(ctx, sig.Ticker)
		if finalAsk <= 0 {
			reason := "unable to verify current price for stale-signal check"
			_ = store.UpdateSignalStatus(ctx, sigID, "blocked", reason)
			logger.Warn("signal execution blocked", "ticker", sig.Ticker, "reason", reason)
			e.bus.Publish(events.SignalBlocked, events.SignalBlockedPayload{
				Ticker: sig.Ticker, Action: sig.Action, Confidence: sig.Confidence,
				BlockReason: reason, FailedCheck: "stale_signal_price_quote",
			})
			return
		}
		if e.rejectStaleSignalExecution(ctx, sigID, sig, signalReferencePrice, finalAsk) {
			return
		}
		sig.SignalPrice = finalAsk
		finalExecutionQuote = quote
		if err := store.UpdateSignalPrice(ctx, sigID, sig.SignalPrice); err != nil {
			logger.Warn("update signal with fresh stale-signal ask", "id", sigID, "err", err)
		}
	}
	if e.cfg.Risk.EntryGates.AntiChase.Enabled && !admission.BrokerHeldReplay {
		antiPolicy := antiChasePolicy(e.cfg.Risk.EntryGates.AntiChase)
		source := "broker_validated_ask"
		if !admission.BrokerAuthoritative {
			source = "fresh_market_ask"
		}
		now := time.Now().UTC()
		daily, news, evidenceReason := e.executionAntiChaseEvidence(ctx, sig.Ticker, now, antiPolicy.StaleCatalystRunupEnabled)
		var quote *data.Quote
		finalAsk := admission.Ask
		if admission.BrokerAuthoritative {
			// The coordinated broker admission ask is coupled to the account,
			// capacity, and quantity snapshots. Never replace it with a generic
			// market-data quote; if it ages during evidence gathering, retry from
			// a complete fresh admission instead of mixing data authorities.
			if err := validateAdmissionTimestamp("quote", admission.QuoteAt, time.Now().UTC()); err != nil {
				e.queueBrokerAdmissionRefresh(ctx, sigID, sig, signalReferencePrice,
					"broker admission aged during anti-chase evidence refresh", err)
				return
			}
		} else {
			quote, finalAsk = e.freshAntiChaseQuote(ctx, sig.Ticker)
		}
		var snapshot *antichase.Snapshot
		if finalAsk > 0 {
			// Daily evidence may take seconds to refresh. The non-broker quote is
			// deliberately fetched afterward, so both this decision and the market
			// order use the final validated ask.
			if !admission.BrokerAuthoritative && e.rejectStaleSignalExecution(ctx, sigID, sig, signalReferencePrice, finalAsk) {
				return
			}
			sig.SignalPrice = finalAsk
			finalExecutionQuote = quote
			if err := store.UpdateSignalPrice(ctx, sigID, sig.SignalPrice); err != nil {
				logger.Warn("update signal with final anti-chase ask", "id", sigID, "err", err)
			}
		}
		if evidenceReason != "" {
			snapshot = &antichase.Snapshot{
				SchemaVersion: antichase.PolicyVersion, Ticker: sig.Ticker, ObservedAt: now,
				ReferencePrice: sig.SignalPrice, ReferenceSource: source, EvidenceStatus: "unavailable",
				HistoryScope: "limited_history", EvidenceReason: evidenceReason,
			}
		} else if finalAsk <= 0 {
			snapshot = &antichase.Snapshot{
				SchemaVersion: antichase.PolicyVersion, Ticker: sig.Ticker, ObservedAt: time.Now().UTC(),
				ReferencePrice: sig.SignalPrice, ReferenceSource: source, EvidenceStatus: "unavailable",
				HistoryScope: "limited_history", EvidenceReason: "anti-chase: fresh two-sided ask is unavailable",
			}
		} else {
			snapshot = buildAntiChaseSnapshot(sig.Ticker, daily, sig.SignalPrice, source, news, time.Now())
		}
		reason, decision := antiChaseEntryGate(snapshot, antiPolicy)
		details, _ := json.Marshal(struct {
			Snapshot *antichase.Snapshot `json:"snapshot"`
			Decision antichase.Decision  `json:"decision"`
		}{snapshot, decision})
		observedAt := time.Now().UTC()
		gate := store.EntryGateDecision{
			Name: "universal_anti_chase_release", Status: "passed",
			Details: details, ObservedAt: &observedAt,
		}
		if reason != "" {
			gate.Status, gate.Reason = "blocked", reason
			if err := store.BlockSignalWithEntryGateDecision(ctx, sigID, reason, gate); err != nil {
				logger.Warn("persist anti-chase execution block", "id", sigID, "err", err)
			}
			logger.Warn("signal execution blocked by universal anti-chase gate",
				"ticker", sig.Ticker, "reason", reason)
			e.bus.Publish(events.SignalBlocked, events.SignalBlockedPayload{
				Ticker: sig.Ticker, Action: sig.Action, Confidence: sig.Confidence,
				BlockReason: reason, FailedCheck: "universal_anti_chase_release",
			})
			return
		}
		if err := store.AppendSignalEntryGateDecision(ctx, sigID, gate); err != nil {
			reason := "anti-chase: unable to persist final timing release audit"
			_ = store.UpdateSignalStatus(ctx, sigID, "blocked", reason)
			logger.Warn("signal execution blocked — final anti-chase audit failed", "ticker", sig.Ticker, "err", err)
			return
		}
	}
	if sig.PullbackStrength && !admission.BrokerHeldReplay {
		requestedMark := 0.0
		if admission.BrokerAuthoritative {
			// Keep the coordinated broker admission ask as the price for sizing and
			// release controls. The final exact-path/reclaim check below still uses
			// the independently refreshed mark to reject a broken or extended setup.
			requestedMark = sig.SignalPrice
		}
		finalMark, finalQuote, verifiedThrough, err := e.revalidatePullbackExecution(ctx, sig, requestedMark)
		if err != nil {
			reason := "pullback strength: final execution revalidation failed: " + err.Error()
			_ = store.UpdateSignalStatus(ctx, sigID, "blocked", reason)
			logger.Warn("pullback execution blocked", "ticker", sig.Ticker, "reason", reason, "err", err)
			e.bus.Publish(events.SignalBlocked, events.SignalBlockedPayload{
				Ticker: sig.Ticker, Action: sig.Action, Confidence: sig.Confidence,
				BlockReason: reason, FailedCheck: "pullback_execution_final",
			})
			return
		}
		if admission.BrokerAuthoritative {
			if err := validateAdmissionTimestamp("quote", admission.QuoteAt, time.Now().UTC()); err != nil {
				e.queueBrokerAdmissionRefresh(ctx, sigID, sig, signalReferencePrice,
					"broker admission aged during final pullback revalidation", err)
				return
			}
		} else {
			// The slow pullback refresh can move the executable ask after the
			// earlier stale-price and anti-chase releases. Reapply both controls
			// to this final mark before it becomes the submitted order price.
			if e.rejectStaleSignalExecution(ctx, sigID, sig, signalReferencePrice, finalMark) ||
				e.recheckPullbackTimingGate(ctx, sigID, sig, finalMark) {
				return
			}
			sig.SignalPrice = finalMark
			finalExecutionQuote = finalQuote
			if err := store.UpdateSignalPrice(ctx, sigID, sig.SignalPrice); err != nil {
				logger.Warn("update final pullback execution price", "id", sigID, "err", err)
			}
		}
		pullbackVerifiedThrough = verifiedThrough
	}
	if !admission.BrokerAuthoritative {
		positionValue := pState.Cash * (sig.SizePct / 100)
		qty = positionValue / sig.SignalPrice
	}
	if qty <= 0 {
		reason := fmt.Sprintf("zero quantity (size_pct=%.2f signal_price=%.4f)", sig.SizePct, sig.SignalPrice)
		if err := store.UpdateSignalStatus(ctx, sigID, "blocked", reason); err != nil {
			logger.Warn("update signal status", "id", sigID, "err", err)
		}
		logger.Warn("signal execution skipped — zero quantity",
			"ticker", sig.Ticker,
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
		Ticker:           sig.Ticker,
		Side:             side,
		Type:             execution.OrderTypeMarket,
		Quantity:         qty,
		ReferencePrice:   sig.SignalPrice, // paper executor fills at this price
		IntentKey:        intentKey,
		PolicyStopLoss:   sig.StopLoss,
		PolicyTakeProfit: sig.TakeProfit,
	}
	replayingExisting := existingRequest != nil
	if replayingExisting {
		orderReq = *existingRequest
		hydrateStockEntryReplayPolicy(&sig, orderReq)
		if !admission.BrokerHeldReplay {
			var repriceErr error
			orderReq, repriceErr = repricePreparedStockEntryRequest(orderReq, qty, sig.SignalPrice)
			if repriceErr != nil {
				reason := "unable to preserve whole-share protected entry replay"
				_ = store.UpdateSignalStatus(ctx, sigID, "blocked", reason)
				logger.Warn("signal execution blocked", "ticker", sig.Ticker, "reason", reason, "err", repriceErr)
				return
			}
		}
	}

	// The initial broker-protection policy is deliberately fixed brackets. An
	// uncapped entry installs a standalone stop only after a fill, so it cannot
	// meet the protected-entry invariant.
	strictBracketPolicy := !replayingExisting && side == execution.OrderSideBuy && e.cfg.Risk.BracketExitsEnabled
	if replayingExisting && side == execution.OrderSideBuy && e.cfg.Risk.BracketExitsEnabled && !orderReq.RequireBrokerProtection {
		if admission.BrokerHeldReplay {
			// A pre-policy working BUY cannot be replayed under the strict
			// invariant. Retire its broker order first; a cancellation-race fill
			// is projected by recovery and then contained under the active policy.
			e.cancelLegacyBrokerHeldEntry(ctx, intentKey)
			return
		}
		reason := "prepared entry predates the required broker-protection policy"
		_ = store.UpdateSignalStatus(ctx, sigID, "blocked", reason)
		logger.Warn("signal execution blocked", "ticker", sig.Ticker, "reason", reason,
			"intent_key", orderReq.IntentKey)
		return
	}
	if strictBracketPolicy {
		if _, paper := e.executor.(*execution.PaperExecutor); paper {
			reason := "broker protection requires an Alpaca bracket-capable executor; the local paper executor cannot simulate broker children"
			_ = store.UpdateSignalStatus(ctx, sigID, "blocked", reason)
			logger.Warn("signal execution blocked", "ticker", sig.Ticker, "reason", reason)
			e.bus.Publish(events.SignalBlocked, events.SignalBlockedPayload{
				Ticker: sig.Ticker, Action: sig.Action, Confidence: sig.Confidence,
				BlockReason: reason, FailedCheck: "broker protection",
			})
			return
		}
	}

	// The uncapped exit policy is active only with the trailing manager: this
	// guarantees the position retains a broker hard stop while the selected
	// trail owns the winning exit. Keeping the manager requirement here makes
	// an accidentally-partial config fail safe to the existing fixed TP.
	uncapRequested := !strictBracketPolicy && !replayingExisting && side == execution.OrderSideBuy && e.cfg.Risk.UncapTakeProfit
	uncapTakeProfit, uncappedQty := resolveUncappedEntry(
		e.cfg.Risk, side, qty, sig.SignalPrice, sig.StopLoss,
		e.trailingManagerAvailable,
	)
	if strictBracketPolicy {
		uncapTakeProfit = false
	}
	if replayingExisting {
		uncapTakeProfit = orderReq.UncappedTakeProfit
	} else if uncapTakeProfit {
		orderReq.Quantity = uncappedQty
		orderReq.UncappedTakeProfit = true
	} else if uncapRequested {
		logger.Warn("exit policy: uncapped entry lacks a running trail, whole-share sizing, or a coherent hard stop; keeping baseline fixed take-profit",
			"ticker", sig.Ticker,
			"qty", qty,
			"signal_price", sig.SignalPrice,
			"stop_loss", sig.StopLoss,
		)
	}

	// Broker-resident bracket exits (#541). With this policy enabled, every
	// admitted BUY is a valid whole-share bracket. There is no app-managed
	// fallback for a fractional or malformed protected entry.
	bracketed, mayBuildBracket := stockEntryBracketReplayState(replayingExisting, orderReq)
	if strictBracketPolicy {
		stopLoss, takeProfit, eligible := normalizedStockBracketPrices(sig.SignalPrice, sig.StopLoss, sig.TakeProfit)
		whole := math.Floor(qty)
		if !eligible || whole < 1 {
			reason := "broker protection requires a compatible whole-share bracket"
			_ = store.UpdateSignalStatus(ctx, sigID, "blocked", reason)
			logger.Warn("signal execution blocked", "ticker", sig.Ticker, "reason", reason,
				"qty", qty, "stop_loss", sig.StopLoss, "take_profit", sig.TakeProfit)
			e.bus.Publish(events.SignalBlocked, events.SignalBlockedPayload{
				Ticker: sig.Ticker, Action: sig.Action, Confidence: sig.Confidence,
				BlockReason: reason, FailedCheck: "broker protection",
			})
			return
		}
		sig.StopLoss, sig.TakeProfit = stopLoss, takeProfit
		orderReq.Quantity = whole
		orderReq.OrderClass = "bracket"
		orderReq.TakeProfitPrice = takeProfit
		orderReq.StopLossPrice = stopLoss
		orderReq.PolicyTakeProfit = takeProfit
		orderReq.PolicyStopLoss = stopLoss
		orderReq.RequireBrokerProtection = true
		orderReq.TimeInForce = "gtc"
		bracketed = true
	} else if mayBuildBracket && stockBracketEligible(e.cfg.Risk, side, sig.SignalPrice, sig.StopLoss, sig.TakeProfit, uncapTakeProfit) {
		if whole := math.Floor(qty); whole >= 1 {
			orderReq.Quantity = whole
			orderReq.OrderClass = "bracket"
			orderReq.TakeProfitPrice = sig.TakeProfit
			orderReq.StopLossPrice = sig.StopLoss
			orderReq.TimeInForce = "gtc"
			bracketed = true
		}
	}
	if uncapTakeProfit {
		logger.Info("exit policy: fixed take-profit uncapped; trailing manager owns winning exit",
			"ticker", sig.Ticker,
			"trailing_rule", e.cfg.Risk.TrailingStop.Rule,
		)
	}

	// Run the complete release gate against the exact quantity and fresh ask
	// after bracket/uncapped transformations. Signal-generation checks omit
	// ProposedQuantity and retain their legacy early-screening behavior.
	owned := []store.Trade(nil)
	var durableOwned []store.Trade
	if store.IsConnected() && !admission.BrokerHeldReplay {
		owned, err = store.GetOwnedPositionTrades(ctx)
		if err != nil {
			reason := "unable to verify open positions for final entry risk check"
			_ = store.UpdateSignalStatus(ctx, sigID, "blocked", reason)
			logger.Warn("signal execution blocked", "ticker", sig.Ticker, "reason", reason, "err", err)
			return
		}
		durableOwned = append(durableOwned, owned...)
		if admission.BrokerAuthoritative {
			owned = append(owned, reservationTrades(admission.Reservations, intentKey)...)
			owned = append(owned, manualWorkingEntryTrades(admission.WorkingOrders, admission.Reservations, admission.Ask)...)
		} else {
			owned, err = withWorkingStockEntryReservations(ctx, owned)
			if err != nil {
				reason := "unable to verify working stock entries for final risk check"
				_ = store.UpdateSignalStatus(ctx, sigID, "blocked", reason)
				logger.Warn("signal execution blocked", "ticker", sig.Ticker, "reason", reason, "err", err)
				return
			}
		}
	}
	riskState := brokerStockRiskState(pState, admission)
	riskIn := buildRiskInput(e.cfg.Risk, sig.Action, sig.Ticker,
		sig.Confidence, sig.SizePct, sig.StopLoss, sig.TakeProfit, sig.SignalPrice,
		riskState, owned)
	// A partially filled entry has an attached trade for the filled shares, so
	// reservationTrades deliberately omits its remaining working shares to avoid
	// counting a second position. Its unfilled remainder is still live exposure
	// if it fills, however, and must count toward the aggregate ticker cap.
	riskIn.ExistingTickerNotional += attachedReservationNotional(
		admission.Reservations, intentKey, sig.Ticker, sig.SignalPrice,
	)
	if admission.BrokerAuthoritative && !admission.BrokerHeldReplay {
		// Broker positions include manual/unadopted shares, but the snapshot can
		// predate local fill projection. The local risk input already includes
		// every pending reservation, so compare it with the broker holding plus
		// that same pending exposure instead of adding pending shares twice.
		riskIn.ExistingTickerNotional = mergedTickerExposure(
			riskIn.ExistingTickerNotional,
			brokerLongNotional(admission.BrokerPositions, sig.Ticker, sig.SignalPrice),
			pendingStockEntryNotional(admission.Reservations, admission.WorkingOrders, intentKey, sig.Ticker, sig.SignalPrice),
		)
		if err := mergeBrokerLongOccupancy(&riskIn, admission.BrokerPositions, sig.Ticker, durableOwned); err != nil {
			reason := err.Error()
			_ = store.UpdateSignalStatus(ctx, sigID, "blocked", reason)
			logger.Warn("signal execution blocked", "ticker", sig.Ticker, "reason", reason)
			return
		}
	}
	if riskIn.HoldsLong && !replayingExisting {
		sig.StopLoss = pyramidTightenedStop(
			sig.StopLoss, riskIn.LongMaxStop, riskIn.LongAvgEntryPrice, sig.SignalPrice,
		)
		orderReq.PolicyStopLoss = sig.StopLoss
		if orderReq.OrderClass == "bracket" {
			orderReq.StopLossPrice = sig.StopLoss
		}
		riskIn.StopLoss = sig.StopLoss
	}
	if orderReq.RequireBrokerProtection {
		stopLoss, takeProfit, eligible := normalizedStockBracketPrices(sig.SignalPrice, sig.StopLoss, sig.TakeProfit)
		if !eligible {
			reason := "broker protection parameters became incompatible after final risk sizing"
			_ = store.UpdateSignalStatus(ctx, sigID, "blocked", reason)
			logger.Warn("signal execution blocked", "ticker", sig.Ticker, "reason", reason)
			return
		}
		sig.StopLoss, sig.TakeProfit = stopLoss, takeProfit
		orderReq.PolicyStopLoss, orderReq.PolicyTakeProfit = stopLoss, takeProfit
		orderReq.StopLossPrice, orderReq.TakeProfitPrice = stopLoss, takeProfit
		riskIn.StopLoss, riskIn.TakeProfit = stopLoss, takeProfit
	}
	riskIn.ProposedQuantity = orderReq.Quantity
	riskIn.ProposedNotional = orderReq.Quantity * sig.SignalPrice
	if admission.BrokerAuthoritative && !admission.BrokerHeldReplay && riskIn.ProposedNotional > admission.RemainingCapacity+1e-7 {
		reason := "exact entry quantity exceeds available buying power"
		_ = store.UpdateSignalStatus(ctx, sigID, "blocked", reason)
		logger.Warn("signal execution blocked", "ticker", sig.Ticker, "reason", reason)
		return
	}
	if results, passed := checkStockEntryReleaseRisk(admission, riskIn); !passed {
		reason := "execution risk re-check failed: " + risk.BlockReason(results)
		_ = store.UpdateSignalStatus(ctx, sigID, "blocked", reason)
		logger.Warn("signal execution blocked", "ticker", sig.Ticker, "reason", reason)
		e.bus.Publish(events.SignalBlocked, events.SignalBlockedPayload{
			Ticker: sig.Ticker, Action: sig.Action, Confidence: sig.Confidence,
			BlockReason: reason, FailedCheck: risk.BlockReason(results),
		})
		return
	}

	if err := validateStockAdmissionFreshness(admission, time.Now().UTC()); err != nil {
		if admission.BrokerAuthoritative && !admission.BrokerHeldReplay {
			e.queueBrokerAdmissionRefresh(ctx, sigID, sig, signalReferencePrice,
				"broker admission aged before release controls", err)
			return
		}
		reason := err.Error()
		_ = store.UpdateSignalStatus(ctx, sigID, "blocked", reason)
		logger.Warn("signal execution blocked before broker release", "ticker", sig.Ticker, "reason", reason)
		return
	}
	// Admission identifies whether this is a client-ID replay of an order the
	// broker already holds. A pause blocks fresh submissions, but it must never
	// abandon that already-live order before its trade/protection projection.
	if !admission.BrokerHeldReplay {
		if enabled, reason := buyEntriesEnabled(ctx); !enabled {
			if err := store.UpdateSignalStatus(ctx, sigID, "blocked", reason); err != nil {
				logger.Warn("update signal status", "id", sigID, "err", err)
			}
			// A client-ID miss is not proof that a prepared order never reached
			// the broker: a crash can occur after acceptance but before its
			// attempt marker commits. Keep that reservation recoverable so a
			// late broker fill is still projected and protected; the pause
			// prevents a fresh release on every retry.
			logger.Warn("signal execution blocked before broker release", "ticker", sig.Ticker, "reason", reason)
			e.bus.Publish(events.SignalBlocked, events.SignalBlockedPayload{
				Ticker: sig.Ticker, Action: sig.Action, Confidence: sig.Confidence,
				BlockReason: reason, FailedCheck: "buy_control",
			})
			return
		}
	}
	if !admission.BrokerHeldReplay && !IsMarketOpen(time.Now()) {
		reason := "market closed before order release — order would not fill this session"
		_ = store.UpdateSignalStatus(ctx, sigID, "blocked", reason)
		logger.Warn("signal execution blocked before broker release", "ticker", sig.Ticker, "reason", reason)
		e.bus.Publish(events.SignalBlocked, events.SignalBlockedPayload{
			Ticker: sig.Ticker, Action: sig.Action, Confidence: sig.Confidence,
			BlockReason: reason, FailedCheck: "market_hours",
		})
		return
	}
	if replayingExisting && !admission.BrokerHeldReplay {
		payload, err := json.Marshal(orderReq)
		if err != nil {
			reason := "unable to prepare repriced stock entry replay"
			_ = store.UpdateSignalStatus(ctx, sigID, "blocked", reason)
			logger.Warn("signal execution blocked", "ticker", sig.Ticker, "reason", reason, "err", err)
			return
		}
		if err := store.RefreshPreparedStockEntryRequest(ctx, intentKey, orderReq.Quantity, payload); err != nil {
			reason := "unable to durably refresh prepared entry at final anti-chase ask"
			_ = store.UpdateSignalStatus(ctx, sigID, "blocked", reason)
			logger.Warn("signal execution blocked", "ticker", sig.Ticker, "reason", reason, "err", err)
			return
		}
	}
	if !admission.BrokerHeldReplay && !IsMarketOpen(time.Now()) {
		reason := "market closed before final order submission — order would not fill this session"
		_ = store.UpdateSignalStatus(ctx, sigID, "blocked", reason)
		logger.Warn("signal execution blocked before final broker submission", "ticker", sig.Ticker, "reason", reason)
		e.bus.Publish(events.SignalBlocked, events.SignalBlockedPayload{
			Ticker: sig.Ticker, Action: sig.Action, Confidence: sig.Confidence,
			BlockReason: reason, FailedCheck: "market_hours",
		})
		return
	}
	// The final slow work above can span several seconds. Extend the exact trade
	// path immediately before the final release so a brief low-of-day breach and
	// recovery cannot be hidden between pullback qualification and submission.
	if sig.PullbackStrength && !admission.BrokerHeldReplay {
		quote, mark, err := e.verifyPullbackReleasePath(ctx, sig, pullbackVerifiedThrough)
		if err != nil {
			reason := "pullback strength: release-path revalidation failed: " + err.Error()
			_ = store.UpdateSignalStatus(ctx, sigID, "blocked", reason)
			logger.Warn("pullback execution blocked", "ticker", sig.Ticker, "reason", reason, "err", err)
			e.bus.Publish(events.SignalBlocked, events.SignalBlockedPayload{
				Ticker: sig.Ticker, Action: sig.Action, Confidence: sig.Confidence,
				BlockReason: reason, FailedCheck: "pullback_execution_release_path",
			})
			return
		}
		if !admission.BrokerAuthoritative {
			if e.rejectStaleSignalExecution(ctx, sigID, sig, signalReferencePrice, mark) ||
				e.recheckPullbackTimingGate(ctx, sigID, sig, mark) {
				return
			}
		}
		finalExecutionQuote = quote
		if _, paper := e.executor.(*execution.PaperExecutor); paper {
			// Paper market orders fill at ReferencePrice, so retain the final
			// verified executable mark rather than the earlier sizing quote.
			sig.SignalPrice = mark
			orderReq.ReferencePrice = mark
			paperRiskIn := buildRiskInput(e.cfg.Risk, sig.Action, sig.Ticker,
				sig.Confidence, sig.SizePct, sig.StopLoss, sig.TakeProfit, mark,
				riskState, owned)
			paperRiskIn.ProposedQuantity = orderReq.Quantity
			paperRiskIn.ProposedNotional = orderReq.Quantity * mark
			if results, passed := checkStockEntryReleaseRisk(admission, paperRiskIn); !passed {
				reason := "execution risk re-check failed after final paper pullback reprice: " + risk.BlockReason(results)
				_ = store.UpdateSignalStatus(ctx, sigID, "blocked", reason)
				logger.Warn("paper pullback execution blocked", "ticker", sig.Ticker, "reason", reason)
				e.bus.Publish(events.SignalBlocked, events.SignalBlockedPayload{
					Ticker: sig.Ticker, Action: sig.Action, Confidence: sig.Confidence,
					BlockReason: reason, FailedCheck: risk.BlockReason(results),
				})
				return
			}
			if err := store.UpdateSignalPrice(ctx, sigID, mark); err != nil {
				logger.Warn("update final paper pullback price", "id", sigID, "err", err)
			}
		}
	}
	if err := validateFinalStockEntryRelease(
		admission, finalExecutionQuoteRequired, finalExecutionQuote, sig.Ticker, time.Now(),
	); err != nil {
		if admission.BrokerAuthoritative && !admission.BrokerHeldReplay {
			e.queueBrokerAdmissionRefresh(ctx, sigID, sig, signalReferencePrice,
				"broker admission aged before final order submission", err)
			return
		}
		reason := err.Error()
		_ = store.UpdateSignalStatus(ctx, sigID, "blocked", reason)
		logger.Warn("signal execution blocked before final order submission", "ticker", sig.Ticker, "reason", reason)
		e.bus.Publish(events.SignalBlocked, events.SignalBlockedPayload{
			Ticker: sig.Ticker, Action: sig.Action, Confidence: sig.Confidence,
			BlockReason: reason, FailedCheck: "final_anti_chase_quote_freshness",
		})
		return
	}
	result, err := e.executor.PlaceOrder(ctx, orderReq)
	if err != nil {
		// Coordinators can return an accepted broker result with a local
		// projection error. Strict brackets must start durable recovery from that
		// identity immediately, before the parent can fill further with held legs.
		if orderReq.RequireBrokerProtection && result.BrokerOrderID != "" {
			e.recoverAndVerifyAcceptedBrokerEntryAsync(orderReq.IntentKey)
		}
		if errors.Is(err, execution.ErrTerminalZeroFillPlacement) {
			if updateErr := store.UpdateSignalStatus(ctx, sigID, "rejected", "entry order terminal zero-fill without a broker fill"); updateErr != nil {
				logger.Warn("reject terminal zero-fill entry signal", "ticker", sig.Ticker, "err", updateErr)
			}
		}
		logger.Warn("order placement failed", "ticker", sig.Ticker, "err", err)
		return
	}
	// A strict entry is durable before this broker request, but its local
	// trade/child projections below can still fail. Ensure every early return
	// after acceptance immediately enters durable recovery and broker coverage
	// verification instead of leaving a partial parent to keep filling.
	strictBrokerVerificationPending := orderReq.RequireBrokerProtection &&
		!brokerBracketProtectionVerified(result, result.FilledQty)
	if strictBrokerVerificationPending {
		defer func() {
			if strictBrokerVerificationPending {
				e.recoverAndVerifyAcceptedBrokerEntryAsync(orderReq.IntentKey)
			}
		}()
	}
	stockIntentID, projectionErr := stockIntentIDForAcceptedOrder(ctx, e.executor, orderReq.IntentKey)
	if projectionErr != nil {
		logger.Error("accepted stock order intent projection unavailable", "ticker", sig.Ticker, "intent_key", orderReq.IntentKey, "err", projectionErr)
		return
	}

	// Mark signal as executed.
	if err := store.UpdateSignalStatus(ctx, sigID, "executed", ""); err != nil {
		logger.Warn("update signal status", "id", sigID, "err", err)
	}
	// A working entry is not a position. Persist its requested quantity under
	// the durable intent, but do not create a zero-quantity trade that would
	// be mistaken for a live holding after restart. Recovery projects the one
	// trade only once broker truth reports a cumulative fill.
	if stockIntentID != "" && result.FilledQty <= 0 {
		if _, err := store.InsertOrderWithRetry(ctx, store.Order{
			BrokerOrderID: result.BrokerOrderID,
			Ticker:        sig.Ticker,
			Side:          string(side),
			OrderType:     string(execution.OrderTypeMarket),
			Quantity:      orderReq.Quantity,
			Status:        execution.NormalizeOrderStatus(result.Status),
			StockIntentID: stockIntentID,
		}); err != nil {
			logger.Warn("insert unfilled entry intent projection", "ticker", sig.Ticker, "err", err)
		}
		// Keep the normal 3-second fill cadence even though there is no trade
		// row yet. The coordinator turns the immutable intent into the one
		// trade/order projection as soon as broker truth shows a fill.
		e.pollUnfilledStockIntent(orderReq.IntentKey, result.BrokerOrderID)
		return
	}

	// Create trade record.
	// The software position monitor reads trades.take_profit. Clearing the
	// persisted value is load-bearing: merely omitting the broker bracket
	// would still let the hourly monitor cap the winner at the model TP.
	tradeTakeProfit := persistedTakeProfit(sig.TakeProfit, uncapTakeProfit)
	trade := store.Trade{
		SignalID:           sigID,
		Ticker:             sig.Ticker,
		Direction:          direction,
		Strategy:           sig.Strategy,
		EntryPrice:         result.FilledPrice,
		Quantity:           result.FilledQty,
		StopLoss:           sig.StopLoss,
		TakeProfit:         tradeTakeProfit,
		EntrySetup:         sig.EntrySetup,
		EntrySetupEvidence: sig.EntrySetupEvidence,
		Status:             "open",
	}
	trade.StockIntentID = stockIntentID
	tradeID, err := store.InsertTradeForStockIntent(ctx, trade)
	if err != nil {
		logger.Warn("insert trade", "ticker", sig.Ticker, "err", err)
		return
	}
	// The durable intent predates the broker request; once this entry becomes
	// a trade, make that relationship explicit on both projections. Recovery
	// can then find the one order/trade to update instead of inferring by
	// ticker when a response was accepted before these writes completed.
	if stockIntentID != "" {
		if err := store.AttachStockOrderIntentTradeByKey(ctx, orderReq.IntentKey, tradeID); err != nil {
			logger.Warn("attach entry stock intent to trade", "trade_id", tradeID, "err", err)
		}
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
		Quantity:      orderReq.Quantity,
		FilledPrice:   result.FilledPrice,
		FilledQty:     result.FilledQty,
		Status:        execution.NormalizeOrderStatus(result.Status),
		StockIntentID: stockIntentID,
	}
	if !result.FilledAt.IsZero() {
		filledAt := result.FilledAt
		entryOrder.FilledAt = &filledAt
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
					ExitReason:   store.ExitReasonTakeProfit,
					ExitEvidence: newExitIntent(store.ExitReasonTakeProfit, map[string]any{"take_profit": sig.TakeProfit}).Evidence,
				},
				{
					TradeID: tradeID, BrokerOrderID: result.ChildOrderIDs[1],
					Ticker: sig.Ticker, Side: string(execution.OrderSideSell),
					OrderType: string(execution.OrderTypeStop), Quantity: orderReq.Quantity,
					StopPrice: sig.StopLoss, Status: "pending",
					IsStopLoss: true, ParentOrderID: result.BrokerOrderID,
					ExitReason:   store.ExitReasonStopLoss,
					ExitEvidence: newExitIntent(store.ExitReasonStopLoss, map[string]any{"stop_loss": sig.StopLoss}).Evidence,
				},
			}
			for i := range legRows {
				if stockIntentID != "" {
					role, price := "take-profit", legRows[i].LimitPrice
					if legRows[i].IsStopLoss {
						role, price = "stop-loss", legRows[i].StopPrice
					}
					childIntentID, err := store.UpsertBracketChildIntent(
						ctx, stockIntentID, tradeID, sig.Ticker, role,
						legRows[i].BrokerOrderID, legRows[i].OrderType, legRows[i].Quantity, price,
					)
					if err != nil {
						// The broker has already created this leg, but without its
						// durable child identity we must not write an unlinked order
						// row. Recovery will replay the parent projection instead.
						logger.Error("persist bracket child intent; deferring leg projection", "trade_id", tradeID,
							"broker_order_id", legRows[i].BrokerOrderID, "err", err)
						return
					}
					legRows[i].StockIntentID = childIntentID
				}
			}
			for i := range legRows {
				applyBracketChildSnapshot(&legRows[i], result.ChildOrders)
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
			if orderReq.RequireBrokerProtection {
				logger.Error("broker protection: bracket response omitted child legs — containing entry",
					"trade_id", tradeID, "ticker", sig.Ticker)
			} else {
				logger.Warn("bracket requested but broker returned no child legs — exits remain app-managed",
					"trade_id", tradeID, "ticker", sig.Ticker)
			}
		}
	}
	discoveryResidencyRenewed := false
	entryProtectionVerified := entryProtectionVerifiedForResidency(orderReq, result, initialProtectedQty)
	if !result.FilledAt.IsZero() && entryProtectionVerified {
		if err := e.RenewRecoveredEntryDiscoveryResidency(ctx, sig.Ticker, result.FilledAt); err != nil {
			logger.Warn("renew discovery residency after protected stock entry", "ticker", sig.Ticker, "err", err)
		} else {
			discoveryResidencyRenewed = true
			if stockIntentID != "" {
				if err := store.MarkStockOrderIntentDiscoveryResidencyRenewed(ctx, stockIntentID); err != nil {
					logger.Warn("mark discovery residency renewal complete", "ticker", sig.Ticker, "stock_intent_id", stockIntentID, "err", err)
				}
			}
		}
	} else if result.FilledAt.IsZero() {
		logger.Debug("skip discovery residency renewal without authoritative fill timestamp", "ticker", sig.Ticker)
	} else {
		logger.Debug("defer discovery residency renewal until entry protection is verified",
			"ticker", sig.Ticker, "filled_qty", result.FilledQty)
	}
	if strictBrokerVerificationPending {
		e.verifyOrContainBrokerEntryAsync(tradeID, result.BrokerOrderID, sig.Ticker, orderReq.Quantity)
		strictBrokerVerificationPending = false
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
		EntrySetup: sig.EntrySetup, SetupEvidence: sig.EntrySetupEvidence,
		SourceNode: executionSourceNode(fromQueue),
	})

	// Poll every nonterminal broker order and backfill until it reaches a final
	// state. A nonzero price does not imply completion: Alpaca can return an
	// immediate partial fill, then continue filling the remaining quantity.
	// Stopping at that first partial freezes the smaller quantity as trade truth
	// and leaves later shares untracked and under-protected.
	if shouldPollEntryFill(result) {
		brokerOrderID := result.BrokerOrderID
		executor := e.executor
		discoveryFillAt := result.FilledAt
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
				// A broker may accept the entry before it fills, so the initial
				// response has no authoritative fill time. Renew discovery
				// residency when polling first observes that fill, but only once
				// the newly filled quantity is protected.
				fillAt := status.FilledAt
				if fillAt.IsZero() {
					fillAt = discoveryFillAt
				}
				protectionConfirmed := entryProtectionVerifiedForResidency(orderReq, status, lastProtectedQty)
				if !discoveryResidencyRenewed && status.FilledQty > 0 &&
					!fillAt.IsZero() && protectionConfirmed {
					renewCtx, renewCancel := context.WithTimeout(context.Background(), 5*time.Second)
					if err := e.RenewRecoveredEntryDiscoveryResidency(
						renewCtx, sig.Ticker, fillAt,
					); err != nil {
						logger.Warn("fill poll: renew discovery residency after protected entry fill",
							"trade_id", tradeID, "ticker", sig.Ticker, "err", err)
					} else {
						discoveryResidencyRenewed = true
						if stockIntentID != "" {
							if err := store.MarkStockOrderIntentDiscoveryResidencyRenewed(
								renewCtx, stockIntentID,
							); err != nil {
								logger.Warn("fill poll: mark discovery residency renewal complete",
									"ticker", sig.Ticker, "stock_intent_id", stockIntentID, "err", err)
							}
						}
					}
					renewCancel()
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

// mergedTickerExposure preserves the complete local projection (held shares
// plus every reservation) unless the broker snapshot reports a larger holding.
// In the latter case, only pending shares are added to broker exposure because
// those fills cannot yet appear in the broker position snapshot.
func mergedTickerExposure(localExposure, brokerExposure, pendingExposure float64) float64 {
	return math.Max(localExposure, brokerExposure+pendingExposure)
}

// applyBracketChildSnapshot preserves an already-reported broker execution
// while the normal accepted-order path creates its local child projection. A
// later direct child lookup may be purged, so a pending zero-fill row would
// otherwise lose the only authoritative cumulative fill.
func applyBracketChildSnapshot(order *store.Order, children []execution.OrderLeg) {
	for _, child := range children {
		if child.BrokerOrderID != order.BrokerOrderID || child.FilledQty <= 0 {
			continue
		}
		order.FilledPrice = child.FilledPrice
		order.FilledQty = child.FilledQty
		order.Status = execution.NormalizeOrderStatusForTimeInForce(child.Status, "gtc")
		if !child.FilledAt.IsZero() {
			filledAt := child.FilledAt
			order.FilledAt = &filledAt
		}
		return
	}
}

// stockIntentIDForAcceptedOrder is the common post-acceptance projection
// guard. A coordinator's durable intent must be present before any normal
// engine path writes a trade/order row. Paper and legacy executors deliberately
// remain intent-free.
func stockIntentIDForAcceptedOrder(ctx context.Context, executor execution.Executor, key string) (string, error) {
	return stockIntentIDForAcceptedOrderWithLookup(ctx, executor, key, store.GetStockOrderIntentByKey)
}

// placeMarketExitWithTerminalRetries advances only durable terminal zero-fill
// generations. A terminal client id can never become live again, so replaying
// the base key would otherwise repeatedly return ErrTerminalZeroFillPlacement
// while the just-released trade claim is immediately reacquired.
//
// Ambiguous failures intentionally do not rotate: the same client id may
// already identify a broker-accepted order, and the durable reconciler must
// keep ownership until that ambiguity is resolved.
func (e *Engine) placeMarketExitWithTerminalRetries(
	ctx context.Context,
	req execution.OrderRequest,
) (execution.OrderResult, string, bool, error) {
	durable, ok := e.executor.(interface{ DurableStockOrderIntents() bool })
	if !ok || !durable.DurableStockOrderIntents() {
		res, err := e.executor.PlaceOrder(ctx, req)
		return res, req.IntentKey, false, err
	}
	// A terminal partial fill consumes part of the position and releases the
	// claim for its residual. That residual must use a fresh immutable payload,
	// not the completed generation whose quantity no longer matches.
	partialRetryKey, resolveErr := store.ResolveTerminalPartialStockOrderIntentRetryGeneration(ctx, req.IntentKey)
	if resolveErr != nil {
		return execution.OrderResult{}, req.IntentKey, false, fmt.Errorf("resolve terminal partial market-exit generation: %w", resolveErr)
	}
	if partialRetryKey != req.IntentKey {
		req.IntentKey, req.ClientOrderID = partialRetryKey, ""
	}

	res, err := e.executor.PlaceOrder(ctx, req)
	if err == nil {
		// A new terminal-zero result is durably recorded and is deliberately
		// handed back to the next scheduled exit attempt. Do not burn an
		// unbounded chain of client ids while a broker-side restriction persists.
		if isTerminalZeroFillStockIntent(res.Status, res.FilledQty, req.TimeInForce) {
			return execution.OrderResult{}, req.IntentKey, true, fmt.Errorf("market exit completed terminally without a fill")
		}
		return res, req.IntentKey, false, nil
	}
	if !errors.Is(err, execution.ErrTerminalZeroFillPlacement) {
		return execution.OrderResult{}, req.IntentKey, false, err
	}
	if errors.Is(err, execution.ErrFreshTerminalZeroFillPlacement) {
		// The just-created generation was conclusively rejected. Keep the
		// release/retry cadence at one broker submission per engine tick rather
		// than immediately creating a second order under the same restriction.
		return execution.OrderResult{}, req.IntentKey, true, err
	}

	// This invocation did not reach the broker: its prior generation is known
	// terminal. Advance once, then make at most one fresh broker request.
	_, retryKey, resolveErr := store.ResolveStockOrderIntentRetryGeneration(ctx, req.IntentKey)
	if resolveErr != nil {
		return execution.OrderResult{}, req.IntentKey, true, fmt.Errorf("resolve terminal market-exit generation: %w", resolveErr)
	}
	if retryKey == req.IntentKey {
		return execution.OrderResult{}, req.IntentKey, true, errors.New("terminal market-exit generation resolver did not advance")
	}
	req.IntentKey, req.ClientOrderID = retryKey, ""
	res, err = e.executor.PlaceOrder(ctx, req)
	if err != nil {
		return execution.OrderResult{}, req.IntentKey, errors.Is(err, execution.ErrTerminalZeroFillPlacement), err
	}
	if isTerminalZeroFillStockIntent(res.Status, res.FilledQty, req.TimeInForce) {
		return execution.OrderResult{}, req.IntentKey, true, fmt.Errorf("replacement market exit completed terminally without a fill")
	}
	return res, req.IntentKey, false, nil
}

func entryProtectionVerifiedForResidency(req execution.OrderRequest, result execution.OrderResult, installedStopQty float64) bool {
	uncappedStopVerified := !req.UncappedTakeProfit || installedStopQty+1e-9 >= result.FilledQty
	bracketVerified := req.OrderClass != "bracket" || !req.RequireBrokerProtection ||
		brokerBracketProtectionVerified(result, result.FilledQty)
	return uncappedStopVerified && bracketVerified
}

func stockIntentIDForAcceptedOrderWithLookup(
	ctx context.Context,
	executor execution.Executor,
	key string,
	lookup func(context.Context, string) (*store.StockOrderIntent, error),
) (string, error) {
	durable, ok := executor.(interface{ DurableStockOrderIntents() bool })
	if !ok || !durable.DurableStockOrderIntents() {
		// Paper and legacy broker paths did not create coordinator intents;
		// do not turn their normal projection into a database lookup failure.
		return "", nil
	}
	intent, err := lookup(ctx, key)
	if err != nil {
		return "", err
	}
	if intent == nil {
		return "", errors.New("accepted stock order durable intent missing")
	}
	return intent.ID, nil
}

func (e *Engine) pollUnfilledStockIntent(intentKey, brokerOrderID string) {
	if brokerOrderID == "" {
		return
	}
	recoverer, ok := e.executor.(interface{ Recover(context.Context) error })
	if !ok {
		return
	}
	pollDelay := unfilledEntryPollDelay
	go func() {
		const retries = 40
		effectsEmitted := false
		for attempt := 0; attempt < retries; attempt++ {
			time.Sleep(pollDelay)
			pollCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			status, err := e.executor.GetOrderStatus(pollCtx, brokerOrderID)
			cancel()
			if err != nil {
				continue
			}
			if status.FilledQty <= 0 && !execution.IsTerminalOrderStatus(status.Status) {
				continue
			}
			recoverCtx, recoverCancel := context.WithTimeout(context.Background(), 10*time.Second)
			err = recoverer.Recover(recoverCtx)
			if err != nil {
				recoverCancel()
				logger.Warn("unfilled entry recovery", "intent_key", intentKey, "broker_order_id", brokerOrderID, "err", err)
				continue
			}
			if !effectsEmitted {
				effectsEmitted = e.emitRecoveredEntryEffects(recoverCtx, intentKey)
			}
			recoverCancel()
			if execution.IsTerminalOrderStatus(status.Status) {
				return
			}
		}
		e.cancelTimedOutStockIntent(intentKey, brokerOrderID, recoverer)
	}()
}

// emitRecoveredEntryEffects restores the in-process observability effects for
// an entry that was accepted with no fill and later projected by recovery. The
// trade itself is durable; the cost counter and event bus are not, so publish
// them once from the poller that observed the completed projection.
func (e *Engine) emitRecoveredEntryEffects(ctx context.Context, intentKey string) bool {
	intent, err := store.GetStockOrderIntentByKey(ctx, intentKey)
	if err != nil || intent == nil || intent.TradeID == "" {
		return false
	}
	trade, err := store.GetTradeByID(ctx, intent.TradeID)
	if err != nil || trade == nil {
		return false
	}
	e.containRecoveredUnprotectedBracket(ctx, intentKey)
	if _, alreadyEmitted := e.recoveredEntryEffects.LoadOrStore(intent.ID, struct{}{}); alreadyEmitted {
		return false
	}
	if e.costTracker != nil {
		e.costTracker.RecordTrade()
	}
	if e.bus != nil {
		e.bus.Publish(events.TradeExecuted, events.TradeExecutedPayload{
			TradeID:       trade.ID,
			SignalID:      trade.SignalID,
			Ticker:        trade.Ticker,
			Direction:     trade.Direction,
			EntryPrice:    trade.EntryPrice,
			Quantity:      trade.Quantity,
			Strategy:      trade.Strategy,
			EntrySetup:    trade.EntrySetup,
			SetupEvidence: trade.EntrySetupEvidence,
		})
	}
	return true
}

// RenewRecoveredEntryDiscoveryResidency implements the stock-order recovery
// hook and is also used immediately after a normal filled entry is persisted.
// The source context is only extended while it is still live; position
// monitoring remains independent from discovery residency.
func (e *Engine) RenewRecoveredEntryDiscoveryResidency(ctx context.Context, ticker string, executedAt time.Time) error {
	expiresAt := MarketSessionOffset(executedAt, 2).Close
	return store.RenewDiscoveryResidencyAfterTrade(ctx, ticker, expiresAt)
}

// RecoveredStockEntry implements stockorders.RecoveryObserver.
func (e *Engine) RecoveredStockEntry(ctx context.Context, intentKey string) {
	e.emitRecoveredEntryEffects(ctx, intentKey)
}

// RecoveredStockEntryProtection is separate from RecoveredStockEntry because
// recovery may revisit a pre-existing trade solely to verify broker coverage;
// that must not emit a second TradeExecuted event or charge a second trade.
func (e *Engine) RecoveredStockEntryProtection(ctx context.Context, intentKey string) {
	e.containRecoveredUnprotectedBracket(ctx, intentKey)
}

// PreparedStockEntryNeedsAdmission keeps a client-ID-missing prepared entry
// reserved, but makes its owning signal retryable by the normal serialized,
// fresh broker-admission path. Generic recovery itself never sends the POST.
func (e *Engine) PreparedStockEntryNeedsAdmission(ctx context.Context, intentKey string) {
	signalID, ok := preparedEntrySignalID(intentKey)
	if !ok {
		logger.Warn("cannot queue prepared stock entry with invalid intent key", "intent_key", intentKey)
		return
	}
	sig, err := store.GetSignalByID(ctx, signalID)
	if err != nil {
		logger.Warn("load prepared stock entry signal after broker miss", "intent_key", intentKey, "err", err)
		return
	}
	terminalReason := ""
	if sig == nil {
		terminalReason = "broker confirmed entry absent and owning signal is missing"
	} else if terminalPreparedEntrySignal(sig, time.Now()) {
		terminalReason = "broker confirmed entry absent after owning signal became terminal"
	}
	if terminalReason != "" {
		if err := store.RetirePreparedStockEntryAfterBrokerMiss(ctx, intentKey, terminalReason); err != nil {
			logger.Warn("retire prepared stock entry after broker miss", "intent_key", intentKey, "err", err)
		}
		return
	}
	if e.preparedEntryPredatesBrokerProtection(ctx, intentKey) {
		// Recovery has just proved this prepared generation never reached the
		// broker. It cannot be replayed after the operator enables strict
		// protection, because its immutable request deliberately lacks that
		// requirement. Retire only with that broker-miss proof, then let the
		// queued signal create a fresh protected successor on its next pass.
		reason := "broker confirmed entry absent; prepared generation predates the required broker-protection policy"
		if err := store.RetirePreparedStockEntryAfterBrokerMiss(ctx, intentKey, reason); err != nil {
			logger.Warn("retire legacy prepared stock entry after broker miss", "intent_key", intentKey, "err", err)
			return
		}
	}
	if err := store.QueueSignalForDeferredExecution(ctx, signalID); err != nil {
		logger.Warn("queue prepared stock entry for fresh admission", "intent_key", intentKey, "err", err)
	}
}

// preparedEntryPredatesBrokerProtection identifies a legacy immutable request
// that can no longer be safely replayed under the currently enabled strict
// admission policy. Its caller is invoked only after client-ID recovery has
// confirmed the prepared generation is absent at the broker.
func (e *Engine) preparedEntryPredatesBrokerProtection(ctx context.Context, intentKey string) bool {
	if e.cfg == nil || !e.cfg.Risk.BracketExitsEnabled {
		return false
	}
	intent, err := store.GetStockOrderIntentByKey(ctx, intentKey)
	if err != nil || intent == nil {
		if err != nil {
			logger.Warn("load prepared stock entry policy after broker miss", "intent_key", intentKey, "err", err)
		}
		return false
	}
	var req execution.OrderRequest
	if err := json.Unmarshal(intent.RequestPayload, &req); err != nil {
		logger.Warn("decode prepared stock entry policy after broker miss", "intent_key", intentKey, "err", err)
		return false
	}
	return !req.RequireBrokerProtection
}

func terminalPreparedEntrySignal(sig *store.Signal, now time.Time) bool {
	if sig == nil {
		return true
	}
	if !sig.ExpiresAt.IsZero() && now.After(sig.ExpiresAt) {
		return true
	}
	switch sig.Status {
	case "executed", "rejected", "expired", "stale":
		return true
	case "blocked":
		return signalHasTerminalReleaseGate(sig)
	default:
		return false
	}
}

// signalHasTerminalReleaseGate distinguishes a durable policy rejection from
// temporary admission failures, both of which use status=blocked. Recovery may
// retry the latter after a broker miss, but must never revive a signal whose
// final anti-chase release gate was persisted as blocked.
func signalHasTerminalReleaseGate(sig *store.Signal) bool {
	if sig == nil || len(sig.EntryGateDecisions) == 0 {
		return false
	}
	var decisions []store.EntryGateDecision
	if err := json.Unmarshal(sig.EntryGateDecisions, &decisions); err != nil {
		return false
	}
	for _, decision := range decisions {
		if decision.Name == "universal_anti_chase_release" && decision.Status == "blocked" {
			return true
		}
	}
	return false
}

func preparedEntrySignalID(intentKey string) (string, bool) {
	if !strings.HasPrefix(intentKey, "entry:") {
		return "", false
	}
	remainder := strings.TrimPrefix(intentKey, "entry:")
	if retryAt := strings.Index(remainder, ":retry:"); retryAt >= 0 {
		remainder = remainder[:retryAt]
	}
	return remainder, remainder != "" && !strings.Contains(remainder, ":")
}

// RecoveredStockExit implements stockorders.RecoveryObserver. Recovery owns
// durable state; the engine owns the process-local cooldown and event bus.
func (e *Engine) RecoveredStockExit(_ context.Context, realized store.TradeRealization) {
	if !realized.JustClosed {
		return
	}
	if realized.PnL < 0 && e.portfolio != nil {
		e.portfolio.RecordLoss()
	}
	if e.bus != nil {
		e.publishTradeClosed(realized)
	}
}

var unfilledEntryPollDelay = 3 * time.Second

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

	// Released by a human from the approval queue.
	e.executeSignal(ctx, sigID, *sig, true)

	// executeSignal reports blocks by updating the signal row, not by
	// returning an error — re-fetch so a manual approver (CLI/Telegram)
	// gets the real outcome instead of "approved and executing" while the
	// signal sits blocked (e.g. the market-hours guard on an after-hours
	// approval, review catch on #437).
	if after, err := store.GetSignalByID(ctx, sigID); err == nil && after != nil && after.Status == "blocked" {
		return fmt.Errorf("signal %s blocked at execution: %s", sigID, after.BlockReason)
	} else if after != nil && signalRequiresManualAdmissionReapproval(*after) {
		return fmt.Errorf("signal %s requires renewed manual approval after broker admission refresh", sigID)
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
	var dailyBaselineKnown bool
	var persistedPeak float64
	brokerAuthoritative := brokerName(e.executor) == "alpaca"

	if brokerAuthoritative {
		view, ok := e.executor.(interface {
			AccountSnapshot(context.Context) (execution.BrokerAccount, error)
		})
		if !ok {
			logger.Warn("broker account snapshot unavailable for portfolio refresh")
			return
		}
		acct, err := view.AccountSnapshot(ctx)
		if err != nil {
			logger.Warn("get broker account for portfolio refresh", "err", err)
			return
		}
		if err := validateBrokerPortfolioAccount(acct, e.cfg.Markets.Stocks.ExpectedAccountNumber, time.Now().UTC()); err != nil {
			logger.Warn("invalid broker account for portfolio refresh", "err", err)
			return
		}
		peakEquity, peakObservedAt := e.updateStockAccountPeak(acct.AccountID, acct.Equity, acct.FetchedAt)
		if store.IsConnected() {
			baseline, baselineErr := store.ObserveAccountRiskBaselineWithPeak(ctx, acct.AccountID, acct.Equity, peakEquity, acct.LastEquity, acct.FetchedAt, peakObservedAt)
			if baselineErr != nil {
				logger.Warn("persist broker account risk baseline", "account_id", acct.AccountID, "err", baselineErr)
			} else {
				persistedPeak = baseline.PeakEquity
				if baseline.LastEquity > 0 {
					// Make dashboard and in-process risk telemetry use the same
					// account-scoped daily baseline as final BUY admission.
					dayPnL = acct.Equity - baseline.LastEquity
					dailyBaselineKnown = true
				}
			}
		}
		cash = acct.Cash
		totalEquity = acct.Equity
		if !dailyBaselineKnown {
			dayPnL = acct.Equity - acct.LastEquity
		}
	} else if acct, err := e.executor.GetAccount(ctx); err != nil {
		logger.Warn("get broker account for portfolio refresh — falling back to local derivation", "err", err)
	} else {
		cash = acct.Cash
		totalEquity = acct.PortfolioEquity
		dayPnL = acct.DayPnL
	}

	if shouldLoadLocalRealizedPnL(store.IsConnected(), brokerAuthoritative, dayPnL) {
		// Realized P&L is independent of Alpaca's equity-last_equity movement,
		// which also contains unrealized gains/losses and transfers. Preserve the
		// legacy fallback for non-authoritative brokers that report no day P&L.
		closedTrades, _ := store.GetRecentTrades(ctx, 100)
		today := time.Now().UTC().Truncate(24 * time.Hour)
		for _, t := range closedTrades {
			if t.Status == "closed" && t.ClosedAt.After(today) {
				realizedToday += t.PnL
			}
		}
	}

	if cash == 0 && !brokerAuthoritative {
		// Local fallback: initial_capital minus cost basis of open positions.
		cash = e.cfg.Engine.InitialCapital
		for _, p := range positions {
			cash -= p.CurrentValue - p.UnrealizedPL
		}
		if cash < 0 {
			cash = 0
		}
	}

	if totalEquity == 0 && !brokerAuthoritative {
		totalEquity = cash
		for _, p := range positions {
			totalEquity += p.CurrentValue
		}
	}

	e.portfolio.Update(eps, cash, totalEquity, dayPnL, realizedToday)
	if persistedPeak > 0 {
		// Update normally only ratchets upward. The persisted baseline is the
		// authoritative account-scoped peak and can intentionally be lower
		// after an operator-confirmed deposit, withdrawal, or reset.
		e.portfolio.SetPeakValue(persistedPeak)
	}
}

func shouldLoadLocalRealizedPnL(storeConnected, brokerAuthoritative bool, dayPnL float64) bool {
	return storeConnected && (brokerAuthoritative || dayPnL == 0)
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
	e.positionExitMu.Lock()
	defer e.positionExitMu.Unlock()
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

		evidence := map[string]any{
			"current_price":   currentPrice,
			"price_timestamp": priceTS.UTC(),
		}
		reasonCode := store.ExitReasonStopLoss
		if exitReason == "stop-loss hit" {
			evidence["stop_loss"] = t.StopLoss
		} else {
			reasonCode = store.ExitReasonTakeProfit
			evidence["take_profit"] = t.TakeProfit
		}
		intent := newExitIntent(reasonCode, evidence)
		claimed, err := store.ClaimTradeExitWithIntent(ctx, t.ID, intent)
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
		exitReq := execution.OrderRequest{
			Ticker:         t.Ticker,
			Side:           exitSide,
			Type:           execution.OrderTypeMarket,
			Quantity:       t.Quantity,
			ReferencePrice: currentPrice,
			TradeID:        t.ID,
			IntentKey:      "software-exit:" + t.ID + ":" + exitReason,
		}
		result, intentKey, terminalZero, err := e.placeMarketExitWithTerminalRetries(ctx, exitReq)
		if err != nil {
			if terminalZero {
				if releaseErr := store.ReleaseTradeExitClaim(ctx, t.ID); releaseErr != nil {
					logger.Warn("release terminal software exit claim", "trade_id", t.ID, "err", releaseErr)
				}
			}
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
		stockIntentID, intentErr := stockIntentIDForAcceptedOrder(ctx, e.executor, intentKey)
		if intentErr != nil {
			// The broker accepted the market exit. Keep its claim and leave the
			// durable coordinator recovery path to project it rather than write
			// an unlinked legacy exit row.
			logger.Error("accepted software exit intent projection unavailable", "trade_id", t.ID, "err", intentErr)
			continue
		}

		exitOrder := store.Order{
			TradeID:       t.ID,
			BrokerOrderID: result.BrokerOrderID,
			Ticker:        t.Ticker,
			Side:          string(exitSide),
			OrderType:     string(execution.OrderTypeMarket),
			Quantity:      t.Quantity,
			FilledQty:     result.FilledQty,
			FilledAt:      filledAtPtr(result.FilledAt),
			Status:        execution.NormalizeOrderStatus(result.Status),
			IsStopLoss:    exitReason == "stop-loss hit",
			IsTakeProfit:  exitReason == "take-profit hit",
			StockIntentID: stockIntentID,
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
		realized, err := store.CloseTradeRealizedWithIntent(ctx, t.ID, exitPrice, result.FilledAt, intent)
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
		e.publishTradeClosed(realized)

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
			Available:              true,
			TotalValue:             snap.TotalValue,
			Cash:                   snap.Cash,
			Invested:               snap.Invested,
			UnrealizedPnL:          snap.UnrealizedPnL,
			RealizedPnLToday:       snap.RealizedPnLDay,
			DayPnL:                 snap.DayPnL,
			OpenPositions:          snap.OpenPositions,
			PositionStatsAvailable: true,
			OpenTradeRows:          openTradeRows,
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
			// A prepared intent can have reached the broker before the local
			// attempt marker committed. Signal expiry is not broker truth, so its
			// reservation must survive for exact client-ID recovery.
			continue // the expiry loop owns time-outs
		}
		prepared, preparedErr := store.HasPreparedStockEntry(ctx, sig.ID)
		if preparedErr != nil {
			logger.Warn("deferred signal: prepared intent lookup failed", "id", sig.ID, "err", preparedErr)
			continue
		}
		if prepared {
			// This is crash recovery, not first-tick confirmation. The durable
			// intent already occupies its own risk slot, so skip the legacy
			// hold/precheck and let executeSignal's self-excluding fresh admission
			// perform the one complete release gate.
			e.executeSignal(ctx, sig.ID, sig, true)
			// executeSignal owns the durable disposition. Its explicit transient
			// admission paths requeue themselves; do not overwrite a final stale,
			// risk, or anti-chase block with a fresh pending state here.
			continue
		}
		if signalNeedsAdmissionRetry(sig) {
			// This signal already passed its ordinary release path. Its broker
			// admission expired only while final evidence was gathered, so retry
			// the complete coordinated admission directly rather than treating it
			// as a first-tick candidate or mixing in a standalone quote.
			logger.Info("retrying stock entry after broker admission refresh", "id", sig.ID, "ticker", sig.Ticker)
			e.executeSignal(ctx, sig.ID, sig, true)
			continue
		}
		if !deferredSignalPathEnabled(e.cfg.Risk.EntryGates.FirstTickDefer, false) {
			continue
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
		openTrades, otErr = withWorkingStockEntryReservations(ctx, openTrades)
		if otErr != nil {
			logger.Warn("deferred signal: working stock entries unavailable — retrying next tick",
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
		// Released from the deferred queue after first-tick confirmation.
		e.executeSignal(ctx, sig.ID, sig, true)
	}
}

func deferredSignalPathEnabled(firstTickDefer, preparedRecovery bool) bool {
	return preparedRecovery || firstTickDefer
}

func signalNeedsAdmissionRetry(sig store.Signal) bool {
	if sig.Status != "pending" || !sig.Deferred {
		return false
	}
	var decisions []store.EntryGateDecision
	if err := json.Unmarshal(sig.EntryGateDecisions, &decisions); err != nil {
		return false
	}
	for _, decision := range decisions {
		if decision.Name == store.AdmissionRetryGateName && decision.Status == "retry_pending_auto" {
			return true
		}
	}
	return false
}

func signalRequiresManualAdmissionReapproval(sig store.Signal) bool {
	if sig.Status != "pending" || sig.Deferred {
		return false
	}
	var decisions []store.EntryGateDecision
	if err := json.Unmarshal(sig.EntryGateDecisions, &decisions); err != nil {
		return false
	}
	for _, decision := range decisions {
		if decision.Name == store.AdmissionRetryGateName && decision.Status == "manual_reapproval_required" {
			return true
		}
	}
	return false
}

// queueBrokerAdmissionRefresh turns an elapsed coordinated broker snapshot
// into a fresh full-admission attempt. Auto/paper signals are rescheduled by
// the deferred sweep; manual signals return to the approval queue so an
// operator explicitly authorizes the new broker admission.
func (e *Engine) queueBrokerAdmissionRefresh(
	ctx context.Context,
	sigID string,
	sig store.Signal,
	signalReferencePrice float64,
	reason string,
	cause error,
) {
	if signalReferencePrice > 0 && sig.AcceptedReferencePrice <= 0 {
		// A migrated signal normally already has this value. This write is only
		// for an in-memory/legacy signal whose positive reference was established
		// before the migration could backfill it.
		if err := store.SetInitialSignalReferencePrice(ctx, sigID, signalReferencePrice); err != nil {
			logger.Error("persist accepted signal reference before admission retry", "id", sigID, "ticker", sig.Ticker, "err", err)
			_ = store.UpdateSignalStatus(ctx, sigID, "blocked", "unable to persist broker-admission retry")
			return
		}
	}
	automatic := e.Mode() == "auto" || e.Mode() == "paper"
	if err := store.QueueSignalForAdmissionRetry(ctx, sigID, reason, automatic); err != nil {
		logger.Error("unable to persist broker admission retry", "id", sigID, "ticker", sig.Ticker, "err", err)
		_ = store.UpdateSignalStatus(ctx, sigID, "blocked", "unable to persist broker-admission retry")
		return
	}
	logger.Warn("broker admission aged — scheduling a fresh coordinated admission",
		"id", sigID, "ticker", sig.Ticker, "automatic", automatic, "reason", reason, "err", cause)
	e.bus.Publish(events.SignalQueued, events.SignalQueuedPayload{
		SignalID: sigID, Ticker: sig.Ticker, Action: sig.Action, Confidence: sig.Confidence,
		ExpiresIn: time.Until(sig.ExpiresAt), EntrySetup: sig.EntrySetup, SetupEvidence: sig.EntrySetupEvidence,
	})
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
	Quote         *data.Quote
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
	evidence.Quote = refreshed
	return evidence, nil
}

// pullbackSignalSessionLow recovers the immutable raw low from the
// qualification audit. Signal.StopLoss can be tightened later by the pyramid
// policy, so it cannot safely stand in for the setup's signal-session low.
func pullbackSignalSessionLow(sig store.Signal) (float64, error) {
	var decisions []store.EntryGateDecision
	if err := json.Unmarshal(sig.EntryGateDecisions, &decisions); err != nil {
		return 0, fmt.Errorf("decode entry gate decisions: %w", err)
	}
	for _, decision := range decisions {
		if decision.Name != "pullback_qualification" || decision.Status != "passed" {
			continue
		}
		var setup pullback.PromptContext
		if err := json.Unmarshal(decision.Details, &setup); err != nil {
			return 0, fmt.Errorf("decode pullback qualification evidence: %w", err)
		}
		if setup.StopLoss <= 0 {
			return 0, fmt.Errorf("pullback qualification has no signal-session low")
		}
		return setup.StopLoss, nil
	}
	return 0, fmt.Errorf("passed pullback qualification not found")
}

// recheckPullbackTimingGate reapplies the universal anti-chase decision after
// a pullback's final slow refresh has produced the actual executable ask. It
// uses the daily/news snapshot refreshed by the first release, then reads it
// back locally so no further slow work can age the quote that supplies mark.
func (e *Engine) recheckPullbackTimingGate(
	ctx context.Context, sigID string, sig store.Signal, mark float64,
) bool {
	if !e.cfg.Risk.EntryGates.AntiChase.Enabled {
		return false
	}
	policy := antiChasePolicy(e.cfg.Risk.EntryGates.AntiChase)
	now := time.Now().UTC()
	daily, err := store.GetCandles(ctx, sig.Ticker, data.Timeframe1Day, now.AddDate(-2, 0, 0), now)
	evidenceReason := ""
	if err != nil {
		evidenceReason = "anti-chase: unable to load refreshed daily evidence"
	}
	items, err := store.GetRecentNews(ctx, sig.Ticker, now.Add(-antiChaseNewsLookback))
	news, newsReason := antiChaseNewsEvidence(items, err, policy.StaleCatalystRunupEnabled)
	if evidenceReason == "" && newsReason != "" {
		evidenceReason = newsReason
	}
	var snapshot *antichase.Snapshot
	if evidenceReason != "" {
		snapshot = &antichase.Snapshot{
			SchemaVersion: antichase.PolicyVersion, Ticker: sig.Ticker, ObservedAt: now,
			ReferencePrice: mark, ReferenceSource: "final_pullback_ask",
			EvidenceStatus: "unavailable", HistoryScope: "limited_history", EvidenceReason: evidenceReason,
		}
	} else {
		snapshot = buildAntiChaseSnapshot(sig.Ticker, daily, mark, "final_pullback_ask", news, now)
	}
	reason, decision := antiChaseEntryGate(snapshot, policy)
	details, _ := json.Marshal(struct {
		Snapshot *antichase.Snapshot `json:"snapshot"`
		Decision antichase.Decision  `json:"decision"`
	}{snapshot, decision})
	gate := store.EntryGateDecision{
		Name: "universal_anti_chase_final_pullback", Status: "passed", Details: details,
		ObservedAt: &now,
	}
	if reason != "" {
		gate.Status, gate.Reason = "blocked", reason
		if err := store.BlockSignalWithEntryGateDecision(ctx, sigID, reason, gate); err != nil {
			logger.Warn("persist final pullback anti-chase block", "id", sigID, "err", err)
		}
		logger.Warn("pullback execution blocked by final universal anti-chase gate",
			"ticker", sig.Ticker, "reason", reason)
		e.bus.Publish(events.SignalBlocked, events.SignalBlockedPayload{
			Ticker: sig.Ticker, Action: sig.Action, Confidence: sig.Confidence,
			BlockReason: reason, FailedCheck: gate.Name,
		})
		return true
	}
	if err := store.AppendSignalEntryGateDecision(ctx, sigID, gate); err != nil {
		reason := "anti-chase: unable to persist final pullback timing audit"
		_ = store.UpdateSignalStatus(ctx, sigID, "blocked", reason)
		logger.Warn("pullback execution blocked — final anti-chase audit failed", "ticker", sig.Ticker, "err", err)
		return true
	}
	return false
}

// verifyPullbackReleasePath is the lightweight final extension of a completed
// pullback revalidation. The daily setup was already refreshed and checked;
// this closes only the remaining high-resolution trade interval at release.
func (e *Engine) verifyPullbackReleasePath(
	ctx context.Context, sig store.Signal, since time.Time,
) (*data.Quote, float64, error) {
	sessionLow, err := pullbackSignalSessionLow(sig)
	if err != nil {
		return nil, 0, fmt.Errorf("recover original signal-session low: %w", err)
	}
	if since.IsZero() {
		since = sig.CreatedAt
	}
	evidence, err := e.pullbackExecutionEvidence(ctx, sig.Ticker, since, time.Now())
	if err != nil {
		return nil, 0, fmt.Errorf("verify exact release path: %w", err)
	}
	if evidence.MinimumTraded <= sessionLow {
		return nil, 0, fmt.Errorf(
			"setup invalidated before release (minimum traded %.2f at/below low-of-day stop %.2f)",
			evidence.MinimumTraded, sessionLow,
		)
	}
	daily, err := store.GetCandles(
		ctx, sig.Ticker, data.Timeframe1Day, time.Now().AddDate(-2, 0, 0), time.Now(),
	)
	if err != nil {
		return nil, 0, fmt.Errorf("load release-time daily setup: %w", err)
	}
	setup := pullback.Evaluate(daily, evidence.Mark, pullback.Config{
		MinMomentumReturnPct:  e.cfg.Risk.EntryGates.PullbackMinMomentumReturnPct,
		ConsolidationSessions: e.cfg.Risk.EntryGates.PullbackConsolidationSessions,
		SupportTolerancePct:   e.cfg.Risk.EntryGates.PullbackSupportTolerancePct,
		MaxStopWidthPct:       e.cfg.Risk.EntryGates.PullbackMaxStopWidthPct,
	})
	if reason := pullback.ValidateExecutionSetup(
		setup, sessionLow, sig.StopLoss, evidence.Mark,
		e.cfg.Risk.EntryGates.PullbackMaxStopWidthPct,
	); reason != "" {
		return nil, 0, errors.New(reason)
	}
	if reason := pullback.ValidateExecutionMark(
		evidence.Mark, sig.StopLoss, e.cfg.Risk.EntryGates.PullbackMaxStopWidthPct,
	); reason != "" {
		return nil, 0, errors.New(reason)
	}
	return evidence.Quote, evidence.Mark, nil
}

// revalidatePullbackExecution repeats every time-sensitive pullback check only
// after the final slow daily refresh has completed. A support break that occurs
// while refreshing candles must not be hidden by the earlier qualification
// snapshot and then recovered before submission.
//
// requestedMark is the broker-authoritative ask when one exists. A non-positive
// value uses the final executable market ask from the exact trade-path check.
func (e *Engine) revalidatePullbackExecution(
	ctx context.Context, sig store.Signal, requestedMark float64,
) (float64, *data.Quote, time.Time, error) {
	sessionLow, err := pullbackSignalSessionLow(sig)
	if err != nil {
		return 0, nil, time.Time{}, fmt.Errorf("recover original signal-session low: %w", err)
	}
	if _, err := e.fetchAndStoreCandlesObserved(ctx, sig.Ticker, data.Timeframe1Day); err != nil {
		return 0, nil, time.Time{}, fmt.Errorf("refresh daily setup: %w", err)
	}
	freshDailyCandles, err := store.GetCandles(
		ctx, sig.Ticker, data.Timeframe1Day,
		time.Now().AddDate(-2, 0, 0), time.Now(),
	)
	if err != nil {
		return 0, nil, time.Time{}, fmt.Errorf("load refreshed daily setup: %w", err)
	}
	// Fetch the quote and exact print path after the slow daily work. This is
	// intentionally last so the evidence reaches the actual order decision.
	evidence, err := e.pullbackExecutionEvidence(ctx, sig.Ticker, sig.CreatedAt, time.Now())
	if err != nil {
		return 0, nil, time.Time{}, fmt.Errorf("verify fresh execution price and post-signal path: %w", err)
	}
	mark := requestedMark
	if mark <= 0 {
		mark = evidence.Mark
	}
	setup := pullback.Evaluate(freshDailyCandles, mark, pullback.Config{
		MinMomentumReturnPct:  e.cfg.Risk.EntryGates.PullbackMinMomentumReturnPct,
		ConsolidationSessions: e.cfg.Risk.EntryGates.PullbackConsolidationSessions,
		SupportTolerancePct:   e.cfg.Risk.EntryGates.PullbackSupportTolerancePct,
		MaxStopWidthPct:       e.cfg.Risk.EntryGates.PullbackMaxStopWidthPct,
	})
	if reason := pullback.ValidateExecutionSetup(
		setup, sessionLow, sig.StopLoss, mark,
		e.cfg.Risk.EntryGates.PullbackMaxStopWidthPct,
	); reason != "" {
		return 0, nil, time.Time{}, errors.New(reason)
	}
	if evidence.MinimumTraded <= sessionLow {
		return 0, nil, time.Time{}, fmt.Errorf(
			"setup invalidated before execution (minimum traded %.2f at/below low-of-day stop %.2f)",
			evidence.MinimumTraded, sessionLow,
		)
	}
	return mark, evidence.Quote, evidence.Quote.Timestamp, nil
}

// withWorkingStockEntryReservations makes accepted entries that have not yet
// produced a trade visible to every BUY risk pass.  The reservation is not a
// persisted trade (there may still be zero broker fill), but it prevents a
// working market entry from leaving a hole in the position-cap and
// per-ticker ownership checks.
func withWorkingStockEntryReservations(ctx context.Context, openTrades []store.Trade) ([]store.Trade, error) {
	reservations, err := store.GetWorkingStockEntryReservations(ctx)
	if err != nil {
		return nil, err
	}
	return append(openTrades, reservations...), nil
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
	var longNotional float64  // mark-to-market long exposure in the target ticker
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
			// Use the signal's current mark rather than cost basis. A winner can
			// grow past its original allocation before a pyramid attempt; only
			// current notional enforces the promised combined exposure cap.
			longNotional += t.Quantity * signalPrice
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
		Cfg:                    cfg,
		Action:                 action,
		Ticker:                 ticker,
		Confidence:             confidence,
		SizePct:                sizePct,
		StopLoss:               stopLoss,
		TakeProfit:             takeProfit,
		SignalPrice:            signalPrice,
		PortfolioValue:         pState.TotalValue,
		Cash:                   pState.Cash,
		OpenPositions:          len(positions),
		OpenByTicker:           byTicker,
		DailyPnL:               pState.RealizedPnLDay,
		PeakValue:              pState.PeakValue,
		LastLossAt:             pState.LastLossAt,
		HoldsLong:              holdsLong,
		LongQuantity:           longQuantity,
		LongAvgEntryPrice:      longAvgEntry,
		LongMaxStop:            longMaxStop,
		ExistingTickerNotional: longNotional,
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

		// Price-based staleness always measures from the accepted thesis price,
		// not an execution-time pullback, first-tick, or broker reprice.
		referencePrice := sig.AcceptedReferencePrice
		if referencePrice <= 0 {
			referencePrice = sig.SignalPrice
		}
		if sig.StalePriceMovePct > 0 && referencePrice > 0 {
			latest, err := store.GetLatestCandle(ctx, sig.Ticker, data.Timeframe1Day)
			if err != nil || latest == nil {
				continue
			}
			if priceDelta, stale := staleSignalPriceMove(referencePrice, latest.Close, sig.StalePriceMovePct); stale {
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

// staleSignalPriceMove is shared by the background expiry sweep and the final
// execution release. Keeping the strict comparison in one place prevents a
// deferred/manual signal from taking a different stale-price path than a
// signal observed by the periodic sweep.
func staleSignalPriceMove(referencePrice, currentPrice, threshold float64) (float64, bool) {
	if threshold <= 0 || referencePrice <= 0 || currentPrice <= 0 ||
		math.IsNaN(referencePrice) || math.IsNaN(currentPrice) ||
		math.IsInf(referencePrice, 0) || math.IsInf(currentPrice, 0) {
		return 0, false
	}
	delta := math.Abs(currentPrice-referencePrice) / referencePrice * 100
	return delta, delta > threshold
}

// needsFreshStaleSignalQuote identifies the one release path that lacks a
// broker admission ask and also does not acquire the final anti-chase quote.
func needsFreshStaleSignalQuote(admission stockEntryAdmission, antiChaseEnabled bool, stalePriceMovePct float64) bool {
	return !admission.BrokerAuthoritative && !admission.BrokerHeldReplay &&
		!antiChaseEnabled && stalePriceMovePct > 0
}

func requiresFinalExecutionQuote(admission stockEntryAdmission, antiChaseEnabled bool, stalePriceMovePct float64) bool {
	return !admission.BrokerAuthoritative && !admission.BrokerHeldReplay &&
		(antiChaseEnabled || stalePriceMovePct > 0)
}

// rejectStaleSignalExecution applies the immutable accepted thesis price to
// the authoritative release price. Callers choose that release price exactly
// once: the coordinated broker ask, a fresh stale-only quote, or the final
// non-broker anti-chase quote after evidence gathering.
func (e *Engine) rejectStaleSignalExecution(ctx context.Context, sigID string, sig store.Signal, referencePrice, executionPrice float64) bool {
	delta, stale := staleSignalPriceMove(referencePrice, executionPrice, sig.StalePriceMovePct)
	if !stale {
		return false
	}
	reason := fmt.Sprintf("price moved %.1f%% from signal (threshold %.1f%%)", delta, sig.StalePriceMovePct)
	if err := store.UpdateSignalStatus(ctx, sigID, "stale", reason); err != nil {
		logger.Warn("stale signal before final execution price", "id", sigID, "err", err)
	}
	logger.Info("signal stale before final execution price", "id", sigID, "ticker", sig.Ticker, "move_pct", delta)
	e.bus.Publish(events.SignalBlocked, events.SignalBlockedPayload{
		Ticker: sig.Ticker, Action: sig.Action, Confidence: sig.Confidence,
		BlockReason: reason, FailedCheck: "stale_signal_price",
	})
	return true
}

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
