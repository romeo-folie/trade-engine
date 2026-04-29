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

func TestPositionOwnership_SellWithPosition(t *testing.T) {
	in := baseInput()
	in.Action = "SELL"
	in.Ticker = "HIMS"
	in.HoldsLong = true
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
