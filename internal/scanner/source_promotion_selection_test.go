package scanner

import (
	"fmt"
	"testing"
)

func TestSelectSourcePromotionCandidatesCapsEachSourceAndMergesSelectedFacets(t *testing.T) {
	candidates := make([]SwingCandidate, 0, 11)
	for index := 1; index <= 6; index++ {
		candidates = append(candidates, SwingCandidate{
			Ticker: fmt.Sprintf("MOVE%d", index), Sources: []string{"mover"},
			SourceScores: map[string]float64{"mover": float64(7 - index)},
		})
	}
	for index := 1; index <= 5; index++ {
		candidates = append(candidates, SwingCandidate{
			Ticker: fmt.Sprintf("ACTIVE%d", index), Sources: []string{"active"},
			SourceScores: map[string]float64{"active": float64(6 - index)},
		})
	}
	selected, excluded := selectSourcePromotionCandidates(candidates)
	if len(selected) != 10 {
		t.Fatalf("selected=%d, want 10", len(selected))
	}
	if len(excluded) != 1 || excluded[0].Ticker != "MOVE6" || excluded[0].Source != "mover" || excluded[0].Rank != 6 {
		t.Fatalf("exclusions=%+v, want MOVE6 mover rank 6", excluded)
	}
	for _, candidate := range selected {
		for _, source := range candidate.Sources {
			if candidate.promotionSourceRanks[source] < 1 || candidate.promotionSourceRanks[source] > 5 {
				t.Fatalf("%s %s rank=%d, want 1..5", candidate.Ticker, source, candidate.promotionSourceRanks[source])
			}
		}
	}
}

func TestSelectSourcePromotionCandidatesDoesNotLetExcludedFacetHitchhike(t *testing.T) {
	candidates := []SwingCandidate{
		{Ticker: "OVERLAP", Sources: []string{"mover", "active"}, SourceScores: map[string]float64{"mover": 0, "active": 10}, WeeklyLeaderEvidence: "unrelated"},
	}
	for index := 1; index <= 5; index++ {
		candidates = append(candidates, SwingCandidate{Ticker: fmt.Sprintf("M%d", index), Sources: []string{"mover"}, SourceScores: map[string]float64{"mover": float64(10 - index)}})
	}
	selected, excluded := selectSourcePromotionCandidates(candidates)
	if len(selected) != 6 {
		t.Fatalf("selected=%d, want 6", len(selected))
	}
	var overlap SwingCandidate
	for _, candidate := range selected {
		if candidate.Ticker == "OVERLAP" {
			overlap = candidate
		}
	}
	if len(overlap.Sources) != 1 || overlap.Sources[0] != "active" {
		t.Fatalf("overlap sources=%v, want only active", overlap.Sources)
	}
	if len(excluded) != 1 || excluded[0].Ticker != "OVERLAP" || excluded[0].Source != "mover" {
		t.Fatalf("exclusions=%+v, want overlap mover", excluded)
	}
}

func TestSelectSourcePromotionCandidatesKeepsFewerThanFive(t *testing.T) {
	selected, excluded := selectSourcePromotionCandidates([]SwingCandidate{
		{Ticker: "B", Sources: []string{"mover"}, SourceScores: map[string]float64{"mover": 1}},
		{Ticker: "A", Sources: []string{"mover"}, SourceScores: map[string]float64{"mover": 1}},
	})
	if len(selected) != 2 || len(excluded) != 0 {
		t.Fatalf("selected/excluded = %d/%d, want 2/0", len(selected), len(excluded))
	}
	if selected[0].Ticker != "B" || selected[0].promotionSourceRanks["mover"] != 2 || selected[1].Ticker != "A" || selected[1].promotionSourceRanks["mover"] != 1 {
		t.Fatalf("stable rank selection=%+v", selected)
	}
}
