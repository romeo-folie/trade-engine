package engine

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/romeo-folie/trade-engine/internal/analysis"
	"github.com/romeo-folie/trade-engine/internal/analysis/prompts"
	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/data"
	"github.com/romeo-folie/trade-engine/internal/events"
	"github.com/romeo-folie/trade-engine/internal/execution"
	"github.com/romeo-folie/trade-engine/internal/indicators"
	"github.com/romeo-folie/trade-engine/internal/logger"
	"github.com/romeo-folie/trade-engine/internal/store"
)

// snapshotInterval is how often we take a snapshot of each monitored contract.
const snapshotInterval = 15 * time.Minute

// discoveryInterval is how often we re-query platforms for newly active contracts.
const discoveryInterval = 60 * time.Minute

// minSnapshotsForAnalysis is the minimum number of historical snapshots required
// before we ask the model to analyse a contract.
const minSnapshotsForAnalysis = 4

// maxPredictionConcurrency caps the number of simultaneous LLM calls from the
// snapshot loop.  Each call can last several seconds; without a bound, 50
// contracts fire 50 concurrent requests and immediately hit rate limits.
const maxPredictionConcurrency = 5

// PredictionRunner is a self-contained 24/7 loop that:
//  1. Discovers new prediction market contracts from configured platforms.
//  2. Stores snapshots every 15 minutes per monitored contract.
//  3. Runs LLM analysis once sufficient history is accumulated.
//  4. Emits signals and executes orders according to engine mode.
type PredictionRunner struct {
	cfg       *config.Config
	platforms map[string]data.PredictionDataProvider
	executors map[string]execution.Executor // platform → executor
	analyzer  *analysis.Analyzer
	bus       *events.Bus
	mode      string
}

// NewPredictionRunner creates a PredictionRunner for all enabled platforms.
//
// Polymarket live execution requires ECDSA signing of each CLOB request; until
// that layer is implemented the Polymarket executor falls back to
// PredictionPaperExecutor so signals are still exercised end-to-end in paper.
func NewPredictionRunner(cfg *config.Config, bus *events.Bus, mode string) *PredictionRunner {
	r := &PredictionRunner{
		cfg:       cfg,
		platforms: make(map[string]data.PredictionDataProvider),
		executors: make(map[string]execution.Executor),
		analyzer:  analysis.NewAnalyzer(cfg.Models),
		bus:       bus,
		mode:      mode,
	}

	for name, pc := range cfg.Markets.Predictions.Platforms {
		if !pc.Enabled {
			continue
		}
		switch name {
		case "polymarket":
			r.platforms[name] = data.NewPolymarketClient()
			// MED-1: Polymarket CLOB requires ECDSA signing — fall back to paper
			// executor until that signing layer is implemented.
			r.executors[name] = execution.NewPredictionPaperExecutor()
		case "kalshi":
			r.platforms[name] = data.NewKalshiClient(pc.APIKey)
			if pc.APIKey != "" {
				r.executors[name] = execution.NewKalshiExecutor(pc.APIKey)
			} else {
				r.executors[name] = execution.NewPredictionPaperExecutor()
			}
		default:
			logger.Warn("unknown prediction platform", "name", name)
		}
	}
	return r
}

// Enabled returns true if at least one prediction platform is configured.
func (r *PredictionRunner) Enabled() bool { return len(r.platforms) > 0 }

// Run starts the prediction market loop. Blocks until ctx is cancelled.
func (r *PredictionRunner) Run(ctx context.Context) {
	if !r.Enabled() {
		logger.Info("prediction markets disabled (no platforms configured)")
		return
	}
	logger.Info("prediction runner started", "platforms", len(r.platforms))

	// Run auto-discovery immediately on startup.
	r.runDiscovery(ctx)

	discoveryTicker := time.NewTicker(discoveryInterval)
	snapshotTicker := time.NewTicker(snapshotInterval)
	defer discoveryTicker.Stop()
	defer snapshotTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			logger.Info("prediction runner stopped")
			return
		case <-discoveryTicker.C:
			r.runDiscovery(ctx)
		case <-snapshotTicker.C:
			r.runSnapshotAndAnalysis(ctx)
		}
	}
}

// ── Discovery ─────────────────────────────────────────────────────────────────

