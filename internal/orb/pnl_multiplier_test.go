package orb

import (
	"math"
	"testing"
)

// approxEq reports whether a and b are within floating-point tolerance. Premium
// arithmetic like (1.80-1.20) introduces representation error, so P&L assertions
// compare with a tolerance rather than exact equality.
func approxEq(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// TestExitPnLM_Multiplier verifies the multiplier-aware realised P&L: options use
// a ×100 contract multiplier, while shares/forex/futures (multiplier 1 or unset)
// keep the exact prior per-unit result.
func TestExitPnLM_Multiplier(t *testing.T) {
	cases := []struct {
		name            string
		side            string
		entry, exit     float64
		qty, multiplier int
		want            float64
	}{
		// 2 call contracts, $1.20 -> $1.80: (1.80-1.20) * 2 * 100 = 120
		{"option call win", "buy", 1.20, 1.80, 2, 100, 120},
		// 1 put contract held long (side buy), premium 2.00 -> 0.50: loss of 150
		{"option long loss", "buy", 2.00, 0.50, 1, 100, -150},
		// multiplier unset (0) must behave as 1 — plain shares result
		{"shares via zero multiplier", "buy", 100, 101, 5, 0, 5},
		// explicit multiplier 1 equals the legacy exitPnL
		{"shares explicit 1", "sell", 100, 98, 5, 1, 10},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ExitPnLM(tc.side, tc.entry, tc.exit, tc.qty, tc.multiplier)
			if !approxEq(got, tc.want) {
				t.Errorf("ExitPnLM(%s, %v, %v, %d, %d) = %v; want %v",
					tc.side, tc.entry, tc.exit, tc.qty, tc.multiplier, got, tc.want)
			}
		})
	}
}

// TestExitPnL_UnchangedByMultiplierRefactor pins that the legacy per-unit exitPnL
// still returns exactly its old result (multiplier 1), guarding the refactor that
// routed it through exitPnLM.
func TestExitPnL_UnchangedByMultiplierRefactor(t *testing.T) {
	if got := ExitPnL("buy", 1.10, 1.12, 5000); !approxEq(got, (1.12-1.10)*5000) {
		t.Errorf("ExitPnL buy = %v; want %v", got, (1.12-1.10)*5000)
	}
	if got := ExitPnL("sell", 1.10, 1.09, 5000); !approxEq(got, (1.10-1.09)*5000) {
		t.Errorf("ExitPnL sell = %v; want %v", got, (1.10-1.09)*5000)
	}
}
