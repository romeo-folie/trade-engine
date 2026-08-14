package prompts

import (
	"strings"
	"testing"
)

func confirmationThesis() *PrimaryThesisContext {
	return &PrimaryThesisContext{
		PrimaryTimeframe: "1D", EntryTimingTimeframe: "4H",
		Action: "BUY", Confidence: 0.84, SelectedLens: "momentum", Regime: "trending",
		RejectedLenses: map[string]string{"mean_reversion": "trend remains intact"},
		Reasoning:      "Daily breakout is holding above its base.", StopLoss: 97.5, TakeProfit: 112.0,
	}
}

func TestConfirmationContextConstrainsGeneralistPrompt(t *testing.T) {
	system, user := BuildStockPromptWithAnalysisContext(
		"NVDA", []string{"momentum", "mean_reversion"}, "4H", nil, Indicators{}, nil, Portfolio{},
		"", nil, "", false, nil, nil, TimeframeRoleEntryConfirmation, confirmationThesis(),
	)
	for _, want := range []string{
		"TIMEFRAME ROLE — ENTRY CONFIRMATION", "checks entry timing only", "do not substitute a different lens",
		"Do not emit SELL from this timing-only pass",
	} {
		if !strings.Contains(system, want) {
			t.Errorf("confirmation system prompt missing %q:\n%s", want, system)
		}
	}
	for _, want := range []string{
		"### TIMEFRAME WORKFLOW", "Role: entry_confirmation", "### PRIMARY THESIS CONTEXT",
		"Primary timeframe: 1D | entry timing timeframe: 4H", "Action: BUY | confidence: 0.84 | selected lens: momentum | regime: trending",
		"Rejected lens[mean_reversion]: trend remains intact", "Proposed stop / target: $97.50 / $112.00",
	} {
		if !strings.Contains(user, want) {
			t.Errorf("confirmation user prompt missing %q:\n%s", want, user)
		}
	}
}

func TestConfirmationContextReachesSpecialistAndRiskPrompts(t *testing.T) {
	thesis := confirmationThesis()
	for _, role := range []string{"technical", "risk_manager"} {
		t.Run(role, func(t *testing.T) {
			system, user := BuildRolePromptWithAnalysisContext(
				role, "NVDA", []string{"momentum"}, "4H", nil, Indicators{}, nil, Portfolio{},
				"", nil, "", nil, nil, TimeframeRoleEntryConfirmation, thesis,
			)
			if !strings.Contains(system, "TIMEFRAME ROLE — ENTRY CONFIRMATION") ||
				!strings.Contains(system, "Do not emit SELL from this timing-only pass") ||
				!strings.Contains(user, "### PRIMARY THESIS CONTEXT") {
				t.Fatalf("%s prompt lost confirmation constraints:\nSYSTEM:\n%s\nUSER:\n%s", role, system, user)
			}
		})
	}

	_, riskUser := BuildRiskManagerReviewPromptWithAnalysisContext(
		"NVDA", []string{"momentum"}, "4H", nil, Indicators{}, nil, Portfolio{},
		"", nil, "", "technical: BUY", nil, nil, TimeframeRoleEntryConfirmation, thesis,
	)
	if !strings.Contains(riskUser, "COMPLETED SPECIALIST FINDINGS") || !strings.Contains(riskUser, "### PRIMARY THESIS CONTEXT") {
		t.Fatalf("risk review did not keep both primary context and findings:\n%s", riskUser)
	}
}

func TestPrimaryThesisRoleDoesNotRequirePriorContext(t *testing.T) {
	system, user := BuildStockPromptWithAnalysisContext(
		"NVDA", []string{"momentum"}, "1D", nil, Indicators{}, nil, Portfolio{},
		"", nil, "", false, nil, nil, TimeframeRolePrimaryThesis, nil,
	)
	if strings.Contains(system, "ENTRY CONFIRMATION") || !strings.Contains(user, "Role: primary_thesis") {
		t.Fatalf("primary thesis prompt rendered incorrect workflow:\nSYSTEM:\n%s\nUSER:\n%s", system, user)
	}
}
