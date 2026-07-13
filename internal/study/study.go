package study

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/romeo-folie/trade-engine/internal/data"
)

// TradeCase is one recorded stock-path trade joined with its signal, the unit
// the study replays. HasSignal is false for trades with no signals row (e.g.
// recovery adoptions); entry-side analyses are skipped for those.
type TradeCase struct {
	ID          string
	Ticker      string
	Direction   string // long | short
	Strategy    string
	EntryPrice  float64
	ExitPrice   float64
	Quantity    float64
	StopLoss    float64
	TakeProfit  float64
	PnLPct      float64
	OpenedAt    time.Time
	ClosedAt    time.Time
	SignalPrice float64
	SignalAt    time.Time
	HasSignal   bool
}

// BarSource fetches historical candles; satisfied by *data.AlpacaClient.
type BarSource interface {
	FetchCandles(ctx context.Context, ticker string, tf data.Timeframe, from, to time.Time) ([]data.Candle, error)
}

// Config holds the study's knobs.
type Config struct {
	LimitATRs    []float64 // patient-limit depths in ATRs below signal (long)
	TrailingMult float64   // ATR multiple for the trailing counterfactual
	BarBufferDur time.Duration
}

// DefaultConfig mirrors the values discussed in the execution audit: three
// patient-limit depths and the trailing manager's default 1.5×ATR.
func DefaultConfig() Config {
	return Config{
		LimitATRs:    []float64{0.25, 0.5, 1.0},
		TrailingMult: 1.5,
		BarBufferDur: 5 * 24 * time.Hour,
	}
}

// TacticResult is one entry tactic's outcome for one trade.
type TacticResult struct {
	Name       string
	Filled     bool
	FillPrice  float64
	ReturnPct  float64 // 0 when unfilled
	ExitReason string  // stop | target | unresolved | "" when unfilled
}

// TradeResult carries every per-trade measurement.
type TradeResult struct {
	Case TradeCase

	// Entry side (HasSignal only)
	ChasePct float64 // signed cost of a live market order vs signal_price (+ = paid worse)
	Tactics  []TacticResult

	// Exit side
	SimExitPrice   float64 // bracket-sim exit for the ACTUAL entry
	SimExitReason  string
	SimExitAt      time.Time
	ExitSlipPct    float64 // actual exit vs sim exit (+ = sim was better)
	ExitLagMinutes float64 // sim trigger time → actual close time
	BreachClass    string  // gap | intraday | "" when sim never stopped

	// Excursions over the actual hold window
	MAEPct float64
	MFEPct float64

	// Trailing counterfactual. TrailComputed is false when the ATR was
	// unavailable — those trades must not count as "trail returned 0%".
	TrailReturnPct  float64
	TrailComputed   bool
	ActualReturnPct float64

	Skipped string // non-empty = excluded from aggregates, with the reason
}

// Runner wires data access to the per-trade replay.
type Runner struct {
	Bars BarSource
	Cfg  Config

	etLoc  *time.Location
	barsCache  map[string][]data.Candle // per (ticker,window) 1m bars
	dailyCache map[string]float64       // per ticker+date ATR
}

// NewRunner builds a Runner. It fails only when the ET tz database is missing.
func NewRunner(bars BarSource, cfg Config) (*Runner, error) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		return nil, fmt.Errorf("load America/New_York: %w", err)
	}
	return &Runner{
		Bars:       bars,
		Cfg:        cfg,
		etLoc:      loc,
		barsCache:  map[string][]data.Candle{},
		dailyCache: map[string]float64{},
	}, nil
}

// rthOnly keeps bars whose timestamp falls inside 09:30–16:00 ET. The study
// models regular-hours execution: the engine's own market orders outside RTH
// don't fill (the MU/TSLA/IREN zero-entry rows), so pre/post bars would let
// the sim trade where the live path cannot.
func (r *Runner) rthOnly(bars []data.Candle) []data.Candle {
	out := bars[:0:0]
	for _, b := range bars {
		et := b.Timestamp.In(r.etLoc)
		mins := et.Hour()*60 + et.Minute()
		if mins >= 9*60+30 && mins < 16*60 {
			out = append(out, b)
		}
	}
	return out
}

// sessionEnd is 16:00 ET on the day of t (in ET). Patient limits expire here.
func (r *Runner) sessionEnd(t time.Time) time.Time {
	et := t.In(r.etLoc)
	return time.Date(et.Year(), et.Month(), et.Day(), 16, 0, 0, 0, r.etLoc)
}

