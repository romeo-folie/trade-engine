package store_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/store"
)

func TestIndustryRotationRunLifecyclePersistsPointInTimeEvidence(t *testing.T) {
	ctx := context.Background()
	base := time.Date(2026, time.July, 23, 20, 0, 0, 0, time.UTC)
	claim := beginClaimedSwingUniverseFixture(t, ctx, store.SwingUniverseVersion{
		Provider: "test-industry-rotation", AsOf: base.Add(-time.Hour),
		CapturedAt: base.Add(-50 * time.Minute),
		Filters:    json.RawMessage(`{"test":true}`),
	})
	versionID := claim.ActiveVersionID
	price, marketCap, dollarVolume := 100.0, 10_000.0, 80_000_000.0
	members := []store.SwingUniverseMember{
		{Ticker: "TEST_IR_A", Eligible: true, Price: &price, MarketCapMM: &marketCap,
			DollarVolume: &dollarVolume, Sector: "Technology", Industry: "Software",
			TaxonomyStatus: "known"},
		{Ticker: "TEST_IR_B", Eligible: true, Price: &price, MarketCapMM: &marketCap,
			DollarVolume: &dollarVolume, Sector: "Technology", Industry: "Software",
			TaxonomyStatus: "known"},
		{Ticker: "TEST_IR_C", Eligible: true, Price: &price, MarketCapMM: &marketCap,
			DollarVolume: &dollarVolume, Sector: "Technology", Industry: "Software",
			TaxonomyStatus: "known"},
	}
	if err := store.ReplaceClaimedSwingUniverseMembers(ctx, claim, members); err != nil {
		t.Fatal(err)
	}
	completeClaimedSwingUniverseFixture(t, ctx, claim, base.Add(-40*time.Minute))
	t.Cleanup(func() {
		_, _ = store.GetDB().Exec(
			context.Background(),
			"DELETE FROM industry_rotation_runs WHERE universe_version_id=$1",
			versionID,
		)
		_, _ = store.GetDB().Exec(
			context.Background(),
			"UPDATE swing_universe_versions SET status='building' WHERE id=$1",
			versionID,
		)
		_, _ = store.GetDB().Exec(
			context.Background(),
			"DELETE FROM swing_universe_versions WHERE id=$1",
			versionID,
		)
	})

	input := store.IndustryRotationRun{
		SessionDate: base, AsOf: base, DecisionAt: base.Add(15 * time.Minute),
		UniverseVersionID: versionID, StartedAt: base.Add(16 * time.Minute),
		ConfigHash:     "industry-rotation-integration-v1",
		ConfigSnapshot: json.RawMessage(`{"min_group_members":5}`),
		FeatureVersion: "potent-industry-v1", MarketBenchmark: "SPY",
		HistoryProvider: "fixture",
	}
	first, claimed, err := store.ClaimIndustryRotationRun(ctx, input, time.Hour)
	if err != nil || !claimed {
		t.Fatalf("first claim = %+v/%v/%v", first, claimed, err)
	}
	existing, claimed, err := store.ClaimIndustryRotationRun(ctx, input, time.Hour)
	if err != nil || claimed || existing == nil || existing.ID != first.ID {
		t.Fatalf("active duplicate claim = %+v/%v/%v", existing, claimed, err)
	}

	score, percentile, coverage := .82, .95, 1.0
	advanceDecline, upVolumeShare := .50, .70
	rank, memberRankA, memberRankC := 1, 2, 1
	memberScoreC := .90
	closePrice, return5 := 112.0, .08
	groups := []store.IndustryRotationGroup{
		{
			RunID: first.ID, GroupType: "industry", GroupKey: "Software",
			Sector: "Technology", Industry: "Software", Benchmark: "XLK",
			Outcome: "qualified", EligibleMemberCount: 3, EvaluatedMemberCount: 2,
			FreshTaxonomyCount: 3, MemberCoverage: 2.0 / 3,
			FreshTaxonomyCoverage: 1, EqualWeightReturn5: &return5,
			AdvanceDecline: &advanceDecline, UpVolumeShare: &upVolumeShare,
			PersistenceSessions: 3,
			PotencyScore:        &score, ScorePercentile: &percentile, Rank: &rank,
			FeatureCoverage: &coverage,
			GroupEvidence:   json.RawMessage(`{"breadth":"broad"}`),
			Components:      json.RawMessage(`{"advance_share":0.75}`),
		},
	}
	memberRows := []store.IndustryRotationMember{
		{
			RunID: first.ID, UniverseVersionID: versionID,
			GroupType: "industry", GroupKey: "Software", Ticker: "TEST_IR_A",
			Outcome: "qualified", HistorySessions: 253, BarSetHash: "bars-a",
			ClosePrice: &closePrice, Return5: &return5, MemberScore: &score,
			MemberRank: &memberRankA, FeatureCoverage: &coverage,
			SourceEvidence: json.RawMessage(`{"source":"potent_industry"}`),
		},
		{
			RunID: first.ID, UniverseVersionID: versionID,
			GroupType: "industry", GroupKey: "Software", Ticker: "TEST_IR_B",
			Outcome: "failed", FailureStage: "history",
			FailureReason: "fixture outage",
		},
		{
			RunID: first.ID, UniverseVersionID: versionID,
			GroupType: "industry", GroupKey: "Software", Ticker: "TEST_IR_C",
			Outcome: "qualified", HistorySessions: 253, BarSetHash: "bars-c",
			ClosePrice: &closePrice, Return5: &return5, MemberScore: &memberScoreC,
			MemberRank: &memberRankC, FeatureCoverage: &coverage,
			SourceEvidence: json.RawMessage(`{"source":"potent_industry"}`),
		},
	}
	completedAt := base.Add(20 * time.Minute)
	if err := store.CompleteIndustryRotationRun(
		ctx, first.ID, first.AttemptCount, completedAt, groups, memberRows,
		json.RawMessage(`{"SPY":"fixture-market-bars"}`),
	); err != nil {
		t.Fatal(err)
	}
	completed, claimed, err := store.ClaimIndustryRotationRun(ctx, input, time.Hour)
	if err != nil || claimed || completed == nil || completed.Status != "completed" {
		t.Fatalf("completed duplicate claim = %+v/%v/%v", completed, claimed, err)
	}
	if completed.EligibleGroupCount != 1 || completed.EvaluatedGroupCount != 1 ||
		completed.PotentGroupCount != 1 || completed.EligibleMemberCount != 3 ||
		completed.EvaluatedMemberCount != 2 || completed.SelectedMemberCount != 2 ||
		completed.FailedMemberCount != 1 || completed.AddedGroupCount != 1 ||
		completed.GroupCoverage != 1 || completed.MemberCoverage != 2.0/3 {
		t.Fatalf("incorrect completed diagnostics: %+v", completed)
	}
	if got, err := store.GetLatestCompletedIndustryRotationRunAsOf(
		ctx, input.DecisionAt.Add(-time.Nanosecond),
	); err != nil || got != nil {
		t.Fatalf("future decision leaked: run=%+v err=%v", got, err)
	}
	latest, err := store.GetLatestCompletedIndustryRotationRunAsOf(ctx, completedAt)
	if err != nil || latest == nil || latest.ID != first.ID {
		t.Fatalf("latest run = %+v/%v", latest, err)
	}

	var sourceTag, failureStage string
	var persistedAdvanceDecline, persistedUpVolumeShare float64
	var persistedSessions int
	var persistedGroup, persistedMember json.RawMessage
	if err := store.GetDB().QueryRow(ctx, `
		SELECT m.source_tag, failed.failure_stage, g.advance_decline,
		       g.up_volume_share, g.persistence_sessions,
		       g.group_evidence, m.source_evidence
		FROM industry_rotation_groups g
		JOIN industry_rotation_members m
		  ON m.run_id=g.run_id AND m.group_type=g.group_type
		 AND m.group_key=g.group_key AND m.ticker='TEST_IR_A'
		JOIN industry_rotation_members failed
		  ON failed.run_id=g.run_id AND failed.group_type=g.group_type
		 AND failed.group_key=g.group_key AND failed.ticker='TEST_IR_B'
		WHERE g.run_id=$1`, first.ID,
	).Scan(
		&sourceTag, &failureStage, &persistedAdvanceDecline,
		&persistedUpVolumeShare, &persistedSessions,
		&persistedGroup, &persistedMember,
	); err != nil {
		t.Fatal(err)
	}
	if sourceTag != "potent_industry" || failureStage != "history" ||
		persistedAdvanceDecline != advanceDecline ||
		persistedUpVolumeShare != upVolumeShare || persistedSessions != 3 ||
		string(persistedGroup) != `{"breadth": "broad"}` ||
		string(persistedMember) != `{"source": "potent_industry"}` {
		t.Fatalf("evidence was not preserved: %s/%s/%s/%s",
			sourceTag, failureStage, persistedGroup, persistedMember)
	}

	reportGroups, err := store.ListIndustryRotationGroups(ctx, []string{first.ID})
	if err != nil || len(reportGroups) != 1 ||
		reportGroups[0].GroupKey != "Software" ||
		reportGroups[0].AdvanceDecline == nil ||
		*reportGroups[0].AdvanceDecline != advanceDecline {
		t.Fatalf("report groups = %+v/%v", reportGroups, err)
	}
	reportTransitions, err := store.ListIndustryRotationTransitions(
		ctx, []string{first.ID},
	)
	if err != nil || len(reportTransitions) != 1 ||
		reportTransitions[0].Event != "added" ||
		reportTransitions[0].GroupKey != "Software" {
		t.Fatalf("report transitions = %+v/%v", reportTransitions, err)
	}
	reportMembers, err := store.ListIndustryRotationTopMembers(
		ctx, []string{first.ID}, 1,
	)
	if err != nil || len(reportMembers) != 1 ||
		reportMembers[0].Ticker != "TEST_IR_C" ||
		reportMembers[0].SourceTag != "potent_industry" {
		t.Fatalf("report members = %+v/%v", reportMembers, err)
	}
}

