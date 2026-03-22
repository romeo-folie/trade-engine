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

	return BracketParams{
		Ticker:     r.Ticker,
		Side:       side,
		EntryStop:  entryStop,
		StopLoss:   stopLoss,
		TakeProfit: takeProfit,
		Qty:        qty,
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
