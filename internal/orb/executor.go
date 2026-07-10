package orb

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/data"
	"github.com/romeo-folie/trade-engine/internal/execution"
)

// TradesFetcher is satisfied by brokers that can return a recent execution history.
// The ORB reconciler uses it to match bracket exit fills back to DB records without
// depending on child order IDs that may not have been captured at placement time.
type TradesFetcher interface {
	FetchTradeRecords(ctx context.Context, days int) ([]execution.IBKRTradeRecord, error)
}

// BracketParams holds the computed bracket order parameters.
type BracketParams struct {
	Ticker     string
	Side       string // "buy" | "sell"
	EntryStop  float64
	StopLoss   float64
	TakeProfit float64
	Qty        int
	SecType    string // for IBKR: "CASH" (forex) or "CFD" (metals)
}

// ComputeBracket derives entry/stop/target from an opening range and config.
func ComputeBracket(r *OpeningRange, side string, portfolioValue float64, cfg config.ORBConfig) BracketParams {
	var entryStop, stopLoss, takeProfit float64
	if side == "buy" {
		entryStop = r.High * 1.001 // 0.1% above range high
		stopLoss = r.Low * 0.999   // 0.1% below range low
		risk := entryStop - stopLoss
		takeProfit = entryStop + risk*cfg.RiskReward
	} else {
		entryStop = r.Low * 0.999 // 0.1% below range low
		stopLoss = r.High * 1.001 // 0.1% above range high
		risk := stopLoss - entryStop
		takeProfit = entryStop - risk*cfg.RiskReward
	}

	riskPerShare := math.Abs(entryStop - stopLoss)
	if riskPerShare < 0.01 {
		riskPerShare = 0.01 // guard against near-zero range width
	}
	maxRiskDollars := portfolioValue * cfg.MaxRiskPct / 100.0
	qty := int(math.Floor(maxRiskDollars / riskPerShare))
	if qty < 1 {
		qty = 1
	}

	// Hard notional cap: qty × entry price must not exceed MaxPositionPct% of portfolio.
	// Without this, a narrow range on a cheap stock can produce an outsized position
	// that exceeds total portfolio value.
	if cfg.MaxPositionPct > 0 && entryStop > 0 {
		maxQtyByNotional := int(math.Floor(portfolioValue * cfg.MaxPositionPct / 100.0 / entryStop))
		if maxQtyByNotional < 1 {
			maxQtyByNotional = 1
		}
		if qty > maxQtyByNotional {
			qty = maxQtyByNotional
		}
	}

	return BracketParams{
		Ticker:     r.Ticker,
		Side:       side,
		EntryStop:  entryStop,
		StopLoss:   stopLoss,
		TakeProfit: takeProfit,
		Qty:        qty,
	}
}

// ComputeForexBracket derives entry/stop/target from an opening range for forex/CFD instruments.
// Uses an absolute pip buffer instead of the percentage-based buffer used by
// ComputeBracket for equities. Position sizing respects MaxRiskPct and
// MaxPositionPct, and is rounded down to the nearest lot size.
// Per-instrument values (PipBuffer, MinLotSize, SecType) are taken from inst.
func ComputeForexBracket(r *OpeningRange, side string, portfolioValue float64, cfg config.ForexORBConfig, inst config.Instrument) BracketParams {
	var entryStop, stopLoss, takeProfit float64
	pip := inst.PipBuffer // e.g. 0.0002 for EUR/USD, 0.50 for XAU/USD

	if side == "buy" {
		entryStop = r.High + pip
		stopLoss = r.Low - pip
		risk := entryStop - stopLoss
		takeProfit = entryStop + risk*cfg.RiskReward
	} else {
		entryStop = r.Low - pip
		stopLoss = r.High + pip
		risk := stopLoss - entryStop
		takeProfit = entryStop - risk*cfg.RiskReward
	}

	qty := sizeForexQty(entryStop, stopLoss, portfolioValue, cfg, inst)

	// If qty is zero after the notional cap, the minimum lot size exceeds the
	// position limit — signal "cannot trade" so the caller can skip this order.
	return BracketParams{
		Ticker:     r.Ticker,
		Side:       side,
		EntryStop:  entryStop,
		StopLoss:   stopLoss,
		TakeProfit: takeProfit,
		Qty:        qty,
		SecType:    inst.SecType,
	}
}

