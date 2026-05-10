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

// TestResolveSpecialist_RiskLevelsAverageOverWinningVotersOnly is the
// regression test for Codex review on PR #342. When 3 BUY voters disagree
// with 1 SELL voter and the BUY action wins, the resulting StopLoss /
// TakeProfit must average over the BUY voters only — otherwise the SELL
// voter's opposite-direction levels corrupt the signal's risk parameters.
func TestResolveSpecialist_RiskLevelsAverageOverWinningVotersOnly(t *testing.T) {
	mk := func(role string, action Action, sl, tp float64) specialistResult {
		return specialistResult{
			Role: role,
			Response: AnalysisResponse{
				Action: action, Confidence: 0.7,
				SizePct: 2.0, StopLoss: sl, TakeProfit: tp,
				Reasoning: role + " says " + string(action),
			},
			Meta: ModelCallMeta{Provider: "test", Model: role + "-model"},
		}
	}
	results := []specialistResult{
		mk("fundamental", ActionBuy, 95.0, 110.0),
		mk("technical", ActionBuy, 95.0, 110.0),
		mk("sentiment", ActionBuy, 95.0, 110.0),
		// Dissenting SELL voter — opposite-direction stop/take. MUST be excluded
		// from the averaging step or the resulting signal would carry SL=97.5,
		// TP=105.0 instead of the BUY voters' 95.0 / 110.0.
		mk("risk_manager", ActionSell, 105.0, 90.0),
	}
	final := resolveSpecialist(results)
	if final.Action != ActionBuy {
		t.Fatalf("expected BUY (3-of-4 majority), got %q", final.Action)
	}
	if final.StopLoss != 95.0 {
		t.Errorf("StopLoss = %.4f; want 95.0 (average over BUY voters only, NOT %.4f which would include the SELL voter)", final.StopLoss, (95.0*3+105.0)/4)
	}
	if final.TakeProfit != 110.0 {
		t.Errorf("TakeProfit = %.4f; want 110.0 (average over BUY voters only, NOT %.4f which would include the SELL voter)", final.TakeProfit, (110.0*3+90.0)/4)
	}
}

// TestComposeSpecialistReasoning_FormatsActionAsString is the regression test
// for the Copilot-flagged %s-with-defined-string-type printf bug. Action is a
// `type Action string`, and using %s on it without a cast renders as
// "%!s(analysis.Action=BUY)". The reasoning string must contain the literal
// action verb (e.g. "BUY"), not the printf error fallback.
func TestComposeSpecialistReasoning_FormatsActionAsString(t *testing.T) {
	successes := []specialistResult{
		makeRoleResult("fundamental", ActionBuy, 0.80, nil),
		makeRoleResult("technical", ActionBuy, 0.75, nil),
		makeRoleResult("sentiment", ActionBuy, 0.70, nil),
		makeRoleResult("risk_manager", ActionBuy, 0.65, nil),
	}
	reasoning := composeSpecialistReasoning(successes, ActionBuy, true, &successes[3])
	if strings.Contains(reasoning, "%!") {
		t.Fatalf("reasoning contains printf error fallback (%%! marker):\n%s", reasoning)
	}
	if !strings.Contains(reasoning, "BUY") {
		t.Errorf("reasoning missing literal 'BUY' verb:\n%s", reasoning)
	}

	// Also verify the veto branch renders cleanly (uses rm.Response.Action).
	veto := []specialistResult{
		makeRoleResult("fundamental", ActionBuy, 0.70, nil),
		makeRoleResult("technical", ActionSell, 0.60, nil),
		makeRoleResult("risk_manager", ActionWait, 0.65, nil),
	}
	vReasoning := composeSpecialistReasoning(veto, ActionWait, false, &veto[2])
	if strings.Contains(vReasoning, "%!") {
		t.Fatalf("veto reasoning contains printf error fallback:\n%s", vReasoning)
	}
	if !strings.Contains(vReasoning, "WAIT") {
		t.Errorf("veto reasoning missing literal 'WAIT' verb:\n%s", vReasoning)
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