// runDiscovery queries each platform for contracts matching configured categories
// and adds new ones to the watchlist with source='auto_discovered'.
func (r *PredictionRunner) runDiscovery(ctx context.Context) {
	for name, provider := range r.platforms {
		pc := r.cfg.Markets.Predictions.Platforms[name]
		contracts, err := provider.FetchContracts(ctx, pc.Categories, pc.MinVolume, pc.MaxContracts)
		if err != nil {
			logger.Warn("prediction discovery failed", "platform", name, "err", err.Error())
			continue
		}
		added := 0
		for _, c := range contracts {
			item := store.WatchlistItem{
				Ticker:     c.ID,
				Platform:   name,
				Strategies: []string{"prediction_mispricing"},
				Source:     "auto_discovered",
				Active:     true,
			}
			if err := store.AddWatchlistItem(ctx, item); err != nil {
				logger.Warn("watchlist add failed", "contract", c.ID, "err", err.Error())
			} else {
				added++
			}
		}
		logger.Info("prediction discovery complete",
			"platform", name,
			"found", len(contracts),
			"added", added,
		)
	}
}

// ── Snapshot + Analysis ───────────────────────────────────────────────────────

// runSnapshotAndAnalysis fetches the latest state of each monitored contract,
// stores a snapshot, then analyses contracts with sufficient history.
//
// Analysis goroutines are bounded by a semaphore of maxPredictionConcurrency to
// prevent simultaneous LLM calls from exceeding API rate limits.  The loop also
// checks ctx.Done() on each iteration so shutdown is prompt.
func (r *PredictionRunner) runSnapshotAndAnalysis(ctx context.Context) {
	items, err := store.GetWatchlistItems(ctx, true)
	if err != nil {
		logger.Warn("prediction: watchlist load failed", "err", err.Error())
		return
	}

	// Semaphore: at most maxPredictionConcurrency goroutines running at once.
	sem := make(chan struct{}, maxPredictionConcurrency)
	var wg sync.WaitGroup

	for _, item := range items {
		// MED-3: stop launching new work after shutdown.
		select {
		case <-ctx.Done():
			break
		default:
		}

		provider, ok := r.platforms[item.Platform]
		if !ok {
			continue // not a prediction platform
		}
		snap, err := provider.FetchSnapshot(ctx, item.Ticker)
		if err != nil {
			logger.Warn("prediction snapshot failed", "contract", item.Ticker, "err", err.Error())
			continue
		}

		// If the contract resolved, deactivate it in the watchlist.
		if snap.Resolved {
			_ = store.RemoveWatchlistItem(ctx, item.Ticker, item.Platform)
			logger.Info("prediction contract resolved — removed from watchlist",
				"contract", item.Ticker, "platform", item.Platform)
			continue
		}

		if err := store.InsertPredictionSnapshot(ctx, *snap); err != nil {
			logger.Warn("insert prediction snapshot failed", "contract", item.Ticker, "err", err.Error())
		}

		// Launch bounded analysis goroutine.
		wg.Add(1)
		sem <- struct{}{} // acquire slot (blocks when at capacity)
		go func(it store.WatchlistItem, sn *data.ContractSnapshot) {
			defer wg.Done()
			defer func() { <-sem }() // release slot
			r.analyseContract(ctx, it, sn)
		}(item, snap)
	}

	wg.Wait()
}

