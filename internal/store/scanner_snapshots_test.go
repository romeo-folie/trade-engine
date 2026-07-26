package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/store"
)

func TestScannerSnapshotLineageAndFunnel(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("DATABASE_URL not set; skipping store integration test")
	}
	ctx := context.Background()
	startedAt := time.Now().UTC()
	runID, err := store.BeginScannerRun(ctx, store.ScannerRun{
		StartedAt:                    startedAt,
		PullbackUniverseEnabled:      true,
		PullbackMinMomentumReturnPct: 30,
		PullbackMinMarketCapMM:       100,
		PullbackMinDollarVolume:      1_000_000,
		CandidateCap:                 10,
		MinConviction:                .55,
		TTLDays:                      3,
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
			ScannerRunID:        runID,
			Ticker:              "SNAP_A",
			Sources:             []string{"mover"},
			QualificationKnown:  true,
			PullbackQualified:   true,
			QualificationStage:  "qualified",
			EligibleForScoring:  true,
			LLMAction:           "add",
			LLMConviction:       .8,
			PassedMinConviction: true,
			CapSelected:         true,
			WatchlistAdded:      true,
			Strategies:          []string{"momentum", "pullback_strength"},
			FinalStage:          "watchlist_added",
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
		},
	})
	if err != nil {
		t.Fatalf("FinishScannerRun: %v", err)
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
		funnel.SignalCandidates != 1 || funnel.Signals != 1 ||
		funnel.ExecutedTradeCandidates != 1 || funnel.Trades != 1 ||
		funnel.ClosedTrades != 1 || funnel.Wins != 1 || funnel.RealizedPnL != 12.5 {
		t.Fatalf("unexpected funnel: %+v", funnel)
	}
	if len(funnel.QualificationStages) != 2 || len(funnel.FinalStages) != 2 {
		t.Fatalf("stage distributions missing: %+v", funnel)
	}
}
