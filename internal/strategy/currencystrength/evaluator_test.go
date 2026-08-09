package currencystrength

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

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

func TestStrongestWeakestBreaksTiesDeterministically(t *testing.T) {
	for _, scores := range []map[string]float64{
		{"EUR": 1, "USD": -2, "AUD": 1},
		{"AUD": 1, "USD": -2, "EUR": 1},
	} {
		strong, weak, gap, ok := StrongestWeakest(scores)
		if !ok || strong != "AUD" || weak != "USD" || gap != 3 {
			t.Fatalf("rank = %s/%s gap=%v ok=%v, want AUD/USD gap=3", strong, weak, gap, ok)
		}
	}
}

func TestEvaluateRejectsNonFiniteWindowsWithSerializableDiagnostics(t *testing.T) {
	for _, tc := range []struct {
		name  string
		start float64
		end   float64
	}{
		{"nan", math.NaN(), 1},
		{"infinity", 1, math.Inf(1)},
		{"extreme", math.SmallestNonzeroFloat64, math.MaxFloat64},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pairs, _ := PairsFromSymbols([]string{"EUR/USD"})
			decision := Evaluate(pairs, map[string]Window{"EUR/USD": {Start: tc.start, End: tc.end}}, Config{})
			if tc.name == "extreme" {
				if decision.SkipReason == "missing_price_window" {
					t.Fatalf("finite extreme prices incorrectly rejected: %+v", decision.Diagnostics)
				}
				return
			}
			if decision.SkipReason != "missing_price_window" || decision.Diagnostics.Pairs[0].Available {
				t.Fatalf("non-finite input accepted: %+v", decision)
			}
			if _, err := json.Marshal(decision.Diagnostics); err != nil {
				t.Fatalf("rejection diagnostics are not serializable: %v", err)
			}
		})
	}
}

