package prompts

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/romeo-folie/trade-engine/internal/tradeprofile"
)

func testDiscoveryContext() *tradeprofile.DiscoveryContext {
	return &tradeprofile.DiscoveryContext{
		Version: tradeprofile.DiscoveryContextVersion,
		Sources: []string{"weekly_leader"}, Thesis: "Relative-strength leader with orderly support.",
		EvidenceBySource: map[string]json.RawMessage{
			"weekly_leader": json.RawMessage(`{"rank":2,"return_3m_pct":18.4}`),
		},
	}
}

func TestBuildStockPromptWithDiscoveryContextRendersStructuredProvenance(t *testing.T) {
	_, user := BuildStockPromptWithDiscoveryContext(
		"NVDA", []string{"momentum"}, "1D", nil, Indicators{}, nil, Portfolio{},
		"", nil, "", false, testDiscoveryContext(),
	)
	for _, want := range []string{"### DISCOVERY CONTEXT", "Sources: weekly_leader", "Relative-strength leader", `Evidence[weekly_leader]: {"rank":2,"return_3m_pct":18.4}`, "not as an execution lens"} {
		if !strings.Contains(user, want) {
			t.Errorf("discovery context missing %q:\n%s", want, user)
		}
	}
}

func TestBuildRolePromptWithDiscoveryContextPropagatesToSpecialists(t *testing.T) {
	_, user := BuildRolePromptWithDiscoveryContext(
		"technical", "NVDA", []string{"momentum"}, "1D", nil, Indicators{}, nil, Portfolio{},
		"", nil, "", testDiscoveryContext(),
	)
	if !strings.Contains(user, "### DISCOVERY CONTEXT") || !strings.Contains(user, "Evidence[weekly_leader]") {
		t.Fatalf("specialist prompt did not receive discovery context:\n%s", user)
	}
}
