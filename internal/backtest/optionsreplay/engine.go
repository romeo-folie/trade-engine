// Package optionsreplay backtests options-ORB by replaying historical breakouts
// through the SAME live functions the production path uses — options.Engine.Select
// for the gate pipeline, orb.DecideOptionExit for the exit decision,
// orb.SizeOptionContracts for sizing, and orb.ExitPnLM for realized P&L. Because
// the backtest calls those functions rather than reimplementing them, its accept/
// abort and exit decisions are identical to live by construction (parity is a test,
// not a hope — see engine_test.go).
//
// Engine.Replay operates on an already-assembled Signal, so the replay logic is
// provider-agnostic and fully tested against fakes. The runnable end-to-end path is
// built on top of it (see Runner): HistoricalOptionData is the historical I/O seam
// (internal/data.AlpacaOptionsHist reconstructs OPRA option bars/quotes), HistChainProvider
// derives the gate's IV/delta via Black-Scholes, AssembleSignals turns underlying
// bars into breakouts using the live ORB range/bracket functions, and Runner ties
// them together. Drive it with `te backtest orb-options`.
package optionsreplay

import (
	"context"
	"time"

	"github.com/romeo-folie/trade-engine/internal/options"
	"github.com/romeo-folie/trade-engine/internal/orb"
)

// Bar is one point on the underlying's path after entry: the underlying price and
// the chosen option's mid at that time (the mid drives the IV-crush time exit and
// the realized exit price).
type Bar struct {
	Time       time.Time
	Underlying float64
	OptionMid  float64
}

// Signal is one historical breakout to replay. The gate providers wired into the
// options.Engine supply the chain/IV-rank AS OF AsOf; Path is the subsequent
// underlying/option series walked for the exit.
type Signal struct {
	Underlying       string
	Direction        string // "buy" (call) | "sell" (put)
	AsOf             time.Time
	UnderlyingStop   float64 // ORB stop on the underlying
	UnderlyingTarget float64 // ORB target on the underlying
	SessionEnd       time.Time
	Path             []Bar
}

// SimTrade is the outcome of replaying one Signal.
type SimTrade struct {
	Underlying string
	AsOf       time.Time
	Accepted   bool
	Gate       string // aborting gate when !Accepted (or "sizing")
	Reason     string

	OCCSymbol   string
	Contracts   int
	Multiplier  int
	EntryMid    float64
	ExitPrice   float64
	ExitReason  string
	HeldMinutes int
	PnL         float64
	// PriceKnown is false when the exit price fell back to the entry mid because the
	// historical option quote was missing at the exit bar (a data gap — common on
	// fast stop-outs). Such trades' P&L is unreliable and Aggregate excludes them
	// from win/loss/P&L rather than silently counting them as scratches.
	PriceKnown bool
}

// ReasonPathExhausted marks a close forced because the replay Path ran out before
// any exit condition fired — a DATA-COVERAGE artifact, NOT an exit decided by the
// live DecideOptionExit. Kept distinct from "eod" so metrics don't over-count real
// flattens. A well-formed Signal ends its Path at/after SessionEnd, so a genuine
// run should exit via the live function, not here.
const ReasonPathExhausted = "path_exhausted"

// Config holds the sizing/exit knobs (the gate thresholds live in the Engine).
type Config struct {
	PremiumBudget   float64
	MaxLossPerTrade float64
	TimeExitMinutes int
	// ModeledSpreadPct is the full modeled bid/ask width (fraction of mid) applied to
	// the EXIT: live closes the option with a MARKET sell, which receives the bid, so
	// the exit fills a half-spread below the mid. The entry stays at the mid (live
	// places a limit-at-mid, so no spread is paid on entry — this asymmetry is
	// deliberate). 0 disables it (exit at mid — optimistic). On free (bars) data this
	// is the only way the spread cost enters P&L, since there is no real NBBO.
	ModeledSpreadPct float64
}

// Engine replays Signals through the live gate engine + exit logic.
type Engine struct {
	gates *options.Engine
	cfg   Config
}

// NewEngine builds a replay engine over a live options gate engine (constructed
// with historical chain/IV-rank providers for a real run, or fakes in tests).
func NewEngine(gates *options.Engine, cfg Config) *Engine {
	return &Engine{gates: gates, cfg: cfg}
}

