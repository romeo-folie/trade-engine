package classifier_test

import (
	"context"
	"testing"

	"github.com/romeo-folie/trade-engine/internal/analysis/classifier"
	"github.com/romeo-folie/trade-engine/internal/config"
)

func testCfg() config.ClassifierConfig {
	return config.ClassifierConfig{
		Mode:                  "active",
		RSIDeltaThreshold:     3.0,
		EMAGapDeltaPct:        0.5,
		PriceMoveThresholdPct: 0.75,
		HoldCooldownMin:       120,
		NewsNoveltyEnabled:    true,
	}
}

func base() classifier.TickState {
	return classifier.TickState{
		Ticker:       "AAPL",
		Timeframe:    "1d",
		Strategy:     "trend_following",
		LastDecision: "BUY",
		ElapsedMin:   180,
		HoldCooldown: 120,
	}
}

func TestRuleBasedPriceMove(t *testing.T) {
	rb := classifier.NewRuleBased(testCfg())
	s := base()

	// at threshold → ANALYZE
	s.PriceMovesPct = 0.75
	res, err := rb.Classify(context.Background(), s)
	if err != nil || res.Decision != classifier.DecisionAnalyze {
		t.Fatalf("expected ANALYZE at threshold, got %v %v", res.Decision, err)
	}
	if res.ReasonCode != "PRICE_MOVE" {
		t.Errorf("expected reason PRICE_MOVE, got %q", res.ReasonCode)
	}

	// below threshold → SKIP
	s.PriceMovesPct = 0.74
	res, _ = rb.Classify(context.Background(), s)
	if res.Decision != classifier.DecisionSkip {
		t.Errorf("expected SKIP below price threshold, got %v", res.Decision)
	}
}

func TestRuleBasedRSIDelta(t *testing.T) {
	rb := classifier.NewRuleBased(testCfg())
	s := base()

	s.RSIDelta = 3.0
	res, _ := rb.Classify(context.Background(), s)
	if res.Decision != classifier.DecisionAnalyze || res.ReasonCode != "RSI_DELTA" {
		t.Errorf("expected ANALYZE/RSI_DELTA, got %v/%q", res.Decision, res.ReasonCode)
	}

	s.RSIDelta = 2.9
	res, _ = rb.Classify(context.Background(), s)
	if res.Decision != classifier.DecisionSkip {
		t.Errorf("expected SKIP below RSI threshold, got %v", res.Decision)
	}
}

func TestRuleBasedEMAGap(t *testing.T) {
	rb := classifier.NewRuleBased(testCfg())
	s := base()

	s.EMAGapDelta = 0.5
	res, _ := rb.Classify(context.Background(), s)
	if res.Decision != classifier.DecisionAnalyze || res.ReasonCode != "EMA_GAP_DELTA" {
		t.Errorf("expected ANALYZE/EMA_GAP_DELTA, got %v/%q", res.Decision, res.ReasonCode)
	}

	s.EMAGapDelta = 0.49
	res, _ = rb.Classify(context.Background(), s)
	if res.Decision != classifier.DecisionSkip {
		t.Errorf("expected SKIP below EMA gap threshold, got %v", res.Decision)
	}
}

func TestRuleBasedNewsNovelty(t *testing.T) {
	rb := classifier.NewRuleBased(testCfg())
	s := base()
	s.LastNewsHash = "old"
	s.NewsHash = "new"

	res, _ := rb.Classify(context.Background(), s)
	if res.Decision != classifier.DecisionAnalyze || res.ReasonCode != "NEWS_NOVELTY" {
		t.Errorf("expected ANALYZE/NEWS_NOVELTY, got %v/%q", res.Decision, res.ReasonCode)
	}

	// same hash → SKIP
	s.NewsHash = "old"
	res, _ = rb.Classify(context.Background(), s)
	if res.Decision != classifier.DecisionSkip {
		t.Errorf("expected SKIP on same hash, got %v", res.Decision)
	}

	// novelty disabled → SKIP even on changed hash
	cfg := testCfg()
	cfg.NewsNoveltyEnabled = false
	rb2 := classifier.NewRuleBased(cfg)
	s.NewsHash = "new"
	res, _ = rb2.Classify(context.Background(), s)
	if res.Decision != classifier.DecisionSkip {
		t.Errorf("expected SKIP when novelty disabled, got %v", res.Decision)
	}
}

func TestRuleBasedHoldCooldown(t *testing.T) {
	rb := classifier.NewRuleBased(testCfg())
	s := base()
	s.LastDecision = "HOLD"
	s.ElapsedMin = 60 // under cooldown

	res, _ := rb.Classify(context.Background(), s)
	if res.Decision != classifier.DecisionSkip || res.ReasonCode != "HOLD_COOLDOWN" {
		t.Errorf("expected SKIP/HOLD_COOLDOWN, got %v/%q", res.Decision, res.ReasonCode)
	}
	if res.Deltas["hold_elapsed_min"] != 60 {
		t.Errorf("expected hold_elapsed_min=60, got %v", res.Deltas["hold_elapsed_min"])
	}

	// expired cooldown + another trigger → ANALYZE
	s.ElapsedMin = 121
	s.RSIDelta = 5.0
	res, _ = rb.Classify(context.Background(), s)
	if res.Decision != classifier.DecisionAnalyze {
		t.Errorf("expected ANALYZE after cooldown expired, got %v", res.Decision)
	}
}

func TestRuleBasedAllBelowThreshold(t *testing.T) {
	rb := classifier.NewRuleBased(testCfg())
	s := base()
	// All below threshold, no news change.
	s.RSIDelta = 1.0
	s.EMAGapDelta = 0.1
	s.PriceMovesPct = 0.2
	s.NewsHash = s.LastNewsHash

	res, _ := rb.Classify(context.Background(), s)
	if res.Decision != classifier.DecisionSkip || res.ReasonCode != "NO_MATERIAL_CHANGE" {
		t.Errorf("expected SKIP/NO_MATERIAL_CHANGE, got %v/%q", res.Decision, res.ReasonCode)
	}
}

func TestRuleBasedDeltasReturned(t *testing.T) {
	rb := classifier.NewRuleBased(testCfg())
	s := base()
	s.RSIDelta = 5.0
	s.EMAGapDelta = 1.0
	s.PriceMovesPct = 2.0

	res, _ := rb.Classify(context.Background(), s)
	if res.Deltas["rsi_delta"] != 5.0 {
		t.Errorf("unexpected rsi_delta in deltas: %v", res.Deltas["rsi_delta"])
	}
	if res.Deltas["price_move_pct"] != 2.0 {
		t.Errorf("unexpected price_move_pct in deltas: %v", res.Deltas["price_move_pct"])
	}
}
