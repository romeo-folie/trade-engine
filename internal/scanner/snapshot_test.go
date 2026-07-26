package scanner

import (
	"errors"
	"reflect"
	"testing"
	"time"

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

func TestScanRecorderRegisterPreservesMultiSourceLineage(t *testing.T) {
	recorder := &scanRecorder{
		candidates: make(map[string]*store.ScannerCandidateSnapshot),
	}
	member := true
	asOf := time.Date(2026, 7, 24, 20, 0, 0, 0, time.UTC)

	recorder.register([]SwingCandidate{{
		Ticker:         "NVDA",
		Sources:        []string{"mover"},
		SourceScores:   map[string]float64{"mover": 0},
		UniverseMember: &member,
		Sector:         "Technology",
		Industry:       "Semiconductors",
		TaxonomyStatus: "known",
		TaxonomyAsOf:   &asOf,
	}})
	recorder.register([]SwingCandidate{{
		Ticker:       "NVDA",
		Sources:      []string{"active", "mover"},
		SourceScores: map[string]float64{"active": 0.73},
	}})

	got := recorder.candidates["NVDA"]
	if want := []string{"mover", "active"}; !reflect.DeepEqual(got.Sources, want) {
		t.Fatalf("sources = %v, want %v", got.Sources, want)
	}
	if want := map[string]float64{"mover": 0, "active": 0.73}; !reflect.DeepEqual(got.SourceScores, want) {
		t.Fatalf("source scores = %v, want %v", got.SourceScores, want)
	}
	if got.UniverseMember == nil || !*got.UniverseMember {
		t.Fatal("universe membership lineage was not preserved")
	}
	if got.Industry != "Semiconductors" || got.TaxonomyStatus != "known" {
		t.Fatalf("taxonomy lineage was not preserved: %+v", got)
	}
}
