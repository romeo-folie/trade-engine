package orb

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/execution"
)

// BracketParams holds the computed bracket order parameters.
type BracketParams struct {
	Ticker     string
	Side       string  // "buy" | "sell"
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
		entryStop = r.High * 1.001  // 0.1% above range high
		stopLoss = r.Low * 0.999    // 0.1% below range low
		risk := entryStop - stopLoss
		takeProfit = entryStop + risk*cfg.RiskReward
	} else {
		entryStop = r.Low * 0.999   // 0.1% below range low
		stopLoss = r.High * 1.001   // 0.1% above range high
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
	maxRiskDollars := portfolioValue * cfg.MaxRiskPct / 100.0
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
func PlaceBracket(ctx context.Context, executor execution.Executor, params BracketParams) (string, error) {
	req := execution.OrderRequest{
		Ticker:          params.Ticker,
		Side:            execution.OrderSide(params.Side),
		Type:            execution.OrderTypeStop,
		Quantity:        float64(params.Qty),
		StopPrice:       params.EntryStop,
		OrderClass:      "bracket",
		TakeProfitPrice: params.TakeProfit,
		StopLossPrice:   params.StopLoss,
		SecType:         params.SecType,
		TradeID:         fmt.Sprintf("orb-%s-%d", params.Ticker, time.Now().UnixMilli()),
	}
	result, err := executor.PlaceOrder(ctx, req)
	if err != nil {
		return "", fmt.Errorf("place bracket %s: %w", params.Ticker, err)
	}
	return result.BrokerOrderID, nil
}

// CancelOrder cancels an open order by its broker order ID.
func CancelOrder(ctx context.Context, executor execution.Executor, orderID string) error {
	return executor.CancelOrder(ctx, orderID)
}
