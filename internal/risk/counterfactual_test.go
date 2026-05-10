package risk

import (
	"testing"

	"github.com/romeo-folie/trade-engine/internal/config"
)

// counterfactualBaseInput is a deliberately minimal CheckInput that passes
// every gate except the ones the per-test mutations target. Each test starts
// from this and mutates only the fields under examination so the gating
// asserts stay surgical (the rest of manager_test.go uses a similar pattern
// via baseInput/defaultCfg, but those default to a 2.0 R:R floor that fights
// the loose-config we're trying to validate).
func counterfactualBaseInput() CheckInput {
	return CheckInput{
		Cfg: config.RiskConfig{
			ConfidenceThreshold: 0.65,
			MinRiskReward:       1.50,
			MaxPositionPct:      5.0,
			MaxOpenPositions:    10,
			MaxPerTicker:        2,
			MaxDollarRiskPct:    1.0,
			DailyLossLimitPct:   3.0,
			MaxDrawdownPct:      10.0,
			CooldownAfterLoss:   30,
		},
		Action:         "BUY",
		Ticker:         "TEST",
		Confidence:     0.80,
		SizePct:        2.0,
		SignalPrice:    100.0,
		StopLoss:       95.0,
		TakeProfit:     110.0, // R:R = 2.0 by default
		PortfolioValue: 10000,
		Cash:           5000,
		OpenPositions:  2,
		PeakValue:      10000,
	}
}

// Confidence-blocked: strict floor 0.65, signal at 0.60, R:R 2.0 already
// good. Loose floor 0.55 admits → flipped_check = confidence_threshold.
func TestEvaluateCounterfactual_ConfidenceBlocked(t *testing.T) {
	in := counterfactualBaseInput()
	in.Confidence = 0.60

	strict, ok := CheckAll(in)
	if ok {
		t.Fatal("setup: strict CheckAll should have failed on confidence")
	}
	if r := BlockReason(strict); !contains(r, "confidence") {
		t.Fatalf("setup: expected confidence reason, got %q", r)
	}

	cf := EvaluateCounterfactual(in, strict)
	if !cf.PassedLoose {
		t.Fatalf("expected loose run to pass, got fail (%+v)", cf.LooseResults)
	}
	if cf.FlippedCheck != "confidence_threshold" {
		t.Errorf("expected flipped_check=confidence_threshold, got %q", cf.FlippedCheck)
	}
}

// R:R-blocked: strict floor 1.50, signal at R:R 1.40. Loose floor 1.30
// admits → flipped_check = risk_reward_ratio. Confidence is high enough to
// clear both tiers.
func TestEvaluateCounterfactual_RiskRewardBlocked(t *testing.T) {
	in := counterfactualBaseInput()
	// Risk = |100 - 95| = 5; reward = |107 - 100| = 7; R:R = 1.40
	in.TakeProfit = 107.0

	strict, ok := CheckAll(in)
	if ok {
		t.Fatal("setup: strict CheckAll should have failed on R:R")
	}
	if r := BlockReason(strict); !contains(r, "R:R") {
		t.Fatalf("setup: expected R:R reason, got %q", r)
	}

	cf := EvaluateCounterfactual(in, strict)
	if !cf.PassedLoose {
		t.Fatalf("expected loose run to pass, got fail (%+v)", cf.LooseResults)
	}
	if cf.FlippedCheck != "risk_reward_ratio" {
		t.Errorf("expected flipped_check=risk_reward_ratio, got %q", cf.FlippedCheck)
	}
}

// Both blocked: confidence too low AND R:R too low. The loose floors clear
// both, so the loose run still passes and reports the FIRST failure as the
// flipped check (CheckAll short-circuits at confidence — confidence is
// checked before R:R in CheckAll order).
func TestEvaluateCounterfactual_BothBlocked_LoosePasses(t *testing.T) {
	in := counterfactualBaseInput()
	in.Confidence = 0.58
	in.TakeProfit = 106.5 // R:R = 1.30 — at loose floor

	strict, ok := CheckAll(in)
	if ok {
		t.Fatal("setup: strict CheckAll should have failed")
	}

	cf := EvaluateCounterfactual(in, strict)
	if !cf.PassedLoose {
		t.Fatalf("expected loose to pass, got fail with results=%+v", cf.LooseResults)
	}
	// CheckAll order puts checkConfidenceTiered before checkRiskReward; the
	// short-circuit fails on confidence first, so that's what flipped.
	if cf.FlippedCheck != "confidence_threshold" {
		t.Errorf("expected flipped_check=confidence_threshold (first strict failure), got %q", cf.FlippedCheck)
	}
}

// Loose still fails: R:R below the loose floor (1.20 < 1.30) means even the
// counterfactual rejects. PassedLoose=false, FlippedCheck="" — the engine
// surfaces the empty string as "none" in its log line.
func TestEvaluateCounterfactual_LooseAlsoBlocked(t *testing.T) {
	in := counterfactualBaseInput()
	// R:R = 6 / 5 = 1.20 — below loose floor of 1.30
	in.TakeProfit = 106.0

	strict, ok := CheckAll(in)
	if ok {
		t.Fatal("setup: strict CheckAll should have failed")
	}

	cf := EvaluateCounterfactual(in, strict)
	if cf.PassedLoose {
		t.Fatalf("expected loose to also fail, got pass")
	}
	if cf.FlippedCheck != "" {
		t.Errorf("expected empty flipped_check when loose fails, got %q", cf.FlippedCheck)
	}
}

// Calibratable classification — state-machine checks must not trigger the
// counterfactual; calibratable + unknown checks must.
func TestIsCalibratable(t *testing.T) {
	calibratable := []string{
		"confidence_threshold", "risk_reward_ratio",
		"max_dollar_risk", "position_size_limit",
		"unknown_future_check", // default-on per spec
	}
	nonCalibratable := []string{
		"max_open_positions", "position_ownership", "daily_loss_limit",
		"max_drawdown", "cooldown_after_loss", "max_per_ticker",
		"pyramid_policy", "pyramid_disabled",
	}
	for _, c := range calibratable {
		if !IsCalibratable(c) {
			t.Errorf("expected %q to be calibratable", c)
		}
	}
	for _, c := range nonCalibratable {
		if IsCalibratable(c) {
			t.Errorf("expected %q NOT to be calibratable", c)
		}
	}
	if IsCalibratable("") {
		t.Error("empty string should not be calibratable")
	}
}
