// Package backtest implements the historical replay engine for TradeEngine.
// It loads candles, queries the analysis pipeline (with prompt caching),
// simulates entry/exit using the PaperExecutor, computes performance metrics,
// and stores the completed run in the database.
package backtest

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/romeo-folie/trade-engine/internal/analysis"
	"github.com/romeo-folie/trade-engine/internal/analysis/depth"
	"github.com/romeo-folie/trade-engine/internal/analysis/prompts"
	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/data"
	"github.com/romeo-folie/trade-engine/internal/execution/trailing"
	"github.com/romeo-folie/trade-engine/internal/indicators"
	"github.com/romeo-folie/trade-engine/internal/logger"
	"github.com/romeo-folie/trade-engine/internal/risk"
	"github.com/romeo-folie/trade-engine/internal/store"
	"github.com/romeo-folie/trade-engine/internal/strategy/pullback"
)

// ── Public types ──────────────────────────────────────────────────────────────

// RunOptions configures a single backtest run.
type RunOptions struct {
	Ticker    string
	Strategy  string
	Timeframe data.Timeframe
	From      time.Time
	To        time.Time
	Model     string // optional model name override (empty = use config primary)
	NoCache   bool   // bypass prompt cache when true

	// ── Exit-policy research knobs (exit-policy-study) ──────────────────────────
	// All default to the zero value, which reproduces the original baseline exit
	// behavior (fixed model SL/TP + ungated SELL). They let us replay the SAME
	// entry signals under different exit rules to measure how much the fixed
	// take-profit truncates winners.
	ExitMode  string // "" | "baseline" | "trail" — "trail" uncaps TP and rides a trailing stop
	TrailRule string // "atr" | "ema" — trailing rule when ExitMode=="trail" (default "atr")
	// TrailActivateGainPct withholds the ATR trail until peak gain from entry
	// reaches this percentage, mirroring risk.trailing_stop.activate_at_gain_pct.
	// 0 = trail from entry. Uses trailing.TrailArmed so the arming rule cannot
	// drift from the live manager's (the failure #518 was written to prevent).
	TrailActivateGainPct float64
	TrailATRMult         float64 // ATR multiple below the high-water mark (default 1.5, matches live trailing_stop)
	TrailEMAPeriod       int     // EMA period for the close-below-EMA trail (default 9, Martin's rule)
	ExitConfMin          float64 // require SELL confidence ≥ this to exit on signal (0 = ungated, original behavior)
	StopWidthPct         float64 // override non-pullback stop to entry×(1-pct/100) (0 = use model stop)

	// PullbackStrength applies the complete Phase 2 deterministic entry setup
	// after the cached analyst BUY. The analyst request is left unchanged, so a
	// replay can reuse the original response cache and isolate entry selection.
	PullbackStrength bool
}

// exitPolicy is the resolved, defaulted view of the exit knobs on RunOptions.
type exitPolicy struct {
	trail       bool // ride a trailing stop and ignore the fixed take-profit
	useEMA      bool // trailing rule is close-below-EMA (else ATR-below-high-water)
	atrMult     float64
	emaPeriod   int
	exitConfMin float64
	stopWidth   float64 // fractional stop width (e.g. 0.02 = 2%); 0 = use model stop
	// activateGainPct gates the ATR trail on peak gain from entry; 0 = ungated.
	activateGainPct float64
}

func resolveExitPolicy(o RunOptions) exitPolicy {
	p := exitPolicy{
		trail:       o.ExitMode == "trail",
		useEMA:      o.TrailRule == "ema",
		atrMult:     o.TrailATRMult,
		emaPeriod:   o.TrailEMAPeriod,
		exitConfMin: o.ExitConfMin,
		stopWidth:   o.StopWidthPct / 100,

		activateGainPct: o.TrailActivateGainPct,
	}
	if p.atrMult <= 0 {
		p.atrMult = 1.5
	}
	if p.emaPeriod <= 0 {
		p.emaPeriod = 9
	}
	return p
}

