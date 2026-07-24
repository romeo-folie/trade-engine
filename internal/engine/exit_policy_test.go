package engine

import (
	"testing"

	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/execution"
)

func TestUncappedTakeProfitRequiresEnabledTrailingManager(t *testing.T) {
	risk := config.RiskConfig{
		UncapTakeProfit: true,
		TrailingStop: config.TrailingStopConfig{
			Enabled: true,
			Rule:    "ema9",
		},
	}
	if !uncappedTakeProfitEnabled(risk, execution.OrderSideBuy) {
		t.Fatal("BUY with uncap + enabled trailing manager must uncap")
	}

	risk.TrailingStop.Enabled = false
	if uncappedTakeProfitEnabled(risk, execution.OrderSideBuy) {
		t.Fatal("uncap without the trailing manager must fail safe to baseline")
	}

	risk.TrailingStop.Enabled = true
	if uncappedTakeProfitEnabled(risk, execution.OrderSideSell) {
		t.Fatal("SELL must never use the entry take-profit policy")
	}
}

func TestUncappedTakeProfitDisablesBrokerAndSoftwareCaps(t *testing.T) {
	risk := config.RiskConfig{
		BracketExitsEnabled: true,
		UncapTakeProfit:     true,
		TrailingStop: config.TrailingStopConfig{
			Enabled: true,
			Rule:    "ema9",
		},
	}
	active, qty := resolveUncappedEntry(risk, execution.OrderSideBuy, 5.8, 100, 95)
	if !active || qty != 5 {
		t.Fatalf("uncapped entry active=%v qty=%v, want true/5 whole shares", active, qty)
	}
	if stockBracketEligible(risk, execution.OrderSideBuy, 100, 95, 110, active) {
		t.Fatal("uncapped entry must not attach a TP/SL bracket")
	}
	if got := persistedTakeProfit(110, true); got != 0 {
		t.Fatalf("persisted take profit = %v, want 0 so software monitor cannot cap it", got)
	}

	risk.UncapTakeProfit = false
	if !stockBracketEligible(risk, execution.OrderSideBuy, 100, 95, 110, false) {
		t.Fatal("baseline bracket behavior must remain enabled when uncap is off")
	}
	if got := persistedTakeProfit(110, false); got != 110 {
		t.Fatalf("baseline persisted take profit = %v, want 110", got)
	}
}

func TestUncappedFractionalEntryFailsSafeToBaseline(t *testing.T) {
	risk := config.RiskConfig{
		UncapTakeProfit: true,
		TrailingStop: config.TrailingStopConfig{
			Enabled: true,
			Rule:    "ema9",
		},
	}
	active, qty := resolveUncappedEntry(risk, execution.OrderSideBuy, 0.75, 100, 95)
	if active {
		t.Fatal("sub-one-share entry must not activate uncapped/GTC policy")
	}
	if qty != 0.75 {
		t.Fatalf("fallback quantity = %v, want original fractional 0.75", qty)
	}
	if got := persistedTakeProfit(110, active); got != 110 {
		t.Fatalf("fallback take profit = %v, want baseline 110", got)
	}
}

func TestUncappedEntryRequiresCoherentHardStop(t *testing.T) {
	risk := config.RiskConfig{
		UncapTakeProfit: true,
		TrailingStop: config.TrailingStopConfig{
			Enabled: true,
			Rule:    "ema9",
		},
	}
	for _, stopLoss := range []float64{0, -1, 100, 105} {
		active, qty := resolveUncappedEntry(
			risk, execution.OrderSideBuy, 5, 100, stopLoss,
		)
		if active {
			t.Fatalf("stop loss %v activated uncapped entry", stopLoss)
		}
		if qty != 5 {
			t.Fatalf("stop loss %v changed fallback quantity to %v", stopLoss, qty)
		}
		if got := persistedTakeProfit(110, active); got != 110 {
			t.Fatalf("stop loss %v removed baseline take profit", stopLoss)
		}
	}
}

func TestSellExitConfidenceGate(t *testing.T) {
	for _, tc := range []struct {
		name       string
		confidence float64
		minimum    float64
		want       bool
	}{
		{name: "disabled", confidence: 0.1, minimum: 0, want: true},
		{name: "below", confidence: 0.71, minimum: 0.72, want: false},
		{name: "equal", confidence: 0.72, minimum: 0.72, want: true},
		{name: "above", confidence: 0.9, minimum: 0.72, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := sellExitConfidenceAllowed(tc.confidence, tc.minimum); got != tc.want {
				t.Fatalf("allowed = %v, want %v", got, tc.want)
			}
		})
	}
}
