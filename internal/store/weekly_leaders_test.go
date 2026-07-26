package store

import "testing"

func TestBuildWeeklyLeaderBreadthPreservesZeroDenominator(t *testing.T) {
	rows := buildWeeklyLeaderBreadth("run-1", nil, nil)
	if len(rows) != 1 {
		t.Fatalf("breadth rows = %d, want one overall row", len(rows))
	}
	overall := rows[0]
	if overall.DimensionType != "overall" || overall.DimensionValue != "all" ||
		overall.EligibleCount != 0 || overall.QualifiedCount != 0 ||
		overall.Breadth != 0 || overall.Coverage != 0 {
		t.Fatalf("zero-denominator breadth = %+v", overall)
	}
}

func TestBuildWeeklyLeaderTransitionsExplainsUniverseExitAndDataFailure(t *testing.T) {
	oldScore, oldRank := 0.9, 1
	previous := map[string]WeeklyLeaderQualification{
		"EXIT": {
			Ticker: "EXIT", CompositeScore: &oldScore, Rank: &oldRank,
		},
		"FAIL": {
			Ticker: "FAIL", CompositeScore: &oldScore, Rank: &oldRank,
		},
	}
	current := map[string]WeeklyLeaderQualification{
		"FAIL": {
			Ticker: "FAIL", Outcome: "failed", FailureStage: "provider_history",
		},
	}
	transitions := buildWeeklyLeaderTransitions("current", "previous", previous, current)
	reasons := make(map[string]string)
	for _, transition := range transitions {
		reasons[transition.Ticker] = transition.Reason
	}
	if reasons["EXIT"] != "universe_exit" || reasons["FAIL"] != "data_failure" {
		t.Fatalf("transition reasons = %v", reasons)
	}
}
