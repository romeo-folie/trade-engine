// Package options implements the options-ORB strike-selection engine: the
// deterministic, fail-closed gate pipeline that turns an ORB breakout on an
// underlying into a specific option contract to buy (issue #412).
//
// The pipeline runs in order and ABORTS THE TRADE on any gate failure or any
// uncomputable input (fail-closed). This inverts the ORB entry-quality filters,
// which fail OPEN — here, missing data means we are blind to the exact risk a
// gate guards (IV crush, slippage), so we must not trade.
package options

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/romeo-folie/trade-engine/internal/execution"
)

// ChainProvider supplies live option-chain snapshots. Implemented by
// *execution.AlpacaBroker (issue #411).
type ChainProvider interface {
	GetOptionChain(ctx context.Context, underlying string, filt execution.OptionChainFilter) ([]execution.OptionSnapshot, error)
}

// IVRankProvider supplies the current IV rank (0–100) for an underlying as of a
// point in time. ok is false when the series is too short / unavailable, which
// makes the IV-Rank gate fail closed. Implemented by the IV pipeline (issue #413).
type IVRankProvider interface {
	IVRank(ctx context.Context, underlying string, asOf time.Time) (rank float64, ok bool)
}

// EarningsProvider reports the next scheduled earnings datetime for a single-name
// underlying. ETFs never consult it. Returns:
//   - (date, true, nil)  when a next earnings date is known;
//   - (zero, false, nil) when the calendar is covered but has nothing imminent (safe pass);
//   - (zero, false, err) when earnings can't be determined — a read failure or no coverage
//     for the symbol — so the gate MUST fail closed rather than trade unchecked.
type EarningsProvider interface {
	NextEarnings(ctx context.Context, underlying string) (time.Time, bool, error)
}

// GateConfig holds the tunable thresholds. Zero values are replaced with the
// documented defaults by DefaultGateConfig / (GateConfig).withDefaults.
type GateConfig struct {
	IVRankCeiling        float64  // abort if IV rank ≥ this (percent); default 70
	MaxDTE               int      // keep contracts with 0 < DTE ≤ this; default 5
	DeltaMin             float64  // |delta| lower bound; default 0.50
	DeltaMax             float64  // |delta| upper bound; default 0.60
	SpreadPctCeiling     float64  // abort if (ask-bid)/mid ≥ this (percent); default 5
	EarningsBlackoutDays int      // abort single names within this many days of earnings; default 3
	ETFUnderlyings       []string // symbols exempt from the earnings gate (e.g. SPY, QQQ)
}

// DefaultGateConfig returns the v1 defaults.
func DefaultGateConfig() GateConfig {
	return GateConfig{
		IVRankCeiling:        70,
		MaxDTE:               5,
		DeltaMin:             0.50,
		DeltaMax:             0.60,
		SpreadPctCeiling:     5,
		EarningsBlackoutDays: 3,
	}
}

func (c GateConfig) withDefaults() GateConfig {
	d := DefaultGateConfig()
	if c.IVRankCeiling == 0 {
		c.IVRankCeiling = d.IVRankCeiling
	}
	if c.MaxDTE == 0 {
		c.MaxDTE = d.MaxDTE
	}
	if c.DeltaMin == 0 {
		c.DeltaMin = d.DeltaMin
	}
	if c.DeltaMax == 0 {
		c.DeltaMax = d.DeltaMax
	}
	if c.SpreadPctCeiling == 0 {
		c.SpreadPctCeiling = d.SpreadPctCeiling
	}
	if c.EarningsBlackoutDays == 0 {
		c.EarningsBlackoutDays = d.EarningsBlackoutDays
	}
	return c
}

// Signal is the ORB breakout the gate engine acts on.
type Signal struct {
	Underlying string    // e.g. "SPY"
	Direction  string    // "buy" (long → call) | "sell" (short → put)
	AsOf       time.Time // the signal time; drives DTE and the earnings window
}

// Gate names, used as Decision.Gate and in logs.
const (
	GateEarnings = "earnings"
	GateIVRank   = "iv_rank"
	GateExpiry   = "expiry"
	GateDelta    = "delta"
	GateSpread   = "spread"
	GateChain    = "chain_error"
	GateInput    = "input"
)

