package orb

import (
	"context"
	"fmt"
	"math"

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
	lotSize := int(inst.MinLotSize)
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
		}
		if maxQtyByNotional < lotSize && lotSize > 0 {
			maxQtyByNotional = lotSize
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
		SecType:    inst.SecType,
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
