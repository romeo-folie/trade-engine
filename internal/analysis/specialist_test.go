package analysis

import (
	"fmt"
	"strings"
	"testing"
)

// makeRoleResult is the analogue of makeResult (ensemble_test.go) but tagged
// with a specialist role so resolveSpecialist can apply the veto rule.
func makeRoleResult(role string, action Action, conf float64, err error) specialistResult {
	return specialistResult{
		Role: role,
		Response: AnalysisResponse{
			Action: action, Confidence: conf,
			SizePct: 2.0, StopLoss: 100.0, TakeProfit: 110.0,
			Reasoning: fmt.Sprintf("%s reasoning", role),
		},
		Meta: ModelCallMeta{Provider: "test", Model: role + "-model"},
		Err:  err,
	}
}

// TestResolveSpecialist covers the four resolution paths:
//   1. Unanimous (4-of-4) → that action wins via majority rule.
//   2. 3-of-4 majority → that action wins.
//   3. 2-of-2 split + risk-manager veto WAIT → WAIT wins.
//   4. 2-of-2 split + risk-manager missing → falls back to majority vote
//      (which itself resolves the tie via the conservative cascade).
func TestResolveSpecialist(t *testing.T) {
	tests := []struct {
		name     string
		results  []specialistResult
		wantAct  Action
		wantNote string // substring expected in reasoning resolution note
	}{
		{
			name: "unanimous_buy",
			results: []specialistResult{
				makeRoleResult("fundamental", ActionBuy, 0.80, nil),
				makeRoleResult("technical", ActionBuy, 0.75, nil),
				makeRoleResult("sentiment", ActionBuy, 0.70, nil),
				makeRoleResult("risk_manager", ActionBuy, 0.65, nil),
			},
			wantAct:  ActionBuy,
			wantNote: "3-of-4 majority",
		},
		{
			name: "three_of_four_buy",
			results: []specialistResult{
				makeRoleResult("fundamental", ActionBuy, 0.75, nil),
				makeRoleResult("technical", ActionBuy, 0.70, nil),
				makeRoleResult("sentiment", ActionWait, 0.60, nil),
				makeRoleResult("risk_manager", ActionBuy, 0.55, nil),
			},
			wantAct:  ActionBuy,
			wantNote: "3-of-4 majority",
		},
		{
			name: "split_with_risk_manager_wait_veto",
			results: []specialistResult{
				makeRoleResult("fundamental", ActionBuy, 0.80, nil),
				makeRoleResult("technical", ActionBuy, 0.75, nil),
				makeRoleResult("sentiment", ActionWait, 0.60, nil),
				makeRoleResult("risk_manager", ActionWait, 0.65, nil),
			},
			wantAct:  ActionWait,
			wantNote: "risk-manager veto",
		},
		{
			name: "split_with_risk_manager_hold_veto",
			results: []specialistResult{
				makeRoleResult("fundamental", ActionBuy, 0.65, nil),
				makeRoleResult("technical", ActionSell, 0.55, nil),
				makeRoleResult("sentiment", ActionWait, 0.60, nil),
				makeRoleResult("risk_manager", ActionHold, 0.70, nil),
			},
			wantAct:  ActionHold,
			wantNote: "risk-manager veto",
		},
		{
			name: "split_risk_manager_failed_falls_back_to_majority",
			results: []specialistResult{
				makeRoleResult("fundamental", ActionBuy, 0.70, nil),
				makeRoleResult("technical", ActionWait, 0.60, nil),
				// Risk-manager errored → veto unavailable → majority fallback.
				makeRoleResult("risk_manager", ActionBuy, 0.0, fmt.Errorf("timeout")),
			},
			// Two successful: BUY and WAIT — tie. majorityVote conservative
			// cascade gives WAIT priority over BUY on a tie.
			wantAct:  ActionWait,
			wantNote: "majority-vote fallback",
		},
		{
			name: "split_risk_manager_says_buy_does_not_veto_majority_fallback",
			results: []specialistResult{
				makeRoleResult("fundamental", ActionBuy, 0.70, nil),
				makeRoleResult("technical", ActionWait, 0.60, nil),
				// Risk-manager voted BUY — NOT a veto. No 3-of-4 majority,
				// so we drop to the majority-vote fallback (cascade → WAIT).
				makeRoleResult("risk_manager", ActionBuy, 0.65, nil),
			},
			wantAct:  ActionBuy, // 2 BUY + 1 WAIT → BUY has 2 votes, wins majority
			wantNote: "majority-vote fallback",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			final := resolveSpecialist(tc.results)
			if final.Action != tc.wantAct {
				t.Fatalf("action = %q, want %q\nreasoning:\n%s", final.Action, tc.wantAct, final.Reasoning)
			}
			if !strings.Contains(final.Reasoning, tc.wantNote) {
				t.Errorf("reasoning missing %q note. Got:\n%s", tc.wantNote, final.Reasoning)
			}
			// Reasoning should always tag each successful role.
			for _, r := range tc.results {
				if r.Err != nil {
					continue
				}
				tag := "[" + r.Role + "]"
				if !strings.Contains(final.Reasoning, tag) {
					t.Errorf("reasoning missing role tag %q", tag)
				}
			}
		})
	}
}

// TestResolveSpecialist_NonActionableSuppressesSizing verifies that WAIT/HOLD
// final actions zero out size_pct / stop_loss / take_profit so downstream
// execution paths do not see stray entry parameters.
func TestResolveSpecialist_NonActionableSuppressesSizing(t *testing.T) {
	results := []specialistResult{
		makeRoleResult("fundamental", ActionBuy, 0.80, nil),
		makeRoleResult("technical", ActionBuy, 0.75, nil),
		makeRoleResult("sentiment", ActionWait, 0.55, nil),
		makeRoleResult("risk_manager", ActionWait, 0.60, nil), // veto
	}
	final := resolveSpecialist(results)
	if final.Action != ActionWait {
		t.Fatalf("expected WAIT via risk-manager veto, got %q", final.Action)
	}
	if final.SizePct != 0 || final.StopLoss != 0 || final.TakeProfit != 0 {
		t.Errorf("WAIT must zero sizing/SL/TP — got size=%.2f sl=%.2f tp=%.2f",
			final.SizePct, final.StopLoss, final.TakeProfit)
	}
}

// TestResolveSpecialist_NoSuccessesYieldsHold guards the failure mode where
// every role errored — we should not crash and we must default to a safe HOLD.
func TestResolveSpecialist_NoSuccessesYieldsHold(t *testing.T) {
	results := []specialistResult{
		makeRoleResult("fundamental", ActionBuy, 0.80, fmt.Errorf("x")),
		makeRoleResult("technical", ActionBuy, 0.75, fmt.Errorf("x")),
		makeRoleResult("risk_manager", ActionWait, 0.60, fmt.Errorf("x")),
	}
	final := resolveSpecialist(results)
	if final.Action != ActionHold {
		t.Fatalf("expected HOLD when every role errored, got %q", final.Action)
	}
}