// SimulatedTrade records a single round-trip trade produced by the replay engine.
type SimulatedTrade struct {
	Ticker     string
	Direction  string // "long" (the stock backtest is long-only; matches the trade_direction enum)
	EntryTS    time.Time
	ExitTS     time.Time // zero if still open at end of backtest
	EntryPrice float64
	ExitPrice  float64
	Quantity   float64
	PnL        float64
	PnLPct     float64
	ExitReason string // "stop_loss" | "take_profit" | "trail_atr" | "trail_ema" | "signal" | "end_of_data"

	// Risk-relative outcome (exit-policy-study). RiskPerShare is the entry-to-
	// initial-stop distance (1R). RMultiple is the trade's P&L in units of that
	// risk — the metric Martin's asymmetric edge lives in. Zero when no initial
	// stop was set (RMultiple left 0 to avoid divide-by-zero).
	RiskPerShare float64
	RMultiple    float64
}

// RunResult contains the outcome of a completed backtest.
type RunResult struct {
	Label           string
	RunID           string
	Options         RunOptions
	Trades          []SimulatedTrade
	Metrics         Metrics
	Hits            int64
	Misses          int64
	EntryCandidates int
	EntryQualified  int
	EntryBlocks     map[string]int
}

// EntryDiagnostics is persisted with each run so zero-trade pullback replays
// remain attributable and comparable after the CLI process exits.
type EntryDiagnostics struct {
	Candidates int            `json:"candidates"`
	Qualified  int            `json:"qualified"`
	Blocks     map[string]int `json:"blocks,omitempty"`
}

// ProgressFunc is called after each candle is processed with (current, total).
type ProgressFunc func(current, total int)

// Runner executes backtests against the configured analysis pipeline.
type Runner struct {
	cfg      *config.Config
	dataProv data.MarketDataProvider
	analyzer *analysis.Analyzer
	cache    *PromptCache
}

// NewRunner creates a Runner wired to the provided dependencies.
func NewRunner(cfg *config.Config, dataProv data.MarketDataProvider, az *analysis.Analyzer) *Runner {
	return &Runner{
		cfg:      cfg,
		dataProv: dataProv,
		analyzer: az,
		cache:    NewPromptCache(),
	}
}

// ── Data loading (task 7.1) ────────────────────────────────────────────────────

// LoadCandles returns candles for the requested range, backfilling from the
// data provider for any dates missing from the local database.
func (r *Runner) LoadCandles(ctx context.Context, ticker string, tf data.Timeframe, from, to time.Time) ([]data.Candle, error) {
	cached, err := store.GetCandles(ctx, ticker, tf, from, to)
	if err != nil {
		return nil, fmt.Errorf("load candles from db: %w", err)
	}

	// Check whether cached data covers the full range.
	if coversRange(cached, from, to, tf) {
		return cached, nil
	}

	// Backfill from provider.
	fetched, err := r.dataProv.FetchCandles(ctx, ticker, tf, from, to)
	if err != nil {
		return nil, fmt.Errorf("backfill candles from provider: %w", err)
	}
	if len(fetched) == 0 {
		return nil, fmt.Errorf("no candles returned from provider for %s %s [%s, %s]",
			ticker, tf, from.Format("2006-01-02"), to.Format("2006-01-02"))
	}
	if err := store.UpsertCandles(ctx, fetched); err != nil {
		return nil, fmt.Errorf("persist backfilled candles: %w", err)
	}

	// Re-query so the returned slice is always DB-canonical order.
	return store.GetCandles(ctx, ticker, tf, from, to)
}

// coversRange returns true when the cached candle slice spans the full [from,to]
// window with no obvious gaps. Uses a simple check: first and last candle
// timestamps are within two candle-durations of the requested boundaries
// (tolerance = candleDuration(tf) * 2).
func coversRange(candles []data.Candle, from, to time.Time, tf data.Timeframe) bool {
	if len(candles) == 0 {
		return false
	}
	tolerance := candleDuration(tf) * 2
	first := candles[0].Timestamp
	last := candles[len(candles)-1].Timestamp
	return !first.After(from.Add(tolerance)) && !last.Before(to.Add(-tolerance))
}

func candleDuration(tf data.Timeframe) time.Duration {
	switch tf {
	case data.Timeframe1Min:
		return time.Minute
	case data.Timeframe5Min:
		return 5 * time.Minute
	case data.Timeframe15Min:
		return 15 * time.Minute
	case data.Timeframe1Hour:
		return time.Hour
	default: // 1d and unknown
		return 24 * time.Hour
	}
}