// Decision is the structured result of the pipeline. Accepted is the single
// source of truth: on any problem it is false. The remaining fields exist for
// observability — every gate's observed value is logged whether it passed or
// aborted.
type Decision struct {
	Accepted bool
	Contract *execution.OptionContract // the contract to buy, when Accepted
	Mid      float64                   // chosen contract mid price

	Gate   string // "" when Accepted; else the aborting gate (one of the Gate* consts)
	Reason string // human-readable explanation

	// Observed values (zero when the pipeline aborted before reaching them).
	IVRank    float64
	IVRankOK  bool
	DTE       int
	Delta     float64
	SpreadPct float64

	Err error // non-nil on infrastructure failure (also an abort)
}

func abort(gate, format string, a ...any) Decision {
	return Decision{Accepted: false, Gate: gate, Reason: fmt.Sprintf(format, a...)}
}

// Engine runs the gate pipeline.
type Engine struct {
	cfg      GateConfig
	chain    ChainProvider
	ivr      IVRankProvider
	earnings EarningsProvider
	etfs     map[string]bool
}

// NewEngine builds a gate engine. earnings may be nil only if every configured
// underlying is an ETF; a single-name signal with a nil earnings provider fails
// closed (see Select).
func NewEngine(cfg GateConfig, chain ChainProvider, ivr IVRankProvider, earnings EarningsProvider) *Engine {
	cfg = cfg.withDefaults()
	etfs := make(map[string]bool, len(cfg.ETFUnderlyings))
	for _, s := range cfg.ETFUnderlyings {
		etfs[strings.ToUpper(s)] = true
	}
	return &Engine{cfg: cfg, chain: chain, ivr: ivr, earnings: earnings, etfs: etfs}
}

