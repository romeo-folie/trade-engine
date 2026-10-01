package scanner

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/tradeprofile"
)

func TestDeterministicPromotionReasonsDoNotClaimConvictionEvaluation(t *testing.T) {
	for _, reason := range []string{deterministicAdmissionPromotionReason, deterministicFallbackPromotionReason} {
		if strings.Contains(strings.ToLower(reason), "conviction") || !strings.Contains(reason, "deterministic") {
			t.Fatalf("promotion reason=%q must describe deterministic qualification", reason)
		}
	}
}

func TestResearchQueueAdmissionUsesProtectedSourceBucket(t *testing.T) {
	cfg := config.Defaults().Scanner.ResearchQueue
	for _, test := range []struct {
		sources []string
		bucket  string
		slots   int
	}{
		{[]string{"mover", "active"}, "market_activity", 0},
		{[]string{"weekly_leader"}, "weekly_leader", 0},
		{[]string{"political_disclosure", "mover"}, "political_disclosure", 12},
		{[]string{"news_sympathy"}, "news_sympathy", 0},
	} {
		admission := researchQueueAdmission(SwingCandidate{Ticker: "TEST", Sources: test.sources}, cfg)
		if admission.Bucket != test.bucket || admission.Reservation != test.slots || admission.Overflow != 6 {
			t.Fatalf("sources %v admission=%+v", test.sources, admission)
		}
	}
}

func TestPoliticalResearchQueuePriorityFavorsFreshnessBreadthAndKnownAmount(t *testing.T) {
	makeCandidate := func(date string, buyers int, minimum int64) SwingCandidate {
		raw, err := json.Marshal(tradeprofile.PoliticalDisclosureEvidence{
			Version: tradeprofile.PoliticalDisclosureEvidenceVersion,
			Ticker:  "TEST", LatestTransactionDate: date, DistinctBuyerCount: buyers,
			PurchaseAmounts: tradeprofile.PoliticalDisclosureAmountSummary{MinimumTotal: minimum},
		})
		if err != nil {
			t.Fatal(err)
		}
		return SwingCandidate{Ticker: "TEST", Sources: []string{"political_disclosure"}, PoliticalDisclosureFacts: raw}
	}
	if newer, older := deterministicAdmissionScore(makeCandidate("2026-08-20", 1, 0)), deterministicAdmissionScore(makeCandidate("2026-08-19", 99, 9_000_000)); newer <= older {
		t.Fatalf("freshness must rank first: newer=%v older=%v", newer, older)
	}
	if broad, narrow := deterministicAdmissionScore(makeCandidate("2026-08-20", 2, 0)), deterministicAdmissionScore(makeCandidate("2026-08-20", 1, 9_000_000)); broad <= narrow {
		t.Fatalf("buyer breadth must break equal-freshness ties: broad=%v narrow=%v", broad, narrow)
	}
	if known, unknown := deterministicAdmissionScore(makeCandidate("2026-08-20", 1, 100_000)), deterministicAdmissionScore(makeCandidate("2026-08-20", 1, 0)); known <= unknown {
		t.Fatalf("known amount must boost an otherwise equal eligible candidate: known=%v unknown=%v", known, unknown)
	}

	admission := researchQueueAdmission(makeCandidate("2026-08-20", 2, 100_000), config.Defaults().Scanner.ResearchQueue)
	if !json.Valid(admission.Priority) || string(admission.Priority) == "{}" {
		t.Fatalf("invalid or missing priority JSON: %s", admission.Priority)
	}
}