// sizeForexQty risk-sizes a forex/CFD position from its entry and stop: qty targets
// MaxRiskPct×RiskMultiplier of the portfolio at the planned stop distance, rounded down to the
// instrument's lot size, then capped so notional does not exceed MaxPositionPct of the portfolio.
// With Instrument.MarginRate set, the cap applies to margin consumed (notional × margin_rate)
// rather than full notional — the leveraged-instrument reading of "% of portfolio per position".
// Returns 0 when even the minimum lot breaches the notional cap ("cannot trade"). Shared by the
// range bracket and the retest bracket so both size identically.
func sizeForexQty(entry, stop, portfolioValue float64, cfg config.ForexORBConfig, inst config.Instrument) int {
	riskPerUnit := math.Abs(entry - stop)
	if riskPerUnit < 0.0001 {
		riskPerUnit = 0.0001
	}
	mult := inst.RiskMultiplier
	if mult <= 0 {
		mult = 1.0
	}
	maxRiskDollars := portfolioValue * cfg.MaxRiskPct * mult / 100.0
	qty := int(math.Floor(maxRiskDollars / riskPerUnit))

	lotSize := inst.MinLotSize
	if lotSize > 0 {
		qty = (qty / lotSize) * lotSize
		if qty < lotSize {
			qty = lotSize
		}
	}
	if qty < 1 {
		qty = 1
	}

	if cfg.MaxPositionPct > 0 && entry > 0 {
		marginRate := inst.MarginRate
		if marginRate <= 0 || marginRate > 1 {
			marginRate = 1.0 // unset (or invalid) → cap on full notional, the pre-margin behaviour
		}
		maxQtyByNotional := int(math.Floor(portfolioValue * cfg.MaxPositionPct / 100.0 / (entry * marginRate)))
		if lotSize > 0 {
			maxQtyByNotional = (maxQtyByNotional / lotSize) * lotSize
			if maxQtyByNotional < lotSize {
				maxQtyByNotional = 0 // cannot trade — min lot exceeds position cap
			}
		}
		if maxQtyByNotional >= 0 && qty > maxQtyByNotional {
			qty = maxQtyByNotional
		}
	}

	// Notional cap: bound order value (entry × qty) so a tight stop on a high-priced
	// instrument can't size into a notional that trips the broker's precautionary
	// order-value limit (IBKR default $100k). Applied last so it also clamps the
	// margin-cap result. If even one lot exceeds the cap, qty→0 (cannot trade) rather
	// than placing an order that would be rejected — same convention as the margin cap.
	if inst.MaxNotionalUSD > 0 && entry > 0 {
		maxQtyByValue := int(math.Floor(inst.MaxNotionalUSD / entry))
		if lotSize > 0 {
			maxQtyByValue = (maxQtyByValue / lotSize) * lotSize
			if maxQtyByValue < lotSize {
				maxQtyByValue = 0 // one lot alone exceeds the notional cap
			}
		}
		if qty > maxQtyByValue {
			qty = maxQtyByValue
		}
	}
	return qty
}

