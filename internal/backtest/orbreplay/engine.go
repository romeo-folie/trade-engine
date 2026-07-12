// Package orbreplay is a purpose-built intraday replay engine for the forex/CFD
// Opening Range Breakout strategy. Unlike the LLM-signal backtest in internal/backtest,
// it runs the actual ORB rules — it reuses internal/orb's OpeningRange and
// ComputeForexBracket for entry/stop/target/sizing and orb.ResolveExitFromCandles for
// exit resolution, so a replay reflects the live rules rather than a re-implementation.
//
// It needs no broker and no LLM. It models commission and bid/ask spread explicitly,
// because broker-truth (PR #359) showed commissions ≈ the entire ORB edge — a
// cost-blind backtest would repeat the −16-vs-−645 illusion.
package orbreplay

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/data"
	"github.com/romeo-folie/trade-engine/internal/indicators"
	"github.com/romeo-folie/trade-engine/internal/orb"
)

// Variant is a named rule set so experiments are reproducible. baseline, time-exit and
// range-filter are wired; confirmed/adaptive land with their live implementations later.
type Variant struct {
	Name              string
	MaxHoldMinutes    int  // 0 = hold until the bracket resolves (baseline)
	ApplyEntryFilters bool // apply the Phase 2 entry-quality gates (ATR range + spread/risk)
	ApplyConfirmation bool // require per-instrument breakout confirmation (Phase 3) instead of a touch
	ApplyDirection    bool // apply per-instrument direction eligibility (Phase 3): trend-aligned side only
	ApplyAdaptiveExit bool // apply per-instrument adaptive exits (Phase 4): break-even / partial / trailing
	ApplyRetest       bool // apply per-instrument retest entry (Phase 6) when BreakoutConfirmation is retest_*
}

// entryWindowMinutes mirrors the live cleanup cutoff: the live scheduler cancels any
// unfilled entry order 60 minutes after session open (sessionCleanupMinutes). A breakout
// after that would never fill live, so the replay must not count it as a trade.
const entryWindowMinutes = 60

// Baseline, TimeExit and RangeFilter are the built-in variants. range-filter is
// time-exit plus the Phase 2 entry-quality gates — the ATR-normalised range filter AND
// the spread/risk filter — using each instrument's configured bounds, the same gates the
// live scheduler applies.
func Baseline() Variant { return Variant{Name: "baseline", MaxHoldMinutes: 0} }
func TimeExit(maxHold int) Variant {
	return Variant{Name: "time-exit", MaxHoldMinutes: maxHold}
}
func RangeFilter(maxHold int) Variant {
	return Variant{Name: "range-filter", MaxHoldMinutes: maxHold, ApplyEntryFilters: true}
}

// Confirmed is range-filter plus per-instrument breakout confirmation (Phase 3): the
// entry waits for the configured BreakoutConfirmation (close_1m/close_3m) instead of
// firing on a touch. Mirrors the live breakout-watch path.
// Confirmed is the full Phase 3 variant: range-filter + per-instrument breakout
// confirmation + direction eligibility (each instrument-gated, so an instrument that
// configures none of them behaves like time-exit).
func Confirmed(maxHold int) Variant {
	return Variant{Name: "confirmed", MaxHoldMinutes: maxHold, ApplyEntryFilters: true, ApplyConfirmation: true, ApplyDirection: true}
}

// Adaptive is the full ladder: confirmed entries plus per-instrument adaptive exit
// management (break-even stop moves, partial take-profit, ATR trailing). Each piece is
// instrument-gated, so an instrument that configures no adaptive fields behaves like
// confirmed.
func Adaptive(maxHold int) Variant {
	v := Confirmed(maxHold)
	v.Name = "adaptive"
	v.ApplyAdaptiveExit = true
	return v
}

