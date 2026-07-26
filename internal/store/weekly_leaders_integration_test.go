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
	claimedCandidates, err := store.ClaimWeeklyLeaderCandidatesAsOf(
		ctx, completedAt, completedAt, time.Hour, "promotion-claim-1",
	)
	if err != nil || len(claimedCandidates) != 1 ||
		claimedCandidates[0].Ticker != "TEST_WL_AAA" {
		t.Fatalf("claimed candidates = %+v/%v", claimedCandidates, err)
	}
	concurrent, err := store.ClaimWeeklyLeaderCandidatesAsOf(
		ctx, completedAt, completedAt, time.Hour, "promotion-claim-2",
	)
	if err != nil || len(concurrent) != 0 {
		t.Fatalf("concurrent promotion claim duplicated work: %+v/%v", concurrent, err)
	}
	concurrent, err = store.ClaimWeeklyLeaderCandidatesAsOf(
		ctx, completedAt, completedAt.Add(time.Hour-time.Nanosecond),
		time.Hour, "promotion-claim-2",
	)
	if err != nil || len(concurrent) != 0 {
		t.Fatalf("fresh promotion lease was stolen: %+v/%v", concurrent, err)
	}
	claimedCandidates, err = store.ClaimWeeklyLeaderCandidatesAsOf(
		ctx, completedAt, completedAt.Add(time.Hour), time.Hour,
		"promotion-claim-2",
	)
	if err != nil || len(claimedCandidates) != 1 ||
		claimedCandidates[0].PromotionClaimToken != "promotion-claim-2" {
		t.Fatalf("expired promotion lease was not reclaimable: %+v/%v", claimedCandidates, err)
	}
	watchlistItem := store.WatchlistItem{
		Ticker: "TEST_WL_AAA", Platform: "stock",
		Strategies: []string{"momentum", "weekly_leader"},
		Timeframes: []string{"1D"}, MinConfidence: 0.8, Source: "scanner",
	}
	if err := store.PromoteWeeklyLeaderToWatchlist(
		ctx, watchlistItem, first.ID, "TEST_WL_AAA", "promotion-claim-1",
		"stale fixture", completedAt,
	); !errors.Is(err, store.ErrWeeklyLeaderPromotionClaimLost) {
		t.Fatalf("stale claim reached watchlist transaction: %v", err)
	}
	if err := store.ReleaseWeeklyLeaderPromotionClaims(
		ctx, "promotion-claim-1", "stale fixture release",
	); err != nil {
		t.Fatal(err)
	}
	concurrent, err = store.ClaimWeeklyLeaderCandidatesAsOf(
		ctx, completedAt, completedAt.Add(time.Hour+time.Minute),
		time.Hour, "promotion-claim-3",
	)
	if err != nil || len(concurrent) != 0 {
		t.Fatalf("stale release reset the current claim: %+v/%v", concurrent, err)
	}
	if err := store.ReleaseWeeklyLeaderPromotionClaims(
		ctx, "promotion-claim-2", "fixture retry",
	); err != nil {
		t.Fatal(err)
	}
	claimedCandidates, err = store.ClaimWeeklyLeaderCandidatesAsOf(
		ctx, completedAt, completedAt.Add(time.Hour+2*time.Minute), time.Hour,
		"promotion-claim-3",
	)
	if err != nil || len(claimedCandidates) != 1 {
		t.Fatalf("released promotion was not reclaimable: %+v/%v", claimedCandidates, err)
	}
	if err := store.PromoteWeeklyLeaderToWatchlist(
		ctx, watchlistItem, first.ID, "TEST_WL_AAA", "promotion-claim-3",
		"fixture", completedAt,
	); err != nil {
		t.Fatal(err)
	}
	var active bool
	if err := store.GetDB().QueryRow(ctx, `
		SELECT active FROM watchlist WHERE ticker='TEST_WL_AAA' AND platform='stock'
	`).Scan(&active); err != nil || !active {
		t.Fatalf("atomic watchlist promotion missing: active=%v err=%v", active, err)
	}
	claimedCandidates, err = store.ClaimWeeklyLeaderCandidatesAsOf(
		ctx, completedAt, completedAt.Add(time.Hour+3*time.Minute), time.Hour,
		"promotion-claim-4",
	)
	if err != nil || len(claimedCandidates) != 0 {
		t.Fatalf("consumed candidate remained claimable: %+v/%v", claimedCandidates, err)
	}

	alternateInput := runInput
	alternateInput.ConfigHash = "weekly-leader-integration-v2"
	alternateInput.ConfigSnapshot = json.RawMessage(`{"selection_percentile":0.85}`)
	alternateInput.StartedAt = base.Add(21 * time.Minute)
	alternate, claimed, err := store.ClaimWeeklyLeaderRun(ctx, alternateInput, time.Hour)
	if err != nil || !claimed {
		t.Fatalf("alternate same-session claim = %+v/%v/%v", alternate, claimed, err)
	}
	scoreB, percentileB, rankB := 0.82, 0.90, 2
	alternateQualifications := append([]store.WeeklyLeaderQualification(nil), qualifications...)
	alternateQualifications[0].RunID = alternate.ID
	alternateQualifications[0].PromotionStatus = "not_requested"
	alternateQualifications[1] = store.WeeklyLeaderQualification{
		RunID: alternate.ID, UniverseVersionID: versionID, Ticker: "TEST_WL_BBB",
		Outcome: "qualified", Sector: "Technology", Industry: "Software",
		MarketCapBand: "mid", VolatilityBand: "medium", Return20: &return20,
		Return60: &return60, Return126: &return126,
		MedianDollarVolume20: &medianDV, CompositeScore: &scoreB,
		ScorePercentile: &percentileB, Rank: &rankB, FeatureCoverage: &coverage,
		BarSetHash: "fixture-bars-b", PromotionStatus: "pending",
	}
	alternateCompletedAt := base.Add(22 * time.Minute)
	if err := store.CompleteWeeklyLeaderRun(
		ctx, alternate.ID, alternate.AttemptCount, alternateCompletedAt,
		alternateQualifications, "fixture-benchmark-2",
	); err != nil {
		t.Fatal(err)
	}
	claimedCandidates, err = store.ClaimWeeklyLeaderCandidatesAsOf(
		ctx, alternateCompletedAt, alternateCompletedAt, time.Hour,
		"same-session-old-claim",
	)
	if err != nil || len(claimedCandidates) != 1 ||
		claimedCandidates[0].Ticker != "TEST_WL_BBB" {
		t.Fatalf("alternate same-session candidates = %+v/%v", claimedCandidates, err)
	}

	newestInput := alternateInput
	newestInput.ConfigHash = "weekly-leader-integration-v3"
	newestInput.ConfigSnapshot = json.RawMessage(`{"selection_percentile":0.8}`)
	newestInput.DecisionAt = alternateInput.DecisionAt.Add(time.Minute)
	newestInput.StartedAt = base.Add(23 * time.Minute)
	newest, claimed, err := store.ClaimWeeklyLeaderRun(ctx, newestInput, time.Hour)
	if err != nil || !claimed {
		t.Fatalf("newest same-session claim = %+v/%v/%v", newest, claimed, err)
	}
	newestQualifications := append(
		[]store.WeeklyLeaderQualification(nil), alternateQualifications...,
	)
	for i := range newestQualifications {
		newestQualifications[i].RunID = newest.ID
		newestQualifications[i].PromotionStatus = "not_requested"
	}
	newestCompletedAt := base.Add(24 * time.Minute)
	if err := store.CompleteWeeklyLeaderRun(
		ctx, newest.ID, newest.AttemptCount, newestCompletedAt,
		newestQualifications, "fixture-benchmark-3",
	); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkWeeklyLeaderPromotion(
		ctx, alternate.ID, "TEST_WL_BBB", "same-session-old-claim",
		"rejected", "", "stale same-session worker", newestCompletedAt,
	); err == nil {
		t.Fatal("older same-session worker retained a valid promotion claim")
	}
	staleWatchlistItem := watchlistItem
	staleWatchlistItem.Ticker = "TEST_WL_BBB"
	if err := store.PromoteWeeklyLeaderToWatchlist(
		ctx, staleWatchlistItem, alternate.ID, "TEST_WL_BBB",
		"same-session-old-claim", "stale same-session worker", newestCompletedAt,
	); !errors.Is(err, store.ErrWeeklyLeaderPromotionClaimLost) {
		t.Fatalf("older same-session worker reached watchlist transaction: %v", err)
	}
	var staleWatchlistCount int
	if err := store.GetDB().QueryRow(ctx, `
		SELECT COUNT(*) FROM watchlist
		WHERE ticker='TEST_WL_BBB' AND platform='stock'`,
	).Scan(&staleWatchlistCount); err != nil {
		t.Fatal(err)
	}
	if staleWatchlistCount != 0 {
		t.Fatal("older same-session worker wrote a watchlist row")
	}
	var alternatePromotionStatus string
	if err := store.GetDB().QueryRow(ctx, `
		SELECT promotion_status FROM weekly_leader_qualifications
		WHERE run_id=$1 AND ticker='TEST_WL_BBB'`, alternate.ID,
	).Scan(&alternatePromotionStatus); err != nil {
		t.Fatal(err)
	}
	if alternatePromotionStatus != "superseded" {
		t.Fatalf("older same-session status = %s, want superseded", alternatePromotionStatus)
	}
	latest, err := store.GetLatestCompletedWeeklyLeaderRunAsOf(ctx, newestCompletedAt)
	if err != nil || latest == nil || latest.ID != newest.ID {
		t.Fatalf("latest same-session run = %+v/%v, want %s", latest, err, newest.ID)
	}

	laggingInput := alternateInput
	laggingInput.ConfigHash = "weekly-leader-integration-v4"
	laggingInput.ConfigSnapshot = json.RawMessage(`{"selection_percentile":0.95}`)
	laggingInput.StartedAt = base.Add(25 * time.Minute)
	lagging, claimed, err := store.ClaimWeeklyLeaderRun(ctx, laggingInput, time.Hour)
	if err != nil || !claimed {
		t.Fatalf("lagging same-session claim = %+v/%v/%v", lagging, claimed, err)
	}
	laggingQualifications := append(
		[]store.WeeklyLeaderQualification(nil), alternateQualifications...,
	)
	for i := range laggingQualifications {
		laggingQualifications[i].RunID = lagging.ID
	}
	laggingQualifications[1].PromotionStatus = "pending"
	laggingCompletedAt := base.Add(26 * time.Minute)
	if err := store.CompleteWeeklyLeaderRun(
		ctx, lagging.ID, lagging.AttemptCount, laggingCompletedAt,
		laggingQualifications, "fixture-benchmark-4",
	); err != nil {
		t.Fatal(err)
	}
	var laggingPromotionStatus string
	if err := store.GetDB().QueryRow(ctx, `
		SELECT promotion_status FROM weekly_leader_qualifications
		WHERE run_id=$1 AND ticker='TEST_WL_BBB'`, lagging.ID,
	).Scan(&laggingPromotionStatus); err != nil {
		t.Fatal(err)
	}
	if laggingPromotionStatus != "superseded" {
		t.Fatalf("lagging same-session status = %s, want superseded", laggingPromotionStatus)
	}
	latest, err = store.GetLatestCompletedWeeklyLeaderRunAsOf(ctx, laggingCompletedAt)
	if err != nil || latest == nil || latest.ID != newest.ID {
		t.Fatalf("lagging completion displaced latest run = %+v/%v", latest, err)
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
	input.DecisionAt = input.DecisionAt.Add(time.Minute)
	input.ConfigSnapshot = json.RawMessage(`{"promotion":"changed"}`)
	input.HistoryProvider = "test-reclaimed"
	reclaimed, claimed, err := store.ClaimWeeklyLeaderRun(ctx, input, time.Hour)
	if err != nil || !claimed {
		t.Fatalf("reclaim = %+v/%v/%v", reclaimed, claimed, err)
	}
	if reclaimed.ID != first.ID || reclaimed.AttemptCount != 2 {
		t.Fatalf("reclaimed run = %+v, want same ID and attempt 2", reclaimed)
	}
	var reclaimedConfig map[string]string
	if err := json.Unmarshal(reclaimed.ConfigSnapshot, &reclaimedConfig); err != nil {
		t.Fatal(err)
	}
	if !reclaimed.DecisionAt.Equal(input.DecisionAt) ||
		reclaimedConfig["promotion"] != "changed" ||
		reclaimed.HistoryProvider != input.HistoryProvider {
		t.Fatalf("reclaimed evidence stayed stale: %+v", reclaimed)
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