// Replay runs one breakout through the gate pipeline, sizes it, and walks the path
// applying the live exit decision, returning the simulated trade. A gate abort or a
// too-small budget yields Accepted=false (still reported, for the abort-by-gate
// stats). A well-formed Path ends at/after SessionEnd so the live exit fires an
// "eod" flatten; if the Path runs out first, the trade is closed under the distinct
// ReasonPathExhausted marker so a data gap isn't miscounted as a real flatten.
func (e *Engine) Replay(ctx context.Context, sig Signal) SimTrade {
	dec := e.gates.Select(ctx, options.Signal{
		Underlying: sig.Underlying,
		Direction:  sig.Direction,
		AsOf:       sig.AsOf,
	})
	return e.ReplayWithDecision(ctx, sig, dec)
}

// ReplayWithDecision replays a breakout using a PRE-COMPUTED gate decision instead of
// running Select itself. The runner peeks the decision to learn the chosen contract
// (so it can fetch that contract's option-mid path) and then hands it back here — the
// gate is deterministic per as-of, so this yields the identical result while running
// the gate (and its per-contract quote fetches) once per signal rather than twice.
func (e *Engine) ReplayWithDecision(ctx context.Context, sig Signal, dec options.Decision) SimTrade {
	out := SimTrade{Underlying: sig.Underlying, AsOf: sig.AsOf}

	if !dec.Accepted || dec.Contract == nil {
		out.Gate = dec.Gate
		out.Reason = dec.Reason
		return out
	}

	contracts := orb.SizeOptionContracts(dec.Mid, dec.Contract.Multiplier, e.cfg.PremiumBudget, e.cfg.MaxLossPerTrade)
	if contracts < 1 {
		out.Gate = "sizing"
		out.Reason = "premium budget too small for one contract"
		return out
	}

	mult := dec.Contract.Multiplier
	if mult <= 0 {
		mult = 100
	}
	out.Accepted = true
	out.OCCSymbol = dec.Contract.OCCSymbol
	out.Contracts = contracts
	out.Multiplier = mult
	out.EntryMid = dec.Mid

	for _, bar := range sig.Path {
		reason, exit := orb.DecideOptionExit(orb.OptionExitInputs{
			Side:               sig.Direction,
			UnderlyingStop:     sig.UnderlyingStop,
			UnderlyingTarget:   sig.UnderlyingTarget,
			CurrentUnderlying:  bar.Underlying,
			EntryOptionPrice:   dec.Mid,
			CurrentOptionPrice: bar.OptionMid,
			OptionPriceKnown:   bar.OptionMid > 0,
			HeldMinutes:        minutesBetween(sig.AsOf, bar.Time),
			TimeExitMinutes:    e.cfg.TimeExitMinutes,
			SessionEnding:      !sig.SessionEnd.IsZero() && !bar.Time.Before(sig.SessionEnd),
		})
		if !exit {
			continue
		}
		return e.finish(out, sig, bar, reason)
	}

	// The Path ran out before any live exit fired — a data-coverage artifact, not a
	// real flatten. Close at the last bar under a DISTINCT reason so it isn't counted
	// as a genuine "eod" exit.
	if n := len(sig.Path); n > 0 {
		return e.finish(out, sig, sig.Path[n-1], ReasonPathExhausted)
	}
	return out // no path at all — accepted but unresolved (degenerate input)
}

// finish records the exit at bar with reason, computing realized P&L on the actual
// contracts via the live ExitPnLM (a long option always closes with the "buy" P&L).
// When the option quote is missing at the bar, the exit price falls back to the
// entry mid (P&L 0) and PriceKnown is set false so Aggregate can exclude it.
func (e *Engine) finish(out SimTrade, sig Signal, bar Bar, reason string) SimTrade {
	exitPrice := bar.OptionMid
	out.PriceKnown = exitPrice > 0
	if !out.PriceKnown {
		exitPrice = out.EntryMid
	} else if e.cfg.ModeledSpreadPct > 0 {
		// The live exit is a MARKET sell → it receives the bid, a half-spread below the
		// mid. Model that cost so the free-data backtest isn't optimistic on fills.
		exitPrice *= 1 - e.cfg.ModeledSpreadPct/2
	}
	out.ExitPrice = exitPrice
	out.ExitReason = reason
	out.HeldMinutes = minutesBetween(sig.AsOf, bar.Time)
	out.PnL = orb.ExitPnLM("buy", out.EntryMid, exitPrice, out.Contracts, out.Multiplier)
	return out
}

func minutesBetween(a, b time.Time) int {
	return int(b.Sub(a).Minutes())
}
