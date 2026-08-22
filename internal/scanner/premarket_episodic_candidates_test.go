package scanner

import (
	"strings"
	"testing"
)

func TestMergePremarketEpisodicRetainsMultiSourceAttribution(t *testing.T) {
	existing := []SwingCandidate{{
		Ticker: "AAPL", Price: 100,
		Sources:           []string{"weekly_leader"},
		SourceScores:      map[string]float64{"weekly_leader": .8},
		WeeklyLeaderRunID: "weekly-1", WeeklyLeaderClaimToken: "weekly-token",
		WeeklyLeaderEvidence: "rank 3",
	}}
	premarket := []SwingCandidate{{
		Ticker: "AAPL", Price: 105, ChangePct: 5,
		Volume: 200_000, TradeCount: 2_000, DollarVolume: 21_000_000,
		Sources:                     []string{"premarket_episodic"},
		SourceScores:                map[string]float64{"premarket_episodic": .9},
		PremarketEpisodicRunID:      "pre-1",
		PremarketEpisodicClaimToken: "pre-token",
		PremarketEpisodicEvidence:   "gap +5%; 4x RVOL",
	}}
	got := mergePremarketEpisodicCandidates(existing, premarket)
	if len(got) != 1 || !hasSource(got[0].Sources, "weekly_leader") ||
		!hasSource(got[0].Sources, "premarket_episodic") {
		t.Fatalf("multi-source attribution = %+v", got)
	}
	if got[0].Price != 105 || got[0].WeeklyLeaderRunID == "" ||
		got[0].PremarketEpisodicRunID == "" {
		t.Fatalf("source lineage or fresh evidence lost: %+v", got[0])
	}
}

func TestPremarketEpisodicEvidenceReachesPrompt(t *testing.T) {
	candidate := SwingCandidate{
		Ticker: "AAPL", Price: 105,
		Sources:                   []string{"premarket_episodic"},
		PremarketEpisodicEvidence: "gap +5%; 4x RVOL; catalyst confirmed",
	}
	_, prompt := BuildSwingPrompt(candidate, .65)
	if !strings.Contains(prompt, "Premarket episodic evidence: gap +5%") {
		t.Fatalf("prompt omitted evidence:\n%s", prompt)
	}
}

func TestPremarketMergedCandidateFailsClosedAfterAnyClaimLoss(t *testing.T) {
	candidate := SwingCandidate{
		Ticker: "AAPL", Sources: []string{"weekly_leader", "premarket_episodic"},
		WeeklyLeaderRunID: "weekly-run", PremarketEpisodicRunID: "pre-run",
	}
	if fallback, ok := candidateAfterPromotionClaimLoss(candidate, "weekly_leader"); ok {
		t.Fatalf("unsafe fallback retained after claim loss: %+v", fallback)
	}
}

func TestScannerSourceClaimsIncludePremarketLineage(t *testing.T) {
	got := scannerSourcePromotionClaims(SwingCandidate{
		Ticker: "AAPL", PremarketEpisodicRunID: "run-1",
		PremarketEpisodicClaimToken: "token-1",
	})
	if got.PremarketEpisodic == nil ||
		got.PremarketEpisodic.RunID != "run-1" ||
		got.PremarketEpisodic.Token != "token-1" {
		t.Fatalf("premarket claim = %+v", got.PremarketEpisodic)
	}
}
