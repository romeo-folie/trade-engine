package sweep

import (
	"encoding/json"
	"math"
	"testing"

	"github.com/romeo-folie/trade-engine/internal/backtest/orbreplay"
	"github.com/romeo-folie/trade-engine/internal/config"
)

func TestCoarseGrid_SizeMatchesGenerated(t *testing.T) {
	g := DefaultCoarseGrid()
	sets := g.GenerateCoarse()
	if len(sets) != g.Size() {
		t.Fatalf("generated %d sets; Size() = %d", len(sets), g.Size())
	}
	if g.Size() != 2880 {
		t.Errorf("default coarse grid size = %d; want 2880 (5 confirmations incl. retest)", g.Size())
	}
}

func TestCoarseGrid_SetsAreUniqueAndDeterministic(t *testing.T) {
	a := DefaultCoarseGrid().GenerateCoarse()
	b := DefaultCoarseGrid().GenerateCoarse()
	if len(a) != len(b) {
		t.Fatalf("nondeterministic count %d vs %d", len(a), len(b))
	}
	seen := make(map[string]int, len(a))
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("order differs at %d: %+v vs %+v", i, a[i], b[i])
		}
		seen[a[i].Hash()]++
	}
	for h, n := range seen {
		if n != 1 {
			t.Errorf("hash %s appears %d times; sets must be unique", h, n)
		}
	}
}

// Resolve must switch each gate on via BOTH the variant flag and the per-instrument config, and
// clear it otherwise — the engine ANDs the two, so a mismatch silently disables the gate.
func TestParamSet_Resolve_GatesConfigAndVariant(t *testing.T) {
	base := config.ForexORBConfig{Session: "london", RangeMinutes: 15, RiskReward: 2.0}
	insts := []config.Instrument{{Symbol: "EUR/USD", SecType: "CASH", MinLotSize: 5000}}

	// All gates ON.
	on := ParamSet{Session: "newyork", RangeMinutes: 30, Confirmation: "close_3m", RiskReward: 1.5,
		MaxHoldMinutes: 120, DirectionElig: true, RangeATRFilter: true, AdaptiveExits: true}
	cfg, out, v := on.Resolve(base, insts)
	if cfg.Session != "newyork" || cfg.RangeMinutes != 30 || cfg.RiskReward != 1.5 {
		t.Errorf("cfg not applied: %+v", cfg)
	}
	i := out[0]
	if i.BreakoutConfirmation != "close_3m" || !i.DirectionEligibility || i.MaxHoldMinutes != 120 {
		t.Errorf("instrument gates not applied: %+v", i)
	}
	if i.MinRangeATR != coarseMinRangeATR || i.MaxRangeATR != coarseMaxRangeATR {
		t.Errorf("ATR band not applied: %v/%v", i.MinRangeATR, i.MaxRangeATR)
	}
	if i.BreakEvenR != coarseBreakEvenR || i.TrailATRMult != coarseTrailATRMult {
		t.Errorf("adaptive preset not applied: %+v", i)
	}
	if !v.ApplyConfirmation || !v.ApplyDirection || !v.ApplyEntryFilters || !v.ApplyAdaptiveExit || v.MaxHoldMinutes != 120 {
		t.Errorf("variant flags not set: %+v", v)
	}

	// All gates OFF (stop_touch = no confirmation).
	off := ParamSet{Session: "london", RangeMinutes: 15, Confirmation: "stop_touch", RiskReward: 2.0}
	_, out2, v2 := off.Resolve(base, insts)
	i2 := out2[0]
	if i2.BreakoutConfirmation != "stop_touch" || i2.DirectionEligibility || i2.MinRangeATR != 0 || i2.BreakEvenR != 0 {
		t.Errorf("gates not cleared when off: %+v", i2)
	}
	if v2.ApplyConfirmation || v2.ApplyDirection || v2.ApplyEntryFilters || v2.ApplyAdaptiveExit {
		t.Errorf("variant flags not cleared when off: %+v", v2)
	}
}

func TestParamSet_Resolve_DoesNotMutateInput(t *testing.T) {
	insts := []config.Instrument{{Symbol: "EUR/USD", BreakoutConfirmation: "stop_touch"}}
	ps := ParamSet{Session: "london", Confirmation: "close_1m", DirectionElig: true}
	ps.Resolve(config.ForexORBConfig{}, insts)
	if insts[0].BreakoutConfirmation != "stop_touch" || insts[0].DirectionEligibility {
		t.Errorf("Resolve mutated the caller's instruments: %+v", insts[0])
	}
}

func TestRobustnessScore_TradeFloorDominates(t *testing.T) {
	// A thin 5-trade set with great expectancy must rank below a 200-trade positive set.
	thin := orbreplay.Metrics{Trades: 5, ExpectancyR: 0.5, ProfitFactor: 3.0}
	thick := orbreplay.Metrics{Trades: 200, ExpectancyR: 0.10, ProfitFactor: 1.3}
	if RobustnessScore(thin, MinTradesFloor) >= RobustnessScore(thick, MinTradesFloor) {
		t.Errorf("thin set outranked thick: thin=%.3f thick=%.3f",
			RobustnessScore(thin, MinTradesFloor), RobustnessScore(thick, MinTradesFloor))
	}
}

