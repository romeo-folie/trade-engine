package risk

import (
	"strings"
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
		MaxDollarRiskPct:    1.0,
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
	reason := BlockReason(results)
	if !contains(reason, "tier-required") {
		t.Fatalf("expected tiered reason from CheckAll, got %q", reason)
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

func TestPositionOwnership_SellSmallPositionStillPasses(t *testing.T) {
	// Since #436 a SELL closes the HELD quantity, so a held position
	// smaller than the signal's cash-derived size must NOT block the exit
	// (the old oversell check did exactly that).
	in := baseInput()
	in.Action = "SELL"
	in.Ticker = "HIMS"
	in.HoldsLong = true
	in.LongQuantity = 0.5
	assertPasses(t, checkPositionOwnership, in)
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

// Inverted long setup: stop sits above entry. With math.Abs alone this would
// produce a positive R:R and pass the gate, then the engine's long-stop
// trigger (price <= stop) would fire immediately at execution. The geometry
// guard rejects upfront with an actionable reason.
func TestRiskReward_BuyWithStopAboveSignal_Fails(t *testing.T) {
	in := baseInput()
	in.Action = "BUY"
	in.SignalPrice = 100
	in.StopLoss = 105
	in.TakeProfit = 110
	r := assertFails(t, checkRiskReward, in)
	if !strings.Contains(r.Reason, "invalid SL/TP geometry for BUY") {
		t.Fatalf("expected geometry reason, got %q", r.Reason)
	}
}

func TestRiskReward_BuyWithTakeProfitBelowSignal_Fails(t *testing.T) {
	in := baseInput()
	in.Action = "BUY"
	in.SignalPrice = 100
	in.StopLoss = 95
	in.TakeProfit = 98
	r := assertFails(t, checkRiskReward, in)
	if !strings.Contains(r.Reason, "invalid SL/TP geometry for BUY") {
		t.Fatalf("expected geometry reason, got %q", r.Reason)
	}
}

func TestRiskReward_BuyValidGeometry_Passes(t *testing.T) {
	in := baseInput()
	in.Action = "BUY"
	in.SignalPrice = 100
	in.StopLoss = 95
	in.TakeProfit = 110
	assertPasses(t, checkRiskReward, in)
	rr, ok := computeRR(in)
	if !ok {
		t.Fatalf("expected computeRR to succeed for valid BUY geometry")
	}
	if rr != 2.0 {
		t.Fatalf("expected R:R=2.0, got %.2f", rr)
	}
}

// SELL is exit-only in this codebase. Analyst SELL signals can ship with
// long-perspective stop/target geometry; we deliberately keep the legacy
// math.Abs semantics for SELL so legitimate exits aren't blocked by the new
// BUY-only geometry guard.
func TestRiskReward_SellWithMath_Abs_PreservedBehavior(t *testing.T) {
	in := baseInput()
	in.Action = "SELL"
	in.SignalPrice = 100
	in.StopLoss = 95
	in.TakeProfit = 110
	assertPasses(t, checkRiskReward, in)
	rr, ok := computeRR(in)
	if !ok {
		t.Fatalf("expected computeRR to succeed for SELL with math.Abs semantics")
	}
	if rr != 2.0 {
		t.Fatalf("expected R:R=2.0 via math.Abs for SELL, got %.2f", rr)
	}
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

// ── Max dollar at risk (#320) ─────────────────────────────────────────────────

func TestMaxDollarRisk_PassWellUnderCap(t *testing.T) {
	// baseInput: qty = (2/100)*5000/100 = 1; risk per share = 5; $@risk = 5
	// pct = 0.05% << 1.0% cap → pass
	assertPasses(t, checkMaxDollarRisk, baseInput())
}

func TestMaxDollarRisk_AtCapExactlyPasses(t *testing.T) {
	in := baseInput()
	// Cash=10000, SizePct=2, SignalPrice=100 → qty=2
	// |100-50|=50; $@risk = 100; pct = 100/10000*100 = 1.00% == cap → pass
	in.Cash = 10000
	in.SignalPrice = 100.0
	in.StopLoss = 50.0
	assertPasses(t, checkMaxDollarRisk, in)
}

func TestMaxDollarRisk_OneBasisPointOverFails(t *testing.T) {
	in := baseInput()
	// Cash=10000, SizePct=2, SignalPrice=100 → qty=2
	// |100-49.495|=50.505; $@risk = 101.01; pct = 1.0101% → rounds to 1.01%
	// Genuine 1 bp over the 1.00% cap; block reason prints "1.01% > 1.00%".
	in.Cash = 10000
	in.SignalPrice = 100.0
	in.StopLoss = 49.495
	r := assertFails(t, checkMaxDollarRisk, in)
	if r.Name != "max_dollar_risk" {
		t.Fatalf("wrong check name: %s", r.Name)
	}
	// Block reason format: "$-at-risk %.2f%% > limit %.2f%%"
	want := "$-at-risk 1.01% > limit 1.00%"
	if r.Reason != want {
		t.Fatalf("block reason mismatch:\n  got:  %q\n  want: %q", r.Reason, want)
	}
}

func TestMaxDollarRisk_PortfolioValueZeroPasses(t *testing.T) {
	in := baseInput()
	in.PortfolioValue = 0
	in.Cash = 10000
	in.SignalPrice = 100.0
	in.StopLoss = 50.0 // would be at cap if portfolio were nonzero
	assertPasses(t, checkMaxDollarRisk, in)
}

func TestMaxDollarRisk_DisabledWhenZero(t *testing.T) {
	in := baseInput()
	in.Cfg.MaxDollarRiskPct = 0
	in.SignalPrice = 100.0
	in.StopLoss = 1.0 // wildly over a 1% cap, but cap is disabled
	assertPasses(t, checkMaxDollarRisk, in)
}

// Mirrors the engine's actual sizing path: position is sized off Cash,
// not PortfolioValue. With most cash already deployed, an equity-based
// qty estimate would overstate $@risk and produce a false block. The
// cash-based estimate keeps the gate consistent with what the engine
// will actually trade.
func TestMaxDollarRisk_UsesCashNotPortfolioValue(t *testing.T) {
	in := baseInput()
	// PortfolioValue=100k, Cash=20k (most deployed), SizePct=5, stop 10% from price.
	// Cash-based qty = (5/100)*20000/100 = 10 → $@risk = 10 * 10 = 100
	// pct = 100/100000 * 100 = 0.10% << 1.0% cap → pass.
	//
	// Equity-based qty (the prior bug) = (5/100)*100000/100 = 50 → $@risk = 500
	// pct = 0.50% — still under cap here, but the multiplier is 5×; the test
	// pins the cash-based formula by asserting a passing reason that matches
	// the smaller estimate.
	in.PortfolioValue = 100000
	in.Cash = 20000
	in.SizePct = 5.0
	in.SignalPrice = 100.0
	in.StopLoss = 90.0
	assertPasses(t, checkMaxDollarRisk, in)
}

func TestMaxDollarRisk_SkipWhenStopLossZero(t *testing.T) {
	// Mirrors checkRiskReward semantics: no stop → nothing to measure → pass.
	in := baseInput()
	in.StopLoss = 0
	in.TakeProfit = 0
	assertPasses(t, checkMaxDollarRisk, in)
}

// SELL signals are exit-only (checkPositionOwnership blocks SELL without an
// open long). The analyst prompt requests stop_loss/take_profit on SELLs too,
// so the entry-risk cap must skip them — otherwise legitimate exits with a
// far stop would be blocked.
func TestMaxDollarRisk_SkipsForSell(t *testing.T) {
	in := baseInput()
	in.Action = "SELL"
	// Stop far enough that the BUY path would fail: $@risk = 2 * 50 = 100,
	// pct = 100/10000*100 = 1.00% (at cap) and we push it well over.
	in.Cash = 10000
	in.SignalPrice = 100.0
	in.StopLoss = 1.0 // wildly over the 1% cap if this check ran
	assertPasses(t, checkMaxDollarRisk, in)
}

// Guards a hole in checkRiskReward: when TakeProfit is zero, that check is
// skipped, so a StopLoss == SignalPrice (zero risk distance) wouldn't be
// caught upstream. checkMaxDollarRisk fails it explicitly, mirroring the
// same defensive pattern checkRiskReward uses for "stop equals signal price".
func TestMaxDollarRisk_FailsWhenStopEqualsSignal(t *testing.T) {
	in := baseInput()
	in.SignalPrice = 100.0
	in.StopLoss = 100.0
	in.TakeProfit = 0 // checkRiskReward would skip this case entirely
	// Confirm the load-bearing assumption: checkRiskReward does NOT catch this.
	if rr := checkRiskReward(in); !rr.Passed {
		t.Fatalf("precondition: expected checkRiskReward to skip when TP==0, got fail: %s", rr.Reason)
	}
	r := assertFails(t, checkMaxDollarRisk, in)
	if r.Name != "max_dollar_risk" {
		t.Fatalf("wrong check name: %s", r.Name)
	}
	want := "stop-loss equals signal price (zero risk, invalid)"
	if r.Reason != want {
		t.Fatalf("block reason mismatch:\n  got:  %q\n  want: %q", r.Reason, want)
	}
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
	// CheckAll order (post #324): position_ownership → pyramid_policy →
	// confidence_threshold → … Action=BUY in baseInput with no open long, so
	// ownership and pyramid both pass as cheap no-ops, then the confidence
	// check fails and short-circuits. Expect exactly 3 results.
	if len(results) != 3 {
		t.Fatalf("expected 3 results (ownership pass + pyramid pass + confidence fail), got %d", len(results))
	}
	if results[0].Name != "position_ownership" || !results[0].Passed {
		t.Fatalf("expected position_ownership to pass first, got %+v", results[0])
	}
	if results[1].Name != "pyramid_policy" || !results[1].Passed {
		t.Fatalf("expected pyramid_policy to pass second, got %+v", results[1])
	}
	if results[2].Name != "confidence_threshold" || results[2].Passed {
		t.Fatalf("expected confidence_threshold to fail third, got %+v", results[2])
	}
}

// SELL signals on tickers we don't hold should now block on
// position_ownership before any other gate gets a chance to flag confidence,
// R:R, or sizing complaints (#319). Verifies the reorder.
func TestCheckAll_SellWithoutPositionFailsFirst(t *testing.T) {
	in := baseInput()
	in.Action = "SELL"
	in.Ticker = "HIMS"
	in.HoldsLong = false
	// Also break R:R and confidence — these should not be the reported reason.
	in.Confidence = 0.10
	in.TakeProfit = 100.5 // tiny reward → bad R:R if it ran
	results, ok := CheckAll(in)
	if ok {
		t.Fatal("expected failure")
	}
	if len(results) != 1 {
		t.Fatalf("expected ownership to be the only result, got %d: %+v", len(results), results)
	}
	if results[0].Name != "position_ownership" {
		t.Fatalf("expected first failure to be position_ownership, got %s", results[0].Name)
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

// ── Pyramiding (#324) ─────────────────────────────────────────────────────────

func TestPyramidPolicy_NoExistingLong_Passes(t *testing.T) {
	in := baseInput()
	in.Action = "BUY"
	in.HoldsLong = false
	in.Cfg.PyramidEnabled = false // even when disabled, fresh entries pass
	assertPasses(t, checkPyramidPolicy, in)
}

func TestPyramidPolicy_SellAlwaysPasses(t *testing.T) {
	in := baseInput()
	in.Action = "SELL"
	in.HoldsLong = true
	in.Cfg.PyramidEnabled = false
	assertPasses(t, checkPyramidPolicy, in)
}

func TestPyramidPolicy_DisabledRejectsExistingLong(t *testing.T) {
	in := baseInput()
	in.Action = "BUY"
	in.Ticker = "AAPL"
	in.HoldsLong = true
	in.LongQuantity = 10
	in.LongAvgEntryPrice = 100
	in.SignalPrice = 110 // up 10%, would otherwise pyramid
	in.Cfg.PyramidEnabled = false
	r := assertFails(t, checkPyramidPolicy, in)
	if !contains(r.Reason, "pyramid_disabled") {
		t.Fatalf("expected reason to include 'pyramid_disabled', got: %s", r.Reason)
	}
}

func TestPyramidPolicy_EnabledAndUp6Pct_Passes(t *testing.T) {
	in := baseInput()
	in.Action = "BUY"
	in.Ticker = "AAPL"
	in.HoldsLong = true
	in.LongQuantity = 10
	in.LongAvgEntryPrice = 100
	in.SignalPrice = 106 // +6%
	in.Cfg.PyramidEnabled = true
	in.Cfg.PyramidMinGainPct = 5.0
	assertPasses(t, checkPyramidPolicy, in)
}

func TestPyramidPolicy_EnabledButFlat_Rejects(t *testing.T) {
	in := baseInput()
	in.Action = "BUY"
	in.Ticker = "AAPL"
	in.HoldsLong = true
	in.LongQuantity = 10
	in.LongAvgEntryPrice = 100
	in.SignalPrice = 100 // 0%
	in.Cfg.PyramidEnabled = true
	in.Cfg.PyramidMinGainPct = 5.0
	r := assertFails(t, checkPyramidPolicy, in)
	if !contains(r.Reason, "pyramid_min_gain_not_met") {
		t.Fatalf("expected reason to include 'pyramid_min_gain_not_met', got: %s", r.Reason)
	}
}

func TestPyramidPolicy_EnabledButLosing_Rejects(t *testing.T) {
	in := baseInput()
	in.Action = "BUY"
	in.HoldsLong = true
	in.LongQuantity = 10
	in.LongAvgEntryPrice = 100
	in.SignalPrice = 95 // -5%
	in.Cfg.PyramidEnabled = true
	in.Cfg.PyramidMinGainPct = 5.0
	assertFails(t, checkPyramidPolicy, in)
}

func TestPyramidPolicy_EnabledMissingPrice_Rejects(t *testing.T) {
	in := baseInput()
	in.Action = "BUY"
	in.HoldsLong = true
	in.LongQuantity = 10
	in.LongAvgEntryPrice = 0 // unknown
	in.SignalPrice = 100
	in.Cfg.PyramidEnabled = true
	in.Cfg.PyramidMinGainPct = 5.0
	assertFails(t, checkPyramidPolicy, in)
}

// Gain criteria met but the existing canonical stop has drifted at or above
// the current signal price (e.g. a fast gap down before monitorPositions
// runs). The new lot's stop would be tightened to that out-of-range value
// and the position would close immediately — block the pyramid attempt
// outright with pyramid_stop_above_price.
func TestPyramidPolicy_StopAtOrAbovePrice_Rejects(t *testing.T) {
	in := baseInput()
	in.Action = "BUY"
	in.Ticker = "AAPL"
	in.HoldsLong = true
	in.LongQuantity = 10
	in.LongAvgEntryPrice = 100
	in.SignalPrice = 106 // +6%, gain met
	in.LongMaxStop = 108 // stop above market (e.g. trailing stop ratcheted up)
	in.Cfg.PyramidEnabled = true
	in.Cfg.PyramidMinGainPct = 5.0
	r := assertFails(t, checkPyramidPolicy, in)
	if !contains(r.Reason, "pyramid_stop_above_price") {
		t.Fatalf("expected reason to include 'pyramid_stop_above_price', got: %s", r.Reason)
	}
}

// Boundary: stop exactly equal to the signal price should also reject — the
// new lot's stop would equal entry, immediate stop-out on first tick.
func TestPyramidPolicy_StopEqualPrice_Rejects(t *testing.T) {
	in := baseInput()
	in.Action = "BUY"
	in.Ticker = "AAPL"
	in.HoldsLong = true
	in.LongQuantity = 10
	in.LongAvgEntryPrice = 100
	in.SignalPrice = 106
	in.LongMaxStop = 106
	in.Cfg.PyramidEnabled = true
	in.Cfg.PyramidMinGainPct = 5.0
	r := assertFails(t, checkPyramidPolicy, in)
	if !contains(r.Reason, "pyramid_stop_above_price") {
		t.Fatalf("expected reason to include 'pyramid_stop_above_price', got: %s", r.Reason)
	}
}

// Sanity: a stop strictly below the signal price still passes (normal case).
func TestPyramidPolicy_StopBelowPrice_Passes(t *testing.T) {
	in := baseInput()
	in.Action = "BUY"
	in.Ticker = "AAPL"
	in.HoldsLong = true
	in.LongQuantity = 10
	in.LongAvgEntryPrice = 100
	in.SignalPrice = 110
	in.LongMaxStop = 102 // healthy: between entry and price
	in.Cfg.PyramidEnabled = true
	in.Cfg.PyramidMinGainPct = 5.0
	assertPasses(t, checkPyramidPolicy, in)
}

func TestCheckAll_PyramidCapHonoured(t *testing.T) {
	// Pyramid criteria met but per-ticker cap reached → blocks naturally.
	in := baseInput()
	in.Action = "BUY"
	in.Ticker = "AAPL"
	in.HoldsLong = true
	in.LongQuantity = 10
	in.LongAvgEntryPrice = 100
	in.SignalPrice = 110
	in.StopLoss = 105   // tightened stop above original entry
	in.TakeProfit = 130 // generous TP keeps R:R > 2 so we land on the cap
	in.Cfg.PyramidEnabled = true
	in.Cfg.PyramidMinGainPct = 5.0
	in.Cfg.MaxPerTicker = 2
	in.OpenByTicker = map[string]int{"AAPL": 2}
	results, ok := CheckAll(in)
	if ok {
		t.Fatal("expected CheckAll to fail when per-ticker cap reached")
	}
	if reason := BlockReason(results); !contains(reason, "per-ticker") {
		t.Fatalf("expected per-ticker block reason, got: %s", reason)
	}
}

func TestCheckAll_PyramidEnabledAndUp_Passes(t *testing.T) {
	// End-to-end: pyramid criteria met, capacity available → all checks pass.
	in := baseInput()
	in.Action = "BUY"
	in.Ticker = "AAPL"
	in.HoldsLong = true
	in.LongQuantity = 10
	in.LongAvgEntryPrice = 100
	in.SignalPrice = 110
	in.StopLoss = 105
	in.TakeProfit = 130
	in.Cfg.PyramidEnabled = true
	in.Cfg.PyramidMinGainPct = 5.0
	in.Cfg.MaxPerTicker = 2
	in.OpenByTicker = map[string]int{"AAPL": 1}
	if _, ok := CheckAll(in); !ok {
		t.Fatal("expected CheckAll to pass for valid pyramid signal")
	}
}

func TestCheckAll_PyramidDisabled_BlocksWithReason(t *testing.T) {
	in := baseInput()
	in.Action = "BUY"
	in.Ticker = "AAPL"
	in.HoldsLong = true
	in.LongQuantity = 10
	in.LongAvgEntryPrice = 100
	in.SignalPrice = 110
	in.StopLoss = 105
	in.TakeProfit = 130
	in.Cfg.PyramidEnabled = false
	results, ok := CheckAll(in)
	if ok {
		t.Fatal("expected CheckAll to fail when pyramid disabled")
	}
	if reason := BlockReason(results); !contains(reason, "pyramid_disabled") {
		t.Fatalf("expected reason to include 'pyramid_disabled', got: %s", reason)
	}
}

// IsSellOnlyStrategy backs the engine's pre-LLM short-circuit (#319). The
// canonical SELL-only names (currently a small reserved set) must return
// true; the default analyst strategies must not, since they emit BUY *or*
// SELL depending on context and the LLM call is still required.
func TestIsSellOnlyStrategy(t *testing.T) {
	sellOnly := []string{"sell_only", "exit_signal", "close_position"}
	for _, s := range sellOnly {
		if !IsSellOnlyStrategy(s) {
			t.Errorf("expected %q to be sell-only", s)
		}
	}
	dualDirection := []string{
		"trend_following", "mean_reversion", "momentum", "news_sentiment",
		"", "TREND_FOLLOWING", "unknown",
	}
	for _, s := range dualDirection {
		if IsSellOnlyStrategy(s) {
			t.Errorf("did not expect %q to be sell-only", s)
		}
	}
}

// NoPositionBlockReason is the canonical reason string both the post-LLM
// gate and the pre-LLM short-circuit write so dashboards can pattern-match
// once. The check fixates on the substring "no open long position" which
// is what existing PR #306 tests already assert against indirectly.
func TestNoPositionBlockReason_FormatStable(t *testing.T) {
	reason := NoPositionBlockReason("HIMS")
	if reason == "" {
		t.Fatal("expected non-empty reason")
	}
	for _, want := range []string{"HIMS", "no open long position", "cannot sell"} {
		if !strings.Contains(reason, want) {
			t.Errorf("expected reason to contain %q, got %q", want, reason)
		}
	}
}

func TestPositionSize_SellSkipsSizeCap(t *testing.T) {
	// Since #436 a SELL's executed size is the held quantity, not the
	// analyst's size_pct — an oversized proposal must not block the exit.
	in := baseInput()
	in.Action = "SELL"
	in.HoldsLong = true
	in.SizePct = in.Cfg.MaxPositionPct + 10
	assertPasses(t, checkPositionSize, in)
}