// ScaleOutBrackets splits a computed forex bracket into a PARTIAL and a RUNNER bracket for
// live split-bracket partial take-profit — the live realisation of the backtest adaptive
// variant's partial (resolveAdaptiveExit). Both brackets share the entry trigger and stop;
// they differ only in quantity and take-profit level:
//   - partial: PartialFraction of the size (lot-rounded), take-profit at partial_take_profit_r
//   - runner:  the remainder, take-profit at runner_take_profit_r (or the base target when
//     runner_take_profit_r is unset)
//
// Levels are measured from the entry trigger over the planned risk R = |EntryStop − StopLoss|,
// the same basis ComputeForexBracket uses for the base target (the realised fill is unknown
// at placement; the trigger is the consistent proxy).
//
// Returns split=false with the base bracket unchanged as the runner when the instrument does
// not configure a partial, when R is degenerate, or when lot-rounding leaves either the
// partial or the runner with no tradeable size — the caller then places the single base
// bracket exactly as before (so a non-configuring instrument is byte-identical to today).
func ScaleOutBrackets(base BracketParams, inst config.Instrument) (partial, runner BracketParams, split bool) {
	if inst.PartialTakeProfitR <= 0 || inst.PartialFraction <= 0 {
		return BracketParams{}, base, false
	}
	R := math.Abs(base.EntryStop - base.StopLoss)
	if R <= 0 {
		return BracketParams{}, base, false
	}
	f := inst.PartialFraction
	if f > 1 {
		f = 1
	}
	pq := int(float64(base.Qty) * f)
	if inst.MinLotSize > 0 { // a partial must be a tradeable lot multiple
		pq = (pq / inst.MinLotSize) * inst.MinLotSize
	}
	rq := base.Qty - pq
	if pq <= 0 || rq <= 0 {
		return BracketParams{}, base, false // too small to split into two tradeable lots
	}

	sign := 1.0
	if base.Side == "sell" {
		sign = -1.0
	}
	partial = base
	partial.Qty = pq
	partial.TakeProfit = base.EntryStop + sign*inst.PartialTakeProfitR*R
	runner = base
	runner.Qty = rq
	if inst.RunnerTakeProfitR > 0 {
		runner.TakeProfit = base.EntryStop + sign*inst.RunnerTakeProfitR*R
	}
	return partial, runner, true
}

// ComputeFuturesBracket derives entry/stop/target for an exchange-listed futures contract.
//
// Prices are snapped to the nearest tick (tickSize) and the buffer beyond the range
// high/low is expressed as an integer number of ticks (inst.TickBuffer).
// Position sizing uses dollar risk per contract (riskTicks × tickValue) against
// MaxRiskPct, then clamps by MaxPositionPct using the contract's notional value
// (entryStop × contractMultiplier where contractMultiplier = tickValue / tickSize).
// Returns qty=0 when even a single contract exceeds the notional cap — the caller
// should skip the order rather than placing a zero-quantity order.
func ComputeFuturesBracket(r *OpeningRange, side string, portfolioValue float64, cfg config.FuturesORBConfig, inst config.FuturesInstrument) BracketParams {
	buf := float64(inst.TickBuffer) * inst.TickSize

	var entryStop, stopLoss, takeProfit float64
	if side == "buy" {
		entryStop = r.High + buf
		stopLoss = r.Low - buf
		risk := entryStop - stopLoss
		takeProfit = entryStop + risk*cfg.RiskReward
	} else {
		entryStop = r.Low - buf
		stopLoss = r.High + buf
		risk := stopLoss - entryStop
		takeProfit = entryStop - risk*cfg.RiskReward
	}

	// Risk sizing: floor range width to at least 1 tick so riskTicks is never zero.
	rangeWidth := math.Abs(entryStop - stopLoss)
	riskTicks := math.Max(1, math.Round(rangeWidth/inst.TickSize))
	riskPerContract := riskTicks * inst.TickValue

	maxRiskDollars := portfolioValue * cfg.MaxRiskPct / 100.0
	qty := int(math.Floor(maxRiskDollars / riskPerContract))
	if qty < 1 {
		qty = 1
	}

	// Position cap: qty × marginPerContract ≤ MaxPositionPct% of portfolio.
	// contractMultiplier converts price points to dollars (e.g. ES: $12.50/0.25 = $50/pt).
	// With inst.MarginRate set the cap applies to MARGIN CONSUMED (notional × margin_rate)
	// rather than full notional — futures are leveraged, so a full-notional cap sizes to
	// zero contracts on a small account (one MES ≈ $28k notional). marginRate unset (or
	// invalid) → 1.0 → cap on full notional, the pre-margin behaviour.
	if cfg.MaxPositionPct > 0 && entryStop > 0 && inst.TickSize > 0 {
		contractMultiplier := inst.TickValue / inst.TickSize
		marginRate := inst.MarginRate
		if marginRate <= 0 || marginRate > 1 {
			marginRate = 1.0
		}
		marginPerContract := entryStop * contractMultiplier * marginRate
		maxMargin := portfolioValue * cfg.MaxPositionPct / 100.0
		maxQtyByMargin := int(math.Floor(maxMargin / marginPerContract))
		if maxQtyByMargin < 1 {
			// Even one contract exceeds the cap — caller must skip this order.
			return BracketParams{Ticker: r.Ticker, Side: side, EntryStop: entryStop, StopLoss: stopLoss, TakeProfit: takeProfit, Qty: 0, SecType: "FUT"}
		}
		if qty > maxQtyByMargin {
			qty = maxQtyByMargin
		}
	}

	return BracketParams{
		Ticker:     r.Ticker,
		Side:       side,
		EntryStop:  entryStop,
		StopLoss:   stopLoss,
		TakeProfit: takeProfit,
		Qty:        qty,
		SecType:    "FUT",
	}
}