// candlesThrough returns history ending at ts (inclusive). Candle slices are
// DB-canonical ascending order, so this is logarithmic and allocation-free.
func candlesThrough(candles []data.Candle, ts time.Time) []data.Candle {
	end := sort.Search(len(candles), func(i int) bool {
		return candles[i].Timestamp.After(ts)
	})
	return candles[:end]
}

// ── Replay engine (task 7.3) ───────────────────────────────────────────────────

const minCandlesForIndicators = 30 // minimum history before signalling

// Run executes the full backtest pipeline for the given options.
// progress is called after each candle and may be nil.
func (r *Runner) Run(ctx context.Context, opts RunOptions, progress ProgressFunc) (*RunResult, error) {
	candles, err := r.LoadCandles(ctx, opts.Ticker, opts.Timeframe, opts.From, opts.To)
	if err != nil {
		return nil, err
	}
	if len(candles) < minCandlesForIndicators+1 {
		return nil, fmt.Errorf("insufficient candle data: need at least %d candles, got %d",
			minCandlesForIndicators+1, len(candles))
	}

	var pullbackCandles []data.Candle
	if opts.PullbackStrength {
		if opts.Timeframe != data.Timeframe1Day {
			return nil, fmt.Errorf("pullback-strength replay requires timeframe 1d")
		}
		// Load a separate warm-up slice. Signal requests continue using
		// `candles` below, preserving their exact cache fingerprints.
		pullbackCandles, err = r.LoadCandles(
			ctx, opts.Ticker, data.Timeframe1Day,
			opts.From.AddDate(-1, 0, 0), opts.To,
		)
		if err != nil {
			return nil, fmt.Errorf("load pullback warm-up: %w", err)
		}
	}

	cash := r.cfg.Backtest.InitialCash
	rfRate := r.cfg.Backtest.RiskFreeRate
	// Use first analyst from registry (or legacy Primary) for backtest model metadata.
	primaryCfg := r.cfg.Models.Primary
	if analysts := r.cfg.Models.AnalystConfigs(); len(analysts) > 0 {
		primaryCfg = analysts[0]
	}
	provider := primaryCfg.Provider
	modelName := primaryCfg.Model
	if opts.Model != "" {
		modelName = opts.Model
	}

	var trades []SimulatedTrade
	var pendingEntry *pendingOrder // entry scheduled for next candle's open
	var openPosition *openPos
	entryBlocks := make(map[string]int)
	entryCandidates := 0
	entryQualified := 0

	// Exit-policy replay state: resolve the (defaulted) policy and precompute the
	// per-candle ATR/EMA series once so the exit check is O(1) per bar.
	policy := resolveExitPolicy(opts)
	atrSer := atrSeries(candles, 14)
	emaSer := emaSeries(candles, policy.emaPeriod)

	for i, candle := range candles {
		if progress != nil {
			progress(i+1, len(candles))
		}

		// 1. Check whether this candle triggers an exit under the active policy
		//    (applies to positions that entered on a previous candle).
		if openPosition != nil {
			if t, hit := evaluateExit(openPosition, candle, atrSer[i], emaSer[i], policy); hit {
				trades = append(trades, t)
				cash += t.PnL + openPosition.cost
				openPosition = nil
			}
		}

		// 2. Execute a pending entry at this candle's open price
		//    (prevents look-ahead bias: signal at i-1, entry at i).
		if pendingEntry != nil {
			order := pendingEntry
			pendingEntry = nil
			qty := math.Floor(order.cashAlloc / candle.Open)
			validFill := true
			if order.requireValidStop {
				if block := pullbackFillBlock(
					candle.Open, order.stopLoss, order.takeProfit,
					order.maxStopWidthPct, order.confidence, order.riskCfg,
				); block != "" {
					entryBlocks[block]++
					validFill = false
				}
			}
			if validFill && qty >= 1 {
				cost := qty * candle.Open
				cash -= cost
				entryPx := candle.Open
				// Initial protective stop: model's stop, or a fixed width override.
				// Pullback entries always retain the qualified signal-session low:
				// a generic replay override must not replace the strategy's
				// structural invalidation point.
				stop := initialEntryStop(
					entryPx, order.stopLoss, policy.stopWidth,
					order.requireValidStop,
				)
				risk := 0.0
				if stop > 0 && stop < entryPx {
					risk = entryPx - stop
				}
				openPosition = &openPos{
					ticker:       opts.Ticker,
					direction:    "long", // trade_direction enum is long|short; backtest is long-only
					entryTS:      candle.Timestamp,
					entryPrice:   entryPx,
					quantity:     qty,
					cost:         cost,
					stopLoss:     stop,
					takeProfit:   order.takeProfit,
					riskPerShare: risk,
					highWater:    entryPx,
					trailStop:    stop, // ratchets upward in ATR-trail mode
				}
				// The entry fills at the open, so the rest of this candle can
				// legitimately hit the new stop or target. evaluateExit checks
				// the hard stop before the target, providing the conservative
				// ordering when both levels lie inside the same OHLC bar.
				if t, hit := evaluateExit(openPosition, candle, atrSer[i], emaSer[i], policy); hit {
					trades = append(trades, t)
					cash += t.PnL + openPosition.cost
					openPosition = nil
				}
			}
		}

		// 3. Generate signal for this candle (requires enough history).
		if i < minCandlesForIndicators {
			continue
		}

		window := candles[max(0, i-199) : i+1] // up to 200 candles, never look ahead
		req, err := buildRequest(opts, window, r.cfg.Risk)
		if err != nil {
			continue // skip candles where indicator computation fails
		}

		resp, err := r.query(ctx, req, provider, modelName, opts.NoCache)
		if err != nil {
			continue // API error: skip candle, do not crash run
		}

		// 4. Act on the signal.
		switch resp.Action {
		case analysis.ActionBuy:
			if openPosition != nil {
				continue // already long, skip duplicate buy
			}
			if !opts.PullbackStrength && resp.Confidence < r.cfg.Risk.ConfidenceThreshold {
				continue
			}
			stopLoss := resp.StopLoss
			if opts.PullbackStrength {
				entryCandidates++
				setupWindow := candlesThrough(pullbackCandles, candle.Timestamp)
				setup := pullback.Evaluate(setupWindow, candle.Close, pullback.Config{
					MinMomentumReturnPct:  r.cfg.Risk.EntryGates.PullbackMinMomentumReturnPct,
					ConsolidationSessions: r.cfg.Risk.EntryGates.PullbackConsolidationSessions,
					SupportTolerancePct:   r.cfg.Risk.EntryGates.PullbackSupportTolerancePct,
					MaxStopWidthPct:       r.cfg.Risk.EntryGates.PullbackMaxStopWidthPct,
				})
				if !setup.Eligible {
					entryBlocks[setup.FailedCheck]++
					continue
				}
				if block := pullbackSignalBlock(
					candle.Close, setup.StopLoss, resp.TakeProfit,
					resp.Confidence, r.cfg.Risk,
				); block != "" {
					entryBlocks[block]++
					continue
				}
				entryQualified++
				stopLoss = setup.StopLoss
			}
			sizePct := resp.SizePct
			if sizePct <= 0 {
				sizePct = r.cfg.Risk.MaxPositionPct
			}
			pendingEntry = &pendingOrder{
				cashAlloc:        cash * (sizePct / 100),
				stopLoss:         stopLoss,
				takeProfit:       resp.TakeProfit,
				requireValidStop: opts.PullbackStrength,
				maxStopWidthPct:  r.cfg.Risk.EntryGates.PullbackMaxStopWidthPct,
				confidence:       resp.Confidence,
				riskCfg:          r.cfg.Risk,
			}

		case analysis.ActionSell:
			if openPosition == nil {
				continue // nothing to sell
			}
			// Exit-conviction gate: only act on a SELL that clears the confidence
			// bar (0 = ungated, the original behavior). Below the bar we hold and
			// let the trailing/hard stop manage the exit — the point of the study
			// is that low-conviction SELLs truncate winners just like a fixed TP.
			if resp.Confidence >= policy.exitConfMin {
				// Close at next candle's open — mark for deferred exit so we never
				// look ahead within the signal candle.
				openPosition.exitSignal = true
			}
		}
	}

	// 5. Force-close any open position at the final candle's close.
	if openPosition != nil {
		lastCandle := candles[len(candles)-1]
		t := closePosition(openPosition, lastCandle.Timestamp, lastCandle.Close, "end_of_data")
		trades = append(trades, t)
		cash += t.PnL + openPosition.cost
	}

	// 6. Also flush any position that received an exit signal but never got
	//    the chance to execute (edge case: exit signal on the very last candle).
	// (Already handled above via end_of_data.)

	finalCash := cash
	m := Compute(trades, r.cfg.Backtest.InitialCash, finalCash, opts.From, opts.To, rfRate)

	// 7. Persist to database.
	cfgSnap, _ := json.Marshal(r.cfg)
	runOptionsJSON, _ := json.Marshal(opts)
	entryDiagnosticsJSON, _ := json.Marshal(EntryDiagnostics{
		Candidates: entryCandidates,
		Qualified:  entryQualified,
		Blocks:     entryBlocks,
	})
	hitRate := r.cache.HitRate()
	hits, misses := r.cache.Stats()

	run := store.BacktestRun{
		Ticker:              opts.Ticker,
		Strategy:            opts.Strategy,
		ModelProvider:       provider,
		ModelName:           modelName,
		FromDate:            opts.From,
		ToDate:              opts.To,
		ConfigSnapshot:      cfgSnap,
		RunOptions:          runOptionsJSON,
		EntryDiagnostics:    entryDiagnosticsJSON,
		TotalReturnPct:      ptr(m.TotalReturnPct),
		AnnualizedReturnPct: ptr(m.AnnualizedReturnPct),
		SharpeRatio:         ptr(m.SharpeRatio),
		SortinoRatio:        ptr(m.SortinoRatio),
		CalmarRatio:         ptr(m.CalmarRatio),
		MaxDrawdownPct:      ptr(m.MaxDrawdownPct),
		WinRatePct:          ptr(m.WinRatePct),
		ProfitFactor:        ptr(m.ProfitFactor),
		Expectancy:          ptr(m.Expectancy),
		TotalTrades:         m.TotalTrades,
		AvgHoldHours:        ptr(m.AvgHoldHours),
		BestTradePct:        ptr(m.BestTradePct),
		WorstTradePct:       ptr(m.WorstTradePct),
		CacheHitRate:        ptr(hitRate * 100),
	}

	label, err := store.InsertRun(ctx, run)
	if err != nil {
		return nil, fmt.Errorf("persist backtest run: %w", err)
	}

	// Fetch the newly created run to get its ID.
	created, err := store.GetRunByLabel(ctx, label)
	if err != nil || created == nil {
		return nil, fmt.Errorf("fetch created run %s: %w", label, err)
	}

	for _, t := range trades {
		exitTS := &t.ExitTS
		if t.ExitTS.IsZero() {
			exitTS = nil
		}
		exitPrice := &t.ExitPrice
		st := store.BacktestTrade{
			RunID:      created.ID,
			Ticker:     t.Ticker,
			Direction:  t.Direction,
			EntryTS:    t.EntryTS,
			ExitTS:     exitTS,
			EntryPrice: t.EntryPrice,
			ExitPrice:  exitPrice,
			Quantity:   t.Quantity,
			PnL:        ptr(t.PnL),
			PnLPct:     ptr(t.PnLPct),
			ExitReason: t.ExitReason,
		}
		if err := store.InsertBacktestTrade(ctx, st); err != nil {
			return nil, fmt.Errorf("persist backtest trade: %w", err)
		}
	}

	return &RunResult{
		Label:           label,
		RunID:           created.ID,
		Options:         opts,
		Trades:          trades,
		Metrics:         m,
		Hits:            hits,
		Misses:          misses,
		EntryCandidates: entryCandidates,
		EntryQualified:  entryQualified,
		EntryBlocks:     entryBlocks,
	}, nil
}