func TestFormatDecisionSummaryExplainsNoRank(t *testing.T) {
	pairs, _ := PairsFromSymbols([]string{"EUR/USD", "GBP/USD", "EUR/GBP"})
	windows := map[string]Window{
		"EUR/USD": {Start: 1, End: 1}, "GBP/USD": {Start: 1, End: 1}, "EUR/GBP": {Start: 1, End: 1},
	}
	decision := Evaluate(pairs, windows, Config{})
	if decision.SkipReason != "no_rank" {
		t.Fatalf("flat basket reason = %q", decision.SkipReason)
	}
	if got := FormatDecisionSummary(decision.Diagnostics, decision.SkipReason); got == "no_rank" || !strings.Contains(got, "tied") {
		t.Fatalf("opaque no-rank summary: %q", got)
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

func TestEvaluateScoreFilterLeavesExcludedPairRoutable(t *testing.T) {
	pairs, err := PairsFromSymbols([]string{"EUR/USD", "GBP/USD", "EUR/GBP"})
	if err != nil {
		t.Fatal(err)
	}
	windows := map[string]Window{
		"EUR/USD": {Start: 1, End: math.Exp(0.02)},
		"GBP/USD": {Start: 1, End: math.Exp(0.01)},
		"EUR/GBP": {Start: 1, End: math.Exp(0.03)}, // deliberately inconsistent and excluded from evidence
	}
	decision := Evaluate(pairs, windows, Config{
		ScoreMethod: "least_squares", BroadMin: 1,
		ScoreInstruments: map[string]bool{"EUR/USD": true, "GBP/USD": true},
	})
	if decision.Signal == nil {
		t.Fatalf("score-filtered decision skipped: %s %+v", decision.SkipReason, decision.Diagnostics)
	}
	if decision.Signal.Symbol != "EUR/USD" || decision.Signal.Side != "buy" {
		t.Fatalf("routed signal = %+v, want BUY EUR/USD", decision.Signal)
	}
	for _, pair := range decision.Diagnostics.Pairs {
		if pair.Symbol == "EUR/GBP" && (!pair.Available || pair.Scored) {
			t.Fatalf("excluded scoring pair diagnostics = %+v", pair)
		}
	}
}

func TestEvaluatePreservesExplicitEmptyScoreSet(t *testing.T) {
	pairs, err := PairsFromSymbols([]string{"EUR/USD", "GBP/USD", "EUR/GBP"})
	if err != nil {
		t.Fatal(err)
	}
	decision := Evaluate(pairs, map[string]Window{
		"EUR/USD": {Start: 1, End: 1.01}, "GBP/USD": {Start: 1, End: 1.01}, "EUR/GBP": {Start: 1, End: 1.01},
	}, Config{ScoreInstruments: map[string]bool{}})
	if decision.Signal != nil {
		t.Fatalf("empty score set produced signal: %+v", decision.Signal)
	}
	for _, pair := range decision.Diagnostics.Pairs {
		if pair.Scored {
			t.Fatalf("empty score set marked pair scored: %+v", pair)
		}
	}
}

func TestEvaluateRetainsDetailedDiagnosticsForRejectedSignal(t *testing.T) {
	pairs := researchPairs(t)
	decision := Evaluate(pairs, researchWindows(), Config{
		MinGap: 1, BroadMin: 2, ConfirmMinMove: 0.0015,
	})
	if decision.SkipReason != "min_gap" {
		t.Fatalf("skip reason = %q, want min_gap", decision.SkipReason)
	}
	d := decision.Diagnostics
	if d.Version != 1 || d.Strongest == "" || d.Weakest == "" || d.Gap <= 0 {
		t.Fatalf("incomplete ranking diagnostics: %+v", d)
	}
	if len(d.Pairs) != len(pairs) || len(d.Currencies) != 4 {
		t.Fatalf("diagnostic coverage pairs=%d currencies=%d", len(d.Pairs), len(d.Currencies))
	}
	if d.Gates[2].Name != "min_gap" || d.Gates[2].Status != "failed" ||
		d.Gates[3].Status != "not_evaluated" {
		t.Fatalf("unexpected ordered gates: %+v", d.Gates)
	}
	summary := FormatDecisionSummary(d, decision.SkipReason)
	for _, want := range []string{"strength gap", "required 1.0000", "strongest", "weakest"} {
		if !strings.Contains(summary, want) {
			t.Fatalf("summary %q missing %q", summary, want)
		}
	}
}

func TestEvaluateRetainsCandidateWhenTradeInstrumentIsFiltered(t *testing.T) {
	pairs := researchPairs(t)
	decision := Evaluate(pairs, researchWindows(), Config{
		MinGap: 0.0001, BroadMin: 2,
		TradeInstruments: map[string]bool{"USD/JPY": true},
	})
	if decision.SkipReason != "trade_instrument_filter" ||
		decision.Diagnostics.RoutedSymbol != "EUR/USD" ||
		decision.Diagnostics.RoutedSide != "buy" {
		t.Fatalf("filtered candidate diagnostics = %+v", decision.Diagnostics)
	}
	if got := FormatDecisionSummary(decision.Diagnostics, decision.SkipReason); !strings.Contains(got, "BUY EUR/USD") {
		t.Fatalf("summary does not identify filtered candidate: %q", got)
	}
}

func TestFormatDecisionSummaryFallsBackForLegacyRows(t *testing.T) {
	if got := FormatDecisionSummary(DecisionDiagnostics{}, "min_gap"); got != "min_gap" {
		t.Fatalf("legacy fallback = %q", got)
	}
}

func TestLeastSquaresStrengthRecoversLatentScoresWithMissingPair(t *testing.T) {
	pairs, err := PairsFromSymbols([]string{
		"EUR/USD", "GBP/USD", "USD/JPY", "EUR/JPY", "GBP/JPY", "EUR/GBP",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]float64{"EUR": 0.004, "GBP": 0.001, "USD": -0.002, "JPY": -0.003}
	windows := make(map[string]Window)
	for _, pair := range pairs {
		windows[pair.Symbol] = Window{Start: 1, End: math.Exp(want[pair.Base] - want[pair.Quote])}
	}
	delete(windows, "GBP/USD")
	decision := Evaluate(pairs, windows, Config{
		ScoreMethod: "least_squares", AllowPartialBasket: true,
		MinGap: 0.001, BroadMin: 1,
	})
	if decision.Signal == nil {
		t.Fatalf("connected degraded basket skipped: %s %+v", decision.SkipReason, decision.Diagnostics)
	}
	if !decision.Diagnostics.Connected || len(decision.Diagnostics.MissingPairs) != 1 ||
		decision.Diagnostics.MissingPairs[0] != "GBP/USD" {
		t.Fatalf("degradation diagnostics = %+v", decision.Diagnostics)
	}
	for currency, expected := range want {
		if math.Abs(decision.Scores[currency]-expected) > 1e-9 {
			t.Errorf("%s score=%g want %g", currency, decision.Scores[currency], expected)
		}
	}
	if decision.Diagnostics.ResidualRMSE > 1e-9 {
		t.Fatalf("consistent graph residual=%g", decision.Diagnostics.ResidualRMSE)
	}
}

func TestLeastSquaresStrengthRejectsDisconnectedPartialBasket(t *testing.T) {
	pairs, _ := PairsFromSymbols([]string{
		"EUR/USD", "GBP/USD", "USD/JPY", "EUR/JPY", "GBP/JPY", "EUR/GBP",
	})
	windows := map[string]Window{
		"EUR/USD": {Start: 1, End: 1.01},
		"GBP/JPY": {Start: 1, End: 1.01},
	}
	decision := Evaluate(pairs, windows, Config{
		ScoreMethod: "least_squares", AllowPartialBasket: true, BroadMin: 1,
	})
	if decision.Signal != nil || decision.SkipReason != "insufficient_basket" || decision.Diagnostics.Connected {
		t.Fatalf("disconnected decision = %+v", decision)
	}
	if got := FormatDecisionSummary(decision.Diagnostics, decision.SkipReason); !strings.Contains(got, "connected currency graph") {
		t.Fatalf("opaque disconnected summary: %q", got)
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