// PlaceBracket submits a bracket stop order via the executor.
// Returns the parent entry order ID plus the take-profit and stop-loss child
// order IDs (both empty when the broker doesn't expose child IDs, e.g. Alpaca).
func PlaceBracket(ctx context.Context, executor execution.Executor, params BracketParams) (parentID, tpOrderID, slOrderID string, err error) {
	return placeBracket(ctx, executor, params, execution.OrderTypeStop)
}

// PlaceMarketBracket places a bracket whose PARENT is a market order — used for breakout
// confirmation entries (Phase 3). Once a close has confirmed the breakout the strategy
// enters at market, rather than resting a stop at the breakout level: a resting stop would
// re-fill on a later pullback wick to that level, the exact false-breakout the confirmation
// was meant to filter. The TP/SL child legs are unchanged.
func PlaceMarketBracket(ctx context.Context, executor execution.Executor, params BracketParams) (parentID, tpOrderID, slOrderID string, err error) {
	return placeBracket(ctx, executor, params, execution.OrderTypeMarket)
}

func placeBracket(ctx context.Context, executor execution.Executor, params BracketParams, orderType execution.OrderType) (parentID, tpOrderID, slOrderID string, err error) {
	req := execution.OrderRequest{
		Ticker:          params.Ticker,
		Side:            execution.OrderSide(params.Side),
		Type:            orderType,
		Quantity:        float64(params.Qty),
		OrderClass:      "bracket",
		TakeProfitPrice: params.TakeProfit,
		StopLossPrice:   params.StopLoss,
		SecType:         params.SecType,
		TradeID:         fmt.Sprintf("orb-%s-%d", params.Ticker, time.Now().UnixMilli()),
	}
	// A stop-entry bracket triggers at the breakout level; a market parent fills now.
	if orderType == execution.OrderTypeStop {
		req.StopPrice = params.EntryStop
	}
	result, err := executor.PlaceOrder(ctx, req)
	if err != nil {
		return "", "", "", fmt.Errorf("place bracket %s: %w", params.Ticker, err)
	}
	if len(result.ChildOrderIDs) >= 2 {
		return result.BrokerOrderID, result.ChildOrderIDs[0], result.ChildOrderIDs[1], nil
	}
	return result.BrokerOrderID, "", "", nil
}

// sanitizeEntryFill guards against a broker-reported entry fill price that is physically
// impossible for a stop order. A buy-stop triggers when price rises to entryStop, so it can
// only fill at entryStop or higher (slippage worsens it upward); a sell-stop, only at entryStop
// or lower. When both sides of a dual-sided bracket fill in the same instant, IBKR has been
// observed to return each leg's avgPrice swapped/garbled — yielding a buy "filled" below its
// stop or a sell above its stop, which then corrupts the recorded entry and all downstream PnL.
// Such a price is rejected and the entry stop level — the order's trigger, a close and unbiased
// proxy for the true fill — is recorded instead. tol absorbs rounding. Returns the price to use
// and whether it was corrected.
func sanitizeEntryFill(side string, entryStop, reported float64) (float64, bool) {
	if entryStop <= 0 || reported <= 0 {
		return reported, false
	}
	const tol = 0.001 // 0.1%
	switch side {
	case "buy":
		if reported < entryStop*(1-tol) {
			return entryStop, true
		}
	case "sell":
		if reported > entryStop*(1+tol) {
			return entryStop, true
		}
	}
	return reported, false
}

// ExitPnL is the exported form of exitPnL, for out-of-package callers such as
// cmd/orb-backfill that resolve exits via ResolveExitViaBarReplay.
func ExitPnL(side string, entryPrice, exitPrice float64, qty int) float64 {
	return exitPnL(side, entryPrice, exitPrice, qty)
}