// Retest is range-filter plus the Phase 6 retest entry: instead of a stop-touch or a close
// confirmation, wait for the breakout to pull back to the level and bounce. Instrument-gated on a
// retest_* BreakoutConfirmation (so an instrument that configures neither behaves like time-exit).
// Also applies direction eligibility, matching Confirmed.
func Retest(maxHold int) Variant {
	return Variant{Name: "retest", MaxHoldMinutes: maxHold, ApplyEntryFilters: true, ApplyDirection: true, ApplyRetest: true}
}

// CostModel captures the trading frictions a forex/CFD ORB trade pays. Spread is the
// full bid/ask spread in price units; half is applied on entry and half on exit (a buy
// fills at the ask, exits at the bid). CommissionPerSide is charged on both fills.
type CostModel struct {
	Spread            float64 // full bid/ask spread in price units (e.g. 0.00008 for EUR/USD)
	CommissionPerSide float64 // currency per fill (a round trip pays 2×)
}

// SimTrade is one simulated ORB trade.
type SimTrade struct {
	Ticker     string
	Side       string // "buy" | "sell"
	EntryTime  time.Time
	ExitTime   time.Time
	EntryPrice float64 // fill incl. half-spread
	ExitPrice  float64 // fill incl. half-spread
	Qty        int
	ExitType   string  // "tp" | "stop" | "time_exit" | "end_of_data"
	GrossPnL   float64 // before commission
	Commission float64
	NetPnL     float64 // gross − commission
	R          float64 // gross PnL in planned-risk multiples
	HoldDur    time.Duration
}

// Engine replays ORB rules over historical candles.
type Engine struct {
	cfg   config.ForexORBConfig
	data  data.MarketDataProvider
	costs map[string]CostModel // by symbol; falls back to defaultCost when absent
	// holdHorizon bounds how long a baseline trade is followed before being marked
	// end_of_data. Caps per-day candle fetches while still capturing same/next-day holds.
	holdHorizon time.Duration
}

// NewEngine builds a replay engine. portfolioValue sizing uses the same MaxRiskPct
// path as live (via ComputeForexBracket). holdHorizon defaults to 48h when zero.
func NewEngine(cfg config.ForexORBConfig, provider data.MarketDataProvider, costs map[string]CostModel, holdHorizon time.Duration) *Engine {
	if holdHorizon <= 0 {
		holdHorizon = 48 * time.Hour
	}
	return &Engine{cfg: cfg, data: provider, costs: costs, holdHorizon: holdHorizon}
}

// defaultCost is used for instruments without an explicit CostModel. Conservative
// round-trip assumptions (~$4) consistent with the broker-statement reconciliation.
var defaultCost = CostModel{Spread: 0.0001, CommissionPerSide: 2.0}

func (e *Engine) costFor(symbol string) CostModel {
	if c, ok := e.costs[symbol]; ok {
		return c
	}
	return defaultCost
}

// commissionRiskOK is the backtest twin of the live passesCommission gate: reject a bracket
// whose estimated round-trip commission is too large relative to its planned dollar risk,
// fills×CommissionPerSide / (|entry−stop|×qty) > MaxCommissionToRisk. Uses the engine's cost
// model (the commission actually charged by resolveTrade) so the gate and the P&L agree.
// An adaptive variant with a configured partial plans THREE fills (entry + partial exit +
// runner exit — resolveTrade's (1+exitFills) charge), not two. Disabled when
// MaxCommissionToRisk is 0 or the bracket has no risk/qty to measure.
func (e *Engine) commissionRiskOK(v Variant, inst config.Instrument, p orb.BracketParams) bool {
	if inst.MaxCommissionToRisk <= 0 {
		return true
	}
	riskDollars := absf(p.EntryStop-p.StopLoss) * float64(p.Qty)
	if riskDollars <= 0 {
		return true
	}
	fills := 2.0
	if v.ApplyAdaptiveExit && inst.PartialTakeProfitR > 0 && inst.PartialFraction > 0 {
		fills = 3.0
	}
	return fills*e.costFor(inst.Symbol).CommissionPerSide/riskDollars <= inst.MaxCommissionToRisk
}

