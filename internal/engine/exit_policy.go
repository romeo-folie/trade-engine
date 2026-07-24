package engine

import (
	"math"

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

// resolveUncappedEntry keeps the uncapped/GTC-protected path on whole shares.
// Alpaca rejects GTC stops for fractional equity quantities, so quantities
// below one share fail safe to the baseline fixed-TP/software-exit behavior.
func resolveUncappedEntry(risk config.RiskConfig, side execution.OrderSide, quantity float64) (bool, float64) {
	if !uncappedTakeProfitEnabled(risk, side) {
		return false, quantity
	}
	whole := math.Floor(quantity)
	if whole < 1 {
		return false, quantity
	}
	return true, whole
}

func stockBracketEligible(risk config.RiskConfig, side execution.OrderSide, signalPrice, stopLoss, takeProfit float64, uncapped bool) bool {
	return !uncapped &&
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
