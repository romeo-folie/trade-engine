package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/store"
)

func TestPremarketEpisodicLifecycleCutoffAndPromotion(t *testing.T) {
	ctx := context.Background()
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	sessionDate := time.Date(2099, time.January, 6, 0, 0, 0, 0, loc)
	checkpoint := time.Date(2099, time.January, 6, 9, 0, 0, 0, loc)
	cutoff := time.Date(2099, time.January, 6, 9, 25, 0, 0, loc)
	const ticker = "TEST_PREMARKET_EPISODIC"

	claim := beginClaimedSwingUniverseFixture(t, ctx, store.SwingUniverseVersion{
		Provider: "fixture", AsOf: sessionDate.Add(-time.Hour),
		CapturedAt: sessionDate.Add(-50 * time.Minute),
		Filters:    json.RawMessage(`{"test":"premarket-episodic"}`),
	})
	versionID := claim.ActiveVersionID
	price, marketCap, dollarVolume := 10.5, 500.0, 20_000_000.0
	if err := store.ReplaceClaimedSwingUniverseMembers(ctx, claim, []store.SwingUniverseMember{{
		Ticker: ticker, Eligible: true, Price: &price, MarketCapMM: &marketCap,
		DollarVolume: &dollarVolume, Sector: "Technology", Industry: "Software",
		TaxonomyStatus: "known", TaxonomyProvider: "fixture",
	}}); err != nil {
		t.Fatal(err)
	}
	completeClaimedSwingUniverseFixture(t, ctx, claim, sessionDate.Add(-40*time.Minute))
	t.Cleanup(func() {
		_, _ = store.GetDB().Exec(context.Background(),
			"DELETE FROM watchlist WHERE ticker=$1", ticker)
		_, _ = store.GetDB().Exec(context.Background(),
			"DELETE FROM premarket_episodic_runs WHERE universe_version_id=$1", versionID)
		_, _ = store.GetDB().Exec(context.Background(),
			"UPDATE swing_universe_versions SET status='building' WHERE id=$1", versionID)
		_, _ = store.GetDB().Exec(context.Background(),
			"DELETE FROM swing_universe_versions WHERE id=$1", versionID)
	})

	run, claimed, err := store.ClaimPremarketEpisodicRun(
		ctx, store.PremarketEpisodicRun{
			SessionDate: sessionDate, CheckpointAt: checkpoint,
			HardCutoffAt: cutoff, UniverseVersionID: versionID,
			StartedAt: checkpoint, ConfigHash: "premarket-fixture",
			ConfigSnapshot:     json.RawMessage(`{"feed":"sip"}`),
			FeatureVersion:     "premarket-episodic-v1",
			MarketDataProvider: "fixture", Feed: "sip",
			EntitlementStatus: "available",
		}, time.Hour,
	)
	if err != nil || !claimed {
		t.Fatalf("claim run = %+v/%v/%v", run, claimed, err)
	}
	score, gap, rvol, spread := .9, 5.0, 4.0, 12.0
	volume, trades := int64(200_000), int64(2_000)
	if err := store.CompletePremarketEpisodicRun(
		ctx, run.ID, run.AttemptCount, checkpoint.Add(10*time.Minute), 100, 1,
		[]store.PremarketEpisodicCandidate{{
			RunID: run.ID, UniverseVersionID: versionID, Ticker: ticker,
			Outcome: "qualified", Score: &score, Rank: intPointer(1),
			GapPct: &gap, PremarketVolume: &volume, TimeAdjustedRVOL: &rvol,
			TradeCount: &trades, SpreadBPS: &spread, LastPrice: &price,
			MarketCapMM: &marketCap, FloatStatus: "unavailable",
			CatalystStatus:    "confirmed_absent",
			CatalystReference: json.RawMessage(`{"status":"confirmed_absent"}`),
			PromotionStatus:   "pending",
		}},
	); err != nil {
		t.Fatal(err)
	}

	late, lateClaimed, err := store.ClaimPremarketEpisodicRun(
		ctx, store.PremarketEpisodicRun{
			SessionDate: sessionDate, CheckpointAt: checkpoint.Add(time.Minute),
			HardCutoffAt: cutoff, UniverseVersionID: versionID,
			StartedAt: checkpoint.Add(time.Minute), ConfigHash: "late-fixture",
			ConfigSnapshot:     json.RawMessage(`{}`),
			FeatureVersion:     "premarket-episodic-v1",
			MarketDataProvider: "fixture", Feed: "sip",
			EntitlementStatus: "available",
		}, time.Hour,
	)
	if err != nil || !lateClaimed {
		t.Fatalf("claim late run = %+v/%v/%v", late, lateClaimed, err)
	}
	err = store.CompletePremarketEpisodicRun(
		ctx, late.ID, late.AttemptCount, cutoff.Add(time.Second), 100, 0, nil,
	)
	if err == nil || !strings.Contains(err.Error(), "after hard cutoff") {
		t.Fatalf("late completion error = %v", err)
	}

	candidates, err := store.ClaimPremarketEpisodicCandidatesAsOf(
		ctx, sessionDate, cutoff, cutoff, time.Hour, "premarket-claim",
	)
	if err != nil || len(candidates) != 1 {
		t.Fatalf("candidates = %+v/%v", candidates, err)
	}
	item := store.WatchlistItem{
		Ticker: ticker, Platform: "stock",
		Strategies: []string{"premarket_episodic"},
		Timeframes: []string{"1H"}, MinConfidence: .8, Source: "scanner",
	}
	if err := store.PromoteScannerSourcesToWatchlist(
		ctx, item, store.ScannerSourcePromotionClaims{
			PremarketEpisodic: &store.PremarketEpisodicPromotionClaim{
				RunID: run.ID, Ticker: ticker, Token: "premarket-claim",
			},
		}, "fixture success", cutoff.Add(time.Minute),
	); err != nil {
		t.Fatal(err)
	}
	var promotionStatus string
	if err := store.GetDB().QueryRow(ctx, `
		SELECT promotion_status FROM premarket_episodic_candidates
		WHERE run_id=$1 AND ticker=$2`, run.ID, ticker,
	).Scan(&promotionStatus); err != nil {
		t.Fatal(err)
	}
	if promotionStatus != "promoted" {
		t.Fatalf("promotion status = %q", promotionStatus)
	}
}