// ── Internal helpers ──────────────────────────────────────────────────────────

type pendingOrder struct {
	cashAlloc        float64
	stopLoss         float64
	takeProfit       float64
	requireValidStop bool
	maxStopWidthPct  float64
	confidence       float64
	riskCfg          config.RiskConfig
}

func initialEntryStop(
	entryPrice, signalStop, overrideWidth float64, preserveSignalStop bool,
) float64 {
	if overrideWidth > 0 && !preserveSignalStop {
		return entryPrice * (1 - overrideWidth)
	}
	return signalStop
}

func pullbackSignalBlock(
	mark, stopLoss, takeProfit, confidence float64, riskCfg config.RiskConfig,
) string {
	results, passed := risk.CheckEntryQuality(risk.CheckInput{
		Cfg:         riskCfg,
		Action:      "BUY",
		Confidence:  confidence,
		StopLoss:    stopLoss,
		TakeProfit:  takeProfit,
		SignalPrice: mark,
	})
	if passed {
		return ""
	}
	if len(results) > 0 && results[len(results)-1].Name == "confidence_threshold" {
		return "signal_confidence_tier"
	}
	return "signal_risk_reward"
}

func pullbackFillBlock(
	mark, stopLoss, takeProfit, maxStopWidthPct, confidence float64,
	riskCfg config.RiskConfig,
) string {
	if pullback.ValidateExecutionMark(mark, stopLoss, maxStopWidthPct) == "" {
		results, passed := risk.CheckEntryQuality(risk.CheckInput{
			Cfg:         riskCfg,
			Action:      "BUY",
			Confidence:  confidence,
			StopLoss:    stopLoss,
			TakeProfit:  takeProfit,
			SignalPrice: mark,
		})
		if passed {
			return ""
		}
		if len(results) > 0 && results[len(results)-1].Name == "confidence_threshold" {
			return "fill_confidence_tier"
		}
		return "fill_risk_reward"
	}
	if stopLoss >= mark {
		return "gap_invalidation"
	}
	// The only remaining invalid fill with a positive open above its stop is
	// an entry-to-stop width beyond the configured production cap.
	return "fill_stop_width"
}

