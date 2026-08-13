package engine

import (
	"testing"

	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/execution"
	"github.com/romeo-folie/trade-engine/internal/store"
)

func TestUncappedTakeProfitRequiresEnabledTrailingManager(t *testing.T) {
	risk := config.RiskConfig{
		UncapTakeProfit: true,
		TrailingStop: config.TrailingStopConfig{
			Enabled: true,
			Rule:    "ema9",
		},
	}
	if !uncappedTakeProfitEnabled(risk, execution.OrderSideBuy, true) {
		t.Fatal("BUY with uncap + enabled trailing manager must uncap")
	}

	risk.TrailingStop.Enabled = false
	if uncappedTakeProfitEnabled(risk, execution.OrderSideBuy, true) {
		t.Fatal("uncap without the trailing manager must fail safe to baseline")
	}

	risk.TrailingStop.Enabled = true
	if uncappedTakeProfitEnabled(risk, execution.OrderSideSell, true) {
		t.Fatal("SELL must never use the entry take-profit policy")
	}
	if uncappedTakeProfitEnabled(risk, execution.OrderSideBuy, false) {
		t.Fatal("uncap must fail safe when the configured trailing worker cannot start")
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
	active, qty := resolveUncappedEntry(risk, execution.OrderSideBuy, 5.8, 100, 95, true)
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
	active, qty := resolveUncappedEntry(risk, execution.OrderSideBuy, 0.75, 100, 95, true)
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
			risk, execution.OrderSideBuy, 5, 100, stopLoss, true,
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

func TestNormalizedStockBracketPricesRejectsUnsafeBrokerRoundedLevels(t *testing.T) {
	// These model values are distinct but both become $10.00 at Alpaca's
	// two-decimal increment, so accepting them would submit an incoherent leg.
	if _, _, ok := normalizedStockBracketPrices(10, 9.9951, 10.0049); ok {
		t.Fatal("broker-rounded equal stop/target levels must be rejected")
	}
	stop, target, ok := normalizedStockBracketPrices(10.01, 9.994, 10.026)
	if !ok || stop != 9.99 || target != 10.03 {
		t.Fatalf("normalized bracket = stop %.4f target %.4f ok %v", stop, target, ok)
	}
}

func TestBrokerBracketProtectionRequiresActiveWholeShareStop(t *testing.T) {
	if brokerBracketProtectionVerified(execution.OrderResult{
		Status: "partial", Quantity: 5, FilledQty: 2,
		ChildOrders: []execution.OrderLeg{{Type: "stop", Status: "held", Quantity: 5, RemainingQty: 5}},
	}, 2) {
		t.Fatal("held child must not protect a partial bracket parent")
	}
	if brokerBracketProtectionVerified(execution.OrderResult{
		Status: "filled", Quantity: 5, FilledQty: 5,
		ChildOrders: []execution.OrderLeg{{Type: "stop", Status: "accepted", Quantity: 4, RemainingQty: 4}},
	}, 5) {
		t.Fatal("undersized stop must not count as protection")
	}
	if !brokerBracketProtectionVerified(execution.OrderResult{
		Status: "filled", Quantity: 5, FilledQty: 5,
		ChildOrders: []execution.OrderLeg{{Type: "stop", Status: "accepted", Quantity: 5, RemainingQty: 5}},
	}, 5) {
		t.Fatal("active whole-share stop covering the position was rejected")
	}
	if !brokerBracketProtectionVerified(execution.OrderResult{
		Status: "filled", Quantity: 5, FilledQty: 5,
		ChildOrders: []execution.OrderLeg{{Type: "stop", Status: "done_for_day", Quantity: 5, RemainingQty: 5}},
	}, 5) {
		t.Fatal("GTC stop reported done_for_day must remain active protection")
	}
}

func TestBrokerBracketProtectionStateDistinguishesHeldAndActiveCoverage(t *testing.T) {
	partial := brokerBracketProtectionState(execution.OrderResult{
		Status: "partial", Quantity: 5, RemainingQty: 3, FilledQty: 2,
		ChildOrders: []execution.OrderLeg{{Type: "stop", Status: "held", Quantity: 5, RemainingQty: 5}},
	}, 2)
	if partial.HeldQty != 2 || partial.WorkingEntryQty != 3 || partial.ProtectedQty != 0 || partial.UnprotectedQty != 2 {
		t.Fatalf("partial protection state = %+v", partial)
	}
	active := brokerBracketProtectionState(execution.OrderResult{
		Status: "filled", Quantity: 5, FilledQty: 5,
		ChildOrders: []execution.OrderLeg{{Type: "stop", Status: "new", Quantity: 3, RemainingQty: 3}},
	}, 3)
	if active.ProtectedQty != 3 || active.UnprotectedQty != 0 {
		t.Fatalf("active protection state = %+v", active)
	}
	partialStop := brokerBracketProtectionState(execution.OrderResult{
		Status: "filled", Quantity: 5, FilledQty: 5,
		ChildOrders: []execution.OrderLeg{{Type: "stop", Status: "partial", Quantity: 5, RemainingQty: 2, FilledQty: 3}},
	}, 5)
	if partialStop.ProtectedQty != 2 || partialStop.UnprotectedQty != 3 {
		t.Fatalf("partially filled stop state = %+v", partialStop)
	}
}

func TestBrokerCoverageHeldQtyUsesFreshParentFill(t *testing.T) {
	parent := execution.OrderResult{Status: "filled", Quantity: 5, FilledQty: 5}
	if got := brokerCoverageHeldQty(2, parent); got != 5 {
		t.Fatalf("held quantity = %v, want broker's newer cumulative fill of 5", got)
	}
	state := brokerBracketProtectionState(execution.OrderResult{
		Status: "filled", Quantity: 5, FilledQty: 5,
		ChildOrders: []execution.OrderLeg{{Type: "stop", Status: "accepted", Quantity: 2, RemainingQty: 2}},
	}, brokerCoverageHeldQty(2, parent))
	if state.UnprotectedQty != 3 {
		t.Fatalf("fresh broker fill must expose 3 uncovered shares, state=%+v", state)
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

func TestTickerHasExitingLongBlocksOnlyMatchingInflightExit(t *testing.T) {
	trades := []store.Trade{
		{Ticker: "AAPL", Direction: "long", Status: "open"},
		{Ticker: "MSFT", Direction: "long", Status: "exiting"},
		{Ticker: "AAPL", Direction: "short", Status: "exiting"},
	}
	if tickerHasExitingLongIn(trades, "AAPL") {
		t.Fatal("open long or exiting short must not look like an exiting AAPL long")
	}
	trades = append(trades, store.Trade{
		Ticker: "AAPL", Direction: "long", Status: "exiting",
	})
	if !tickerHasExitingLongIn(trades, "AAPL") {
		t.Fatal("matching exiting long was not detected")
	}
}