// exitPnL computes realised P&L in quote-currency units when an exit leg fills.
// For a buy: profit = (exitPrice - entryPrice) × qty.
// For a sell: profit = (entryPrice - exitPrice) × qty.
func exitPnL(side string, entryPrice, exitPrice float64, qty int) float64 {
	if side == "sell" {
		return (entryPrice - exitPrice) * float64(qty)
	}
	return (exitPrice - entryPrice) * float64(qty)
}

// resolveExitFromCandles walks an ordered series of OHLC bars and reports the first
// bar whose range touches a bracket leg for a filled trade, returning the exit type
// ("tp"/"stop"), the level price to fill at, and the bar's timestamp. found is false
// when no bar reaches either leg (the position never exited within the bars given).
//
// This replays the actual price path rather than sampling a single live quote, so it
// records the leg that fired first — correct for trades observed live, after a restart,
// or backfilled long after the fact. A non-positive level is treated as "unset" so a
// missing stop or target never fires. When a single bar straddles both legs (its range
// spans stop and target), the stop is taken as the conservative tie-break, since we
// cannot know the intra-bar order and must not over-report wins.
// ResolveExitFromCandles is the exported entry point to the bar-walk exit resolver,
// so the ORB backtest engine replays exits with exactly the live tie-break semantics
// (stop checked before take-profit on a same-bar straddle — the conservative choice).
func ResolveExitFromCandles(side string, stopLoss, takeProfit float64, candles []data.Candle) (exitType string, exitPrice float64, exitTime time.Time, found bool) {
	return resolveExitFromCandles(side, stopLoss, takeProfit, candles)
}

func resolveExitFromCandles(side string, stopLoss, takeProfit float64, candles []data.Candle) (exitType string, exitPrice float64, exitTime time.Time, found bool) {
	for _, c := range candles {
		switch side {
		case "buy":
			if stopLoss > 0 && c.Low <= stopLoss {
				return "stop", stopLoss, c.Timestamp, true
			}
			if takeProfit > 0 && c.High >= takeProfit {
				return "tp", takeProfit, c.Timestamp, true
			}
		case "sell":
			if stopLoss > 0 && c.High >= stopLoss {
				return "stop", stopLoss, c.Timestamp, true
			}
			if takeProfit > 0 && c.Low <= takeProfit {
				return "tp", takeProfit, c.Timestamp, true
			}
		}
	}
	return "", 0, time.Time{}, false
}

// floorToBar rounds a time down to the start of the bar that contains it, for the given
// timeframe, so a fetch window starting mid-bar still includes that whole bar (providers
// key and filter bars by their start timestamp). Times are UTC in this package.
func floorToBar(t time.Time, tf data.Timeframe) time.Time {
	switch tf {
	case data.Timeframe1Min:
		return t.Truncate(time.Minute)
	case data.Timeframe1Hour:
		return t.Truncate(time.Hour)
	case data.Timeframe1Day:
		return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
	default:
		return t
	}
}

// barReplayTimeframe picks the finest market-data bar resolution that the provider
// still serves for a lookback of the given age. IBKR's history depth shrinks as the
// bar gets finer (1-min reaches only ~1–2 days; hourly ~1 month; daily ~6+ months),
// so an old filled trade must be replayed on coarser bars or no data comes back.
// Finer bars give more precise TP-vs-stop tie-breaking; coarser bars still resolve
// the outcome from each bar's high/low, just more conservatively on same-bar straddles.
func barReplayTimeframe(age time.Duration) data.Timeframe {
	switch {
	case age <= 24*time.Hour:
		return data.Timeframe1Min
	case age <= 28*24*time.Hour:
		return data.Timeframe1Hour
	default:
		return data.Timeframe1Day
	}
}

