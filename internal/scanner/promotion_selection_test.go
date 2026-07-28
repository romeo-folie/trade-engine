package scanner

import (
	"testing"

	"github.com/romeo-folie/trade-engine/internal/store"
)

func TestSelectPromotionCandidatesDeterministicCaps(t *testing.T) {
	result := SelectPromotionCandidates([]PromotionCandidate{
		{Key: "B", Industry: "Tech", TaxonomyStatus: "known", Score: 0.9},
		{Key: "A", Industry: "Tech", TaxonomyStatus: "known", Score: 0.9},
		{Key: "C", Industry: "Energy", TaxonomyStatus: "known", Score: 0.8},
		{Key: "D", Industry: "Energy", TaxonomyStatus: "known", Score: 0.7},
	}, 3, 1, 0)
	if got := []string{result.Selected[0].Key, result.Selected[1].Key, result.Rejected[0].Candidate.Key}; got[0] != "A" || got[1] != "C" || got[2] != "B" {
		t.Fatalf("selection = %+v, rejected = %+v", result.Selected, result.Rejected)
	}
	if result.Rejected[0].Reason != "industry_cap" {
		t.Fatalf("rejection reason = %q, want industry_cap", result.Rejected[0].Reason)
	}
}

func TestSelectScoredForPromotionPreservesDuplicateAttribution(t *testing.T) {
	scored := []scoredCandidate{
		{candidate: SwingCandidate{Ticker: "AAPL", Industry: "Technology", TaxonomyStatus: "known"}, score: SwingScoreResult{Conviction: .9}},
		{candidate: SwingCandidate{Ticker: " aapl ", Industry: "technology", TaxonomyStatus: "known"}, score: SwingScoreResult{Conviction: .8}},
		{candidate: SwingCandidate{Ticker: "MSFT", Industry: "Technology", TaxonomyStatus: "known"}, score: SwingScoreResult{Conviction: .7}},
	}
	selected, rejected := selectScoredForPromotion(scored, 2, 1)
	if len(selected) != 1 || selected[0].candidate.Ticker != "AAPL" {
		t.Fatalf("selected = %+v, want only highest-scoring AAPL", selected)
	}
	if len(rejected) != 2 || rejected[0].Candidate.Index != 1 || rejected[1].Candidate.Index != 2 {
		t.Fatalf("rejected = %+v, want original indexes 1 and 2", rejected)
	}
	if rejected[0].Reason != "duplicate_key" || rejected[1].Reason != "industry_cap" {
		t.Fatalf("rejection reasons = %+v", rejected)
	}
}

func TestPromotionSelectionFlowRecordsSuppressedCandidate(t *testing.T) {
	scored := []scoredCandidate{
		{candidate: SwingCandidate{Ticker: "AAA", Industry: "Technology", TaxonomyStatus: "known"}, score: SwingScoreResult{Conviction: .9}},
		{candidate: SwingCandidate{Ticker: "BBB", Industry: "technology", TaxonomyStatus: "known"}, score: SwingScoreResult{Conviction: .8}},
		{candidate: SwingCandidate{Ticker: "CCC", Industry: "Energy", TaxonomyStatus: "known"}, score: SwingScoreResult{Conviction: .7}},
	}
	recorder := &scanRecorder{candidates: make(map[string]*store.ScannerCandidateSnapshot)}
	for _, candidate := range scored {
		recorder.register([]SwingCandidate{candidate.candidate})
		recorder.scored(candidate.candidate, candidate.score, nil, .55)
	}

	input := scored
	selected, rejected := selectScoredForPromotion(input, 2, 1)
	for _, rejection := range rejected {
		recorder.promotionRejected(input[rejection.Candidate.Index].candidate.Ticker, rejection.Reason)
	}
	recorder.selected(selected)

	if len(selected) != 2 || selected[0].candidate.Ticker != "AAA" || selected[1].candidate.Ticker != "CCC" {
		t.Fatalf("selected = %+v", selected)
	}
	if got := recorder.candidates["BBB"]; got.FinalStage != "promotion_suppressed" || got.FailureReason != "industry_cap" {
		t.Fatalf("suppressed snapshot = %+v", got)
	}
	if got := recorder.candidates["AAA"]; got.FinalStage != "selected" || !got.CapSelected {
		t.Fatalf("selected snapshot = %+v", got)
	}
}

func TestSelectPromotionCandidatesCorrelationEitherDirection(t *testing.T) {
	result := SelectPromotionCandidates([]PromotionCandidate{
		{Key: "A", Industry: "Tech", Score: 0.9, Correlations: map[string]float64{"B": 0.91}},
		{Key: "B", Industry: "Energy", Score: 0.8},
		{Key: "C", Industry: "Finance", Score: 0.7, Correlations: map[string]float64{"A": -0.95}},
	}, 0, 0, 0.9)
	if len(result.Selected) != 1 || result.Selected[0].Key != "A" {
		t.Fatalf("selected = %+v, want only A", result.Selected)
	}
	if len(result.Rejected) != 2 || result.Rejected[0].Reason != "correlation_cap" ||
		result.Rejected[1].ConflictKey != "A" {
		t.Fatalf("rejected = %+v, want correlation rejections against A", result.Rejected)
	}
}

func TestSelectPromotionCandidatesDuplicateAndUnknownIndustry(t *testing.T) {
	result := SelectPromotionCandidates([]PromotionCandidate{
		{Key: " A ", Score: 1},
		{Key: "A", Score: 0.5},
		{Key: "B", Score: 0.4},
	}, 0, 1, 0)
	if len(result.Selected) != 2 || result.Selected[0].Key != "A" || result.Selected[1].Key != "B" {
		t.Fatalf("selected = %+v", result.Selected)
	}
	if len(result.Rejected) != 1 || result.Rejected[0].Reason != "duplicate_key" {
		t.Fatalf("rejected = %+v", result.Rejected)
	}
	result = SelectPromotionCandidates([]PromotionCandidate{
		{Key: "C", Industry: "Unknown", Score: 0.9},
		{Key: "D", Industry: "Unknown", Score: 0.8},
	}, 0, 1, 0)
	if len(result.Selected) != 2 {
		t.Fatalf("unknown taxonomy should not share a synthetic cap: %+v", result)
	}
	result = SelectPromotionCandidates([]PromotionCandidate{
		{Key: "E", Industry: "Technology", TaxonomyStatus: "stale", Score: 0.9},
		{Key: "F", Industry: "Technology", TaxonomyStatus: "unavailable", Score: 0.8},
	}, 0, 1, 0)
	if len(result.Selected) != 2 {
		t.Fatalf("stale/unavailable taxonomy should not consume the cap: %+v", result)
	}
	result = SelectPromotionCandidates([]PromotionCandidate{
		{Key: "G", Industry: "Technology", Score: 0.9},
		{Key: "H", Industry: "Technology", Score: 0.8},
	}, 0, 1, 0)
	if len(result.Selected) != 2 {
		t.Fatalf("missing taxonomy status should not consume the cap: %+v", result)
	}
}