// Run replays the variant over [from, to] for the given instruments and returns the
// per-trade results. One opening-range session is simulated per UTC calendar day per
// instrument; weekend/holiday days simply yield no candles and are skipped.
func (e *Engine) Run(ctx context.Context, v Variant, from, to time.Time, instruments []config.Instrument, portfolioValue float64) ([]SimTrade, error) {
	openMin, ok := sessionOpenMinutes(e.cfg.Session)
	if !ok {
		return nil, fmt.Errorf("orbreplay: unsupported session %q (use \"london\" or \"newyork\"; run them separately rather than \"both\")", e.cfg.Session)
	}
	rangeMin := e.cfg.RangeMinutes
	if rangeMin <= 0 {
		rangeMin = 15
	}

	// openUntil tracks, per instrument, the exit time of the last trade still running.
	// The live scheduler skips placing brackets on a ticker that still holds an unexited
	// fill (tickersWithOpenFilledTrades); the replay mirrors that so baseline / long
	// max-hold runs don't stack a position live trading would suppress.
	openUntil := make(map[string]time.Time)

	var trades []SimTrade
	from = from.UTC()
	to = to.UTC()
	for day := truncateDay(from); !day.After(to); day = day.AddDate(0, 0, 1) {
		sessionOpen := day.Add(time.Duration(openMin) * time.Minute)
		if sessionOpen.Before(from) || sessionOpen.After(to) {
			continue
		}
		// Bracket placement happens after the range locks (≈ rangeEnd); skip the session
		// if the prior trade is still open at that point.
		placement := sessionOpen.Add(time.Duration(rangeMin) * time.Minute)
		for _, inst := range instruments {
			if exit, open := openUntil[inst.Symbol]; open && exit.After(placement) {
				continue
			}
			t, err := e.simulateSession(ctx, v, inst, sessionOpen, rangeMin, portfolioValue)
			if err != nil {
				return nil, err
			}
			for _, tr := range t {
				trades = append(trades, tr)
				openUntil[inst.Symbol] = tr.ExitTime
			}
		}
	}
	return trades, nil
}

