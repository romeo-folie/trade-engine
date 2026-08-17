package store_test

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/analysis"
	"github.com/romeo-folie/trade-engine/internal/store"
	"github.com/romeo-folie/trade-engine/internal/tradeprofile"
)

func TestScannerSnapshotLineageAndFunnel(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("DATABASE_URL not set; skipping store integration test")
	}
	ctx := context.Background()
	startedAt := time.Now().UTC().Truncate(time.Microsecond)
	universeAsOf := startedAt.Add(-time.Minute)
	claim := beginClaimedSwingUniverseFixture(t, ctx, store.SwingUniverseVersion{
		Provider: "scanner-snapshot-test", AsOf: universeAsOf, CapturedAt: universeAsOf,
	})
	universeID := claim.ActiveVersionID
	if _, err := store.BeginScannerRun(ctx, store.ScannerRun{
		StartedAt:         startedAt,
		UniverseVersionID: universeID,
		UniverseAsOf:      &universeAsOf,
	}); err == nil {
		t.Fatal("BeginScannerRun accepted a building universe")
	}
	completedAt := universeAsOf.Add(30 * time.Second)
	completeClaimedSwingUniverseFixture(t, ctx, claim, completedAt)
	callerUniverseAsOf := startedAt.Add(time.Hour)
	runID, err := store.BeginScannerRun(ctx, store.ScannerRun{
		StartedAt:                    startedAt,
		PullbackUniverseEnabled:      true,
		PullbackMinMomentumReturnPct: 30,
		PullbackMinMarketCapMM:       100,
		PullbackMinDollarVolume:      1_000_000,
		CandidateCap:                 10,
		PromotionMaxPerIndustry:      2,
		MinConviction:                .55,
		TTLDays:                      3,
		UniverseVersionID:            universeID,
		UniverseAsOf:                 &callerUniverseAsOf,
		RegimeReference:              "pending",
	})
	if err != nil {
		t.Fatalf("BeginScannerRun: %v", err)
	}

	run := store.ScannerRun{
		ID:                      runID,
		CompletedAt:             startedAt.Add(time.Second),
		Status:                  "completed",
		RawCandidateCount:       2,
		EvaluatedCandidateCount: 2,
		ScoredCandidateCount:    2,
		AddedCandidateCount:     1,
	}
	err = store.FinishScannerRun(ctx, run, []store.ScannerCandidateSnapshot{
		{
			ScannerRunID: runID,
			Ticker:       "SNAP_A",
			Sources:      []string{"weekly_leader", "mover", "political_disclosure"},
			DiscoveryContext: &tradeprofile.DiscoveryContext{
				Version: tradeprofile.DiscoveryContextVersion, Sources: []string{"mover", "weekly_leader", "political_disclosure"},
				Thesis: "Leader at scan time", EvidenceBySource: map[string]json.RawMessage{
					"weekly_leader":        json.RawMessage(`{"rank":1}`),
					"mover":                json.RawMessage(`{"change_pct":8.2}`),
					"political_disclosure": json.RawMessage(`{"version":1,"ticker":"SNAP_A","distinct_filer_count":2,"distinct_buyer_count":2,"report_count":2,"transaction_count":2,"purchase_count":2,"sale_count":0,"exchange_count":0,"other_transaction_count":0,"purchase_amounts":{"known_transaction_count":0,"unknown_transaction_count":2,"minimum_total":0,"maximum_total":0},"sale_amounts":{"known_transaction_count":0,"unknown_transaction_count":0,"minimum_total":0,"maximum_total":0},"disclosure_lag_days":{"known_transaction_count":0,"minimum_days":0,"maximum_days":0,"latest_transaction_days":0},"repeated_activity_by_one_filer":false,"independent_buyer_activity":true,"contradictory_buying_and_selling":false,"filers":[],"data_quality":{"canonical_identity_basis":"normalized source, chamber, and filer-name tokens"}}`),
				},
			},
			ExecutionProfile: &tradeprofile.ExecutionProfile{
				Version: tradeprofile.ExecutionProfileVersion, RecommendedLens: "momentum",
				AdmissibleLenses: []string{"momentum", "trend_following"}, PrimaryTimeframe: "1D",
				EntryTimingTimeframe: "4H", ScorerRationale: "Leadership needs timed entry.",
			},
			QualificationKnown:     true,
			PullbackQualified:      true,
			QualificationStage:     "qualified",
			EligibleForScoring:     true,
			LLMAction:              "add",
			LLMConviction:          .8,
			PassedMinConviction:    true,
			CapSelected:            true,
			WatchlistAdded:         true,
			Strategies:             []string{"momentum", "pullback_strength"},
			FinalStage:             "watchlist_added",
			SourceScores:           map[string]float64{"weekly_leader": 0, "mover": 0},
			UniverseMember:         boolPtr(true),
			Sector:                 "Technology",
			Industry:               "Software",
			TaxonomyStatus:         "known",
			TaxonomyAsOf:           &universeAsOf,
			TaxonomyProvider:       "scanner-snapshot-test",
			TaxonomyEvidence:       json.RawMessage(`{"confidence":0}`),
			PotentIndustryEvidence: "Software broad_strong; member rank 1",
		},
		{
			ScannerRunID:       runID,
			Ticker:             "SNAP_B",
			Sources:            []string{"active"},
			QualificationKnown: true,
			QualificationStage: "momentum",
			EligibleForScoring: true,
			LLMAction:          "skip",
			LLMConviction:      .2,
			FinalStage:         "llm_skipped",
			PromotionEvidence:  json.RawMessage(`{"reason":"exposure_correlation_cap","conflict_key":"HELD","conflict_correlation":0.91}`),
		},
	})
	if err != nil {
		t.Fatalf("FinishScannerRun: %v", err)
	}
	var storedUniverseID, regime string
	var storedPromotionCap int
	var storedUniverseAsOf time.Time
	var sourceScores, discoveryContext, executionProfile json.RawMessage
	var universeMember bool
	var potentIndustryEvidence string
	err = store.GetDB().QueryRow(ctx, `
		SELECT r.universe_version_id::text, r.universe_as_of, r.regime_reference,
		       r.promotion_max_per_industry,
		       c.source_scores, c.universe_member, c.potent_industry_evidence,
		       c.discovery_context, c.execution_profile
		FROM scanner_runs r
		JOIN scanner_candidate_snapshots c ON c.scanner_run_id=r.id
		WHERE r.id=$1 AND c.ticker='SNAP_A'`,
		runID,
	).Scan(
		&storedUniverseID, &storedUniverseAsOf, &regime, &storedPromotionCap,
		&sourceScores, &universeMember, &potentIndustryEvidence,
		&discoveryContext, &executionProfile,
	)
	storedContext, contextErr := tradeprofile.DecodeDiscoveryContext(discoveryContext)
	storedProfile, profileErr := tradeprofile.DecodeExecutionProfile(executionProfile)
	if err != nil || storedUniverseID != universeID || regime != "pending" ||
		storedPromotionCap != 2 ||
		!storedUniverseAsOf.Equal(universeAsOf) ||
		!universeMember || string(sourceScores) != `{"mover": 0, "weekly_leader": 0}` ||
		potentIndustryEvidence != "Software broad_strong; member rank 1" ||
		contextErr != nil || storedContext == nil || storedContext.Thesis != "Leader at scan time" ||
		profileErr != nil || storedProfile == nil || storedProfile.RecommendedLens != "momentum" {
		t.Fatalf(
			"extended snapshot = universe=%q as_of=%s regime=%q scores=%s member=%v err=%v",
			storedUniverseID, storedUniverseAsOf, regime, sourceScores, universeMember, err,
		)
	}
	var storedPolitical tradeprofile.PoliticalDisclosureEvidence
	if err := json.Unmarshal(storedContext.EvidenceBySource["political_disclosure"], &storedPolitical); err != nil ||
		storedPolitical.DistinctFilerCount != 2 || storedPolitical.DistinctBuyerCount != 2 {
		t.Fatalf("snapshot political evidence = %+v, err=%v", storedPolitical, err)
	}
	evidence, err := store.ListScannerPromotionEvidence(
		ctx, startedAt.Add(-time.Minute), startedAt.Add(time.Minute),
	)
	if err != nil || len(evidence) != 1 {
		t.Fatalf("promotion evidence = %+v, err=%v", evidence, err)
	}
	var gotEvidence, wantEvidence map[string]any
	_ = json.Unmarshal(evidence[0].Evidence, &gotEvidence)
	_ = json.Unmarshal(json.RawMessage(`{"reason":"exposure_correlation_cap","conflict_key":"HELD","conflict_correlation":0.91}`), &wantEvidence)
	if evidence[0].Ticker != "SNAP_B" ||
		evidence[0].Stage != "llm_skipped" || !reflect.DeepEqual(gotEvidence, wantEvidence) {
		t.Fatalf("promotion evidence = %+v, err=%v", evidence, err)
	}

	if err := store.AddWatchlistItem(ctx, store.WatchlistItem{
		Ticker: "SNAP_A", Platform: "stock", Source: "scanner",
		Strategies: []string{"momentum", "pullback_strength"},
		Timeframes: []string{"1H", "4H"}, ScannerRunID: runID,
	}); err != nil {
		t.Fatalf("AddWatchlistItem: %v", err)
	}
	watch, err := store.GetWatchlistItem(ctx, "SNAP_A", "stock")
	if err != nil || watch == nil || watch.ScannerRunID != runID {
		t.Fatalf("watchlist lineage = %+v, err=%v", watch, err)
	}

	analysisID, err := store.InsertAnalysisWithScannerRun(
		ctx, "SNAP_A", "momentum",
		analysis.AnalysisResponse{Action: analysis.ActionHold, Confidence: .7},
		analysis.ModelCallMeta{
			Provider: "test", Model: "test", PromptHash: "scanner-lineage-test",
		},
		runID,
	)
	if err != nil || analysisID == "" {
		t.Fatalf("InsertAnalysisWithScannerRun: id=%q err=%v", analysisID, err)
	}
	if err := store.InsertScannerEngineOutcome(ctx, store.ScannerEngineOutcome{
		ScannerRunID: runID,
		Ticker:       "SNAP_B",
		Stage:        "model_unavailable",
	}); err != nil {
		t.Fatalf("InsertScannerEngineOutcome: %v", err)
	}

	signalID, err := store.InsertSignal(ctx, store.Signal{
		Ticker: "SNAP_A", Action: "BUY", Strategy: "momentum",
		SignalPrice: 100, Confidence: .8, SizePct: .1, Status: "executed",
		ExpiresAt: startedAt.Add(time.Hour), ScannerRunID: runID,
	})
	if err != nil {
		t.Fatalf("InsertSignal: %v", err)
	}
	signal, err := store.GetSignalByID(ctx, signalID)
	if err != nil || signal == nil || signal.ScannerRunID != runID {
		t.Fatalf("signal lineage = %+v, err=%v", signal, err)
	}

	tradeID, err := store.InsertTrade(ctx, store.Trade{
		SignalID: signalID, Ticker: "SNAP_A", Direction: "long",
		Strategy: "momentum", EntryPrice: 100, Quantity: 1,
	})
	if err != nil {
		t.Fatalf("InsertTrade: %v", err)
	}
	if _, err := store.GetDB().Exec(ctx, `
		UPDATE trades SET status='closed', pnl=12.5, closed_at=NOW() WHERE id=$1`,
		tradeID,
	); err != nil {
		t.Fatalf("close trade: %v", err)
	}

	funnel, err := store.GetScannerFunnel(
		ctx, startedAt.Add(-time.Minute), startedAt.Add(time.Minute),
	)
	if err != nil {
		t.Fatalf("GetScannerFunnel: %v", err)
	}
	if funnel.Runs != 1 || funnel.Sourced != 2 || funnel.QualificationKnown != 2 ||
		funnel.PullbackQualified != 1 || funnel.Scored != 2 ||
		funnel.Recommended != 1 || funnel.WatchlistAdded != 1 ||
		funnel.AnalyzedCandidates != 1 || funnel.Analyses != 1 ||
		funnel.HoldWaitCandidates != 1 ||
		funnel.EngineOutcomeCandidates != 1 || funnel.EngineOutcomes != 1 ||
		funnel.SignalCandidates != 1 || funnel.Signals != 1 ||
		funnel.ExecutedTradeCandidates != 1 || funnel.Trades != 1 ||
		funnel.ClosedTrades != 1 || funnel.Wins != 1 || funnel.RealizedPnL != 12.5 {
		t.Fatalf("unexpected funnel: %+v", funnel)
	}
	if len(funnel.QualificationStages) != 2 || len(funnel.FinalStages) != 2 ||
		len(funnel.EngineStages) != 1 {
		t.Fatalf("stage distributions missing: %+v", funnel)
	}
}

