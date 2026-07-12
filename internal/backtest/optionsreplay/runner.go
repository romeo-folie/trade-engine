package optionsreplay

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/romeo-folie/trade-engine/internal/data"
	"github.com/romeo-folie/trade-engine/internal/options"
)

// RunnerConfig bundles every knob a runnable backtest needs. The gate thresholds,
// sizing, and exit horizon are the SAME types the live engine is configured with,
// so a backtest and production can be driven from one config.
type RunnerConfig struct {
	Gate     options.GateConfig // strike-selection thresholds (IV-rank ceiling, delta band, ...)
	Replay   Config             // sizing + time-exit (PremiumBudget, MaxLossPerTrade, TimeExitMinutes)
	Assemble AssembleConfig     // opening-range / breakout timing + risk-reward
	IVLookbackDays int          // trailing window for the IV-rank service (<=0 → 252)
	IVMinSamples   int          // fail-closed below this many seeded IV days (<=0 → 40)
	IVRefMinuteET  int          // minutes after ET session open at which the daily ATM IV is sampled (default 15)
	RiskFreeRate   float64      // r for Black-Scholes greeks (default 0)
	DividendYield  float64      // q for Black-Scholes greeks (default 0)
}

// Runner executes a runnable options-ORB backtest end to end: it seeds the IV-rank
// history from historical ATM quotes, assembles breakouts from underlying bars using
// the LIVE ORB range/bracket logic, and replays each through the parity-tested
// Engine.Replay (which re-runs the real gate → sizing → exit → P&L). The runner does
// NOT reimplement any trading decision — it only supplies as-of historical data.
type Runner struct {
	hd     HistoricalOptionData
	chain  *HistChainProvider
	gates  *options.Engine
	replay *Engine
	asm    AssembleConfig
}

// NewRunner wires the runner. It WRITE-THROUGH-seeds the daily IV history for every
// underlying over [from-lookback, to] into ivstore (the IV-rank gate needs history
// before the first backtest day) — fetching only days not already stored, so repeat
// runs are near-instant — then constructs the gate + replay engines reading from that
// same store. Pass options.NewStoreIVStore(db) to persist to option_iv_history, or
// NewMemIVStore() for a DB-free run. earnings may be nil when every underlying is an
// ETF (SPY/QQQ) — single names without an earnings provider fail closed, as live.
func NewRunner(ctx context.Context, hd HistoricalOptionData, ivstore options.IVStore, underlyings []string, from, to time.Time, cfg RunnerConfig, earnings options.EarningsProvider) (*Runner, error) {
	if cfg.IVRefMinuteET <= 0 {
		cfg.IVRefMinuteET = 15
	}
	chain := NewHistChainProvider(hd, cfg.RiskFreeRate, cfg.DividendYield)

	lookback := cfg.IVLookbackDays
	if lookback <= 0 {
		lookback = 252
	}
	// Seed enough calendar days before `from` to clear IVMinSamples trading days.
	ivFrom := from.AddDate(0, 0, -(lookback + 10))
	for _, u := range underlyings {
		if err := chain.SeedIVHistory(ctx, ivstore, u, ivFrom, to, cfg.IVRefMinuteET); err != nil {
			return nil, fmt.Errorf("seed IV history for %s: %w", u, err)
		}
	}
	ivr := options.NewIVRankService(ivstore, cfg.IVLookbackDays, cfg.IVMinSamples)

	gates := options.NewEngine(cfg.Gate, chain, ivr, earnings)
	replay := NewEngine(gates, cfg.Replay)
	return &Runner{hd: hd, chain: chain, gates: gates, replay: replay, asm: cfg.Assemble}, nil
}

// RunUnderlying backtests one underlying over [from, to], returning one SimTrade per
// assembled breakout (accepted or gate-aborted — aborts are reported too).
func (r *Runner) RunUnderlying(ctx context.Context, underlying string, from, to time.Time) ([]SimTrade, error) {
	bars, err := r.hd.UnderlyingBars(ctx, underlying, from, to)
	if err != nil {
		return nil, fmt.Errorf("underlying bars for %s: %w", underlying, err)
	}
	signals := AssembleSignals(underlying, bars, r.asm)
	trades := make([]SimTrade, 0, len(signals))
	for _, sig := range signals {
		trades = append(trades, r.replayOne(ctx, sig))
	}
	return trades, nil
}

// Run backtests every underlying and returns all trades concatenated.
func (r *Runner) Run(ctx context.Context, underlyings []string, from, to time.Time) ([]SimTrade, error) {
	var all []SimTrade
	for _, u := range underlyings {
		ts, err := r.RunUnderlying(ctx, u, from, to)
		if err != nil {
			return nil, err
		}
		all = append(all, ts...)
	}
	return all, nil
}

// replayOne evaluates a single assembled signal. It stamps the as-of context, PEEKS
// the gate to learn the contract the exit walk will trade, fills that contract's
// option-mid path from historical option bars, then defers to Engine.Replay — which
// re-runs the same deterministic gate decision and is the single source of P&L.
func (r *Runner) replayOne(ctx context.Context, sig Signal) SimTrade {
	spot := sig.Path[0].Underlying // breakout bar close = the as-of underlying spot
	aoCtx := WithAsOf(ctx, sig.AsOf, spot)

	dec := r.gates.Select(aoCtx, options.Signal{
		Underlying: sig.Underlying,
		Direction:  sig.Direction,
		AsOf:       sig.AsOf,
	})
	if dec.Accepted && dec.Contract != nil {
		if obars, err := r.hd.OptionBars(ctx, dec.Contract.OCCSymbol, sig.AsOf, sig.SessionEnd); err != nil {
			slog.Warn("optionsreplay: option bars fetch failed; exit path unpriced",
				"occ", dec.Contract.OCCSymbol, "err", err)
		} else {
			fillOptionMids(sig.Path, obars)
		}
	}
	// Hand the peeked decision back so Replay doesn't re-run the gate (and its
	// per-contract quote fetches) a second time — the gate is deterministic per as-of.
	return r.replay.ReplayWithDecision(aoCtx, sig, dec)
}

// fillOptionMids sets each Path bar's OptionMid to the historical option bar Close at
// the same minute (a standard last-trade proxy for the option mid — a full NBBO-mid
// reconstruction would be one quote read per minute per trade). Path bars with no
// matching option bar keep OptionMid=0, so Engine.finish marks them PriceKnown=false
// and Aggregate excludes them from win/loss/P&L rather than fabricating a value.
func fillOptionMids(path []Bar, obars []data.Candle) {
	byMin := make(map[int64]float64, len(obars))
	for _, c := range obars {
		if c.Close > 0 {
			byMin[c.Timestamp.Unix()/60] = c.Close
		}
	}
	for i := range path {
		if mid, ok := byMin[path[i].Time.Unix()/60]; ok {
			path[i].OptionMid = mid
		}
	}
}
