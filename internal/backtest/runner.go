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
	"time"

	"github.com/romeo-folie/trade-engine/internal/analysis"
	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/data"
	"github.com/romeo-folie/trade-engine/internal/indicators"
	"github.com/romeo-folie/trade-engine/internal/store"
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
}

// SimulatedTrade records a single round-trip trade produced by the replay engine.
type SimulatedTrade struct {
	Ticker     string
	Direction  string // "buy"
	EntryTS    time.Time
	ExitTS     time.Time // zero if still open at end of backtest
	EntryPrice float64
	ExitPrice  float64
	Quantity   float64
	PnL        float64
	PnLPct     float64
	ExitReason string // "stop_loss" | "take_profit" | "signal" | "end_of_data"
}

// RunResult contains the outcome of a completed backtest.
type RunResult struct {
	Label   string
	RunID   string
	Options RunOptions
	Trades  []SimulatedTrade
	Metrics Metrics
	Hits    int64
	Misses  int64
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
// timestamps are within one trading day of the requested boundaries.
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

	cash := r.cfg.Backtest.InitialCash
	rfRate := r.cfg.Backtest.RiskFreeRate
	provider := r.cfg.Models.Primary.Provider
	modelName := r.cfg.Models.Primary.Model
	if opts.Model != "" {
		modelName = opts.Model
	}

	var trades []SimulatedTrade
	var pendingEntry *pendingOrder // entry scheduled for next candle's open
	var openPosition *openPos

	for i, candle := range candles {
		if progress != nil {
			progress(i+1, len(candles))
		}

		// 1. Check if an SL or TP was hit within this candle's range
		//    (applies to positions that entered on a previous candle).
		if openPosition != nil {
			if t, hit := checkSLTP(openPosition, candle); hit {
				trades = append(trades, t)
				cash += t.PnL + openPosition.cost
				openPosition = nil
			}
		}

		// 2. Execute a pending entry at this candle's open price
		//    (prevents look-ahead bias: signal at i-1, entry at i).
		if pendingEntry != nil {
			qty := math.Floor(pendingEntry.cashAlloc / candle.Open)
			if qty >= 1 {
				cost := qty * candle.Open
				cash -= cost
				openPosition = &openPos{
					ticker:     opts.Ticker,
					direction:  "buy",
					entryTS:    candle.Timestamp,
					entryPrice: candle.Open,
					quantity:   qty,
					cost:       cost,
					stopLoss:   pendingEntry.stopLoss,
					takeProfit: pendingEntry.takeProfit,
				}
			}
			pendingEntry = nil
		}

		// 3. Generate signal for this candle (requires enough history).
		if i < minCandlesForIndicators {
			continue
		}

		window := candles[max(0, i-199) : i+1] // up to 200 candles, never look ahead
		req, err := buildRequest(opts, window)
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
			if resp.Confidence < r.cfg.Risk.ConfidenceThreshold {
				continue
			}
			sizePct := resp.SizePct
			if sizePct <= 0 {
				sizePct = r.cfg.Risk.MaxPositionPct
			}
			pendingEntry = &pendingOrder{
				cashAlloc:  cash * (sizePct / 100),
				stopLoss:   resp.StopLoss,
				takeProfit: resp.TakeProfit,
			}

		case analysis.ActionSell:
			if openPosition == nil {
				continue // nothing to sell
			}
			// Close at next candle's open — schedule as a forced exit.
			// We reuse pendingEntry to carry the signal; openPosition remains
			// open until we process the exit at the top of the next iteration.
			// Simplest correct approach: mark openPosition for exit.
			openPosition.exitSignal = true
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
		Label:   label,
		RunID:   created.ID,
		Options: opts,
		Trades:  trades,
		Metrics: m,
		Hits:    hits,
		Misses:  misses,
	}, nil
}

// ── Internal helpers ──────────────────────────────────────────────────────────

type pendingOrder struct {
	cashAlloc  float64
	stopLoss   float64
	takeProfit float64
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
	exitSignal bool // SELL signal received — close at next candle open
}

// checkSLTP checks whether the candle's high/low range triggers SL or TP.
// If both are triggered in the same candle, SL wins (conservative).
func checkSLTP(pos *openPos, candle data.Candle) (SimulatedTrade, bool) {
	slHit := pos.stopLoss > 0 && candle.Low <= pos.stopLoss
	tpHit := pos.takeProfit > 0 && candle.High >= pos.takeProfit

	// Also handle deferred exit signal from previous candle.
	if pos.exitSignal {
		return closePosition(pos, candle.Timestamp, candle.Open, "signal"), true
	}

	if slHit {
		return closePosition(pos, candle.Timestamp, pos.stopLoss, "stop_loss"), true
	}
	if tpHit {
		return closePosition(pos, candle.Timestamp, pos.takeProfit, "take_profit"), true
	}
	return SimulatedTrade{}, false
}

func closePosition(pos *openPos, ts time.Time, price float64, reason string) SimulatedTrade {
	proceeds := pos.quantity * price
	pnl := proceeds - pos.cost
	pnlPct := 0.0
	if pos.cost != 0 {
		pnlPct = pnl / pos.cost * 100
	}
	return SimulatedTrade{
		Ticker:     pos.ticker,
		Direction:  pos.direction,
		EntryTS:    pos.entryTS,
		ExitTS:     ts,
		EntryPrice: pos.entryPrice,
		ExitPrice:  price,
		Quantity:   pos.quantity,
		PnL:        pnl,
		PnLPct:     pnlPct,
		ExitReason: reason,
	}
}

// buildRequest converts a candle window into an AnalysisRequest.
func buildRequest(opts RunOptions, window []data.Candle) (analysis.AnalysisRequest, error) {
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

	resp, meta, err := r.analyzer.Analyze(ctx, req)
	if err != nil {
		return analysis.AnalysisResponse{}, err
	}

	if !noCache {
		if raw, err := json.Marshal(resp); err == nil {
			_ = r.cache.Put(ctx, PromptCacheEntry{
				PromptHash:   hash,
				Provider:     meta.Provider,
				Model:        meta.Model,
				Response:     string(raw),
				InputTokens:  meta.InputTokens,
				OutputTokens: meta.OutputTokens,
			})
		}
	}
	return resp, nil
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
