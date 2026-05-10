package risk

import (
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/config"
)

func defaultCfg() config.RiskConfig {
	return config.RiskConfig{
		ConfidenceThreshold: 0.65,
		MinRiskReward:       2.0,
		MaxPositionPct:      5.0,
		MaxOpenPositions:    10,
		MaxPerTicker:        2,
		DailyLossLimitPct:   3.0,
		MaxDrawdownPct:      10.0,
		CooldownAfterLoss:   30,
	}
}

func baseInput() CheckInput {
	return CheckInput{
		Cfg:            defaultCfg(),
		Action:         "BUY",
		Confidence:     0.80,
		SizePct:        2.0,
		SignalPrice:    100.0,
		StopLoss:       95.0,
		TakeProfit:     110.0,
		PortfolioValue: 10000,
		Cash:           5000,
		OpenPositions:  2,
		DailyPnL:       0,
		PeakValue:      10000,
	}
}

// Helpers

func assertPasses(t *testing.T, fn func(CheckInput) CheckResult, in CheckInput) {
	t.Helper()
	r := fn(in)
	if !r.Passed {
		t.Fatalf("%s: expected pass, got fail: %s", r.Name, r.Reason)
	}
}

func assertFails(t *testing.T, fn func(CheckInput) CheckResult, in CheckInput) CheckResult {
	t.Helper()
	r := fn(in)
	if r.Passed {
		t.Fatalf("%s: expected fail, got pass", r.Name)
	}
	return r
}

// ── Confidence ────────────────────────────────────────────────────────────────

func TestConfidence_Pass(t *testing.T) {
	assertPasses(t, checkConfidence, baseInput())
}

func TestConfidence_Fail(t *testing.T) {
	in := baseInput()
	in.Confidence = 0.50
	r := assertFails(t, checkConfidence, in)
	if r.Name != "confidence_threshold" {
		t.Fatalf("wrong check name: %s", r.Name)
	}
}

// ── Tiered confidence (#317) ─────────────────────────────────────────────────

func tieredCfg() config.RiskConfig {
	cfg := defaultCfg()
	cfg.MinRiskReward = 1.4
	cfg.ConfidenceTiers = []config.ConfidenceTier{
		{RRMin: 2.0, ConfMin: 0.60},
		{RRMin: 1.4, ConfMin: 0.65},
	}
	return cfg
}

// High R:R band: R:R >= 2.0 → conf floor 0.60. A 0.62 confidence trade that
// would have been rejected by the 0.65 static floor must now pass.
func TestConfidenceTiered_HighRR_LooserConfFloor(t *testing.T) {
	in := baseInput()
	in.Cfg = tieredCfg()
	in.Confidence = 0.62
	// reward=10, risk=5 → R:R=2.0 (matches first tier exactly).
	assertPasses(t, checkConfidenceTiered, in)
}

// Mid R:R band (1.4 ≤ R:R < 2.0): conf floor stays at 0.65. A 0.62 confidence
// trade is rejected with the new tier-aware reason string.
func TestConfidenceTiered_MidRR_StricterConfFloor(t *testing.T) {
	in := baseInput()
	in.Cfg = tieredCfg()
	in.Confidence = 0.62
	// reward=7.5, risk=5 → R:R=1.5 (mid tier).
	in.TakeProfit = 107.5
	r := assertFails(t, checkConfidenceTiered, in)
	if !contains(r.Reason, "tier-required 0.65") || !contains(r.Reason, "R:R 1.50") {
		t.Fatalf("expected reason to mention tier-required floor and R:R, got %q", r.Reason)
	}
}

// Mid R:R band: 0.66 confidence clears the 0.65 tier floor.
func TestConfidenceTiered_MidRR_PassesAtTierFloor(t *testing.T) {
	in := baseInput()
	in.Cfg = tieredCfg()
	in.Confidence = 0.66
	in.TakeProfit = 107.5 // R:R = 1.5
	assertPasses(t, checkConfidenceTiered, in)
}

// Below every tier AND below MinRiskReward: tiered check defers to
// checkRiskReward by passing, so the operator gets the accurate R:R reason
// rather than a confidence reason that masks the real cause (#317 review).
func TestConfidenceTiered_BelowMinRR_DefersToRiskRewardCheck(t *testing.T) {
	in := baseInput()
	in.Cfg = tieredCfg() // MinRiskReward = 1.4
	in.TakeProfit = 105.0 // reward=5, risk=5 → R:R = 1.0 (below MinRiskReward)

	// High confidence is irrelevant — pass either way.
	in.Confidence = 0.66
	assertPasses(t, checkConfidenceTiered, in)

	// Low confidence must STILL pass the tiered check so the downstream
	// checkRiskReward can produce the real "R:R 1.00 < minimum 1.40" reason.
	in.Confidence = 0.50
	assertPasses(t, checkConfidenceTiered, in)

	// Sanity check: the deferred checkRiskReward does indeed reject.
	r := assertFails(t, checkRiskReward, in)
	if !contains(r.Reason, "R:R 1.00") || !contains(r.Reason, "minimum 1.40") {
		t.Fatalf("expected R:R reason from checkRiskReward, got %q", r.Reason)
	}
}

