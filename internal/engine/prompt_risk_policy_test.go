package engine

import (
	"testing"

	"github.com/romeo-folie/trade-engine/internal/config"
)

func TestPromptRiskPolicyUsesActiveRiskConfig(t *testing.T) {
	cfg := config.RiskConfig{
		MaxPositionPct: 10,
		MaxPerTicker:   2,
		MinRiskReward:  1.5,
	}
	policy := promptRiskPolicy(cfg)
	if policy.MaxTickerExposurePct != 10 || policy.MaxPerTicker != 2 || policy.MinRiskReward != 1.5 {
		t.Fatalf("prompt risk policy = %+v, want 10%% / 2 / 1.5", policy)
	}
}