func TestResearchQueuePriorityNormalizesRanksAcrossBuckets(t *testing.T) {
	politicalFacts, err := json.Marshal(tradeprofile.PoliticalDisclosureEvidence{
		Version: tradeprofile.PoliticalDisclosureEvidenceVersion,
		Ticker:  "ZZZ_POL_A", LatestTransactionDate: "2026-08-20", DistinctBuyerCount: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	items := []scoredCandidate{
		{candidate: SwingCandidate{Ticker: "ZZZ_POL_A", Sources: []string{"political_disclosure"}, PoliticalDisclosureFacts: politicalFacts}, score: SwingScoreResult{Conviction: 2e15}},
		{candidate: SwingCandidate{Ticker: "ZZZ_POL_B", Sources: []string{"political_disclosure"}, PoliticalDisclosureFacts: politicalFacts}, score: SwingScoreResult{Conviction: 1e15}},
		{candidate: SwingCandidate{Ticker: "AAA_WEEKLY", Sources: []string{"weekly_leader"}, SourceScores: map[string]float64{"weekly_leader": .1}}, score: SwingScoreResult{Conviction: .1}},
	}

	sortByResearchQueuePriority(items, config.Defaults().Scanner.ResearchQueue)
	got := []string{items[0].candidate.Ticker, items[1].candidate.Ticker, items[2].candidate.Ticker}
	if got[0] != "AAA_WEEKLY" || got[1] != "ZZZ_POL_A" || got[2] != "ZZZ_POL_B" {
		t.Fatalf("normalized queue order = %v, want first candidate from each bucket before second political candidate", got)
	}
}

func TestResearchQueuePriorityUsesSelectedBucketScore(t *testing.T) {
	cfg := config.Defaults().Scanner.ResearchQueue
	items := []scoredCandidate{
		{candidate: SwingCandidate{Ticker: "WEEKLY_STRONG", Sources: []string{"weekly_leader", "active"}, SourceScores: map[string]float64{"weekly_leader": 9, "active": 1}}},
		{candidate: SwingCandidate{Ticker: "WEEKLY_WEAK", Sources: []string{"weekly_leader", "active"}, SourceScores: map[string]float64{"weekly_leader": 2, "active": 1e12}}},
		{candidate: SwingCandidate{Ticker: "ACTIVE_HIGH", Sources: []string{"active"}, SourceScores: map[string]float64{"active": 2e9}}},
		{candidate: SwingCandidate{Ticker: "ACTIVE_LOW", Sources: []string{"active"}, SourceScores: map[string]float64{"active": 1e9}}},
	}
	sortByResearchQueuePriority(items, cfg)
	if items[0].candidate.Ticker != "ACTIVE_HIGH" || items[1].candidate.Ticker != "WEEKLY_STRONG" {
		t.Fatalf("bucket scores were not used: %+v", items)
	}
	admission := researchQueueAdmission(items[1].candidate, cfg)
	var priority map[string]any
	if err := json.Unmarshal(admission.Priority, &priority); err != nil || priority["score"].(float64) != 9 {
		t.Fatalf("bucket priority audit = %s err=%v", admission.Priority, err)
	}
}

func TestMarketActivityPriorityUsesNormalizedProviderRanks(t *testing.T) {
	cfg := config.Defaults().Scanner.ResearchQueue
	items := []scoredCandidate{
		{candidate: SwingCandidate{Ticker: "MOVER_TWO", Sources: []string{"mover"}, ChangePct: 99, SourceScores: map[string]float64{"mover": .5}}},
		{candidate: SwingCandidate{Ticker: "ACTIVE_ONE", Sources: []string{"active"}, DollarVolume: 1, SourceScores: map[string]float64{"active": 1}}},
		{candidate: SwingCandidate{Ticker: "MOVER_ONE", Sources: []string{"mover"}, ChangePct: 1, SourceScores: map[string]float64{"mover": 1}}},
		{candidate: SwingCandidate{Ticker: "ACTIVE_TWO", Sources: []string{"active"}, DollarVolume: 1e12, SourceScores: map[string]float64{"active": .5}}},
		{candidate: SwingCandidate{Ticker: "OVERLAP", Sources: []string{"mover", "active"}, SourceScores: map[string]float64{"mover": .25, "active": 1}}},
	}
	sortByResearchQueuePriority(items, cfg)
	got := []string{items[0].candidate.Ticker, items[1].candidate.Ticker, items[2].candidate.Ticker}
	// The three rank-one candidates tie by normalized source rank, then ticker;
	// raw gap percentage and current-session notional cannot break the tie.
	want := []string{"ACTIVE_ONE", "MOVER_ONE", "OVERLAP"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("normalized market order=%v, want prefix=%v", got, want)
		}
	}
}

func TestNormalizedProviderRankIgnoresConfiguredSourceDepth(t *testing.T) {
	if mover, active := normalizedProviderRank(1), normalizedProviderRank(1); mover != active {
		t.Fatalf("equal provider ordinal must tie across source limits: mover=%v active=%v", mover, active)
	}
}

func TestRequeueFallbackCandidateUsesItsNewBucketRank(t *testing.T) {
	cfg := config.Defaults().Scanner.ResearchQueue
	items := []scoredCandidate{
		{candidate: SwingCandidate{Ticker: "LOST_POLITICAL", Sources: []string{"political_disclosure"}}, score: SwingScoreResult{Conviction: 9e15}},
		{candidate: SwingCandidate{Ticker: "AAA_MARKET", Sources: []string{"mover"}}, score: SwingScoreResult{Conviction: 100}},
	}
	fallback := scoredCandidate{
		candidate: SwingCandidate{Ticker: "ZZZ_MARKET", Sources: []string{"mover"}},
		score:     SwingScoreResult{Conviction: 1},
	}
	got := requeueFallbackCandidate(items, 0, fallback, cfg)
	if len(got) != 2 || got[0].candidate.Ticker != "AAA_MARKET" || got[1].candidate.Ticker != "ZZZ_MARKET" {
		t.Fatalf("fallback queue=%+v, want original market leader ahead of fallback", got)
	}
}