// simulateSession replays one instrument's opening-range session. Returns 0..2 trades
// (a side fires only if its breakout triggers; the opposing side is OCO-cancelled).
func (e *Engine) simulateSession(ctx context.Context, v Variant, inst config.Instrument, sessionOpen time.Time, rangeMin int, portfolioValue float64) ([]SimTrade, error) {
	candles, err := e.data.FetchCandles(ctx, inst.Symbol, data.Timeframe1Min, sessionOpen, sessionOpen.Add(e.holdHorizon))
	if err != nil {
		return nil, fmt.Errorf("orbreplay: fetch %s @ %s: %w", inst.Symbol, sessionOpen.Format(time.RFC3339), err)
	}
	sort.Slice(candles, func(i, j int) bool { return candles[i].Timestamp.Before(candles[j].Timestamp) })

	rangeEnd := sessionOpen.Add(time.Duration(rangeMin) * time.Minute)
	rb := orb.NewRangeBuffer(inst.Symbol, rangeMin)
	var watch []data.Candle
	for _, c := range candles {
		if c.Timestamp.Before(rangeEnd) {
			rb.Add(c)
		} else {
			watch = append(watch, c)
		}
	}
	or := rb.Range()
	if or == nil || len(watch) == 0 {
		return nil, nil // not enough data this day (weekend/holiday/gap)
	}

	// Near-flat range filter: the live scheduler skips an instrument whose opening range
	// is narrower than 2× the pip buffer (a noisy, easily-wicked range). Mirror it.
	if inst.PipBuffer > 0 && (or.High-or.Low) < inst.PipBuffer*2 {
		return nil, nil
	}

	// Entry-quality gate 1 — ATR range filter: mirror the live passesRangeQuality gate
	// using the instrument's configured bounds. Fails open when ATR is unavailable.
	if v.ApplyEntryFilters && (inst.MinRangeATR > 0 || inst.MaxRangeATR > 0) {
		if atr := e.sessionATR(ctx, inst.Symbol, sessionOpen); atr > 0 {
			ratio := (or.High - or.Low) / atr
			if (inst.MinRangeATR > 0 && ratio < inst.MinRangeATR) || (inst.MaxRangeATR > 0 && ratio > inst.MaxRangeATR) {
				return nil, nil // outside the configured range_atr_ratio band
			}
		}
	}

	buy := orb.ComputeForexBracket(or, "buy", portfolioValue, e.cfg, inst)
	sell := orb.ComputeForexBracket(or, "sell", portfolioValue, e.cfg, inst)

	// Entry-quality gate 2 — spread/risk filter: mirror the live passesSpread gate. The
	// replay's spread is the static CostModel.Spread; risk is the bracket's stop distance
	// (identical for buy and sell). spread / risk > max → skip, as live would.
	if v.ApplyEntryFilters && inst.MaxSpreadToRisk > 0 {
		if risk := absf(buy.EntryStop - buy.StopLoss); risk > 0 {
			if e.costFor(inst.Symbol).Spread/risk > inst.MaxSpreadToRisk {
				return nil, nil // spread too wide relative to planned risk
			}
		}
	}

	// Entry-quality gate 3 — commission/dollar-risk filter: mirror the live passesCommission
	// gate. The replay charges CostModel.CommissionPerSide per fill regardless of size, so at
	// notional-capped qtys the round trip can exceed the entire planned dollar risk. Checked
	// PER SIDE like live: the sides share the risk magnitude but the notional cap can round
	// their qtys to different lots, so on the boundary one side can pass while the other
	// fails — zero the failing side (skipped below), don't drop the whole session.
	if v.ApplyEntryFilters {
		if !e.commissionRiskOK(v, inst, buy) {
			buy.Qty = 0
		}
		if !e.commissionRiskOK(v, inst, sell) {
			sell.Qty = 0
		}
		if buy.Qty == 0 && sell.Qty == 0 {
			return nil, nil // commission too large relative to planned dollar risk on both sides
		}
	}

	// Direction eligibility (Phase 3): place only the trend-aligned side, mirroring the live
	// directionEligibility gate. Zeroing a side's Qty makes the breakout/confirmation logic
	// below skip it. Fails open (both sides kept) when the trend EMA is unavailable.
	if v.ApplyDirection && inst.DirectionEligibility {
		if longOK, shortOK, ok := e.sessionTrend(ctx, inst.Symbol, sessionOpen); ok {
			if !longOK {
				buy.Qty = 0
			}
			if !shortOK {
				sell.Qty = 0
			}
			if buy.Qty == 0 && sell.Qty == 0 {
				return nil, nil // neither direction trend-eligible
			}
		}
	}

	// Resolve the entry within the live entry window (orders are cancelled
	// entryWindowMinutes after session open). The opposing side is OCO-cancelled, so only
	// the side that triggers first produces a trade.
	entryDeadline := sessionOpen.Add(entryWindowMinutes * time.Minute)
	inWindow := candlesBefore(watch, entryDeadline)

	var side string
	var entryIdx int
	var entryMid float64
	var retestParams *orb.BracketParams // set by the retest branch; overrides the range bracket
	if v.ApplyRetest && orb.IsRetestMode(inst.BreakoutConfirmation) {
		// Retest entry (Phase 6): wait for the breakout to pull back to the level and bounce.
		// Direction eligibility disables a zeroed side by nulling its trigger, same as elsewhere.
		buyTrig, sellTrig := buy.EntryStop, sell.EntryStop
		if buy.Qty == 0 {
			buyTrig = 0
		}
		if sell.Qty == 0 {
			sellTrig = 0
		}
		tol := inst.RetestTolerance
		if tol <= 0 {
			tol = 0.25 // default: pull back within 25% of the range width of the level
		}
		timeoutBars := inst.RetestTimeoutMinutes
		if timeoutBars <= 0 {
			timeoutBars = 30
		}
		r := orb.DetectRetest(inWindow, buyTrig, sellTrig, or.High, or.Low, inst.PipBuffer, tol, timeoutBars)
		if !r.OK {
			return nil, nil // no retest entry within the window
		}
		side = r.Side
		entryIdx = r.EntryIdx
		entryMid = r.EntryPrice
		// Stop placement: retest_swing → just beyond the retest swing (tighter); retest_range →
		// the opposite side of the opening range (the range bracket's stop).
		var stopLevel float64
		if inst.BreakoutConfirmation == orb.ConfirmRetestSwing {
			if side == "buy" {
				stopLevel = r.SwingLow - inst.PipBuffer
			} else {
				stopLevel = r.SwingHigh + inst.PipBuffer
			}
		} else { // retest_range
			if side == "buy" {
				stopLevel = buy.StopLoss
			} else {
				stopLevel = sell.StopLoss
			}
		}
		rp := orb.ComputeRetestBracket(inst.Symbol, side, entryMid, stopLevel, portfolioValue, e.cfg, inst)
		// Re-apply the spread/risk gate against the RETEST bracket's risk: retest_swing's stop is
		// much tighter than the range stop, so spread/retest-risk can exceed the cap even when
		// spread/range-risk (checked above) did not — the gate must see the actual entry's risk.
		if v.ApplyEntryFilters && inst.MaxSpreadToRisk > 0 {
			if risk := absf(rp.EntryStop - rp.StopLoss); risk > 0 {
				if e.costFor(inst.Symbol).Spread/risk > inst.MaxSpreadToRisk {
					return nil, nil // spread too wide relative to the retest stop distance
				}
			}
		}
		// Same for the commission gate: the retest bracket's dollar risk (tighter stop,
		// re-sized qty) is what the round-trip commission is actually paid against. The
		// retest entry is single-sided, so a failure here does skip the session.
		if v.ApplyEntryFilters && !e.commissionRiskOK(v, inst, rp) {
			return nil, nil // commission too large relative to the retest dollar risk
		}
		retestParams = &rp
	} else if v.ApplyConfirmation && orb.RequiresConfirmation(inst.BreakoutConfirmation) {
		// Confirmation is a GATE; the entry is market-on-confirmation, matching live (which
		// places a MARKET bracket once a close confirms). So the fill is the NEXT bar's open
		// (≈ the price at the next scheduler tick), not the breakout level — a later, market
		// fill in exchange for filtering wicks. No resting stop, so no pullback re-fill.
		// Respect direction eligibility: a zeroed side is disabled (trigger 0).
		buyTrig, sellTrig := buy.EntryStop, sell.EntryStop
		if buy.Qty == 0 {
			buyTrig = 0
		}
		if sell.Qty == 0 {
			sellTrig = 0
		}
		cSide, confIdx, ok := orb.DetectConfirmation(inst.BreakoutConfirmation, inWindow, buyTrig, sellTrig)
		if !ok {
			return nil, nil // breakout never confirmed within the entry window
		}
		entryIdx = confIdx + 1
		if entryIdx >= len(watch) {
			return nil, nil // confirmed on the last available bar — no bar to fill on
		}
		side = cSide
		entryMid = watch[entryIdx].Open
	} else {
		var ok bool
		side, entryIdx, ok = orb.FirstBreakout(inWindow, buy, sell, entryDeadline)
		if !ok {
			return nil, nil // range never broke within the entry window
		}
		// A stop entry fills at the breakout level.
		if side == "buy" {
			entryMid = buy.EntryStop
		} else {
			entryMid = sell.EntryStop
		}
	}

	params := buy
	if side == "sell" {
		params = sell
	}
	if retestParams != nil {
		params = *retestParams // retest overrides entry / stop / target / qty
	}
	if params.Qty == 0 {
		return nil, nil // min lot exceeds the position cap for this instrument
	}

	// ATR for adaptive trailing (only fetched when the variant + instrument need it).
	var atr float64
	if v.ApplyAdaptiveExit && inst.TrailATRMult > 0 {
		atr = e.sessionATR(ctx, inst.Symbol, sessionOpen)
	}

	// Time-exit deadline is PER-INSTRUMENT, mirroring the live EffectiveMaxHoldMinutes:
	// the variant only gates whether time exits apply, the minutes come from config (the
	// variant's own value is the fallback for an instrument with no configured max-hold).
	maxHoldMin := v.MaxHoldMinutes
	if v.MaxHoldMinutes > 0 {
		if eff := effectiveMaxHold(inst, e.cfg); eff > 0 {
			maxHoldMin = eff
		}
	}

	trade := e.resolveTrade(v, inst, params, entryMid, atr, maxHoldMin, watch, entryIdx)
	if trade == nil {
		return nil, nil
	}
	return []SimTrade{*trade}, nil
}

