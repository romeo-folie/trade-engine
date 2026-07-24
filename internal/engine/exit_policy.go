package engine

import (
	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/execution"
)

// uncappedTakeProfitEnabled deliberately requires the trailing manager. This
// keeps a partial config (uncap=true, trailing disabled) on the proven baseline
// instead of silently removing its only winning exit.
func uncappedTakeProfitEnabled(risk config.RiskConfig, side execution.OrderSide) bool {
	return side == execution.OrderSideBuy &&
		risk.UncapTakeProfit &&
		risk.TrailingStop.Enabled
}

func stockBracketEligible(risk config.RiskConfig, side execution.OrderSide, signalPrice, stopLoss, takeProfit float64) bool {
	return !uncappedTakeProfitEnabled(risk, side) &&
		risk.BracketExitsEnabled &&
		side == execution.OrderSideBuy &&
		stopLoss > 0 && takeProfit > 0 &&
		stopLoss < signalPrice && takeProfit > signalPrice
}

func persistedTakeProfit(modelTakeProfit float64, uncapped bool) float64 {
	if uncapped {
		return 0
	}
	return modelTakeProfit
}
