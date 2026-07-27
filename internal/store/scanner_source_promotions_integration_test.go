package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/store"
)

func TestScannerSourcePromotionIsAtomicAcrossWeeklyAndIndustryClaims(t *testing.T) {
	ctx := context.Background()
	base := time.Date(2099, time.January, 5, 20, 0, 0, 0, time.UTC)
	const ticker = "TEST_MULTI_SOURCE"
	versionID, err := store.BeginSwingUniverseVersion(ctx, store.SwingUniverseVersion{
		Provider: "test-multi-source-promotion", AsOf: base.Add(-time.Hour),
		CapturedAt: base.Add(-50 * time.Minute),
		Filters:    json.RawMessage(`{"test":"multi-source-promotion"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	price, marketCap, dollarVolume := 120.0, 12_000.0, 90_000_000.0
	taxonomyAt := base.Add(-24 * time.Hour)
	if err := store.ReplaceSwingUniverseMembers(ctx, versionID, []store.SwingUniverseMember{{
		Ticker: ticker, Eligible: true, Price: &price, MarketCapMM: &marketCap,
		DollarVolume: &dollarVolume, Sector: "Technology", Industry: "Software",
		TaxonomyStatus: "known", TaxonomyAsOf: &taxonomyAt,
		TaxonomyProvider: "fixture",
	}}); err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteSwingUniverseVersion(
		ctx, versionID, base.Add(-40*time.Minute),
	); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = store.GetDB().Exec(context.Background(),
			"DELETE FROM watchlist WHERE ticker=$1", ticker)
		_, _ = store.GetDB().Exec(context.Background(),
			"DELETE FROM industry_rotation_runs WHERE universe_version_id=$1", versionID)
		_, _ = store.GetDB().Exec(context.Background(),
			"DELETE FROM weekly_leader_runs WHERE universe_version_id=$1", versionID)
		_, _ = store.GetDB().Exec(context.Background(),
			"UPDATE swing_universe_versions SET status='building' WHERE id=$1", versionID)
		_, _ = store.GetDB().Exec(context.Background(),
			"DELETE FROM swing_universe_versions WHERE id=$1", versionID)
	})

	weeklyRun, claimed, err := store.ClaimWeeklyLeaderRun(ctx, store.WeeklyLeaderRun{
		SessionDate: base, AsOf: base, DecisionAt: base.Add(15 * time.Minute),
		UniverseVersionID: versionID, StartedAt: base.Add(16 * time.Minute),
		ConfigHash: "multi-source-weekly", ConfigSnapshot: json.RawMessage(`{}`),
		FeatureVersion: "weekly-leader-v2", Benchmark: "SPY",
		HistoryProvider: "fixture",
	}, time.Hour)
	if err != nil || !claimed {
		t.Fatalf("claim weekly run = %+v/%v/%v", weeklyRun, claimed, err)
	}
	score, percentile, coverage := .91, .99, 1.0
	rank, sessions := 1, 253
	return20, return60, return126 := .10, .20, .35
	medianDollarVolume := 90_000_000.0
	if err := store.CompleteWeeklyLeaderRun(
		ctx, weeklyRun.ID, weeklyRun.AttemptCount, base.Add(20*time.Minute),
		[]store.WeeklyLeaderQualification{{
			RunID: weeklyRun.ID, UniverseVersionID: versionID, Ticker: ticker,
			Outcome: "qualified", BarSetHash: "weekly-bars",
			HistorySessions: sessions, Sector: "Technology", Industry: "Software",
			ClosePrice: &price, Return20: &return20, Return60: &return60,
			Return126: &return126, CompositeScore: &score,
			MedianDollarVolume20: &medianDollarVolume,
			ScorePercentile:      &percentile, Rank: &rank, FeatureCoverage: &coverage,
			PromotionStatus: "pending",
		}}, "weekly-benchmark",
	); err != nil {
		t.Fatal(err)
	}

	industryRun, claimed, err := store.ClaimIndustryRotationRun(
		ctx, store.IndustryRotationRun{
			SessionDate: base, AsOf: base, DecisionAt: base.Add(15 * time.Minute),
			UniverseVersionID: versionID, StartedAt: base.Add(16 * time.Minute),
			ConfigHash: "multi-source-industry", ConfigSnapshot: json.RawMessage(`{}`),
			FeatureVersion: "potent-industry-v1", MarketBenchmark: "SPY",
			HistoryProvider: "fixture",
		}, time.Hour,
	)
	if err != nil || !claimed {
		t.Fatalf("claim industry run = %+v/%v/%v", industryRun, claimed, err)
	}
	if err := store.CompleteIndustryRotationRun(
		ctx, industryRun.ID, industryRun.AttemptCount, base.Add(21*time.Minute),
		[]store.IndustryRotationGroup{{
			RunID: industryRun.ID, GroupType: "industry", GroupKey: "Software",
			Sector: "Technology", Industry: "Software", Benchmark: "XLK",
			Outcome: "qualified", Archetype: "broad_strong",
			EligibleMemberCount: 1, EvaluatedMemberCount: 1,
			FreshTaxonomyCount: 1, MemberCoverage: 1,
			FreshTaxonomyCoverage: 1, PotencyScore: &score,
			ScorePercentile: &percentile, Rank: &rank, FeatureCoverage: &coverage,
		}},
		[]store.IndustryRotationMember{{
			RunID: industryRun.ID, UniverseVersionID: versionID,
			GroupType: "industry", GroupKey: "Software", Ticker: ticker,
			Outcome: "qualified", HistorySessions: sessions,
			BarSetHash: "industry-bars", ClosePrice: &price, Return20: &return20,
			MemberScore: &score, MemberRank: &rank, FeatureCoverage: &coverage,
			PromotionStatus: "pending",
		}}, json.RawMessage(`{"SPY":"fixture"}`),
	); err != nil {
		t.Fatal(err)
	}

	weeklyCandidates, err := store.ClaimWeeklyLeaderCandidatesAsOf(
		ctx, base, base.Add(21*time.Minute), base.Add(22*time.Minute),
		time.Hour, "weekly-claim",
	)
	if err != nil || len(weeklyCandidates) != 1 {
		t.Fatalf("weekly candidates = %+v/%v", weeklyCandidates, err)
	}
	staleIndustryCandidates, err := store.ClaimIndustryRotationCandidatesAsOf(
		ctx, base.AddDate(0, 0, 1), base.Add(21*time.Minute),
		base.Add(22*time.Minute), time.Hour, "stale-industry-session",
	)
	if err != nil || len(staleIndustryCandidates) != 0 {
		t.Fatalf("stale industry session was claimable: %+v/%v",
			staleIndustryCandidates, err)
	}
	industryCandidates, err := store.ClaimIndustryRotationCandidatesAsOf(
		ctx, base, base.Add(21*time.Minute), base.Add(22*time.Minute),
		time.Hour, "industry-claim",
	)
	if err != nil || len(industryCandidates) != 1 ||
		industryCandidates[0].GroupKey != "Software" ||
		industryCandidates[0].MemberScore != score ||
		industryCandidates[0].TaxonomyProvider != "fixture" {
		t.Fatalf("industry candidates = %+v/%v", industryCandidates, err)
	}

	item := store.WatchlistItem{
		Ticker: ticker, Platform: "stock",
		Strategies: []string{"momentum", "weekly_leader", "potent_industry"},
		Timeframes: []string{"1D"}, MinConfidence: .8, Source: "scanner",
	}
	claims := store.ScannerSourcePromotionClaims{
		WeeklyLeader: &store.WeeklyLeaderPromotionClaim{
			RunID: weeklyRun.ID, Ticker: ticker, Token: "weekly-claim",
		},
		PotentIndustry: &store.PotentIndustryPromotionClaim{
			RunID: industryRun.ID, GroupType: "industry", GroupKey: "Software",
			Ticker: ticker, Token: "stale-industry-claim",
		},
	}
	err = store.PromoteScannerSourcesToWatchlist(
		ctx, item, claims, "fixture stale attempt", base.Add(23*time.Minute),
	)
	var claimErr *store.ScannerSourcePromotionClaimError
	if !errors.As(err, &claimErr) || claimErr.Source != "potent_industry" {
		t.Fatalf("stale multi-source claim error = %v", err)
	}
	var watchlistCount int
	var weeklyStatus string
	if err := store.GetDB().QueryRow(ctx,
		"SELECT COUNT(*) FROM watchlist WHERE ticker=$1", ticker,
	).Scan(&watchlistCount); err != nil {
		t.Fatal(err)
	}
	if err := store.GetDB().QueryRow(ctx, `
		SELECT promotion_status FROM weekly_leader_qualifications
		WHERE run_id=$1 AND ticker=$2`, weeklyRun.ID, ticker,
	).Scan(&weeklyStatus); err != nil {
		t.Fatal(err)
	}
	if watchlistCount != 0 || weeklyStatus != "processing" {
		t.Fatalf("partial promotion escaped rollback: watchlist=%d weekly=%s",
			watchlistCount, weeklyStatus)
	}

	claims.PotentIndustry.Token = "industry-claim"
	if err := store.PromoteScannerSourcesToWatchlist(
		ctx, item, claims, "fixture success", base.Add(24*time.Minute),
	); err != nil {
		t.Fatal(err)
	}
	var industryStatus string
	if err := store.GetDB().QueryRow(ctx, `
		SELECT w.promotion_status, i.promotion_status
		FROM weekly_leader_qualifications w
		JOIN industry_rotation_members i ON i.ticker=w.ticker
		WHERE w.run_id=$1 AND i.run_id=$2 AND w.ticker=$3`,
		weeklyRun.ID, industryRun.ID, ticker,
	).Scan(&weeklyStatus, &industryStatus); err != nil {
		t.Fatal(err)
	}
	if weeklyStatus != "promoted" || industryStatus != "promoted" {
		t.Fatalf("claims not completed together: weekly=%s industry=%s",
			weeklyStatus, industryStatus)
	}

	if _, err := store.GetDB().Exec(ctx, `
		UPDATE weekly_leader_qualifications
		SET promotion_status='processing', promotion_claim_token='weekly-fallback',
		    promotion_claimed_at=$3
		WHERE run_id=$1 AND ticker=$2`,
		weeklyRun.ID, ticker, base.Add(25*time.Minute),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetDB().Exec(ctx, `
		UPDATE industry_rotation_members
		SET promotion_status='processing', promotion_claim_token='industry-fallback',
		    promotion_claimed_at=$5
		WHERE run_id=$1 AND group_type=$2 AND group_key=$3 AND ticker=$4`,
		industryRun.ID, "industry", "Software", ticker, base.Add(25*time.Minute),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetDB().Exec(ctx, `
		UPDATE watchlist
		SET strategies=ARRAY['newer_worker'], min_confidence=.99
		WHERE ticker=$1 AND platform='stock'`, ticker,
	); err != nil {
		t.Fatal(err)
	}
	claims.WeeklyLeader.Token = "weekly-fallback"
	claims.PotentIndustry.Token = "industry-fallback"
	err = store.PromoteScannerSourcesToWatchlistUnlessActive(
		ctx, item, claims, "fallback must preserve active row", base.Add(26*time.Minute),
	)
	if !errors.Is(err, store.ErrWatchlistActiveItemExists) {
		t.Fatalf("active-row fallback race = %v", err)
	}
	var strategies []string
	var confidence float64
	if err := store.GetDB().QueryRow(ctx, `
		SELECT w.strategies, w.min_confidence, q.promotion_status,
		       i.promotion_status
		FROM watchlist w
		JOIN weekly_leader_qualifications q ON q.ticker=w.ticker
		JOIN industry_rotation_members i ON i.ticker=w.ticker
		WHERE w.ticker=$1 AND w.platform='stock'
		  AND q.run_id=$2 AND i.run_id=$3`,
		ticker, weeklyRun.ID, industryRun.ID,
	).Scan(&strategies, &confidence, &weeklyStatus, &industryStatus); err != nil {
		t.Fatal(err)
	}
	if len(strategies) != 1 || strategies[0] != "newer_worker" ||
		confidence != .99 || weeklyStatus != "active_watchlist" ||
		industryStatus != "active_watchlist" {
		t.Fatalf(
			"fallback overwrote active row or stranded claims: strategies=%v confidence=%v weekly=%s industry=%s",
			strategies, confidence, weeklyStatus, industryStatus,
		)
	}
}