// Misconfiguration safety net: if a tier list leaves a gap *above*
// MinRiskReward (e.g. MinRiskReward=1.0, lowest tier RRMin=2.0, R:R=1.5),
// the static ConfidenceThreshold floor still gates so signals don't slip
// through ungated.
func TestConfidenceTiered_BelowAllTiersAboveMinRR_FallsBackToStatic(t *testing.T) {
	in := baseInput()
	cfg := defaultCfg()
	cfg.MinRiskReward = 1.0 // lower than the lowest tier floor
	cfg.ConfidenceTiers = []config.ConfidenceTier{
		{RRMin: 2.0, ConfMin: 0.60},
	}
	in.Cfg = cfg
	in.TakeProfit = 107.5 // R:R = 1.5 (above MinRiskReward, below lowest tier)

	in.Confidence = 0.66 // ≥ static 0.65 → pass
	assertPasses(t, checkConfidenceTiered, in)

	in.Confidence = 0.50 // < static 0.65 → fail with static reason
	r := assertFails(t, checkConfidenceTiered, in)
	if !contains(r.Reason, "threshold 0.65") {
		t.Fatalf("expected static-floor reason, got %q", r.Reason)
	}
}

// Empty tier list must preserve the pre-#317 behaviour exactly.
func TestConfidenceTiered_EmptyTiersBackwardsCompat(t *testing.T) {
	in := baseInput()
	in.Cfg.ConfidenceTiers = nil // explicit
	in.Confidence = 0.50
	r := assertFails(t, checkConfidenceTiered, in)
	if !contains(r.Reason, "threshold") || contains(r.Reason, "tier-required") {
		t.Fatalf("empty tiers should produce static-floor reason, got %q", r.Reason)
	}

	in.Confidence = 0.80
	assertPasses(t, checkConfidenceTiered, in)
}

// Without a stop/TP, R:R can't be computed; tiered check falls back to static.
func TestConfidenceTiered_NoStopTPFallsBackToStatic(t *testing.T) {
	in := baseInput()
	in.Cfg = tieredCfg()
	in.StopLoss = 0
	in.TakeProfit = 0
	in.Confidence = 0.62 // < 0.65 static
	r := assertFails(t, checkConfidenceTiered, in)
	if !contains(r.Reason, "threshold 0.65") {
		t.Fatalf("expected static-floor reason when R:R unknown, got %q", r.Reason)
	}
}

// CheckAll with tiered config short-circuits with the tiered reason string,
// confirming the wiring uses checkConfidenceTiered (not checkConfidence).
func TestCheckAll_UsesTieredConfidence(t *testing.T) {
	in := baseInput()
	in.Cfg = tieredCfg()
	in.Confidence = 0.62
	in.TakeProfit = 107.5 // R:R = 1.5 → tier requires 0.65
	results, ok := CheckAll(in)
	if ok {
		t.Fatal("expected tiered confidence to block")
	}
	if !contains(results[0].Reason, "tier-required") {
		t.Fatalf("expected tiered reason from CheckAll, got %q", results[0].Reason)
	}
}

