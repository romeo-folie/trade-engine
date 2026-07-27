package scanner

import (
	"strings"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/store"
)

func TestMarkPotentIndustryPromotionsUsesGlobalDeterministicCap(t *testing.T) {
	one, two := 1, 2
	groupScore, high, low := .9, .8, .7
	groups := []store.IndustryRotationGroup{
		{GroupType: "industry", GroupKey: "Software", Outcome: "qualified", Rank: &one},
		{GroupType: "industry", GroupKey: "Semiconductors", Outcome: "qualified", Rank: &two},
	}
	members := []store.IndustryRotationMember{
		{GroupType: "industry", GroupKey: "Semiconductors", Ticker: "BBB",
			Outcome: "qualified", MemberRank: &one, MemberScore: &high,
			PromotionStatus: "not_requested"},
		{GroupType: "industry", GroupKey: "Software", Ticker: "AAA",
			Outcome: "qualified", MemberRank: &two, MemberScore: &low,
			PromotionStatus: "not_requested"},
		{GroupType: "industry", GroupKey: "Software", Ticker: "AAA",
			Outcome: "qualified", MemberRank: &one, MemberScore: &groupScore,
			PromotionStatus: "not_requested"},
		{GroupType: "industry", GroupKey: "Software", Ticker: "CCC",
			Outcome: "failed", PromotionStatus: "not_requested"},
	}
	markPotentIndustryPromotions(groups, members, config.PotentIndustryConfig{
		PromoteCandidates: true, MaxPromotionsPerRun: 2,
	})
	if members[2].PromotionStatus != "pending" ||
		members[0].PromotionStatus != "pending" {
		t.Fatalf("global rank was not respected: %+v", members)
	}
	if members[1].PromotionStatus != "not_requested" ||
		members[3].PromotionStatus != "not_requested" {
		t.Fatalf("duplicate or failed member consumed promotion cap: %+v", members)
	}
}

func TestMergePotentIndustryCandidateRetainsEverySource(t *testing.T) {
	taxonomyAt := time.Date(2026, 7, 24, 20, 0, 0, 0, time.UTC)
	existing := SwingCandidate{
		Ticker: "AAA", Price: 100, ChangePct: 4,
		Sources: []string{"mover", "weekly_leader"},
		SourceScores: map[string]float64{
			"mover": 0, "weekly_leader": .91,
		},
		WeeklyLeaderRunID: "weekly-run", WeeklyLeaderClaimToken: "weekly-token",
		WeeklyLeaderEvidence: "rank 1",
		Return1M:             10, Return3M: 20, Return6M: 30,
	}
	industry := SwingCandidate{
		Ticker: "AAA", Price: 101, Return1M: 8,
		Sources:                  []string{"potent_industry"},
		SourceScores:             map[string]float64{"potent_industry": .82},
		PotentIndustryRunID:      "industry-run",
		PotentIndustryGroupType:  "industry",
		PotentIndustryGroupKey:   "Software",
		PotentIndustryClaimToken: "industry-token",
		PotentIndustryEvidence:   "Software broad_strong",
		TaxonomyAsOf:             &taxonomyAt,
	}
	got := mergePotentIndustryCandidates(existingSlice(existing), []SwingCandidate{industry})
	if len(got) != 1 ||
		!hasSource(got[0].Sources, "mover") ||
		!hasSource(got[0].Sources, "weekly_leader") ||
		!hasSource(got[0].Sources, "potent_industry") ||
		got[0].SourceScores["weekly_leader"] != .91 ||
		got[0].SourceScores["potent_industry"] != .82 ||
		got[0].WeeklyLeaderEvidence == "" ||
		got[0].PotentIndustryEvidence == "" {
		t.Fatalf("multi-source evidence was lost: %+v", got)
	}

	withoutIndustry, ok := candidateAfterPromotionClaimLoss(got[0], "potent_industry")
	if !ok || hasSource(withoutIndustry.Sources, "potent_industry") ||
		!hasSource(withoutIndustry.Sources, "weekly_leader") ||
		withoutIndustry.PotentIndustryEvidence != "" {
		t.Fatalf("industry claim fallback corrupted remaining sources: %+v",
			withoutIndustry)
	}
	withoutWeekly, ok := candidateAfterPromotionClaimLoss(got[0], "weekly_leader")
	if !ok || hasSource(withoutWeekly.Sources, "weekly_leader") ||
		!hasSource(withoutWeekly.Sources, "potent_industry") ||
		withoutWeekly.Return3M != 0 || withoutWeekly.WeeklyLeaderEvidence != "" {
		t.Fatalf("weekly claim fallback retained stale evidence: %+v", withoutWeekly)
	}
}

func TestPotentIndustryEvidenceIsPromptedAsConfirmation(t *testing.T) {
	system, user := BuildSwingPrompt(SwingCandidate{
		Ticker: "AAA", Price: 100, Sources: []string{"potent_industry"},
		PotentIndustryEvidence: "Software broad_strong; member rank 1",
	}, .55)
	if !strings.Contains(system, "not permission to add") ||
		!strings.Contains(user,
			"Potent industry evidence: Software broad_strong; member rank 1") {
		t.Fatalf("potent industry evidence missing from prompt:\n%s\n%s", system, user)
	}
}

func TestBuildTelegramMessageLabelsPotentIndustryEvidence(t *testing.T) {
	scanner := &MarketScanner{}
	got := scanner.buildTelegramMessage([]scoredCandidate{{
		candidate: SwingCandidate{
			Ticker:                 "AAA",
			PotentIndustryEvidence: "Software broad_strong; member rank 1",
		},
		score: SwingScoreResult{SetupType: "momentum_breakout", Conviction: .8},
	}}, 0)
	if !strings.Contains(got,
		"Potent industry: Software broad_strong; member rank 1") {
		t.Fatalf("Telegram omitted potent industry evidence: %s", got)
	}
}

func existingSlice(candidate SwingCandidate) []SwingCandidate {
	return []SwingCandidate{candidate}
}