// effectiveMaxHold mirrors config.EffectiveMaxHoldMinutes for the (possibly CLI-overridden)
// instrument passed to the engine: the instrument's own max-hold when set, otherwise the
// strategy-level fallback. Keeps the replay's per-instrument deadline aligned with live.
func effectiveMaxHold(inst config.Instrument, cfg config.ForexORBConfig) int {
	if inst.MaxHoldMinutes > 0 {
		return inst.MaxHoldMinutes
	}
	return cfg.MaxHoldMinutes
}

// candlesBefore returns the prefix of watch whose timestamps precede the deadline. Index
// positions are preserved (it's a prefix), so an index into the result is also an index
// into watch.
func candlesBefore(watch []data.Candle, deadline time.Time) []data.Candle {
	for i, c := range watch {
		if !c.Timestamp.Before(deadline) {
			return watch[:i]
		}
	}
	return watch
}

// resolveTrade simulates entry fill and exit for a triggered breakout starting at
// watch[entryIdx]. Spread is applied half on entry, half on exit; commission per side.
//
// The entry bar is first checked for an ADVERSE same-bar stop-out: if the breakout
// bar's range also spans the stop loss, the trade is stopped on that bar. Intra-bar
// order is unknown, so this assumes the adverse fill (conservative — it avoids the
// optimistic bias of holding a same-bar stop into a later recovery). A favorable
// same-bar move to TP is NOT credited; it resolves on subsequent bars instead.
func (e *Engine) resolveTrade(v Variant, inst config.Instrument, p orb.BracketParams, entryMid, atr float64, maxHoldMin int, watch []data.Candle, entryIdx int) *SimTrade {
	cost := e.costFor(inst.Symbol)
	half := cost.Spread / 2

	entryBar := watch[entryIdx]
	entryTime := entryBar.Timestamp
	// Fill at entryMid (the breakout level for a stop touch, the next-bar open for a
	// market-on-confirmation entry), paying the spread (buy at ask, sell at bid).
	entryFill := entryMid
	if p.Side == "buy" {
		entryFill += half
	} else {
		entryFill -= half
	}

	var exitType string
	var exitTime time.Time
	var gross, exitFill float64 // exitFill 0 for adaptive (blended across partial + runner)
	exitFills := 1              // entry + this many exit fills determines commission

	switch {
	case entryBarStopOut(p, entryBar):
		// Adverse same-bar stop — adaptive management never gets a chance.
		exitType, exitTime, exitFill = "stop", entryTime, exitWithSpread(p.Side, p.StopLoss, half)
		gross = pnl(p.Side, entryFill, exitFill, p.Qty)
	case v.ApplyAdaptiveExit && instHasAdaptiveExits(inst):
		exitType, gross, exitTime, exitFills = e.resolveAdaptiveExit(maxHoldMin, p, inst, entryFill, half, atr, watch[entryIdx+1:], entryTime)
		// Blended effective exit price (partial + runner) for display: entry + signed
		// average PnL per unit, so ExitPrice isn't a misleading 0 for adaptive trades.
		if p.Qty > 0 {
			if p.Side == "buy" {
				exitFill = entryFill + gross/float64(p.Qty)
			} else {
				exitFill = entryFill - gross/float64(p.Qty)
			}
		}
	default:
		et, exitMid, ts := e.resolveExit(maxHoldMin, p, watch[entryIdx+1:], entryTime)
		exitType, exitTime, exitFill = et, ts, exitWithSpread(p.Side, exitMid, half)
		gross = pnl(p.Side, entryFill, exitFill, p.Qty)
	}

	commission := float64(1+exitFills) * cost.CommissionPerSide
	riskPerUnit := absf(p.EntryStop - p.StopLoss)
	var r float64
	if riskPerUnit > 0 {
		r = gross / (riskPerUnit * float64(p.Qty))
	}
	return &SimTrade{
		Ticker:     inst.Symbol,
		Side:       p.Side,
		EntryTime:  entryTime,
		ExitTime:   exitTime,
		EntryPrice: entryFill,
		ExitPrice:  exitFill,
		Qty:        p.Qty,
		ExitType:   exitType,
		GrossPnL:   gross,
		Commission: commission,
		NetPnL:     gross - commission,
		R:          r,
		HoldDur:    exitTime.Sub(entryTime),
	}
}