// The trade floor is HARD: even a sub-floor all-winner at high RR must rank below every
// above-floor candidate (the exact scenario the linear penalty let through).
func TestRobustnessScore_SubFloorNeverTopsAboveFloor(t *testing.T) {
	subFloorStar := orbreplay.Metrics{Trades: 5, ExpectancyR: 2.0, ProfitFactor: math.Inf(1)}
	aboveFloorMeh := orbreplay.Metrics{Trades: 200, ExpectancyR: 0.10, ProfitFactor: 1.3}
	justBelow := orbreplay.Metrics{Trades: 99, ExpectancyR: 1.5, ProfitFactor: 3.0}

	star := RobustnessScore(subFloorStar, MinTradesFloor)
	meh := RobustnessScore(aboveFloorMeh, MinTradesFloor)
	near := RobustnessScore(justBelow, MinTradesFloor)
	if star >= meh {
		t.Errorf("5-trade star (%.3f) must rank below 200-trade above-floor (%.3f)", star, meh)
	}
	if near >= meh {
		t.Errorf("99-trade set (%.3f) must rank below any above-floor set (%.3f) — floor is hard", near, meh)
	}
	// Within the sub-floor band, more trades still ranks higher.
	if RobustnessScore(orbreplay.Metrics{Trades: 90, ExpectancyR: 0.2}, MinTradesFloor) <=
		RobustnessScore(orbreplay.Metrics{Trades: 20, ExpectancyR: 0.2}, MinTradesFloor) {
		t.Error("within sub-floor band, more trades should rank higher")
	}
}

func TestRobustnessScore_EmptyIsWorst(t *testing.T) {
	if got := RobustnessScore(orbreplay.Metrics{Trades: 0}, MinTradesFloor); !math.IsInf(got, -1) {
		t.Errorf("empty score = %v; want -Inf", got)
	}
}

func TestPassesAcceptanceGates(t *testing.T) {
	good := orbreplay.Metrics{Trades: 150, ExpectancyR: 0.12, ProfitFactor: 1.30}
	if !PassesAcceptanceGates(good, MinTradesFloor) {
		t.Error("good metrics should pass gates")
	}
	for _, bad := range []orbreplay.Metrics{
		{Trades: 50, ExpectancyR: 0.20, ProfitFactor: 2.0},  // too few trades
		{Trades: 150, ExpectancyR: 0.02, ProfitFactor: 2.0}, // expectancy too low
		{Trades: 150, ExpectancyR: 0.20, ProfitFactor: 1.05}, // PF too low
	} {
		if PassesAcceptanceGates(bad, MinTradesFloor) {
			t.Errorf("metrics should fail gates: %+v", bad)
		}
	}
}

// validate mode reads survivors back from sweep_results.resolved_parameters, so ParamSet.JSON()
// must round-trip through json.Unmarshal into an identical ParamSet.
func TestParamSet_JSONRoundTrip(t *testing.T) {
	for _, p := range DefaultCoarseGrid().GenerateCoarse()[:50] {
		var got ParamSet
		if err := json.Unmarshal(p.JSON(), &got); err != nil {
			t.Fatalf("unmarshal %s: %v", p.Label(), err)
		}
		if got != p {
			t.Errorf("round-trip mismatch:\n in=%+v\nout=%+v", p, got)
		}
		if got.Hash() != p.Hash() {
			t.Errorf("hash changed across round-trip for %s", p.Label())
		}
	}
}

func TestParamSet_Resolve_CloseFrac(t *testing.T) {
	base := config.ForexORBConfig{Session: "london", RangeMinutes: 15, RiskReward: 2.0}
	insts := []config.Instrument{{Symbol: "EUR/USD", SecType: "CASH", MinLotSize: 5000}}

	// CloseFrac set → threshold copied onto the instrument.
	on := ParamSet{Session: "london", RangeMinutes: 15, Confirmation: "stop_touch", RiskReward: 2.0, CloseFrac: 0.65}
	_, out, _ := on.Resolve(base, insts)
	if out[0].CloseFracThreshold != 0.65 {
		t.Errorf("CloseFracThreshold = %v; want 0.65", out[0].CloseFracThreshold)
	}

	// CloseFrac 0 → guard off on the instrument.
	off := ParamSet{Session: "london", RangeMinutes: 15, Confirmation: "stop_touch", RiskReward: 2.0}
	_, out2, _ := off.Resolve(base, insts)
	if out2[0].CloseFracThreshold != 0 {
		t.Errorf("CloseFracThreshold = %v; want 0 when unset", out2[0].CloseFracThreshold)
	}
}

func TestCoarseGrid_CloseFracDimensionMultiplies(t *testing.T) {
	g := DefaultCoarseGrid()
	baseSize := g.Size()
	g.CloseFrac = []float64{0, 0.6, 0.65}
	if got := g.Size(); got != baseSize*3 {
		t.Errorf("Size with 3 CloseFrac levels = %d; want %d", got, baseSize*3)
	}
	if len(g.GenerateCoarse()) != g.Size() {
		t.Errorf("GenerateCoarse count %d != Size %d", len(g.GenerateCoarse()), g.Size())
	}

	// An empty CloseFrac defaults to one level (guard off), not zero sets.
	g2 := DefaultCoarseGrid()
	g2.CloseFrac = nil
	if len(g2.GenerateCoarse()) != g2.Size() || len(g2.GenerateCoarse()) == 0 {
		t.Errorf("empty CloseFrac should default to 1 level: gen=%d size=%d", len(g2.GenerateCoarse()), g2.Size())
	}
}
