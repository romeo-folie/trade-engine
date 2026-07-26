package scanner

import (
	"strings"
	"testing"
	"time"
)

func TestMergeWeeklyLeaderCandidatesPreservesIndependentSources(t *testing.T) {
	member := true
	existing := []SwingCandidate{{
		Ticker: "NVDA", Price: 150, Volume: 1_000_000,
		Sources:      []string{"mover"},
		SourceScores: map[string]float64{"mover": 8.5},
	}}
	leaders := []SwingCandidate{{
		Ticker: "NVDA", Return1M: 12, Return3M: 25, Return6M: 40,
		DollarVolume: 500_000_000, MarketCapMM: 3_500_000,
		Sources:        []string{"weekly_leader"},
		SourceScores:   map[string]float64{"weekly_leader": 0.91},
		UniverseMember: &member, Sector: "Technology", Industry: "Semiconductors",
		WeeklyLeaderRunID: "run-1", WeeklyLeaderClaimToken: "claim-1",
		UniverseVersionID:    "universe-1",
		WeeklyLeaderEvidence: "rank 4, composite score 0.910 across the full eligible universe",
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
		candidate.WeeklyLeaderClaimToken != "claim-1" ||
		candidate.UniverseVersionID != "universe-1" ||
		candidate.Return6M != 40 ||
		candidate.MarketCapMM != 3_500_000 ||
		candidate.WeeklyLeaderEvidence == "" {
		t.Fatalf("weekly leader evidence missing: %+v", candidate)
	}
}

func TestWeeklyLeaderEvidenceIsNotRenderedAsGraphRelationship(t *testing.T) {
	candidate := SwingCandidate{
		Ticker: "NVDA", Price: 150,
		Sources:              []string{"weekly_leader"},
		WeeklyLeaderEvidence: "rank 4, composite score 0.910 across the full eligible universe",
	}
	system, user := BuildSwingPrompt(candidate, 0.55)
	if !strings.Contains(system, `if "Weekly leader evidence" is shown`) {
		t.Fatalf("system prompt does not explain leader evidence:\n%s", system)
	}
	if !strings.Contains(user, "Weekly leader evidence: rank 4") {
		t.Fatalf("leader evidence missing from user prompt:\n%s", user)
	}
	if strings.Contains(user, "Graph link:") {
		t.Fatalf("leader evidence was mislabeled as graph context:\n%s", user)
	}
}

func TestWeeklyLeaderMergeKeepsGraphThesisIndependent(t *testing.T) {
	existing := []SwingCandidate{{
		Ticker:       "NVDA",
		Sources:      []string{"graph_neighbor"},
		MarketCapMM:  3_400_000,
		GraphContext: "TSMC (on watchlist) supplies to this company",
	}}
	leaders := []SwingCandidate{{
		Ticker:               "NVDA",
		Sources:              []string{"weekly_leader"},
		WeeklyLeaderEvidence: "rank 4, composite score 0.910 across the full eligible universe",
	}}
	got := mergeWeeklyLeaderCandidates(existing, leaders)[0]
	if got.GraphContext != existing[0].GraphContext {
		t.Fatalf("graph thesis changed during leader merge: %q", got.GraphContext)
	}
	if got.WeeklyLeaderEvidence != leaders[0].WeeklyLeaderEvidence {
		t.Fatalf("weekly evidence missing after merge: %+v", got)
	}
	if got.MarketCapMM != existing[0].MarketCapMM {
		t.Fatalf("missing leader market cap erased ordinary evidence: %+v", got)
	}
	_, user := BuildSwingPrompt(got, 0.55)
	if strings.Count(user, "Graph link:") != 1 ||
		strings.Count(user, "Weekly leader evidence:") != 1 {
		t.Fatalf("independent evidence was not rendered independently:\n%s", user)
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

func TestOrdinaryFallbackRestoresIndependentEvidence(t *testing.T) {
	member := true
	taxonomyAt := time.Now()
	existing := []SwingCandidate{{
		Ticker: "NVDA", Price: 150, ChangePct: 3.5, Volume: 1_000_000,
		Sources:      []string{"mover"},
		SourceScores: map[string]float64{"mover": 8.5},
		GraphContext: "ordinary thesis",
	}}
	leaders := []SwingCandidate{{
		Ticker: "NVDA", Return1M: 12, Return3M: 25, Return6M: 40,
		DollarVolume: 500_000_000, Sources: []string{"weekly_leader"},
		SourceScores:   map[string]float64{"weekly_leader": 0.91},
		UniverseMember: &member, Sector: "Technology", Industry: "Semiconductors",
		TaxonomyAsOf: &taxonomyAt, WeeklyLeaderRunID: "run-1",
		WeeklyLeaderClaimToken: "claim-1", UniverseVersionID: "universe-1",
		WeeklyLeaderEvidence: "rank 4, composite score 0.910 across the full eligible universe",
	}}
	merged := mergeWeeklyLeaderCandidates(existing, leaders)[0]
	merged.News = []SwingNewsItem{{Headline: "fresh"}}
	merged.PullbackQualified = true
	fallback, ok := ordinaryCandidateAfterWeeklyClaimLoss(merged)
	if !ok {
		t.Fatal("mixed-source candidate had no ordinary fallback")
	}
	if len(fallback.Sources) != 1 || fallback.Sources[0] != "mover" ||
		len(fallback.SourceScores) != 1 || fallback.SourceScores["mover"] != 8.5 ||
		fallback.GraphContext != "ordinary thesis" ||
		fallback.WeeklyLeaderEvidence != "" ||
		fallback.WeeklyLeaderRunID != "" || fallback.WeeklyLeaderClaimToken != "" ||
		fallback.UniverseVersionID != "" || fallback.Return6M != 0 ||
		fallback.Price != 150 || fallback.Volume != 1_000_000 ||
		len(fallback.News) != 1 || !fallback.PullbackQualified {
		t.Fatalf("ordinary evidence was not restored: %+v", fallback)
	}
	fallback.SourceScores["mover"] = 1
	if existing[0].SourceScores["mover"] != 8.5 {
		t.Fatal("ordinary fallback source scores alias the original map")
	}
	if _, ok := ordinaryCandidateAfterWeeklyClaimLoss(leaders[0]); ok {
		t.Fatal("weekly-only candidate received an ordinary fallback")
	}
}

func TestBuildTelegramMessageLabelsWeeklyLeaderEvidence(t *testing.T) {
	scanner := &MarketScanner{}
	message := scanner.buildTelegramMessage([]scoredCandidate{{
		candidate: SwingCandidate{
			Ticker:               "NVDA",
			WeeklyLeaderEvidence: "rank 4, composite score 0.910 across the full eligible universe",
		},
		score: SwingScoreResult{SetupType: "momentum_breakout", Conviction: 0.82},
	}}, 0)
	if !strings.Contains(message, "Weekly leader: rank 4") {
		t.Fatalf("Telegram message omitted leader evidence:\n%s", message)
	}
	if strings.Contains(message, "🔗") {
		t.Fatalf("Telegram message rendered leader evidence as a graph link:\n%s", message)
	}
}