// fetchWithBackoff retries failed fetches with a long pause. The shared
// client's withRetry already handles transient failures, but a batch study at
// the free-tier ceiling exhausts its attempts (sustained 429s escalate to
// hung connections and client timeouts); an offline audit can afford to wait
// instead of dropping trades. Every error is retried except the caller's own
// context ending — this tool has no partial-failure mode worth preserving.
func (r *Runner) fetchWithBackoff(ctx context.Context, ticker string, tf data.Timeframe, from, to time.Time) ([]data.Candle, error) {
	var lastErr error
	for attempt := range 4 {
		bars, err := r.Bars.FetchCandles(ctx, ticker, tf, from, to)
		if err == nil {
			return bars, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		slog.Info("study: fetch failed, backing off", "ticker", ticker, "attempt", attempt+1, "err", err)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(45 * time.Second):
		}
	}
	return nil, lastErr
}

func (r *Runner) minuteBars(ctx context.Context, ticker string, from, to time.Time) ([]data.Candle, error) {
	key := fmt.Sprintf("%s|%d|%d", ticker, from.Unix(), to.Unix())
	if bars, ok := r.barsCache[key]; ok {
		return bars, nil
	}
	bars, err := r.fetchWithBackoff(ctx, ticker, data.Timeframe1Min, from, to)
	if err != nil {
		return nil, err
	}
	bars = r.rthOnly(bars)
	r.barsCache[key] = bars
	return bars, nil
}

// dailyATR returns ATR14 from daily bars ending the day BEFORE asOf, so the
// limit depths and trail width only use information available at signal time.
// Alpaca's bars `end` parameter is inclusive and daily bars are stamped at
// midnight ET of their own trading day, so the window must stop one day short
// of the signal day or the signal day's full-session range leaks in.
func (r *Runner) dailyATR(ctx context.Context, ticker string, asOf time.Time) (float64, error) {
	et := asOf.In(r.etLoc)
	key := ticker + "|" + et.Format("2006-01-02")
	if atr, ok := r.dailyCache[key]; ok {
		return atr, nil
	}
	dayStart := time.Date(et.Year(), et.Month(), et.Day(), 0, 0, 0, 0, r.etLoc)
	daily, err := r.fetchWithBackoff(ctx, ticker, data.Timeframe1Day, dayStart.AddDate(0, 0, -46), dayStart.AddDate(0, 0, -1))
	if err != nil {
		return 0, err
	}
	atr := ATR14(daily)
	r.dailyCache[key] = atr
	return atr, nil
}

// Run replays every trade case and returns per-trade results. Failures to
// fetch data skip the individual trade (recorded in Skipped), never abort the
// study.
func (r *Runner) Run(ctx context.Context, cases []TradeCase) []TradeResult {
	results := make([]TradeResult, 0, len(cases))
	for _, tc := range cases {
		res := r.runOne(ctx, tc)
		results = append(results, res)
		if res.Skipped != "" {
			slog.Warn("study: trade skipped", "ticker", tc.Ticker, "opened_at", tc.OpenedAt, "reason", res.Skipped)
		}
	}
	return results
}

