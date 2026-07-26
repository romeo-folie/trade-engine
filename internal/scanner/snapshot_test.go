package scanner

import (
	"errors"
	"testing"

	"github.com/romeo-folie/trade-engine/internal/store"
)

func TestScanRecorderRetainsEveryTerminalStage(t *testing.T) {
	recorder := &scanRecorder{
		run:        store.ScannerRun{ID: "run-id"},
		candidates: make(map[string]*store.ScannerCandidateSnapshot),
	}
	all := []SwingCandidate{
		{Ticker: "ACTIVE", Sources: []string{"mover"}},
		{Ticker: "SKIP", Sources: []string{"active"}},
		{Ticker: "ADD", Sources: []string{"mover", "active"}},
		{Ticker: "ERROR", Sources: []string{"graph_neighbor"}},
	}
	recorder.register(all)
	remaining := all[1:]
	recorder.markRemoved(all, remaining, "active_watchlist")
	recorder.setEligible(remaining)
	recorder.scored(remaining[0], SwingScoreResult{Action: "skip", Conviction: .2}, nil, .55)
	recorder.scored(remaining[1], SwingScoreResult{
		Action: "add", Conviction: .8, Strategy: "momentum",
	}, nil, .55)
	recorder.scored(remaining[2], SwingScoreResult{}, errors.New("model unavailable"), .55)
	selected := []scoredCandidate{{candidate: remaining[1], score: SwingScoreResult{
		Action: "add", Conviction: .8, Strategy: "momentum",
	}}}
	recorder.selected(selected)
	recorder.watchlistResult(
		remaining[1], selected[0].score, []string{"momentum", "pullback_strength"}, nil,
	)

	if got := recorder.candidates["ACTIVE"]; got.FinalStage != "active_watchlist" ||
		!got.ActiveWatchlistDuplicate {
		t.Fatalf("active snapshot = %+v", got)
	}
	if got := recorder.candidates["SKIP"]; got.FinalStage != "llm_skipped" {
		t.Fatalf("skip snapshot = %+v", got)
	}
	if got := recorder.candidates["ERROR"]; got.FinalStage != "scoring_error" ||
		got.LLMError == "" {
		t.Fatalf("error snapshot = %+v", got)
	}
	if got := recorder.candidates["ADD"]; got.FinalStage != "watchlist_added" ||
		!got.WatchlistAdded || !got.CapSelected ||
		len(got.Strategies) != 2 {
		t.Fatalf("added snapshot = %+v", got)
	}
	if recorder.run.RawCandidateCount != 4 ||
		recorder.run.EvaluatedCandidateCount != 3 ||
		recorder.run.ScoredCandidateCount != 2 ||
		recorder.run.AddedCandidateCount != 1 {
		t.Fatalf("run counts = %+v", recorder.run)
	}
}
