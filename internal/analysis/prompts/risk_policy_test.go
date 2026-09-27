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
		"Remaining ticker room = max(0, 10% × portfolio equity - existing same-ticker notional)",
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
		"size_pct is the proposed incremental BUY order size as a percentage of available cash; it must be between 0 and 0",
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

func TestRiskPolicyNamesActualSizingBaseAndEquityCap(t *testing.T) {
	for _, tc := range []struct{ basis, want string }{
		{SizeBasisCash, "percentage of available cash"},
		{SizeBasisEquity, "percentage of portfolio equity"},
	} {
		t.Run(tc.basis, func(t *testing.T) {
			policy := RiskPolicy{Configured: true, MaxTickerExposurePct: 10, SizeBasis: tc.basis}
			rendered := RenderRiskPolicy(policy)
			for _, want := range []string{tc.want, "Remaining ticker room = max(0, 10% × portfolio equity - existing same-ticker notional)", "choose size_pct so the proposed order notional fits that room"} {
				if !strings.Contains(rendered, want) {
					t.Errorf("%s policy missing %q", tc.basis, want)
				}
			}
		})
	}
}

func TestPortfolioPromptShowsEquityAndOpenEntryCount(t *testing.T) {
	user := buildUser("AAPL", "1d", nil, Indicators{}, nil, Portfolio{
		CashAvailable: 200, CashKnown: true, PortfolioEquity: 1000,
		OpenPositions: []Position{{Ticker: "AAPL", Side: "long", Quantity: 2, AvgCostBasis: 100, EntryCount: 2, EntryCountKnown: true}},
	}, "", nil, "", nil, nil)
	for _, want := range []string{"Portfolio equity: ≈$1000", "Cash available: ≈$200", "AAPL: long 2.0000 @ 100.00  open entries=2"} {
		if !strings.Contains(user, want) {
			t.Errorf("portfolio prompt missing %q", want)
		}
	}
}

func TestPortfolioPromptKeepsSmallCashBalanceVisible(t *testing.T) {
	user := buildUser("AAPL", "1d", nil, Indicators{}, nil,
		Portfolio{CashKnown: true, CashAvailable: 40, PortfolioEquity: 940}, "", nil, "", nil, nil)
	if !strings.Contains(user, "Cash available: ≈$40") || !strings.Contains(user, "Portfolio equity: ≈$940") {
		t.Fatalf("small account balance was over-rounded: %s", user)
	}
	if strings.Contains(user, "fully deployed") {
		t.Fatal("positive small cash balance was treated as fully deployed")
	}
}