type openPos struct {
	ticker     string
	direction  string
	entryTS    time.Time
	entryPrice float64
	quantity   float64
	cost       float64
	stopLoss   float64
	takeProfit float64
	exitSignal bool // gated SELL signal received — close at next candle open

	// Trailing / risk state (exit-policy-study). stopLoss is the original hard
	// stop recorded at entry and is never reassigned; it stays the floor and
	// the 1R reference. Under the ATR rule trailStop is the level actually
	// resting at the broker (it starts equal to stopLoss and only ratchets
	// up), so activeStop reports it in place of stopLoss.
	riskPerShare float64 // entryPrice - stopLoss (1R)
	highWater    float64 // highest high seen since entry
	trailStop    float64 // ratcheted ATR trailing stop (never decreases)
}

// activeStop returns the protective stop level resting at the broker during
// this bar.
//
// In ATR-trail mode the live manager REPLACES the broker stop with the
// ratcheted level (trailing.submitOrReplaceStop), so there is only ever one
// stop and it can never sit below the original hard stop — trailing.
// ComputeNewStop clamps with max(currentStop, hwm-mult*atr). The max() here
// restates that invariant defensively rather than trusting callers.
//
// The EMA rule deliberately leaves the broker stop where it started
// (trailing.updateOneEMA9: moving it would turn a close-confirmed rule into an
// intraday touch rule), so it always reports the original hard stop.
func (pos *openPos) activeStop(p exitPolicy) float64 {
	if p.trail && !p.useEMA {
		return math.Max(pos.trailStop, pos.stopLoss)
	}
	return pos.stopLoss
}