func TestIndustryRotationReportSelectsCanonicalRunPerSession(t *testing.T) {
	ctx := context.Background()
	sessionDate := time.Date(2041, time.January, 3, 0, 0, 0, 0, time.UTC)
	versionIDs := make([]string, 0, 2)
	for i := 0; i < 2; i++ {
		versionID, err := store.BeginSwingUniverseVersion(
			ctx, store.SwingUniverseVersion{
				Provider:   "test-industry-report-canonical",
				AsOf:       sessionDate.Add(time.Duration(i-2) * time.Hour),
				CapturedAt: sessionDate.Add(time.Duration(i-2)*time.Hour + time.Minute),
				Filters:    json.RawMessage(`{"test":"report-canonical"}`),
			},
		)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.CompleteSwingUniverseVersion(
			ctx, versionID, sessionDate.Add(time.Duration(i-2)*time.Hour+2*time.Minute),
		); err != nil {
			t.Fatal(err)
		}
		versionIDs = append(versionIDs, versionID)
	}
	var olderID, canonicalID string
	fixtures := []struct {
		versionID   string
		configHash  string
		decisionAt  time.Time
		completedAt time.Time
		id          *string
	}{
		{
			versionID: versionIDs[0], configHash: "industry-report-v1",
			decisionAt:  sessionDate.Add(20*time.Hour + 15*time.Minute),
			completedAt: sessionDate.Add(20*time.Hour + 45*time.Minute),
			id:          &olderID,
		},
		{
			versionID: versionIDs[1], configHash: "industry-report-v2",
			decisionAt:  sessionDate.Add(20*time.Hour + 30*time.Minute),
			completedAt: sessionDate.Add(20*time.Hour + 35*time.Minute),
			id:          &canonicalID,
		},
	}
	for _, fixture := range fixtures {
		if err := store.GetDB().QueryRow(ctx, `
			INSERT INTO industry_rotation_runs (
				session_date, as_of, decision_at, universe_version_id, status,
				started_at, completed_at, lease_until, config_hash,
				config_snapshot, feature_version, market_benchmark,
				benchmark_evidence, history_provider
			) VALUES (
				$1,$2,$3,$4,'completed',$5,$6,$7,$8,'{}',
				'potent-industry-v1','SPY','{}','fixture'
			)
			RETURNING id`,
			sessionDate, sessionDate, fixture.decisionAt, fixture.versionID,
			fixture.decisionAt.Add(time.Minute), fixture.completedAt,
			fixture.completedAt, fixture.configHash,
		).Scan(fixture.id); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		_, _ = store.GetDB().Exec(
			context.Background(),
			"DELETE FROM industry_rotation_runs WHERE id=ANY($1::uuid[])",
			[]string{olderID, canonicalID},
		)
		_, _ = store.GetDB().Exec(
			context.Background(),
			"DELETE FROM swing_universe_versions WHERE id=ANY($1::uuid[])",
			versionIDs,
		)
	})

	tokyo, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		t.Fatal(err)
	}
	writtenSessionDate := time.Date(2041, time.January, 3, 23, 59, 0, 0, tokyo)
	runs, err := store.ListCompletedIndustryRotationRunsBySessionDate(
		ctx, writtenSessionDate, writtenSessionDate,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].ID != canonicalID ||
		runs[0].UniverseVersionID != versionIDs[1] ||
		runs[0].ConfigHash != "industry-report-v2" {
		t.Fatalf(
			"canonical report runs = %+v, want only run %s",
			runs, canonicalID,
		)
	}
}
