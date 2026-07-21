package currencystrength

import "testing"

func TestRoutePairUsesDirectOrInversePair(t *testing.T) {
	pairs := []Pair{
		{Symbol: "EUR/USD", Base: "EUR", Quote: "USD"},
		{Symbol: "USD/JPY", Base: "USD", Quote: "JPY"},
	}

	pair, side, ok := RoutePair(pairs, "EUR", "USD")
	if !ok || pair.Symbol != "EUR/USD" || side != "buy" {
		t.Fatalf("direct route = %s/%s/%v, want EUR/USD buy true", pair.Symbol, side, ok)
	}

	pair, side, ok = RoutePair(pairs, "JPY", "USD")
	if !ok || pair.Symbol != "USD/JPY" || side != "sell" {
		t.Fatalf("inverse route = %s/%s/%v, want USD/JPY sell true", pair.Symbol, side, ok)
	}
}

func TestEvaluateRequiresBroadStrongAndWeakVotes(t *testing.T) {
	pairs := researchPairs(t)
	windows := researchWindows()
	cfg := Config{MinGap: 0.0001, BroadMin: 2}

	decision := Evaluate(pairs, windows, cfg)
	if decision.Signal == nil {
		t.Fatalf("expected signal, got skip %q", decision.SkipReason)
	}
	if decision.Signal.Symbol != "EUR/USD" || decision.Signal.Side != "buy" {
		t.Fatalf("signal = %s %s, want EUR/USD buy", decision.Signal.Symbol, decision.Signal.Side)
	}

	cfg.BroadMin = 3
	decision = Evaluate(pairs, windows, cfg)
	if decision.Signal != nil || decision.SkipReason != "breadth" {
		t.Fatalf("broad-min=3 decision = %+v, want breadth skip", decision)
	}
}

func TestEvaluateAppliesRoutedPairConfirmation(t *testing.T) {
	pairs := researchPairs(t)
	windows := researchWindows()
	windows["EUR/USD"] = Window{Start: 1.10, End: 1.102}

	decision := Evaluate(pairs, windows, Config{MinGap: 0.0001, BroadMin: 2, ConfirmMinMove: 0.01})
	if decision.Signal != nil || decision.SkipReason != "confirm_min_move" {
		t.Fatalf("high confirm decision = %+v, want confirm_min_move skip", decision)
	}

	decision = Evaluate(pairs, windows, Config{MinGap: 0.0001, BroadMin: 2, ConfirmMinMove: 0.001})
	if decision.Signal == nil {
		t.Fatalf("lower confirm skipped with %q, want signal", decision.SkipReason)
	}
}

func TestEvaluateAppliesTradeInstrumentFilter(t *testing.T) {
	pairs := researchPairs(t)
	windows := researchWindows()
	cfg := Config{
		MinGap:           0.0001,
		BroadMin:         2,
		TradeInstruments: map[string]bool{"USD/JPY": true},
	}

	decision := Evaluate(pairs, windows, cfg)
	if decision.Signal != nil || decision.SkipReason != "trade_instrument_filter" {
		t.Fatalf("non-matching filter decision = %+v, want trade_instrument_filter skip", decision)
	}

	cfg.TradeInstruments = map[string]bool{"EUR/USD": true}
	decision = Evaluate(pairs, windows, cfg)
	if decision.Signal == nil {
		t.Fatalf("matching filter skipped with %q, want signal", decision.SkipReason)
	}
}

func TestPairsFromSymbolsRejectsInvalidPair(t *testing.T) {
	if _, err := PairsFromSymbols([]string{"EUR/USD", "MES"}); err == nil {
		t.Fatal("expected invalid pair error")
	}
}

func researchPairs(t *testing.T) []Pair {
	t.Helper()
	pairs, err := PairsFromSymbols([]string{"EUR/USD", "GBP/USD", "USD/JPY", "EUR/JPY", "GBP/JPY"})
	if err != nil {
		t.Fatalf("PairsFromSymbols: %v", err)
	}
	return pairs
}

func researchWindows() map[string]Window {
	return map[string]Window{
		"EUR/USD": {Start: 1.10, End: 1.11},
		"GBP/USD": {Start: 1.20, End: 1.21},
		"USD/JPY": {Start: 140.0, End: 139.0},
		"EUR/JPY": {Start: 154.0, End: 155.0},
		"GBP/JPY": {Start: 168.0, End: 169.0},
	}
}
