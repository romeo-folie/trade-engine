package main

import (
	"context"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	csresearch "github.com/romeo-folie/trade-engine/internal/backtest/currencystrengthresearch"
	"github.com/romeo-folie/trade-engine/internal/backtest/orbreplay"
	"github.com/romeo-folie/trade-engine/internal/config"
)

func TestEvaluateStrengthDayScopedSeparatesScoringExecutionAndPricing(t *testing.T) {
	pairs := []strengthPair{
		{Instrument: config.Instrument{Symbol: "EUR/USD", SecType: "CASH"}, Base: "EUR", Quote: "USD"},
		{Instrument: config.Instrument{Symbol: "GBP/USD", SecType: "CASH"}, Base: "GBP", Quote: "USD"},
		{Instrument: config.Instrument{Symbol: "EUR/GBP", SecType: "CASH"}, Base: "EUR", Quote: "GBP"},
	}
	day := map[string]map[string]float64{
		"EUR/USD": {"06:00": 1, "09:00": math.Exp(.02), "18:00": 1.03},
		"GBP/USD": {"06:00": 1, "09:00": math.Exp(-.02), "18:00": .97},
		"EUR/GBP": {"06:00": 1, "09:00": math.Exp(.04), "18:00": 1.08},
	}
	costs := map[string]orbreplay.CostModel{
		"EUR/USD": {Spread: .0001}, "GBP/USD": {Spread: .0001}, "EUR/GBP": {Spread: .0001},
	}
	param := strengthParam{Start: "06:00", RankEnd: "09:00", Exit: "18:00", BroadMin: 1, Units: 100000, CurrencyModel: "least_squares", AllowPartial: true}
	scoreSymbols := map[string]bool{"EUR/USD": true, "GBP/USD": true}
	date := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	outcome := evaluateStrengthDayScoped(day, pairs, costs, param, date, strengthEvalScope{ScoreSymbols: scoreSymbols})
	if outcome.Trade == nil || outcome.Trade.Ticker != "EUR/GBP" {
		t.Fatalf("scoring-only exclusion did not leave EUR/GBP routable: %+v", outcome)
	}
	if math.IsNaN(outcome.Trade.NetPnL) || math.IsInf(outcome.Trade.NetPnL, 0) {
		t.Fatalf("full pricing graph produced invalid PnL: %+v", outcome.Trade)
	}
	blocked := evaluateStrengthDayScoped(day, pairs, costs, param, date, strengthEvalScope{
		ScoreSymbols: scoreSymbols, TradeSymbols: map[string]bool{"EUR/USD": true, "GBP/USD": true},
	})
	if blocked.Trade != nil || blocked.Reason != "trade_instrument_filter" {
		t.Fatalf("execution exclusion outcome=%+v", blocked)
	}
}

func TestMultiplyStrengthResearchCostsScalesEveryComponent(t *testing.T) {
	input := map[string]orbreplay.CostModel{"EUR/USD": {
		Spread: .0001, CommissionPerSide: 2, CommissionBpsPerSide: .2,
		CommissionMinPerSide: 2, CommissionPerUnit: .01,
	}}
	got := multiplyStrengthResearchCosts(input, 2)["EUR/USD"]
	if got.Spread != .0002 || got.CommissionPerSide != 4 || got.CommissionBpsPerSide != .4 || got.CommissionMinPerSide != 4 || got.CommissionPerUnit != .02 {
		t.Fatalf("2x costs=%+v", got)
	}
	if input["EUR/USD"].CommissionPerSide != 2 {
		t.Fatal("input cost map mutated")
	}
}

func TestSymbolSetFromSliceDistinguishesAllFromNone(t *testing.T) {
	if symbolSetFromSlice(nil) != nil {
		t.Fatal("nil symbols should preserve the score-all sentinel")
	}
	if got := symbolSetFromSlice([]string{}); got == nil || len(got) != 0 {
		t.Fatalf("empty symbols=%v, want explicit empty set", got)
	}
}

func TestCurrencyStrengthResearchRejectsInvalidOptionsBeforeDataLoad(t *testing.T) {
	base := strengthResearchOptions{
		From: "2022-01-01", To: "2026-08-07", Starts: "06:00", RankEnds: "09:00", Exits: "18:00",
		Gaps: ".001", BroadMins: "1", Confirms: "0", CurrencyModel: "least_squares",
		AllowPartial: true, Units: 100000, MinTradesPerYear: 75, CostMultipliers: "1,2",
		MinImprovementPerAffectedDay: 5, MinAggregateImprovement: 250,
	}
	tests := []struct {
		name, want string
		change     func(*strengthResearchOptions)
	}{
		{"fold count", "exactly four folds", func(o *strengthResearchOptions) { o.To = "2025-12-31" }},
		{"zero units", "must be positive", func(o *strengthResearchOptions) { o.Units = 0 }},
		{"zero materiality", "thresholds must be finite and positive", func(o *strengthResearchOptions) { o.MinImprovementPerAffectedDay = 0 }},
		{"singleton universe", "at least three CASH FX pairs", func(o *strengthResearchOptions) { o.Instruments = "EUR/USD" }},
		{"invalid model", "currency-model", func(o *strengthResearchOptions) { o.CurrencyModel = "magic" }},
		{"sum partial", "requires least_squares", func(o *strengthResearchOptions) { o.CurrencyModel = "sum" }},
		{"nan gap", "must be positive", func(o *strengthResearchOptions) { o.Gaps = "NaN" }},
		{"nan confirm", "must not be negative", func(o *strengthResearchOptions) { o.Confirms = "NaN" }},
		{"infinite cost", "must be positive", func(o *strengthResearchOptions) { o.CostMultipliers = "1,2,+Inf" }},
		{"duplicate cost", "duplicate", func(o *strengthResearchOptions) { o.CostMultipliers = "1,1,2" }},
		{"missing cost", "must include 1 and 2", func(o *strengthResearchOptions) { o.CostMultipliers = "1,1.5" }},
		{"invalid times", "no valid time combinations", func(o *strengthResearchOptions) { o.Starts, o.RankEnds, o.Exits = "10:00", "09:00", "08:00" }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			opts := base
			tc.change(&opts)
			err := runCurrencyStrengthResearch(context.Background(), io.Discard, io.Discard, opts)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err=%v, want containing %q", err, tc.want)
			}
		})
	}
}

func TestWriteStrengthResearchJSONAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "report.json")
	report := csresearch.Report{SchemaVersion: csresearch.SchemaVersion, GeneratedAt: time.Date(2026, 8, 9, 0, 0, 0, 0, time.UTC)}
	if err := writeStrengthResearchJSONAtomic(path, report); err != nil {
		t.Fatal(err)
	}
	blob, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(blob), `"schema_version": 1`) {
		t.Fatalf("report=%s", blob)
	}
	matches, err := filepath.Glob(filepath.Join(dir, ".currency-strength-research-*.tmp"))
	if err != nil || len(matches) != 0 {
		t.Fatalf("temporary files=%v err=%v", matches, err)
	}
}
