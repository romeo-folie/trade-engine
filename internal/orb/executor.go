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

	riskPerUnit := math.Abs(entryStop - stopLoss)
	if riskPerUnit < 0.0001 {
		riskPerUnit = 0.0001
	}
	// Per-instrument risk multiplier (Phase 5): size a higher-tail-risk instrument down
	// without disabling it. <= 0 means 1.0 (no change).
	mult := inst.RiskMultiplier
	if mult <= 0 {
		mult = 1.0
	}
	maxRiskDollars := portfolioValue * cfg.MaxRiskPct * mult / 100.0
	qty := int(math.Floor(maxRiskDollars / riskPerUnit))

	// Round down to lot size
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

	// Hard notional cap: qty × entry price must not exceed MaxPositionPct% of portfolio.
	if cfg.MaxPositionPct > 0 && entryStop > 0 {
		maxQtyByNotional := int(math.Floor(portfolioValue * cfg.MaxPositionPct / 100.0 / entryStop))
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

	// Notional cap: qty × entryStop × contractMultiplier ≤ MaxPositionPct% of portfolio.
	// contractMultiplier converts price points to dollars (e.g. ES: $12.50/0.25 = $50/pt).
	if cfg.MaxPositionPct > 0 && entryStop > 0 && inst.TickSize > 0 {
		contractMultiplier := inst.TickValue / inst.TickSize
		notionalPerContract := entryStop * contractMultiplier
		maxNotional := portfolioValue * cfg.MaxPositionPct / 100.0
		maxQtyByNotional := int(math.Floor(maxNotional / notionalPerContract))
		if maxQtyByNotional < 1 {
			// Even one contract exceeds the cap — caller must skip this order.
			return BracketParams{Ticker: r.Ticker, Side: side, EntryStop: entryStop, StopLoss: stopLoss, TakeProfit: takeProfit, Qty: 0, SecType: "FUT"}
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
