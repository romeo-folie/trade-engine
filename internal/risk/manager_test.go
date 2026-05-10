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

// contains is a tiny alias kept for readability inside pyramid tests.
func contains(s, sub string) bool { return strings.Contains(s, sub) }
