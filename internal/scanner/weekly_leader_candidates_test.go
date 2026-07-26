package scanner

import "testing"

func TestMergeWeeklyLeaderCandidatesPreservesIndependentSources(t *testing.T) {
	member := true
	existing := []SwingCandidate{{
		Ticker: "NVDA", Price: 150, Volume: 1_000_000,
		Sources:      []string{"mover"},
		SourceScores: map[string]float64{"mover": 8.5},
	}}
	leaders := []SwingCandidate{{
		Ticker: "NVDA", Return1M: 12, Return3M: 25, Return6M: 40,
		DollarVolume:   500_000_000,
		Sources:        []string{"weekly_leader"},
		SourceScores:   map[string]float64{"weekly_leader": 0.91},
		UniverseMember: &member, Sector: "Technology", Industry: "Semiconductors",
		WeeklyLeaderRunID: "run-1", UniverseVersionID: "universe-1",
		GraphContext: "Weekly leader outcome.",
	}}
	got := mergeWeeklyLeaderCandidates(existing, leaders)
	if len(got) != 1 {
		t.Fatalf("merged candidates = %d, want 1", len(got))
	}
	candidate := got[0]
	if len(candidate.Sources) != 2 ||
		candidate.SourceScores["mover"] != 8.5 ||
		candidate.SourceScores["weekly_leader"] != 0.91 {
		t.Fatalf("source evidence lost: %+v", candidate)
	}
	if candidate.Price != 150 || candidate.Volume != 1_000_000 {
		t.Fatalf("intraday source evidence overwritten: %+v", candidate)
	}
	if candidate.WeeklyLeaderRunID != "run-1" ||
		candidate.UniverseVersionID != "universe-1" ||
		candidate.Return6M != 40 {
		t.Fatalf("weekly leader evidence missing: %+v", candidate)
	}
}

func TestWeeklyLeaderSourceAddsTraceableStrategyTag(t *testing.T) {
	strategies := candidateStrategies("momentum", false)
	if hasSource([]string{"mover", "weekly_leader"}, "weekly_leader") {
		strategies = appendUniqueStrategy(strategies, "weekly_leader")
	}
	if len(strategies) != 2 || strategies[0] != "momentum" ||
		strategies[1] != "weekly_leader" {
		t.Fatalf("strategies = %v", strategies)
	}
}