// stopFill models a resting broker stop order. It triggers when the bar trades
// at or below the stop and fills at the stop price — unless the bar gaps open
// below it, in which case the stop is swept through and fills at the open.
// Filling at the stop price on a gap-down would credit a price that never
// traded.
func stopFill(stop float64, candle data.Candle) float64 {
	if candle.Open < stop {
		return candle.Open
	}
	return stop
}

// ratchet advances trailing state using this bar's high.
//
// It deliberately runs only AFTER the bar's stop check. The stop resting during
// a bar is the one the manager placed from previously observed prices; letting
// this bar's own high raise the stop before testing that same bar's low would
// be lookahead — the live manager cannot place a stop derived from a high that
// has not printed yet.
func (pos *openPos) ratchet(candle data.Candle, barATR float64, p exitPolicy) {
	if candle.High > pos.highWater {
		pos.highWater = candle.High
	}
	// The arming check runs AFTER highWater updates, so this bar's own high can
	// arm the trail — matching the live manager, which seeds/ratchets the HWM
	// from the current price before evaluating the gate.
	if p.trail && !p.useEMA && barATR > 0 &&
		trailing.TrailArmed(pos.entryPrice, pos.highWater, p.activateGainPct) {
		if cand := pos.highWater - p.atrMult*barATR; cand > pos.trailStop {
			pos.trailStop = cand
		}
	}
}