func contains(haystack, needle string) bool {
	return len(needle) == 0 || (len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// ── Position ownership ────────────────────────────────────────────────────────

func TestPositionOwnership_BuyAlwaysPasses(t *testing.T) {
	in := baseInput()
	in.Action = "BUY"
	in.HoldsLong = false
	assertPasses(t, checkPositionOwnership, in)
}

func TestPositionOwnership_SellWithSufficientPosition(t *testing.T) {
	in := baseInput()
	in.Action = "SELL"
	in.Ticker = "HIMS"
	in.HoldsLong = true
	// proposedQty = (5000 * 2.0/100) / 100.0 = 1.0; held = 10 → ok
	in.LongQuantity = 10
	assertPasses(t, checkPositionOwnership, in)
}

func TestPositionOwnership_SellExceedsHeldQuantity(t *testing.T) {
	in := baseInput()
	in.Action = "SELL"
	in.Ticker = "HIMS"
	in.HoldsLong = true
	// proposedQty = (5000 * 2.0/100) / 100.0 = 1.0; held = 0.5 → oversell
	in.LongQuantity = 0.5
	r := assertFails(t, checkPositionOwnership, in)
	if r.Name != "position_ownership" {
		t.Fatalf("wrong check name: %s", r.Name)
	}
}

func TestPositionOwnership_SellWithoutPosition(t *testing.T) {
	in := baseInput()
	in.Action = "SELL"
	in.Ticker = "HIMS"
	in.HoldsLong = false
	r := assertFails(t, checkPositionOwnership, in)
	if r.Name != "position_ownership" {
		t.Fatalf("wrong check name: %s", r.Name)
	}
}

// ── Risk/Reward ───────────────────────────────────────────────────────────────

func TestRiskReward_Pass(t *testing.T) {
	// reward=10, risk=5 → R:R=2.0
	assertPasses(t, checkRiskReward, baseInput())
}

func TestRiskReward_Fail(t *testing.T) {
	in := baseInput()
	in.TakeProfit = 101 // reward=1, risk=5 → R:R=0.2
	assertFails(t, checkRiskReward, in)
}

func TestRiskReward_SkipWhenNoStopLoss(t *testing.T) {
	in := baseInput()
	in.StopLoss = 0
	in.TakeProfit = 0
	assertPasses(t, checkRiskReward, in)
}

// ── Position size ─────────────────────────────────────────────────────────────

func TestPositionSize_Pass(t *testing.T) {
	assertPasses(t, checkPositionSize, baseInput())
}

func TestPositionSize_Fail(t *testing.T) {
	in := baseInput()
	in.SizePct = 6.0
	assertFails(t, checkPositionSize, in)
}

// ── Max open positions ────────────────────────────────────────────────────────

func TestMaxOpenPositions_Pass(t *testing.T) {
	assertPasses(t, checkMaxOpenPositions, baseInput())
}

func TestMaxOpenPositions_Fail(t *testing.T) {
	in := baseInput()
	in.OpenPositions = 10
	assertFails(t, checkMaxOpenPositions, in)
}

// ── Max per ticker (#312) ─────────────────────────────────────────────────────

func TestMaxPerTicker_PassWhenBelowLimit(t *testing.T) {
	in := baseInput()
	in.Ticker = "JMIA"
	in.OpenByTicker = map[string]int{"JMIA": 1}
	assertPasses(t, checkMaxPerTicker, in)
}

func TestMaxPerTicker_FailWhenAtLimit(t *testing.T) {
	in := baseInput()
	in.Ticker = "JMIA"
	in.OpenByTicker = map[string]int{"JMIA": 2}
	r := assertFails(t, checkMaxPerTicker, in)
	if r.Name != "max_per_ticker" {
		t.Fatalf("wrong check name: %s", r.Name)
	}
}

func TestMaxPerTicker_DisabledWhenZero(t *testing.T) {
	in := baseInput()
	in.Cfg.MaxPerTicker = 0
	in.Ticker = "JMIA"
	// Even an absurdly large count should pass when the cap is disabled.
	in.OpenByTicker = map[string]int{"JMIA": 99}
	assertPasses(t, checkMaxPerTicker, in)
}

func TestMaxPerTicker_NilMapTreatedAsZero(t *testing.T) {
	in := baseInput()
	in.Ticker = "JMIA"
	in.OpenByTicker = nil
	assertPasses(t, checkMaxPerTicker, in)
}

// Mirrors the engine-side aggregation: 8 open rows for the same
// (ticker, direction) collapse to a single distinct position and a
// per-ticker count that depends on the action.
//
// The implementation only counts a row toward byTicker when the action
// matches the row's direction (BUY+long or SELL+short). Subtests pin
// each action variant so a future change to the gating logic can't
// silently regress HOLD/WAIT to count as live additions.
func TestDistinctPositionCounting_EngineAggregation(t *testing.T) {
	type openTrade struct {
		Ticker    string
		Direction string
	}
	openTrades := []openTrade{
		{"JMIA", "long"}, {"JMIA", "long"}, {"JMIA", "long"}, {"JMIA", "long"},
		{"JMIA", "long"}, {"JMIA", "long"}, {"JMIA", "long"}, {"JMIA", "long"},
	}

	aggregate := func(action string) (positions map[string]struct{}, byTicker map[string]int) {
		positions = make(map[string]struct{}, len(openTrades))
		byTicker = make(map[string]int, len(openTrades))
		for _, ot := range openTrades {
			key := ot.Ticker + "|" + ot.Direction
			positions[key] = struct{}{}
			if (action == "BUY" && ot.Direction == "long") ||
				(action == "SELL" && ot.Direction == "short") {
				byTicker[ot.Ticker]++
			}
		}
		return positions, byTicker
	}

	t.Run("BUY", func(t *testing.T) {
		positions, byTicker := aggregate("BUY")
		if got := len(positions); got != 1 {
			t.Fatalf("distinct positions: want 1, got %d", got)
		}
		if got := byTicker["JMIA"]; got != 8 {
			t.Fatalf("byTicker[JMIA]: want 8, got %d", got)
		}

		// Feeding into CheckAll: max_open_positions sees 1 (passes), but the
		// per-ticker cap of 2 catches the burst.
		in := baseInput()
		in.Ticker = "JMIA"
		in.OpenPositions = len(positions)
		in.OpenByTicker = byTicker
		results, ok := CheckAll(in)
		if ok {
			t.Fatalf("expected CheckAll to block, got pass; results=%+v", results)
		}
		if reason := BlockReason(results); reason == "" {
			t.Fatal("expected non-empty block reason")
		}
	})

	// SELL against 8 long rows must NOT count any of them — a sell
	// against held longs is a close, not an additional position.
	t.Run("SELL", func(t *testing.T) {
		positions, byTicker := aggregate("SELL")
		if got := len(positions); got != 1 {
			t.Fatalf("distinct positions: want 1, got %d", got)
		}
		if got := byTicker["JMIA"]; got != 0 {
			t.Fatalf("byTicker[JMIA] for SELL on longs: want 0, got %d", got)
		}
	})

	// HOLD/WAIT are non-actions: they must never inflate per-ticker
	// counts regardless of how many open rows the ticker has.
	for _, action := range []string{"HOLD", "WAIT"} {
		action := action
		t.Run(action, func(t *testing.T) {
			positions, byTicker := aggregate(action)
			if got := len(positions); got != 1 {
				t.Fatalf("distinct positions: want 1, got %d", got)
			}
			if got := byTicker["JMIA"]; got != 0 {
				t.Fatalf("byTicker[JMIA] for %s: want 0, got %d", action, got)
			}
		})
	}
}

// ── Daily loss ────────────────────────────────────────────────────────────────

func TestDailyLoss_Pass(t *testing.T) {
	assertPasses(t, checkDailyLossLimit, baseInput())
}

func TestDailyLoss_Fail(t *testing.T) {
	in := baseInput()
	in.DailyPnL = -400 // 4% of 10000 > 3% limit
	assertFails(t, checkDailyLossLimit, in)
}

// ── Max drawdown ──────────────────────────────────────────────────────────────

func TestMaxDrawdown_Pass(t *testing.T) {
	assertPasses(t, checkMaxDrawdown, baseInput())
}

func TestMaxDrawdown_Fail(t *testing.T) {
	in := baseInput()
	in.PortfolioValue = 8900 // 11% drawdown from peak 10000
	assertFails(t, checkMaxDrawdown, in)
}

// ── Cooldown ──────────────────────────────────────────────────────────────────

func TestCooldown_PassWhenNoRecentLoss(t *testing.T) {
	in := baseInput()
	in.LastLossAt = time.Time{} // zero
	assertPasses(t, checkCooldownAfterLoss, in)
}

func TestCooldown_PassWhenCooldownExpired(t *testing.T) {
	in := baseInput()
	in.LastLossAt = time.Now().Add(-45 * time.Minute) // 45m ago > 30m cooldown
	assertPasses(t, checkCooldownAfterLoss, in)
}

func TestCooldown_Fail(t *testing.T) {
	in := baseInput()
	in.LastLossAt = time.Now().Add(-5 * time.Minute) // 5m ago < 30m cooldown
	assertFails(t, checkCooldownAfterLoss, in)
}

// ── CheckAll ──────────────────────────────────────────────────────────────────

func TestCheckAll_AllPass(t *testing.T) {
	_, ok := CheckAll(baseInput())
	if !ok {
		t.Fatal("expected all checks to pass")
	}
}

func TestCheckAll_ShortCircuitsOnFirstFail(t *testing.T) {
	in := baseInput()
	in.Confidence = 0.10
	results, ok := CheckAll(in)
	if ok {
		t.Fatal("expected failure")
	}
	// Should stop after first failed check.
	if len(results) != 1 {
		t.Fatalf("expected 1 result (short-circuit), got %d", len(results))
	}
}

func TestBlockReason_ReturnsFirstFailReason(t *testing.T) {
	in := baseInput()
	in.Confidence = 0.10
	results, _ := CheckAll(in)
	reason := BlockReason(results)
	if reason == "" {
		t.Fatal("expected non-empty block reason")
	}
}