// exitWithSpread applies the bid/ask spread to an exit price: a buy exits by selling at
// the bid (−half), a sell exits by buying at the ask (+half).
func exitWithSpread(side string, mid, half float64) float64 {
	if side == "buy" {
		return mid - half
	}
	return mid + half
}

// instHasAdaptiveExits reports whether any adaptive-exit feature is configured.
func instHasAdaptiveExits(inst config.Instrument) bool {
	return inst.BreakEvenR > 0 || inst.TrailATRMult > 0 || (inst.PartialTakeProfitR > 0 && inst.PartialFraction > 0)
}

// resolveExit returns the exit type, mid price and time. For the time-exit variant it
// honours a max-hold: if a TP/SL is touched before the deadline it is taken, otherwise
// the position is flattened at the first bar at/after the deadline (market = that bar's
// open). Baseline holds until a leg is touched or data runs out (end_of_data).
func (e *Engine) resolveExit(maxHoldMin int, p orb.BracketParams, rest []data.Candle, entryTime time.Time) (string, float64, time.Time) {
	if len(rest) == 0 {
		return "end_of_data", p.EntryStop, entryTime
	}
	if maxHoldMin > 0 {
		deadline := entryTime.Add(time.Duration(maxHoldMin) * time.Minute)
		var preDeadline []data.Candle
		for _, c := range rest {
			if c.Timestamp.Before(deadline) {
				preDeadline = append(preDeadline, c)
			}
		}
		if et, ep, ts, found := orb.ResolveExitFromCandles(p.Side, p.StopLoss, p.TakeProfit, preDeadline); found {
			return et, ep, ts
		}
		// Time exit: flatten at the first bar at/after the deadline (market order ≈ open).
		for _, c := range rest {
			if !c.Timestamp.Before(deadline) {
				return "time_exit", c.Open, c.Timestamp
			}
		}
		// Deadline beyond available data — fall through to end_of_data below.
	} else {
		if et, ep, ts, found := orb.ResolveExitFromCandles(p.Side, p.StopLoss, p.TakeProfit, rest); found {
			return et, ep, ts
		}
	}
	last := rest[len(rest)-1]
	return "end_of_data", last.Close, last.Timestamp
}