// evaluateExit decides whether the position exits on this candle under the given
// policy, returning the closed trade when it does. barATR/barEMA are this
// candle's ATR(14) and EMA(policy.emaPeriod) values (0 when unavailable).
//
// Precedence: a gated SELL exits at the open; then the protective stop resting
// at the broker (intrabar); then TP (baseline) or the EMA rule (close-based).
//
// The two trailing rules exit through different mechanisms because the live
// ones do:
//
//   - atr:  the ratcheted stop REPLACES the broker stop, so it rests there and
//     fills INTRABAR at its own price (trailing.submitOrReplaceStop).
//   - ema9: the broker stop stays put and the winning exit is confirmed on the
//     CLOSE below the EMA (trailing.updateOneEMA9).
func evaluateExit(pos *openPos, candle data.Candle, barATR, barEMA float64, p exitPolicy) (SimulatedTrade, bool) {
	// 1. Gated SELL signal from the previous candle — exit at this open.
	if pos.exitSignal {
		return closePosition(pos, candle.Timestamp, candle.Open, "signal"), true
	}
	// 2. Move through the protective stop resting at the broker (intrabar).
	//    Under the ATR rule this is the ratcheted level; attribute the exit to
	//    the trail only once it has actually ratcheted above the hard stop, so
	//    the exit-reason split stays meaningful.
	if stop := pos.activeStop(p); stop > 0 && candle.Low <= stop {
		reason := "stop_loss"
		if p.trail && !p.useEMA && stop > pos.stopLoss {
			reason = "trail_atr"
		}
		return closePosition(pos, candle.Timestamp, stopFill(stop, candle), reason), true
	}
	if p.trail {
		// 3a. EMA trailing exit (close-based).
		if p.useEMA && barEMA > 0 && candle.Close < barEMA {
			return closePosition(pos, candle.Timestamp, candle.Close, "trail_ema"), true
		}
		pos.ratchet(candle, barATR, p)
		return SimulatedTrade{}, false
	}
	// 3b. Baseline: fixed take-profit cap (intrabar high).
	if pos.takeProfit > 0 && candle.High >= pos.takeProfit {
		return closePosition(pos, candle.Timestamp, pos.takeProfit, "take_profit"), true
	}
	pos.ratchet(candle, barATR, p)
	return SimulatedTrade{}, false
}

func closePosition(pos *openPos, ts time.Time, price float64, reason string) SimulatedTrade {
	proceeds := pos.quantity * price
	pnl := proceeds - pos.cost
	pnlPct := 0.0
	if pos.cost != 0 {
		pnlPct = pnl / pos.cost * 100
	}
	rMultiple := 0.0
	if pos.riskPerShare > 0 {
		rMultiple = (price - pos.entryPrice) / pos.riskPerShare
	}
	return SimulatedTrade{
		Ticker:       pos.ticker,
		Direction:    pos.direction,
		EntryTS:      pos.entryTS,
		ExitTS:       ts,
		EntryPrice:   pos.entryPrice,
		ExitPrice:    price,
		Quantity:     pos.quantity,
		PnL:          pnl,
		PnLPct:       pnlPct,
		ExitReason:   reason,
		RiskPerShare: pos.riskPerShare,
		RMultiple:    rMultiple,
	}
}

// buildRequest converts a candle window into an AnalysisRequest.
func buildRequest(opts RunOptions, window []data.Candle, riskCfg config.RiskConfig) (analysis.AnalysisRequest, error) {
	set, err := indicators.Compute(window)
	if err != nil {
		return analysis.AnalysisRequest{}, err
	}

	// Build recent candle snapshots (last 20, newest first).
	recentCount := min(20, len(window))
	recent := make([]analysis.CandleSnapshot, recentCount)
	for i := 0; i < recentCount; i++ {
		c := window[len(window)-1-i]
		recent[i] = analysis.CandleSnapshot{
			Timestamp: c.Timestamp,
			Open:      c.Open,
			High:      c.High,
			Low:       c.Low,
			Close:     c.Close,
			Volume:    c.Volume,
			VWAP:      c.VWAP,
		}
	}

	return analysis.AnalysisRequest{
		RiskPolicy: prompts.RiskPolicy{
			MaxTickerExposurePct: riskCfg.MaxPositionPct,
			MaxPerTicker:         riskCfg.MaxPerTicker,
			MinRiskReward:        riskCfg.MinRiskReward,
			Configured:           true,
		},
		Ticker:        opts.Ticker,
		Strategy:      opts.Strategy,
		Timeframe:     string(opts.Timeframe),
		RecentCandles: recent,
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
			ADX:           set.ADX,
			ATR:           set.ATR,
			OBV:           set.OBV,
			StochK:        set.StochK,
			StochD:        set.StochD,
			VWAP:          set.VWAP,
			VolSMA20:      set.VolumeSMA20,
			VolRatio: func() float64 {
				if set.VolumeSMA20 == 0 {
					return 0
				}
				// Use last candle volume for the ratio.
				return float64(window[len(window)-1].Volume) / set.VolumeSMA20
			}(),
		},
		// News is left empty during backtesting — historical news is not available.
		// Knowledge graph context is a Phase 10 addition.
	}, nil
}

