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
	// IVHistoryFloor clamps the IV-seed lookback: no day before this is fetched, since
	// the source has no data there (and a no-data day can't be cached, so it would be
	// re-fetched every run). Zero disables the clamp. Set it to the data source's floor
	// (e.g. data.AlpacaOptionHistoryStart).
	IVHistoryFloor time.Time
}

// clampIVFrom raises ivFrom to the data floor when it would otherwise reach before it.
// A zero floor leaves ivFrom unchanged (clamp disabled).
func clampIVFrom(ivFrom, floor time.Time) time.Time {
	if !floor.IsZero() && ivFrom.Before(floor) {
		return floor
	}
	return ivFrom
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
	// Seed enough calendar days before `from` to clear IVMinSamples trading days, but
	// never before the source's data floor (those days can't have data, so seeding them
	// just re-fetches empty every run).
	ivFrom := clampIVFrom(from.AddDate(0, 0, -(lookback+10)), cfg.IVHistoryFloor)
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
// the same minute (a last-trade proxy for the mid). Minutes with no trade are
// FORWARD-FILLED from the last observed price (marked MidCarried) so the terminal exit
// is still priced+counted; only bars before the first observed price (an entry-side
// gap) keep OptionMid=0 → PriceKnown=false → excluded. A carried price prices the exit
// but does NOT feed the decision loop's OptionPriceKnown (see Replay), so it can't
// drive a spurious time exit.
func fillOptionMids(path []Bar, obars []data.Candle) {
	byMin := make(map[int64]float64, len(obars))
	for _, c := range obars {
		if c.Close > 0 {
			byMin[c.Timestamp.Unix()/60] = c.Close
		}
	}
	// Forward-fill (last-observation-carried-forward): when the option didn't trade at a
	// path minute, carry the last known price rather than leaving it 0. A 0 exit would be
	// dropped from P&L, and the drop rate varies by config (different contracts, different
	// liquidity) — so dropping biases cross-config comparisons. Carrying counts the trade
	// at a stale-but-honest estimate (flagged MidCarried). Bars before the FIRST observed
	// price (an entry-side gap) stay 0 — there's nothing to carry.
	last := 0.0
	for i := range path {
		if mid, ok := byMin[path[i].Time.Unix()/60]; ok {
			path[i].OptionMid = mid
			last = mid
			continue
		}
		if last > 0 {
			path[i].OptionMid = last
			path[i].MidCarried = true
		}
	}
}
