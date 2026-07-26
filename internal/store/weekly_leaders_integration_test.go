package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/store"
)

func TestWeeklyLeaderRunLifecycleAndHistory(t *testing.T) {
	ctx := context.Background()
	base := time.Date(2026, time.July, 20, 20, 0, 0, 0, time.UTC)
	versionID, err := store.BeginSwingUniverseVersion(ctx, store.SwingUniverseVersion{
		Provider: "test-weekly-leader", AsOf: base.Add(-time.Hour),
		CapturedAt:     base.Add(-50 * time.Minute),
		Filters:        json.RawMessage(`{"test":true}`),
		SourceEvidence: json.RawMessage(`{"fixture":"weekly-leader"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	capMM := 5_000.0
	dollarVolume := 50_000_000.0
	members := []store.SwingUniverseMember{
		{Ticker: "TEST_WL_AAA", Eligible: true, MarketCapMM: &capMM,
			DollarVolume: &dollarVolume, Sector: "Technology",
			Industry: "Software", TaxonomyStatus: "known"},
		{Ticker: "TEST_WL_BBB", Eligible: true, MarketCapMM: &capMM,
			DollarVolume: &dollarVolume, Sector: "Technology",
			Industry: "Software", TaxonomyStatus: "known"},
	}
	if err := store.ReplaceSwingUniverseMembers(ctx, versionID, members); err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteSwingUniverseVersion(ctx, versionID, base.Add(-40*time.Minute)); err != nil {
		t.Fatal(err)
	}

	runInput := store.WeeklyLeaderRun{
		SessionDate: base, AsOf: base, DecisionAt: base.Add(15 * time.Minute),
		UniverseVersionID: versionID, StartedAt: base.Add(16 * time.Minute),
		ConfigHash:     "weekly-leader-integration-v1",
		ConfigSnapshot: json.RawMessage(`{"selection_percentile":0.9}`),
		FeatureVersion: "weekly-leader-v1", Benchmark: "SPY",
		HistoryProvider: "test",
	}
	first, claimed, err := store.ClaimWeeklyLeaderRun(ctx, runInput, time.Hour)
	if err != nil || !claimed {
		t.Fatalf("first claim = %+v/%v/%v", first, claimed, err)
	}
	if _, claimed, err := store.ClaimWeeklyLeaderRun(ctx, runInput, time.Hour); err != nil || claimed {
		t.Fatalf("duplicate active claim = %v/%v, want no claim", claimed, err)
	}
	scoreA, coverage, rankA := 0.91, 1.0, 1
	return20, return60, return126 := 0.10, 0.20, 0.35
	medianDV := 50_000_000.0
	qualifications := []store.WeeklyLeaderQualification{
		{
			RunID: first.ID, UniverseVersionID: versionID, Ticker: "TEST_WL_AAA",
			Outcome: "qualified", Sector: "Technology", Industry: "Software",
			MarketCapBand: "mid", VolatilityBand: "medium",
			Return20: &return20, Return60: &return60, Return126: &return126,
			MedianDollarVolume20: &medianDV, CompositeScore: &scoreA,
			ScorePercentile: &coverage, Rank: &rankA, FeatureCoverage: &coverage,
			BarSetHash:      "fixture-bars-a",
			Features:        json.RawMessage(`{"return_20d":0.1}`),
			Components:      json.RawMessage(`{"feature_coverage":1}`),
			PromotionStatus: "pending",
		},
		{
			RunID: first.ID, UniverseVersionID: versionID, Ticker: "TEST_WL_BBB",
			Outcome: "failed", FailureStage: "provider_history",
			FailureReason: "fixture outage", Sector: "Technology",
			Industry: "Software",
		},
	}
	completedAt := base.Add(20 * time.Minute)
	if err := store.CompleteWeeklyLeaderRun(
		ctx, first.ID, first.AttemptCount, completedAt, qualifications,
		"fixture-benchmark-1",
	); err != nil {
		t.Fatal(err)
	}
	if _, claimed, err := store.ClaimWeeklyLeaderRun(ctx, runInput, time.Hour); err != nil || claimed {
		t.Fatalf("completed rerun claim = %v/%v, want no claim", claimed, err)
	}
	if got, err := store.GetLatestCompletedWeeklyLeaderRunAsOf(
		ctx, runInput.DecisionAt.Add(-time.Nanosecond),
	); err != nil || got != nil {
		t.Fatalf("future decision leaked: run=%+v err=%v", got, err)
	}
	got, err := store.GetLatestCompletedWeeklyLeaderRunAsOf(ctx, completedAt)
	if err != nil || got == nil {
		t.Fatalf("latest completed run = %+v/%v", got, err)
	}
	if got.EligibleUniverseCount != 2 || got.EvaluatedCount != 1 ||
		got.QualifiedCount != 1 || got.FailureCount != 1 ||
		got.LeaderBreadth != 0.5 || got.EvaluationCoverage != 0.5 {
		t.Fatalf("incorrect run diagnostics: %+v", got)
	}
	pending, err := store.ListPendingWeeklyLeaderCandidatesAsOf(ctx, completedAt)
	if err != nil || len(pending) != 1 || pending[0].Ticker != "TEST_WL_AAA" {
		t.Fatalf("pending candidates = %+v/%v", pending, err)
	}
	if err := store.MarkWeeklyLeaderPromotion(
		ctx, first.ID, "TEST_WL_AAA", "promoted", "", "fixture", completedAt,
	); err != nil {
		t.Fatal(err)
	}
	pending, err = store.ListPendingWeeklyLeaderCandidatesAsOf(ctx, completedAt)
	if err != nil || len(pending) != 0 {
		t.Fatalf("consumed candidate remained pending: %+v/%v", pending, err)
	}

	secondInput := runInput
	secondInput.SessionDate = base.AddDate(0, 0, 1)
	secondInput.AsOf = base.AddDate(0, 0, 1)
	secondInput.DecisionAt = secondInput.AsOf.Add(15 * time.Minute)
	secondInput.StartedAt = secondInput.AsOf.Add(16 * time.Minute)
	second, claimed, err := store.ClaimWeeklyLeaderRun(ctx, secondInput, time.Hour)
	if err != nil || !claimed {
		t.Fatalf("second session claim = %+v/%v/%v", second, claimed, err)
	}
	scoreB, percentileB, rankB := 0.82, 0.90, 2
	qualifications[0].RunID = second.ID
	qualifications[0].CompositeScore = &scoreA
	qualifications[0].PromotionStatus = "not_requested"
	qualifications[1] = store.WeeklyLeaderQualification{
		RunID: second.ID, UniverseVersionID: versionID, Ticker: "TEST_WL_BBB",
		Outcome: "qualified", Sector: "Technology", Industry: "Software",
		MarketCapBand: "mid", VolatilityBand: "medium", Return20: &return20,
		Return60: &return60, Return126: &return126,
		MedianDollarVolume20: &medianDV, CompositeScore: &scoreB,
		ScorePercentile: &percentileB, Rank: &rankB, FeatureCoverage: &coverage,
		BarSetHash: "fixture-bars-b",
	}
	if err := store.CompleteWeeklyLeaderRun(
		ctx, second.ID, second.AttemptCount,
		secondInput.StartedAt.Add(time.Minute), qualifications,
		"fixture-benchmark-2",
	); err != nil {
		t.Fatal(err)
	}
	var added, persistent, removed int
	if err := store.GetDB().QueryRow(ctx, `
		SELECT
			COUNT(*) FILTER (WHERE event='added'),
			COUNT(*) FILTER (WHERE event='persistent'),
			COUNT(*) FILTER (WHERE event='removed')
		FROM weekly_leader_transitions WHERE run_id=$1`, second.ID,
	).Scan(&added, &persistent, &removed); err != nil {
		t.Fatal(err)
	}
	if added != 1 || persistent != 1 || removed != 0 {
		t.Fatalf("transitions added/persistent/removed = %d/%d/%d", added, persistent, removed)
	}
	var streak int
	if err := store.GetDB().QueryRow(ctx, `
		SELECT qualification_streak FROM weekly_leader_qualifications
		WHERE run_id=$1 AND ticker='TEST_WL_AAA'`, second.ID,
	).Scan(&streak); err != nil {
		t.Fatal(err)
	}
	if streak != 2 {
		t.Fatalf("persistent qualification streak = %d, want 2", streak)
	}
}

func TestWeeklyLeaderFailedRunCanBeReclaimed(t *testing.T) {
	ctx := context.Background()
	var versionID string
	if err := store.GetDB().QueryRow(ctx, `
		SELECT id FROM swing_universe_versions
		WHERE provider='test-weekly-leader' AND status='completed'
		ORDER BY as_of DESC LIMIT 1`).Scan(&versionID); err != nil {
		t.Skip("weekly leader lifecycle fixture unavailable")
	}
	base := time.Date(2026, time.July, 22, 20, 0, 0, 0, time.UTC)
	input := store.WeeklyLeaderRun{
		SessionDate: base, AsOf: base, DecisionAt: base.Add(15 * time.Minute),
		UniverseVersionID: versionID, StartedAt: base.Add(16 * time.Minute),
		ConfigHash:     "weekly-leader-integration-reclaim",
		ConfigSnapshot: json.RawMessage(`{}`), FeatureVersion: "weekly-leader-v1",
		Benchmark: "SPY", HistoryProvider: "test",
	}
	first, claimed, err := store.ClaimWeeklyLeaderRun(ctx, input, time.Hour)
	if err != nil || !claimed {
		t.Fatalf("claim = %+v/%v/%v", first, claimed, err)
	}
	if err := store.FailWeeklyLeaderRun(
		ctx, first.ID, first.AttemptCount,
		input.StartedAt.Add(time.Minute), errors.New("fixture"),
	); err != nil {
		t.Fatal(err)
	}
	input.StartedAt = input.StartedAt.Add(2 * time.Minute)
	reclaimed, claimed, err := store.ClaimWeeklyLeaderRun(ctx, input, time.Hour)
	if err != nil || !claimed {
		t.Fatalf("reclaim = %+v/%v/%v", reclaimed, claimed, err)
	}
	if reclaimed.ID != first.ID || reclaimed.AttemptCount != 2 {
		t.Fatalf("reclaimed run = %+v, want same ID and attempt 2", reclaimed)
	}
	if err := store.FailWeeklyLeaderRun(
		ctx, first.ID, first.AttemptCount,
		input.StartedAt.Add(3*time.Minute), errors.New("stale worker"),
	); err == nil {
		t.Fatal("superseded claim was allowed to mutate the reclaimed run")
	}
	if err := store.FailWeeklyLeaderRun(
		ctx, reclaimed.ID, reclaimed.AttemptCount,
		input.StartedAt.Add(4*time.Minute), errors.New("fixture cleanup"),
	); err != nil {
		t.Fatal(err)
	}
}