// query checks the prompt cache (unless noCache) and calls the analyzer on miss.
// Cache hits that fail to unmarshal are treated as misses — the live call heals
// the entry on the next Put.
func (r *Runner) query(ctx context.Context, req analysis.AnalysisRequest, provider, model string, noCache bool) (analysis.AnalysisResponse, error) {
	hash, err := RequestHash(req)
	if err != nil {
		return analysis.AnalysisResponse{}, err
	}

	if !noCache {
		entry, hit, err := r.cache.Get(ctx, hash, provider, model)
		if err != nil {
			return analysis.AnalysisResponse{}, err
		}
		if hit {
			var cached analysis.AnalysisResponse
			if json.Unmarshal([]byte(entry.Response), &cached) == nil {
				return cached, nil
			}
			// Corrupt cache entry — fall through to live call.
		}
	}

	resp, meta, err := r.analyzer.Analyze(ctx, req, depth.ImpactScore{Score: 1.0, Depth: 3})
	if err != nil {
		return analysis.AnalysisResponse{}, err
	}

	if !noCache {
		if raw, err := json.Marshal(resp); err == nil {
			// The cache key MUST match the Get key above — the stable primary
			// (provider, model), NOT meta.Provider/meta.Model. meta reflects
			// whichever internal model produced the final response (the ARBITER
			// for an arbitrated candle, an analyst for a fast-path candle), so
			// keying the write on meta wrote arbitrated results under the arbiter's
			// key while the lookup used the primary — a permanent miss that
			// re-generated every arbitrated candle on every run (broke resumability).
			if err := r.cache.Put(ctx, PromptCacheEntry{
				PromptHash:   hash,
				Provider:     provider,
				Model:        model,
				Response:     string(raw),
				InputTokens:  meta.InputTokens,
				OutputTokens: meta.OutputTokens,
			}); err != nil {
				logger.Warn("backtest: prompt cache write failed", "ticker", req.Ticker, "err", err.Error())
			}
		}
	}
	return resp, nil
}

// atrSeries returns a per-candle Wilder ATR aligned to candles (0 before the
// series has warmed up). Used by the ATR trailing-stop exit rule.
func atrSeries(candles []data.Candle, period int) []float64 {
	out := make([]float64, len(candles))
	if period <= 0 || len(candles) <= period {
		return out
	}
	tr := make([]float64, len(candles))
	for i := 1; i < len(candles); i++ {
		h, l, pc := candles[i].High, candles[i].Low, candles[i-1].Close
		tr[i] = math.Max(h-l, math.Max(math.Abs(h-pc), math.Abs(l-pc)))
	}
	// Seed ATR at index `period` with the simple average of the first TRs.
	var sum float64
	for i := 1; i <= period; i++ {
		sum += tr[i]
	}
	out[period] = sum / float64(period)
	for i := period + 1; i < len(candles); i++ {
		out[i] = (out[i-1]*float64(period-1) + tr[i]) / float64(period)
	}
	return out
}

// emaSeries returns a per-candle EMA of close aligned to candles (0 before the
// series has warmed up). Used by the close-below-EMA trailing-stop exit rule.
func emaSeries(candles []data.Candle, period int) []float64 {
	out := make([]float64, len(candles))
	if period <= 0 || len(candles) < period {
		return out
	}
	var sum float64
	for i := 0; i < period; i++ {
		sum += candles[i].Close
	}
	out[period-1] = sum / float64(period)
	k := 2.0 / float64(period+1)
	for i := period; i < len(candles); i++ {
		out[i] = candles[i].Close*k + out[i-1]*(1-k)
	}
	return out
}

func ptr(f float64) *float64 { return &f }

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