// entryBarStopOut reports whether the breakout bar's own range also spans the stop
// loss (a buy's low ≤ stop, a sell's high ≥ stop) — i.e. the bar that entered the
// trade also could have stopped it. Treated as an adverse same-bar stop-out.
func entryBarStopOut(p orb.BracketParams, bar data.Candle) bool {
	if p.StopLoss <= 0 {
		return false
	}
	if p.Side == "buy" {
		return bar.Low <= p.StopLoss
	}
	return bar.High >= p.StopLoss
}

// ── small helpers ──────────────────────────────────────────────────────────────

// sessionATR fetches a thin lookback of the configured ATR timeframe ending at the
// session open and returns the ATR (0 when data is unavailable or too shallow — the
// same fail-open convention as the live filter). Uses ComputeATR so the backtest's ATR
// matches the live engine's exactly.
func (e *Engine) sessionATR(ctx context.Context, symbol string, sessionOpen time.Time) float64 {
	period := e.cfg.ATRPeriod
	if period <= 0 {
		period = 14
	}
	tf := data.Timeframe(e.cfg.ATRTimeframe)
	if tf == "" {
		tf = data.Timeframe1Hour
	}
	// Pad the intraday lookback by 72h to clear the forex weekend gap, matching the live
	// instrumentATR — otherwise a Monday session's ATR window returns too few bars and
	// ComputeATR yields 0 (filter fails open), diverging from live.
	lookback := time.Duration(period+2) * atrBarDuration(tf)
	if atrBarDuration(tf) < 24*time.Hour {
		lookback += 72 * time.Hour
	}
	from := sessionOpen.Add(-lookback)
	candles, err := e.data.FetchCandles(ctx, symbol, tf, from, sessionOpen)
	if err != nil {
		return 0
	}
	return indicators.ComputeATR(candles, period)
}