func TestScannerDependencyBlockIsDurableAndResolvable(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("DATABASE_URL not set; skipping store integration test")
	}
	ctx := context.Background()
	decisionAt := time.Now().UTC().Truncate(time.Microsecond)
	sessionDate := decisionAt
	for i := 0; i < 2; i++ {
		if err := store.RecordScannerDependencyBlock(
			ctx, "premarket_episodic", sessionDate, decisionAt,
			"swing_universe", errors.New("expected session universe missing"),
		); err != nil {
			t.Fatal(err)
		}
	}
	var status string
	var attempts int
	if err := store.GetDB().QueryRow(ctx, `
		SELECT status, attempt_count FROM scanner_dependency_blocks
		WHERE scanner='premarket_episodic' AND session_date=$1::date
		  AND decision_at=$2 AND dependency='swing_universe'`,
		sessionDate, decisionAt,
	).Scan(&status, &attempts); err != nil {
		t.Fatal(err)
	}
	if status != "blocked" || attempts != 2 {
		t.Fatalf("dependency block = status %q attempts %d", status, attempts)
	}
	if err := store.ResolveScannerDependencyBlock(
		ctx, "premarket_episodic", sessionDate, decisionAt, "swing_universe",
	); err != nil {
		t.Fatal(err)
	}
	if err := store.GetDB().QueryRow(ctx, `
		SELECT status FROM scanner_dependency_blocks
		WHERE scanner='premarket_episodic' AND session_date=$1::date
		  AND decision_at=$2 AND dependency='swing_universe'`,
		sessionDate, decisionAt,
	).Scan(&status); err != nil || status != "resolved" {
		t.Fatalf("resolved dependency block = %q err=%v", status, err)
	}
}

func intPointer(value int) *int {
	return &value
}
