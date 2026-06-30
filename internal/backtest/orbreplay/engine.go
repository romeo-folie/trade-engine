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
	"github.com/romeo-folie/trade-engine/internal/orb"
)

// Variant is a named rule set so experiments are reproducible. The MVP ships baseline
// and time-exit (the doc's first two variants); range-filter/confirmed/adaptive land
// with their live implementations in later PRs.
type Variant struct {
	Name           string
	MaxHoldMinutes int // 0 = hold until the bracket resolves (baseline)
}

// Baseline and TimeExit are the built-in MVP variants.
func Baseline() Variant { return Variant{Name: "baseline", MaxHoldMinutes: 0} }
func TimeExit(maxHold int) Variant {
	return Variant{Name: "time-exit", MaxHoldMinutes: maxHold}
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

	var trades []SimTrade
	from = from.UTC()
	to = to.UTC()
	for day := truncateDay(from); !day.After(to); day = day.AddDate(0, 0, 1) {
		sessionOpen := day.Add(time.Duration(openMin) * time.Minute)
		if sessionOpen.Before(from) || sessionOpen.After(to) {
			continue
		}
		for _, inst := range instruments {
			t, err := e.simulateSession(ctx, v, inst, sessionOpen, rangeMin, portfolioValue)
			if err != nil {
				return nil, err
			}
			trades = append(trades, t...)
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

	buy := orb.ComputeForexBracket(or, "buy", portfolioValue, e.cfg, inst)
	sell := orb.ComputeForexBracket(or, "sell", portfolioValue, e.cfg, inst)

	// Find the first breakout. The opposing side is cancelled (OCO), so only the side
	// that triggers first produces a trade.
	side, entryIdx, ok := firstBreakout(watch, buy, sell)
	if !ok {
		return nil, nil // range never broke within the window
	}
	params := buy
	if side == "sell" {
		params = sell
	}
	if params.Qty == 0 {
		return nil, nil // min lot exceeds the position cap for this instrument
	}

	trade := e.resolveTrade(v, inst, params, watch, entryIdx)
	if trade == nil {
		return nil, nil
	}
	return []SimTrade{*trade}, nil
}

// resolveTrade simulates entry fill and exit for a triggered breakout starting at
// watch[entryIdx]. Spread is applied half on entry, half on exit; commission per side.
//
// The entry bar is first checked for an ADVERSE same-bar stop-out: if the breakout
// bar's range also spans the stop loss, the trade is stopped on that bar. Intra-bar
// order is unknown, so this assumes the adverse fill (conservative — it avoids the
// optimistic bias of holding a same-bar stop into a later recovery). A favorable
// same-bar move to TP is NOT credited; it resolves on subsequent bars instead.
func (e *Engine) resolveTrade(v Variant, inst config.Instrument, p orb.BracketParams, watch []data.Candle, entryIdx int) *SimTrade {
	cost := e.costFor(inst.Symbol)
	half := cost.Spread / 2

	entryBar := watch[entryIdx]
	entryTime := entryBar.Timestamp
	// A stop entry fills at the trigger price, paying the spread (buy at ask, sell at bid).
	entryFill := p.EntryStop
	if p.Side == "buy" {
		entryFill += half
	} else {
		entryFill -= half
	}

	var exitType string
	var exitMid float64
	var exitTime time.Time
	if entryBarStopOut(p, entryBar) {
		exitType, exitMid, exitTime = "stop", p.StopLoss, entryTime
	} else {
		exitType, exitMid, exitTime = e.resolveExit(v, p, watch[entryIdx+1:], entryTime)
	}

	// Apply spread on exit: a buy exits by selling at the bid, a sell by buying at the ask.
	exitFill := exitMid
	if p.Side == "buy" {
		exitFill -= half
	} else {
		exitFill += half
	}

	gross := pnl(p.Side, entryFill, exitFill, p.Qty)
	commission := 2 * cost.CommissionPerSide
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

// resolveExit returns the exit type, mid price and time. For the time-exit variant it
// honours a max-hold: if a TP/SL is touched before the deadline it is taken, otherwise
// the position is flattened at the first bar at/after the deadline (market = that bar's
// open). Baseline holds until a leg is touched or data runs out (end_of_data).
func (e *Engine) resolveExit(v Variant, p orb.BracketParams, rest []data.Candle, entryTime time.Time) (string, float64, time.Time) {
	if len(rest) == 0 {
		return "end_of_data", p.EntryStop, entryTime
	}
	if v.MaxHoldMinutes > 0 {
		deadline := entryTime.Add(time.Duration(v.MaxHoldMinutes) * time.Minute)
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

// firstBreakout scans the post-range candles for the first bar that triggers either
// stop-entry. A buy triggers when price rises to the buy-stop (High ≥ entry); a sell
// when price falls to the sell-stop (Low ≤ entry). If both trigger on the same bar
// (a whipsaw straddle), the bar's own direction breaks the tie: a bullish bar is taken
// as buy-first, a bearish bar as sell-first.
func firstBreakout(watch []data.Candle, buy, sell orb.BracketParams) (side string, idx int, ok bool) {
	for i, c := range watch {
		buyHit := buy.Qty > 0 && buy.EntryStop > 0 && c.High >= buy.EntryStop
		sellHit := sell.Qty > 0 && sell.EntryStop > 0 && c.Low <= sell.EntryStop
		switch {
		case buyHit && sellHit:
			if c.Close >= c.Open {
				return "buy", i, true
			}
			return "sell", i, true
		case buyHit:
			return "buy", i, true
		case sellHit:
			return "sell", i, true
		}
	}
	return "", 0, false
}

// ── small helpers ──────────────────────────────────────────────────────────────

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
