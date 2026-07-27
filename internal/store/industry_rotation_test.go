package store

import "testing"

func TestBuildIndustryRotationTransitionsRecordsFailureAndUniverseExit(t *testing.T) {
	score, nextScore := .8, .7
	rank := 1
	previous := map[string]IndustryRotationGroup{
		industryGroupMapKey("industry", "Software"): {
			GroupType: "industry", GroupKey: "Software",
			Outcome: "qualified", PotencyScore: &score, Rank: &rank,
		},
		industryGroupMapKey("industry", "Semiconductors"): {
			GroupType: "industry", GroupKey: "Semiconductors",
			Outcome: "qualified", PotencyScore: &score, Rank: &rank,
		},
	}
	current := map[string]IndustryRotationGroup{
		industryGroupMapKey("industry", "Software"): {
			GroupType: "industry", GroupKey: "Software",
			Outcome: "failed", PotencyScore: &nextScore,
		},
	}
	got := buildIndustryRotationTransitions("current", "previous", previous, current)
	if len(got) != 2 {
		t.Fatalf("transitions = %+v, want two removals", got)
	}
	reasons := map[string]string{}
	for _, transition := range got {
		reasons[transition.GroupKey] = transition.Reason
	}
	if reasons["Software"] != "data_failure" ||
		reasons["Semiconductors"] != "universe_exit" {
		t.Fatalf("unexpected transition reasons: %+v", reasons)
	}
}

func TestSummarizeIndustryRotationPreservesFailedDenominators(t *testing.T) {
	got := summarizeIndustryRotation(
		[]IndustryRotationGroup{
			{Outcome: "qualified"}, {Outcome: "not_qualified"}, {Outcome: "failed"},
		},
		[]IndustryRotationMember{
			{Outcome: "qualified"}, {Outcome: "not_qualified"}, {Outcome: "failed"},
		},
		[]IndustryRotationTransition{
			{Event: "added"}, {Event: "removed"}, {Event: "persistent"},
		},
	)
	if got.EligibleGroupCount != 3 || got.EvaluatedGroupCount != 2 ||
		got.PotentGroupCount != 1 || got.FailedGroupCount != 1 ||
		got.EligibleMemberCount != 3 || got.EvaluatedMemberCount != 2 ||
		got.SelectedMemberCount != 1 || got.FailedMemberCount != 1 ||
		got.GroupCoverage != 2.0/3 || got.MemberCoverage != 2.0/3 ||
		got.AddedGroupCount != 1 || got.RemovedGroupCount != 1 ||
		got.PersistentGroupCount != 1 {
		t.Fatalf("incorrect observation summary: %+v", got)
	}
}

func TestIndustryGroupMapKeyNormalizesProviderFormatting(t *testing.T) {
	if industryGroupMapKey("industry", " Software  Services ") !=
		industryGroupMapKey("industry", "software services") {
		t.Fatal("semantically identical industry labels split transition lineage")
	}
}