// Select runs the fail-closed pipeline for a breakout signal and returns the
// chosen contract (Decision.Accepted) or the aborting gate. It never returns an
// error separately from Decision — callers check Decision.Accepted only, so
// there is no error path that could be ignored and accidentally trade.
func (e *Engine) Select(ctx context.Context, sig Signal) Decision {
	underlying := strings.ToUpper(sig.Underlying)
	right, ok := rightForDirection(sig.Direction)
	if !ok {
		return abort(GateInput, "unknown direction %q", sig.Direction)
	}

	// Gate 0 — earnings blackout (single names only; ETFs are earnings-free).
	if !e.etfs[underlying] {
		if e.earnings == nil {
			return abort(GateEarnings, "no earnings provider for single name %s (fail closed)", underlying)
		}
		when, has, eerr := e.earnings.NextEarnings(ctx, underlying)
		if eerr != nil {
			// Can't determine earnings (read failure or no coverage for this single name) →
			// fail closed, same posture as a nil provider. Never trade a single name blind
			// to its earnings.
			return abort(GateEarnings, "earnings lookup for %s failed (fail closed): %v", underlying, eerr)
		}
		if has {
			// Compare on DAY granularity. A store-backed provider returns a DATE (midnight
			// UTC) while sig.AsOf is an intraday instant, so a raw instant compare would see
			// an earnings report scheduled the SAME day as already "past" (midnight < 09:45)
			// and skip the blackout on the very day it matters most. dayFloor both sides so
			// the window is the inclusive day range [T−N … T].
			window := time.Duration(e.cfg.EarningsBlackoutDays) * 24 * time.Hour
			sigDay, whenDay := dayFloor(sig.AsOf), dayFloor(when)
			if !whenDay.Before(sigDay) && whenDay.Sub(sigDay) <= window {
				return abort(GateEarnings, "earnings for %s at %s within %dd blackout",
					underlying, when.Format("2006-01-02"), e.cfg.EarningsBlackoutDays)
			}
		}
		// A known date outside the window, or no scheduled date, passes: absence of
		// a scheduled earnings date means none is imminent. (This differs from the
		// IV-Rank/spread gates: earnings dates are public, so "none" is real signal,
		// not blindness.)
	}

	// Gate 1 — IV rank. Missing rank ⇒ blind to IV-crush risk ⇒ abort.
	rank, rankOK := e.ivr.IVRank(ctx, underlying, sig.AsOf)
	dec := Decision{IVRank: rank, IVRankOK: rankOK}
	if !rankOK {
		return decorate(dec, abort(GateIVRank, "IV rank unavailable for %s (fail closed)", underlying))
	}
	if rank >= e.cfg.IVRankCeiling {
		return decorate(dec, abort(GateIVRank, "IV rank %.1f ≥ ceiling %.1f (IV-crush risk)", rank, e.cfg.IVRankCeiling))
	}

	// Gate 2 — expiry: fetch the chain narrowed to the DTE window and right.
	loExpiry := dayFloor(sig.AsOf).AddDate(0, 0, 1)            // DTE ≥ 1 (exclude 0DTE)
	hiExpiry := dayFloor(sig.AsOf).AddDate(0, 0, e.cfg.MaxDTE) // DTE ≤ MaxDTE
	snaps, err := e.chain.GetOptionChain(ctx, underlying, execution.OptionChainFilter{
		Right:     right,
		ExpiryGTE: loExpiry,
		ExpiryLTE: hiExpiry,
	})
	if err != nil {
		d := decorate(dec, abort(GateChain, "chain fetch for %s failed: %v", underlying, err))
		d.Err = err
		return d
	}

	// Gate 3 — delta: among contracts within the DTE window whose |delta| is in
	// band, pick the one closest to the band midpoint. Null delta ⇒ skip (can't
	// evaluate); if nothing qualifies, abort.
	bandMid := (e.cfg.DeltaMin + e.cfg.DeltaMax) / 2
	var best *execution.OptionSnapshot
	var bestContract execution.OptionContract
	var bestDist, bestDelta float64
	var bestDTE int
	sawContract := false
	for i := range snaps {
		s := snaps[i]
		oc, perr := execution.ParseOCCSymbol(s.OCCSymbol)
		if perr != nil {
			continue
		}
		dte := dteDays(sig.AsOf, oc.Expiry)
		if dte < 1 || dte > e.cfg.MaxDTE {
			continue
		}
		sawContract = true
		if s.Delta == nil {
			continue
		}
		ad := math.Abs(*s.Delta)
		if ad < e.cfg.DeltaMin || ad > e.cfg.DeltaMax {
			continue
		}
		dist := math.Abs(ad - bandMid)
		if best == nil || dist < bestDist {
			snap := s
			best = &snap
			bestContract = oc
			bestDist = dist
			bestDelta = *s.Delta
			bestDTE = dte
		}
	}
	if !sawContract {
		return decorate(dec, abort(GateExpiry, "no %s contracts for %s within %d DTE", right, underlying, e.cfg.MaxDTE))
	}
	if best == nil {
		return decorate(dec, abort(GateDelta, "no %s contract with |delta| in [%.2f,%.2f]", right, e.cfg.DeltaMin, e.cfg.DeltaMax))
	}
	dec.Delta = bestDelta
	dec.DTE = bestDTE

	// Gate 4 — spread on the chosen contract.
	mid := best.Mid()
	if mid <= 0 {
		return decorate(dec, abort(GateSpread, "chosen %s has no valid mid (bid=%.2f ask=%.2f)", bestContract.OCCSymbol, best.Bid, best.Ask))
	}
	// A crossed quote (ask < bid) is bad/stale data — common near the open, exactly
	// when ORB fires. Abort (fail closed) rather than let the resulting negative
	// spread slip under the ceiling.
	if best.Ask < best.Bid {
		return decorate(dec, abort(GateSpread, "chosen %s has crossed quote (bid=%.2f > ask=%.2f)", bestContract.OCCSymbol, best.Bid, best.Ask))
	}
	spreadPct := (best.Ask - best.Bid) / mid * 100
	dec.SpreadPct = spreadPct
	if spreadPct >= e.cfg.SpreadPctCeiling {
		return decorate(dec, abort(GateSpread, "%s spread %.2f%% ≥ ceiling %.2f%% (slippage risk)", bestContract.OCCSymbol, spreadPct, e.cfg.SpreadPctCeiling))
	}

	// Accept.
	dec.Accepted = true
	dec.Contract = &bestContract
	dec.Mid = mid
	return dec
}

// decorate copies the observed values gathered so far onto an abort decision so
// the log line carries what we knew at the point of abort.
func decorate(observed, ab Decision) Decision {
	ab.IVRank = observed.IVRank
	ab.IVRankOK = observed.IVRankOK
	ab.DTE = observed.DTE
	ab.Delta = observed.Delta
	ab.SpreadPct = observed.SpreadPct
	return ab
}

// rightForDirection maps an ORB side to the option right and chain filter value.
func rightForDirection(direction string) (filter string, ok bool) {
	switch strings.ToLower(direction) {
	case "buy", "long":
		return "call", true
	case "sell", "short":
		return "put", true
	}
	return "", false
}

func dayFloor(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

// dteDays is whole calendar days from asOf's date to expiry's date.
func dteDays(asOf, expiry time.Time) int {
	return int(dayFloor(expiry).Sub(dayFloor(asOf)).Hours() / 24)
}