// analyseContract loads recent snapshots, computes indicators, and calls the LLM.
func (r *PredictionRunner) analyseContract(ctx context.Context, item store.WatchlistItem, latest *data.ContractSnapshot) {
	// HIGH-1: recover from any panic so a single bad contract cannot crash the loop.
	defer func() {
		if rec := recover(); rec != nil {
			logger.Error("prediction analysis panic recovered",
				"contract", item.Ticker,
				"platform", item.Platform,
				"panic", fmt.Sprintf("%v", rec),
			)
		}
	}()

	snaps, err := store.GetRecentSnapshots(ctx, item.Platform, item.Ticker, 48)
	if err != nil || len(snaps) < minSnapshotsForAnalysis {
		return
	}

	// Convert to prompts input types.
	history := make([]prompts.ContractPricePoint, len(snaps))
	for i, s := range snaps {
		history[i] = prompts.ContractPricePoint{Timestamp: s.CreatedAt, Price: s.Price}
	}

	// store.GetRecentSnapshots returns newest-first; reverse to oldest→newest.
	for i, j := 0, len(snaps)-1; i < j; i, j = i+1, j-1 {
		snaps[i], snaps[j] = snaps[j], snaps[i]
		history[i], history[j] = history[j], history[i]
	}

	ind := indicators.ComputePredictionIndicators(snaps)
	var promptInd *prompts.PredictionIndicators
	if ind != nil {
		promptInd = &prompts.PredictionIndicators{
			PriceTrend:          ind.PriceTrend,
			PriceVelocity:       ind.PriceVelocity,
			VolumeTrend:         ind.VolumeTrend,
			SpreadTrend:         ind.SpreadTrend,
			DaysToResolution:    ind.DaysToResolution,
			ProbabilityVelocity: ind.ProbabilityVelocity,
			Spread:              ind.Spread,
			SampleCount:         ind.SampleCount,
		}
	}

	contract := prompts.ContractInfo{
		Platform:           latest.Platform,
		ContractID:         latest.ContractID,
		Title:              latest.ContractTitle,
		Categories:         latest.Categories,
		MarketPrice:        latest.Price,
		YesPrice:           latest.YesPrice,
		NoPrice:            latest.NoPrice,
		Bid:                latest.Bid,
		Ask:                latest.Ask,
		Volume24h:          latest.Volume24h,
		OpenInterest:       latest.OpenInterest,
		ResolutionDeadline: latest.ResolutionDeadline,
	}

	sysPrompt, userMsg := prompts.BuildPredictionPrompt(contract, history, promptInd, nil, nil)

	areq := analysis.AnalysisRequest{
		Ticker:             fmt.Sprintf("%s:%s", item.Platform, item.Ticker),
		Timeframe:          "15m",
		Strategy:           "prediction_mispricing",
		CustomSystemPrompt: sysPrompt,
		CustomUserMessage:  userMsg,
	}

	resp, meta, err := r.analyzer.Analyze(ctx, areq)
	if err != nil {
		logger.Warn("prediction analysis failed", "contract", item.Ticker, "err", err.Error())
		return
	}

	logger.Info("prediction analysis complete",
		"contract", item.Ticker,
		"action", resp.Action,
		"confidence", resp.Confidence,
		"tokens_in", meta.InputTokens,
		"tokens_out", meta.OutputTokens,
	)

	// Only act on buy / sell with sufficient confidence.
	if resp.Action == analysis.ActionHold || resp.Confidence < item.MinConfidence {
		return
	}

	r.emitSignal(ctx, item, contract, resp)
}

// emitSignal records a signal and optionally executes it.
func (r *PredictionRunner) emitSignal(ctx context.Context, item store.WatchlistItem, contract prompts.ContractInfo, resp analysis.AnalysisResponse) {
	sigID, err := store.InsertSignal(ctx, store.Signal{
		Ticker:     item.Ticker,
		Action:     string(resp.Action), // signal_action enum expects BUY|SELL|HOLD
		Strategy:   "prediction_mispricing",
		Confidence: resp.Confidence,
		Status:     "pending",
	})
	if err != nil {
		logger.Warn("prediction signal insert failed", "err", err.Error())
		return
	}

	r.bus.Publish(events.SignalGenerated, events.SignalGeneratedPayload{
		SignalID:   sigID,
		Ticker:     item.Ticker,
		Action:     string(resp.Action),
		Confidence: resp.Confidence,
		Mode:       r.mode,
	})

	if r.mode != "auto" {
		return
	}

	exec, ok := r.executors[item.Platform]
	if !ok {
		return
	}

	side := execution.OrderSideBuy
	limitPrice := contract.Ask
	if resp.Action == analysis.ActionSell {
		side = execution.OrderSideSell
		limitPrice = contract.Bid
	}

	orderResult, err := exec.PlaceOrder(ctx, execution.OrderRequest{
		Ticker:     item.Ticker,
		Side:       side,
		Type:       execution.OrderTypeLimit,
		LimitPrice: limitPrice,
		Quantity:   1.0,
		TradeID:    sigID,
	})
	if err != nil {
		logger.Warn("prediction order failed", "contract", item.Ticker, "err", err.Error())
		return
	}

	logger.Info("prediction order placed",
		"contract", item.Ticker,
		"action", resp.Action,
		"broker_id", orderResult.BrokerOrderID,
		"filled_price", orderResult.FilledPrice,
	)

	direction := strings.ToLower(string(resp.Action))
	r.bus.Publish(events.TradeExecuted, events.TradeExecutedPayload{
		Ticker:     item.Ticker,
		SignalID:   sigID,
		Direction:  direction,
		EntryPrice: orderResult.FilledPrice,
		Quantity:   orderResult.FilledQty,
		Strategy:   "prediction_mispricing",
	})
}
