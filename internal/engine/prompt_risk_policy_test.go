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
	policy := promptRiskPolicy(cfg, true)
	if policy.MaxTickerExposurePct != 10 || policy.MaxPerTicker != 2 || policy.MinRiskReward != 1.5 {
		t.Fatalf("prompt risk policy = %+v, want 10%% / 2 / 1.5", policy)
	}
	if !policy.Configured {
		t.Fatal("active engine policy must be marked configured")
	}
	if policy.SizeBasis != "equity" {
		t.Fatalf("Alpaca stock entry basis = %q, want equity", policy.SizeBasis)
	}
	if cashPolicy := promptRiskPolicy(cfg, false); cashPolicy.SizeBasis != "cash" {
		t.Fatalf("non-broker entry basis = %q, want cash", cashPolicy.SizeBasis)
	}
}