// sessionTrend mirrors the live directionEligibility gate: it computes the trend EMA from
// the configured trend timeframe ending at the session open and reports which sides are
// aligned (long when the latest close ≥ EMA, short when ≤). ok=false on thin data so the
// caller leaves both sides eligible — identical to live.
func (e *Engine) sessionTrend(ctx context.Context, symbol string, sessionOpen time.Time) (longOK, shortOK, ok bool) {
	period := e.cfg.TrendEMAPeriod
	if period <= 0 {
		period = 20
	}
	tf := data.Timeframe(e.cfg.TrendTimeframe)
	if tf == "" {
		tf = data.Timeframe1Hour
	}
	lookback := time.Duration(period+2) * atrBarDuration(tf)
	if atrBarDuration(tf) < 24*time.Hour {
		lookback += 72 * time.Hour // clear the forex weekend gap, as sessionATR does
	}
	candles, err := e.data.FetchCandles(ctx, symbol, tf, sessionOpen.Add(-lookback), sessionOpen)
	if err != nil || len(candles) < period {
		return false, false, false
	}
	emaVal := indicators.ComputeEMA(candles, period)
	if emaVal <= 0 {
		return false, false, false
	}
	last := candles[len(candles)-1].Close
	return last >= emaVal, last <= emaVal, true
}

// atrBarDuration is the wall-clock span of one bar for the supported ATR timeframes.
func atrBarDuration(tf data.Timeframe) time.Duration {
	switch tf {
	case data.Timeframe1Min:
		return time.Minute
	case data.Timeframe5Min:
		return 5 * time.Minute
	case data.Timeframe15Min:
		return 15 * time.Minute
	case data.Timeframe1Hour:
		return time.Hour
	case data.Timeframe4Hour:
		return 4 * time.Hour
	case data.Timeframe1Day:
		return 24 * time.Hour
	default:
		return time.Hour
	}
}

// sessionOpenMinutes mirrors the live scheduler: London 08:00 UTC, New York 13:30 UTC.
// "both" is rejected: the engine runs one session open per day, so silently mapping it
// to London would simulate only half the sessions and understate the trade count.
func sessionOpenMinutes(session string) (int, bool) {
	switch session {
	case "london":
		return 8 * 60, true
	case "newyork":
		return 13*60 + 30, true
	default:
		return 0, false
	}
}

func truncateDay(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

func pnl(side string, entry, exit float64, qty int) float64 {
	if side == "sell" {
		return (entry - exit) * float64(qty)
	}
	return (exit - entry) * float64(qty)
}

func absf(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}
