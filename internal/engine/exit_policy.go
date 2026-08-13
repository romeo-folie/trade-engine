package engine

import (
	"math"

	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/execution"
)

// uncappedTakeProfitEnabled deliberately requires the trailing manager. This
// keeps a partial config (uncap=true, trailing disabled) on the proven baseline
// instead of silently removing its only winning exit.
func uncappedTakeProfitEnabled(
	risk config.RiskConfig,
	side execution.OrderSide,
	trailingManagerAvailable bool,
) bool {
	return side == execution.OrderSideBuy &&
		risk.UncapTakeProfit &&
		risk.TrailingStop.Enabled &&
		trailingManagerAvailable
}

// resolveUncappedEntry keeps the uncapped/GTC-protected path on whole shares
// with a coherent hard stop. Alpaca rejects GTC stops for fractional equity
// quantities, and removing the TP without a usable stop would leave the
// position unprotected, so either condition fails safe to baseline behavior.
func resolveUncappedEntry(
	risk config.RiskConfig,
	side execution.OrderSide,
	quantity, signalPrice, stopLoss float64,
	trailingManagerAvailable bool,
) (bool, float64) {
	if !uncappedTakeProfitEnabled(risk, side, trailingManagerAvailable) {
		return false, quantity
	}
	if signalPrice <= 0 || stopLoss <= 0 || stopLoss >= signalPrice {
		return false, quantity
	}
	whole := math.Floor(quantity)
	if whole < 1 {
		return false, quantity
	}
	return true, whole
}

func stockBracketEligible(risk config.RiskConfig, side execution.OrderSide, signalPrice, stopLoss, takeProfit float64, uncapped bool) bool {
	if uncapped || !risk.BracketExitsEnabled || side != execution.OrderSideBuy {
		return false
	}
	_, _, ok := normalizedStockBracketPrices(signalPrice, stopLoss, takeProfit)
	return ok
}

// normalizedStockBracketPrices applies Alpaca's minimum price increment before
// deciding whether a proposed bracket is coherent. Checking raw model prices
// is insufficient: two distinct values can collapse to the same broker price.
func normalizedStockBracketPrices(signalPrice, stopLoss, takeProfit float64) (float64, float64, bool) {
	if !finitePositive(signalPrice) || !finitePositive(stopLoss) || !finitePositive(takeProfit) {
		return 0, 0, false
	}
	stopLoss = roundStockOrderPrice(stopLoss)
	takeProfit = roundStockOrderPrice(takeProfit)
	if !finitePositive(stopLoss) || !finitePositive(takeProfit) ||
		stopLoss >= signalPrice || takeProfit <= signalPrice || takeProfit <= stopLoss {
		return 0, 0, false
	}
	return stopLoss, takeProfit, true
}

func roundStockOrderPrice(price float64) float64 {
	scale := 100.0
	if price < 1 {
		scale = 10_000
	}
	return math.Round(price*scale) / scale
}

func persistedTakeProfit(modelTakeProfit float64, uncapped bool) float64 {
	if uncapped {
		return 0
	}
	return modelTakeProfit
}
