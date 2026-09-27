package prompts

import (
	"strings"
	"testing"
)

func TestConfiguredRiskPolicyInEveryStockPrompt(t *testing.T) {
	policy := RiskPolicy{MaxTickerExposurePct: 10, MaxPerTicker: 2, MinRiskReward: 1.5}
	for _, momentum := range []bool{false, true} {
		name := "generalist"
		if momentum {
			name = "momentum"
		}
		t.Run(name, func(t *testing.T) {
			system, _ := BuildStockPromptWithAnalysisContext(
				"AAPL", []string{"momentum"}, "1h", nil, Indicators{}, nil, Portfolio{},
				"", nil, "", momentum, nil, nil, TimeframeRolePrimaryThesis, nil, policy,
			)
			assertConfiguredRiskPolicy(t, system)
		})
	}
	for _, role := range []string{"fundamental", "technical", "sentiment", "risk_manager"} {
		t.Run(role, func(t *testing.T) {
			system, _ := BuildRolePromptWithAnalysisContext(
				role, "AAPL", []string{"trend_following"}, "1h", nil, Indicators{}, nil, Portfolio{},
				"", nil, "", nil, nil, TimeframeRolePrimaryThesis, nil, policy,
			)
			assertConfiguredRiskPolicy(t, system)
		})
	}
	t.Run("sequential risk review", func(t *testing.T) {
		system, _ := BuildRiskManagerReviewPromptWithAnalysisContext(
			"AAPL", []string{"trend_following"}, "1h", nil, Indicators{}, nil, Portfolio{},
			"", nil, "", "technical: BUY", nil, nil, TimeframeRolePrimaryThesis, nil, policy,
		)
		assertConfiguredRiskPolicy(t, system)
	})
}

func assertConfiguredRiskPolicy(t *testing.T, system string) {
	t.Helper()
	for _, want := range []string{
		"proposed incremental BUY order size",
		"Combined long exposure in this ticker after the proposed BUY must not exceed 10%",
		"Subtract existing same-ticker exposure before recommending a pyramid add",
		"At most 2 open long positions in this ticker; this limits entry count",
		"If R:R < 1.5",
		"Only emit BUY if R:R >= 1.5",
	} {
		if !strings.Contains(system, want) {
			t.Errorf("configured prompt missing %q", want)
		}
	}
	for _, stale := range []string{"R:R < 1.2", "R:R >= 1.2", "exceed 5%", "1–3% range"} {
		if strings.Contains(system, stale) {
			t.Errorf("configured prompt has stale policy %q", stale)
		}
	}
}

func TestRiskPolicyFallbackForDirectCallers(t *testing.T) {
	system := buildSystem("", "", nil)
	for _, want := range []string{
		"Combined long exposure in this ticker after the proposed BUY must not exceed 5%",
		"If R:R < 1.2",
	} {
		if !strings.Contains(system, want) {
			t.Errorf("fallback prompt missing %q", want)
		}
	}
	if strings.Contains(system, "At most 0 open long positions") {
		t.Error("zero-value policy must not invent an entry-count limit")
	}
}

func TestRiskPolicyPreservesConfiguredZeroLimits(t *testing.T) {
	system := buildSystem("", "", nil, RiskPolicy{Configured: true})
	for _, want := range []string{
		"size_pct is the proposed incremental BUY order size as a percentage of portfolio equity; it must be between 0 and 0",
		"Combined long exposure in this ticker after the proposed BUY must not exceed 0%",
		"No minimum R:R floor is configured",
	} {
		if !strings.Contains(system, want) {
			t.Errorf("explicit-zero prompt missing %q", want)
		}
	}
	for _, stale := range []string{"exceed 5%", "If R:R < 1.2"} {
		if strings.Contains(system, stale) {
			t.Errorf("explicit-zero prompt acquired fallback %q", stale)
		}
	}
}