func TestScannerSnapshotDiscoverySourcesMustMatchLegacySources(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("DATABASE_URL not set; skipping store integration test")
	}
	ctx := context.Background()
	runID, err := store.BeginScannerRun(ctx, store.ScannerRun{StartedAt: time.Now().UTC()})
	if err != nil {
		t.Fatalf("BeginScannerRun: %v", err)
	}
	t.Cleanup(func() { _, _ = store.GetDB().Exec(ctx, "DELETE FROM scanner_runs WHERE id=$1", runID) })

	err = store.FinishScannerRun(ctx, store.ScannerRun{ID: runID}, []store.ScannerCandidateSnapshot{{
		ScannerRunID: runID, Ticker: "SNAP_SOURCE_MISMATCH", Sources: []string{"weekly_leader"},
		DiscoveryContext: &tradeprofile.DiscoveryContext{
			Version: tradeprofile.DiscoveryContextVersion, Sources: []string{"premarket_episodic"},
			Thesis: "A different source", EvidenceBySource: map[string]json.RawMessage{
				"premarket_episodic": json.RawMessage(`{"gap_pct":8.2}`),
			},
		},
		ExecutionProfile: &tradeprofile.ExecutionProfile{
			Version: tradeprofile.ExecutionProfileVersion, RecommendedLens: "news_sentiment",
			AdmissibleLenses: []string{"news_sentiment"}, PrimaryTimeframe: "1H",
			ScorerRationale: "Fresh event requires intraday confirmation.",
		},
	}})
	if err == nil || !strings.Contains(err.Error(), "legacy sources do not match discovery context sources") {
		t.Fatalf("FinishScannerRun error = %v, want source-set mismatch", err)
	}
}

func boolPtr(value bool) *bool { return &value }