func (r *Runner) runOne(ctx context.Context, tc TradeCase) TradeResult {
	res := TradeResult{Case: tc, ActualReturnPct: tc.PnLPct}
	if tc.EntryPrice <= 0 {
		res.Skipped = "zero entry price (unfilled after-hours order recorded as trade)"
		return res
	}
	if tc.ClosedAt.IsZero() || tc.ClosedAt.Before(tc.OpenedAt) {
		res.Skipped = "not closed"
		return res
	}

	signalAt := tc.SignalAt
	if !tc.HasSignal {
		signalAt = tc.OpenedAt
	}
	from := signalAt.Add(-time.Hour)
	to := tc.ClosedAt.Add(r.Cfg.BarBufferDur)
	if now := time.Now(); to.After(now) {
		to = now
	}
	bars, err := r.minuteBars(ctx, tc.Ticker, from, to)
	if err != nil {
		res.Skipped = fmt.Sprintf("fetch 1m bars: %v", err)
		return res
	}
	if len(bars) == 0 {
		res.Skipped = "no 1m bars in window"
		return res
	}

	// ── Exit fidelity: bracket-sim from the ACTUAL entry ──────────────────
	// Anchor at the bar containing the entry instant — the trade is live for
	// the remainder of that minute and a broker bracket would already be
	// armed. The fidelity sim is bounded at the recorded close: the bar
	// window extends days past ClosedAt for the counterfactuals below, but a
	// trade closed by another path (trailing manager, SELL signal) must not
	// be credited with a "bracket exit" that happens after it was already
	// flat — that fabricates improvements and negative detection lags.
	entryIdx := barContaining(bars, tc.OpenedAt)
	if entryIdx < 0 {
		res.Skipped = "no bars after entry"
		return res
	}
	fidelityEnd := lastBarAtOrBefore(bars, tc.ClosedAt)
	if tc.StopLoss > 0 || tc.TakeProfit > 0 {
		sim := SimBracketExit(bars[:fidelityEnd+1], entryIdx, tc.Direction, tc.StopLoss, tc.TakeProfit)
		res.SimExitPrice = sim.Price
		res.SimExitReason = sim.Reason
		if sim.Resolved {
			res.SimExitAt = bars[sim.Idx].Timestamp
			res.ExitLagMinutes = tc.ClosedAt.Sub(res.SimExitAt).Minutes()
			if sim.Reason == "stop" {
				res.BreachClass = ClassifyBreach(bars, sim.Idx, tc.Direction, tc.StopLoss)
			}
		}
		if tc.ExitPrice > 0 && sim.Price > 0 {
			// + = the bracket exit beat the recorded exit, in favorable-direction %.
			res.ExitSlipPct = dirSign(tc.Direction) * (sim.Price - tc.ExitPrice) / tc.ExitPrice * 100
		}
	}

	// ── Excursions over the actual hold ────────────────────────────────────
	// Bounded at the recorded close for the same reason as the fidelity sim:
	// post-close bars are not part of the hold.
	closeIdx := fidelityEnd
	if closeIdx < 0 {
		closeIdx = len(bars) - 1
	}
	res.MAEPct, res.MFEPct = Excursions(bars, entryIdx, closeIdx, tc.Direction, tc.EntryPrice)

	// ── Trailing counterfactual from the actual entry ──────────────────────
	atr, err := r.dailyATR(ctx, tc.Ticker, signalAt)
	if err != nil {
		slog.Warn("study: daily ATR unavailable", "ticker", tc.Ticker, "err", err)
	}
	if atr > 0 {
		trail := SimTrailingExit(bars, entryIdx, tc.Direction, tc.EntryPrice, tc.StopLoss, tc.TakeProfit, atr, r.Cfg.TrailingMult)
		res.TrailReturnPct = pctReturn(tc.Direction, tc.EntryPrice, trail.Price)
		res.TrailComputed = true
	}

	// ── Entry side (needs a signal row) ────────────────────────────────────
	if tc.HasSignal && tc.SignalPrice > 0 {
		ttl := r.sessionEnd(signalAt)
		market := SimMarketEntry(bars, signalAt)
		if market.OK {
			res.ChasePct = dirSign(tc.Direction) * (market.Price - tc.SignalPrice) / tc.SignalPrice * 100
			res.Tactics = append(res.Tactics, r.tacticOutcome(bars, tc, "market", market))
		}
		for _, k := range r.Cfg.LimitATRs {
			name := fmt.Sprintf("limit-%.2fATR", k)
			if atr <= 0 {
				res.Tactics = append(res.Tactics, TacticResult{Name: name})
				continue
			}
			limit := tc.SignalPrice - dirSign(tc.Direction)*k*atr
			fill := SimLimitEntry(bars, signalAt, ttl, tc.Direction, limit)
			res.Tactics = append(res.Tactics, r.tacticOutcome(bars, tc, name, fill))
		}
	}
	return res
}

// tacticOutcome resolves an entry fill through the trade's own bracket.
func (r *Runner) tacticOutcome(bars []data.Candle, tc TradeCase, name string, fill EntryFill) TacticResult {
	t := TacticResult{Name: name, Filled: fill.OK}
	if !fill.OK {
		return t
	}
	t.FillPrice = fill.Price
	exit := SimBracketExit(bars, fill.Idx, tc.Direction, tc.StopLoss, tc.TakeProfit)
	t.ExitReason = exit.Reason
	if exit.Price > 0 {
		t.ReturnPct = pctReturn(tc.Direction, fill.Price, exit.Price)
	}
	return t
}
