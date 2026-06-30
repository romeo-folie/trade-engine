package orb

import (
	"math"
	"testing"

	"github.com/romeo-folie/trade-engine/internal/config"
)

func baseBracket(side string) BracketParams {
	// R = |1.1000 - 1.0950| = 0.0050; base target at RR=2 → 1.1100 (buy).
	if side == "sell" {
		return BracketParams{Ticker: "EUR/USD", Side: "sell", EntryStop: 1.0950, StopLoss: 1.1000, TakeProfit: 1.0850, Qty: 10000, SecType: "CASH"}
	}
	return BracketParams{Ticker: "EUR/USD", Side: "buy", EntryStop: 1.1000, StopLoss: 1.0950, TakeProfit: 1.1100, Qty: 10000, SecType: "CASH"}
}

// No partial configured → no split; the base bracket is returned unchanged as the runner.
func TestScaleOutBrackets_Disabled(t *testing.T) {
	_, runner, split := ScaleOutBrackets(baseBracket("buy"), config.Instrument{})
	if split || runner != baseBracket("buy") {
		t.Errorf("disabled: split=%v runner=%+v; want false + unchanged base", split, runner)
	}
}

// Basic long split: partial closes the configured fraction at partial_take_profit_r, the
// runner takes the rest at runner_take_profit_r; quantities sum to the base size.
func TestScaleOutBrackets_SplitLong(t *testing.T) {
	inst := config.Instrument{PartialTakeProfitR: 1.0, PartialFraction: 0.4, RunnerTakeProfitR: 2.0, MinLotSize: 1000}
	partial, runner, split := ScaleOutBrackets(baseBracket("buy"), inst)
	if !split {
		t.Fatal("expected split=true")
	}
	if partial.Qty != 4000 || runner.Qty != 6000 || partial.Qty+runner.Qty != 10000 {
		t.Errorf("qty split = %d/%d; want 4000/6000 summing to 10000", partial.Qty, runner.Qty)
	}
	if math.Abs(partial.TakeProfit-1.1050) > 1e-9 { // entry 1.1000 + 1R(0.0050)
		t.Errorf("partial TP = %v; want 1.1050", partial.TakeProfit)
	}
	if math.Abs(runner.TakeProfit-1.1100) > 1e-9 { // entry 1.1000 + 2R(0.0050)
		t.Errorf("runner TP = %v; want 1.1100", runner.TakeProfit)
	}
	// Shared geometry preserved.
	if partial.StopLoss != runner.StopLoss || partial.EntryStop != runner.EntryStop {
		t.Error("partial and runner must share entry trigger and stop")
	}
}

// Short split mirrors the long: partial/runner targets are below the entry trigger.
func TestScaleOutBrackets_SplitShort(t *testing.T) {
	inst := config.Instrument{PartialTakeProfitR: 1.0, PartialFraction: 0.4, RunnerTakeProfitR: 2.0, MinLotSize: 1000}
	partial, runner, split := ScaleOutBrackets(baseBracket("sell"), inst)
	if !split {
		t.Fatal("expected split=true")
	}
	if math.Abs(partial.TakeProfit-1.0900) > 1e-9 { // entry 1.0950 − 1R(0.0050)
		t.Errorf("short partial TP = %v; want 1.0900", partial.TakeProfit)
	}
	if math.Abs(runner.TakeProfit-1.0850) > 1e-9 { // entry 1.0950 − 2R(0.0050)
		t.Errorf("short runner TP = %v; want 1.0850", runner.TakeProfit)
	}
}

// runner_take_profit_r unset → the runner keeps the base bracket's target.
func TestScaleOutBrackets_RunnerKeepsBaseTarget(t *testing.T) {
	inst := config.Instrument{PartialTakeProfitR: 1.0, PartialFraction: 0.5, MinLotSize: 1000}
	_, runner, split := ScaleOutBrackets(baseBracket("buy"), inst)
	if !split || math.Abs(runner.TakeProfit-1.1100) > 1e-9 {
		t.Errorf("runner TP = %v (split=%v); want base 1.1100", runner.TakeProfit, split)
	}
}

// Lot-rounding that leaves the partial or the runner with no tradeable size → no split, so
// the caller places the single base bracket rather than an untradeable sub-lot.
func TestScaleOutBrackets_TooSmallToSplit(t *testing.T) {
	// fraction 0.4 of 1000 = 400, rounds down to 0 lots at MinLotSize 1000 → partial empty.
	small := baseBracket("buy")
	small.Qty = 1000
	if _, runner, split := ScaleOutBrackets(small, config.Instrument{PartialTakeProfitR: 1, PartialFraction: 0.4, MinLotSize: 1000}); split || runner.Qty != 1000 {
		t.Errorf("sub-lot partial: split=%v runner.Qty=%d; want false + base qty 1000", split, runner.Qty)
	}
	// fraction 1.0 → partial takes everything, runner empty → no split.
	if _, _, split := ScaleOutBrackets(baseBracket("buy"), config.Instrument{PartialTakeProfitR: 1, PartialFraction: 1.0, MinLotSize: 1000}); split {
		t.Error("fraction 1.0 leaves no runner; want split=false")
	}
}

// clearEntryID removes a resolved entry from the runner / partial maps and forgets the
// ticker only once BOTH maps are empty for it — so a split-bracket ticker whose runner has
// filled but whose partial is still working stays tracked (else it could be re-placed).
func TestClearEntryID(t *testing.T) {
	s := &ORBScheduler{
		orderIDs:         map[string][2]string{"EUR/USD": {"buy-runner", ""}},
		scaleOutEntryIDs: map[string][2]string{"EUR/USD": {"buy-partial", ""}},
	}

	// Clear the runner: its slot empties, but the ticker is retained because the partial
	// is still working.
	s.clearEntryID("EUR/USD", "buy-runner")
	if _, ok := s.orderIDs["EUR/USD"]; ok {
		t.Error("runner map should have dropped the now-empty EUR/USD pair")
	}
	if ids := s.scaleOutEntryIDs["EUR/USD"]; ids[0] != "buy-partial" {
		t.Errorf("partial must be retained while still working; got %v", ids)
	}

	// Clear the partial: now nothing remains, so the ticker is fully forgotten.
	s.clearEntryID("EUR/USD", "buy-partial")
	if _, ok := s.scaleOutEntryIDs["EUR/USD"]; ok {
		t.Error("ticker should be forgotten once no working entry remains in either map")
	}
}

// A dual-sided no-partial ticker behaves exactly as before: clearing one side leaves the
// other, and the ticker is forgotten only when both sides are cleared.
func TestClearEntryID_NoPartialUnchanged(t *testing.T) {
	s := &ORBScheduler{
		orderIDs:         map[string][2]string{"EUR/USD": {"buy", "sell"}},
		scaleOutEntryIDs: map[string][2]string{},
	}
	s.clearEntryID("EUR/USD", "buy")
	if ids := s.orderIDs["EUR/USD"]; ids != [2]string{"", "sell"} {
		t.Errorf("after clearing buy: got %v; want ['' sell]", ids)
	}
	s.clearEntryID("EUR/USD", "sell")
	if _, ok := s.orderIDs["EUR/USD"]; ok {
		t.Error("ticker should be forgotten once both sides cleared")
	}
}