// barReplayTimeframeFallbacks returns the ordered resolutions to try for a lookback
// of the given age: the age-appropriate timeframe first, then each coarser one. The
// coarser fallbacks rescue a replay when the finer bars come back empty (data gap or
// the lookback just exceeds the finer bar's depth). Coarser is never finer than 1-min
// and daily is always last, so the list is short and terminates.
func barReplayTimeframeFallbacks(age time.Duration) []data.Timeframe {
	switch barReplayTimeframe(age) {
	case data.Timeframe1Min:
		return []data.Timeframe{data.Timeframe1Min, data.Timeframe1Hour, data.Timeframe1Day}
	case data.Timeframe1Hour:
		return []data.Timeframe{data.Timeframe1Hour, data.Timeframe1Day}
	default:
		return []data.Timeframe{data.Timeframe1Day}
	}
}

// BarReplayResult reports the outcome of replaying a filled trade's price path.
type BarReplayResult struct {
	ExitType  string         // "tp" | "stop"
	ExitPrice float64        // the bracket leg price the path touched
	ExitTime  time.Time      // bar timestamp at which the leg was touched
	Timeframe data.Timeframe // bar resolution that resolved (or last tried) the exit
	Found     bool           // false when no bar reached a leg within the window
	Bars      int            // number of bars fetched at the resolving timeframe
}

// ResolveExitViaBarReplay fetches market-data bars over the replay window [from, to]
// for a filled ORB trade and reports the first bracket leg (TP or stop) the price path
// touches. The bar resolution is chosen from how long ago the trade filled relative to
// `now` (data-retention depth shrinks for finer bars), walking from the finest the
// lookback allows toward coarser bars and using the first timeframe that returns data —
// so a trade filled minutes ago resolves on 1-min bars while one filled months ago
// resolves on daily bars. `to` bounds the fetch (callers cap it at a horizon past the
// fill so an old stuck trade is replayed over its own post-fill bars, never an unrelated
// recent window). Returns the resolved exit (Found=true), a not-found result (bars
// existed but no leg was touched), or an error only when every timeframe returned no bars.
func ResolveExitViaBarReplay(ctx context.Context, md data.MarketDataProvider, ticker, side string, stopLoss, takeProfit float64, from, to, now time.Time) (BarReplayResult, error) {
	age := now.Sub(from)
	var lastErr error
	var result BarReplayResult

	for _, tf := range barReplayTimeframeFallbacks(age) {
		// Floor the fetch start to the bar boundary. A provider keys a bar by its START
		// timestamp and drops bars before `from`; passing the exact fill time (e.g. 08:15)
		// would exclude the 08:00 hourly / current-day bar that CONTAINS the fill — and that
		// bar is exactly where an intraday ORB exit usually lands. Without this, an hourly/
		// daily replay can miss the real exit and either leave the trade open or match a later
		// unrelated bar. The containing bar also spans pre-fill price (an unavoidable coarse-bar
		// approximation already noted on resolveExitFromCandles), but including it is strictly
		// better than dropping the bar the exit happened in.
		candles, err := md.FetchCandles(ctx, ticker, tf, floorToBar(from, tf), to)
		if err != nil {
			lastErr = err
			continue
		}
		if len(candles) == 0 {
			continue // no data at this resolution — try a coarser bar with deeper history
		}
		// The finest timeframe that returns data is authoritative for the requested
		// window. We do NOT escalate to coarser bars on a no-hit: a coarser bar can span a
		// wider/earlier interval (e.g. a daily bar includes pre-entry price), so its high/low
		// could trip a leg the actual post-fill path never reached — fabricating a false
		// exit. Coarser bars are a fallback ONLY for missing data, never to override a hit/no-hit
		// the finer bars already resolved.
		result = BarReplayResult{Timeframe: tf, Bars: len(candles)}
		exitType, exitPrice, exitTime, found := resolveExitFromCandles(side, stopLoss, takeProfit, candles)
		if found {
			result.ExitType, result.ExitPrice, result.ExitTime, result.Found = exitType, exitPrice, exitTime, true
		}
		return result, nil
	}

	if lastErr != nil {
		return BarReplayResult{}, fmt.Errorf("bar replay %s: no bars from any timeframe: %w", ticker, lastErr)
	}
	// No timeframe returned data and none errored → no history available; treat as still open.
	return result, nil
}

// CancelOrder cancels an open order by its broker order ID.
func CancelOrder(ctx context.Context, executor execution.Executor, orderID string) error {
	return executor.CancelOrder(ctx, orderID)
}
