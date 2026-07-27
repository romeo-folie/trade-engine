package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/store"
)

func TestRenderPotentIndustryReportDistinguishesArchetypesAndMembers(t *testing.T) {
	session := time.Date(2026, time.July, 24, 0, 0, 0, 0, time.UTC)
	score, ew5, median5, advance := .82, .04, .03, .8
	rank, memberRank, memberScore, industryRS, marketRS := 1, 1, .77, .06, .05
	var out bytes.Buffer
	err := renderPotentIndustryReport(
		&out, session, session, "",
		[]store.IndustryRotationRun{{
			ID: "run-1", SessionDate: session,
			EligibleGroupCount: 2, EvaluatedGroupCount: 2, PotentGroupCount: 1,
			AddedGroupCount: 1, GroupCoverage: 1, MemberCoverage: .9,
		}},
		[]store.IndustryRotationGroup{
			{
				RunID: "run-1", GroupType: "industry", GroupKey: "Software",
				Industry: "Software", Benchmark: "XLK", Archetype: "broad_strong",
				Outcome: "qualified", PotencyScore: &score, Rank: &rank,
				EqualWeightReturn5: &ew5, MedianMemberReturn5: &median5,
				AdvanceShare: &advance, PersistenceSessions: 4, MemberCoverage: 1,
			},
			{
				RunID: "run-1", GroupType: "industry", GroupKey: "Semiconductors",
				Industry: "Semiconductors", Benchmark: "SMH",
				Archetype: "strong_narrow", Outcome: "not_qualified",
				FailureStage:  "breadth_strength_gate",
				FailureReason: "group is strong_narrow", MemberCoverage: .8,
			},
		},
		[]store.IndustryRotationTransition{{
			RunID: "run-1", GroupType: "industry", GroupKey: "Software",
			Event: "added", Reason: "newly_potent",
		}},
		[]store.IndustryRotationMember{{
			RunID: "run-1", GroupType: "industry", GroupKey: "Software",
			Ticker: "AAA", MemberRank: &memberRank, MemberScore: &memberScore,
			StockVsIndustryRS: &industryRS, StockVsMarketRS: &marketRS,
		}},
	)
	if err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{
		"broad_strong", "strong_narrow", "breadth_strength_gate",
		"#1 AAA", "transition: added", "Software", "Semiconductors",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("report omitted %q:\n%s", want, got)
		}
	}
}

func TestRenderPotentIndustryReportFiltersExactIndustry(t *testing.T) {
	session := time.Date(2026, time.July, 24, 0, 0, 0, 0, time.UTC)
	var out bytes.Buffer
	err := renderPotentIndustryReport(
		&out, session, session, " software  services ",
		[]store.IndustryRotationRun{{ID: "run-1", SessionDate: session}},
		[]store.IndustryRotationGroup{
			{RunID: "run-1", GroupType: "industry", GroupKey: "Software Services",
				Industry: "Software Services", Archetype: "weak", Outcome: "not_qualified"},
			{RunID: "run-1", GroupType: "industry", GroupKey: "Energy",
				Industry: "Energy", Archetype: "weak", Outcome: "not_qualified"},
		},
		nil, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, "Software Services") ||
		!strings.Contains(got, "Run totals remain full-universe.") ||
		strings.Contains(got, "  Energy") {
		t.Fatalf("industry filter failed:\n%s", got)
	}
}
