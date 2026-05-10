package prompts

import (
	"strings"
	"testing"
)

// rrSelfFilterSentence is the literal R:R guidance sentence required by PR #334
// in every system prompt. The role-specialist prompts (Issue #323) MUST keep it.
const rrSelfFilterSentence = `R:R SELF-FILTER (MANDATORY): Compute R:R as: reward = abs(take_profit - signal_price); risk = abs(signal_price - stop_loss); R:R = reward / risk. If R:R < 1.5, set action to WAIT and do not propose entry. Only emit BUY if R:R >= 1.5.`

func TestRolePrompts_RoleSpecificMarkers(t *testing.T) {
	cases := []struct {
		role    string
		markers []string
	}{
		{"fundamental", []string{"fundamental research analyst", "earnings", "catalyst"}},
		{"technical", []string{"technical analyst", "EMA", "indicator"}},
		{"sentiment", []string{"sentiment analyst", "news", "headline"}},
		// Risk-manager: must use the words "veto" and "discipline" (the spec
		// asks the prompt to bias toward WAIT when discipline is breached).
		{"risk_manager", []string{"risk manager", "veto", "discipline"}},
	}

	for _, tc := range cases {
		t.Run(tc.role, func(t *testing.T) {
			sys, user := BuildRolePrompt(
				tc.role, "AAPL", "swing", "1h",
				nil, Indicators{}, nil, Portfolio{}, "", nil, "",
			)
			if sys == "" {
				t.Fatalf("BuildRolePrompt(%q) returned empty system prompt", tc.role)
			}
			if user == "" {
				t.Fatalf("BuildRolePrompt(%q) returned empty user message", tc.role)
			}
			for _, m := range tc.markers {
				if !strings.Contains(sys, m) {
					t.Errorf("%s prompt missing expected marker %q", tc.role, m)
				}
			}
		})
	}
}

// TestRolePrompts_RetainRRSelfFilter is the regression guard for PR #334:
// every role prompt MUST include the verbatim R:R self-filter sentence so
// the specialist mode does not weaken the entry guard added by Wave 1.
func TestRolePrompts_RetainRRSelfFilter(t *testing.T) {
	roles := []string{"fundamental", "technical", "sentiment", "risk_manager"}
	for _, role := range roles {
		t.Run(role, func(t *testing.T) {
			sys, _ := BuildRolePrompt(
				role, "AAPL", "swing", "1h",
				nil, Indicators{}, nil, Portfolio{}, "", nil, "",
			)
			if !strings.Contains(sys, rrSelfFilterSentence) {
				t.Errorf("role %q dropped the verbatim R:R self-filter sentence (PR #334 regression)", role)
			}
		})
	}
}

// TestRolePrompts_UnknownRole returns empty prompts so callers can detect
// the misconfiguration and skip the role with a WARN log instead of crashing.
func TestRolePrompts_UnknownRole(t *testing.T) {
	sys, user := BuildRolePrompt(
		"astrologer", "AAPL", "swing", "1h",
		nil, Indicators{}, nil, Portfolio{}, "", nil, "",
	)
	if sys != "" || user != "" {
		t.Fatalf("expected empty prompts for unknown role, got sys=%q user=%q", sys, user)
	}
}

// TestRolePrompts_ShareJSONSchema asserts each role prompt emits the same
// strict-JSON-only output rule so downstream parsing is unchanged.
func TestRolePrompts_ShareJSONSchema(t *testing.T) {
	roles := []string{"fundamental", "technical", "sentiment", "risk_manager"}
	for _, role := range roles {
		sys, _ := BuildRolePrompt(role, "AAPL", "swing", "1h", nil, Indicators{}, nil, Portfolio{}, "", nil, "")
		if !strings.Contains(sys, `"action":      "BUY" | "SELL" | "HOLD" | "WAIT"`) {
			t.Errorf("role %q does not emit the standard action enum schema", role)
		}
		if !strings.Contains(sys, "ONLY a valid JSON object") {
			t.Errorf("role %q dropped the strict-JSON-only output rule", role)
		}
	}
}
