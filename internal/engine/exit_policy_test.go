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
	if stockBracketEligible(risk, execution.OrderSideBuy, 100, 95, 110) {
		t.Fatal("uncapped entry must not attach a TP/SL bracket")
	}
	if got := persistedTakeProfit(110, true); got != 0 {
		t.Fatalf("persisted take profit = %v, want 0 so software monitor cannot cap it", got)
	}

	risk.UncapTakeProfit = false
	if !stockBracketEligible(risk, execution.OrderSideBuy, 100, 95, 110) {
		t.Fatal("baseline bracket behavior must remain enabled when uncap is off")
	}
	if got := persistedTakeProfit(110, false); got != 110 {
		t.Fatalf("baseline persisted take profit = %v, want 110", got)
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
