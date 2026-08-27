package scanner

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
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
	recorder.watchlistResult(remaining[1], []string{"momentum", "pullback_strength"}, nil)

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
	if got := recorder.candidates["ADD"]; got.FinalStage != "research_queue_admitted" ||
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

func TestDeterministicSnapshotMetadataRefreshesAfterFallback(t *testing.T) {
	recorder := &scanRecorder{candidates: make(map[string]*store.ScannerCandidateSnapshot)}
	candidate := SwingCandidate{Ticker: "TEST", Sources: []string{"political_disclosure"}}
	recorder.register([]SwingCandidate{candidate})
	recorder.deterministicQualification(candidate, &store.ResearchQueueAdmission{
		Bucket: "political_disclosure", Priority: json.RawMessage(`{"ticker":"TEST","score":2}`),
	})
	fallback := candidate
	fallback.Sources = []string{"weekly_leader"}
	recorder.deterministicQualification(fallback, &store.ResearchQueueAdmission{
		Bucket: "weekly_leader", Priority: json.RawMessage(`{"ticker":"TEST","score":1}`),
	})
	if !strings.Contains(string(recorder.candidates["TEST"].PromotionEvidence), `"bucket":"weekly_leader"`) ||
		strings.Contains(string(recorder.candidates["TEST"].PromotionEvidence), `"bucket":"political_disclosure"`) {
		t.Fatalf("fallback metadata was not refreshed: %s", recorder.candidates["TEST"].PromotionEvidence)
	}
}

func TestSourceTopFiveExclusionUsesSelectedSnapshotLineage(t *testing.T) {
	recorder := &scanRecorder{candidates: make(map[string]*store.ScannerCandidateSnapshot)}
	candidate := SwingCandidate{
		Ticker: "TEST", Sources: []string{"mover", "graph_neighbor"},
		SourceScores: map[string]float64{"mover": .9, "graph_neighbor": .8},
		GraphContext: "graph-only context",
	}
	recorder.register([]SwingCandidate{candidate})
	selected := retainPromotionSources(candidate, map[string]int{"mover": 1})
	recorder.sourceTopFiveExcluded([]SwingCandidate{selected}, []sourcePromotionExclusion{{
		Ticker: "TEST", Source: "graph_neighbor", Rank: 6,
	}})

	snapshot := recorder.candidates["TEST"]
	if !reflect.DeepEqual(snapshot.Sources, []string{"mover"}) ||
		!reflect.DeepEqual(snapshot.SourceScores, map[string]float64{"mover": .9}) ||
		snapshot.GraphContext != "" || snapshot.FinalStage != "sourced" {
		t.Fatalf("selected snapshot lineage=%+v", snapshot)
	}
	if !strings.Contains(string(snapshot.PromotionEvidence), `"graph_neighbor"`) {
		t.Fatalf("missing excluded-facet evidence: %s", snapshot.PromotionEvidence)
	}
}

func TestPoliticalRejectionDoesNotPoisonIndependentSourceSnapshot(t *testing.T) {
	recorder := &scanRecorder{candidates: make(map[string]*store.ScannerCandidateSnapshot)}
	political := SwingCandidate{Ticker: "TEST", Sources: []string{"political_disclosure"}, PoliticalDisclosureEvidence: "rejected filing"}
	recorder.register([]SwingCandidate{political})
	recorder.deterministicRejected(political, "same-filer sale")
	recorder.register([]SwingCandidate{{Ticker: "TEST", Sources: []string{"mover"}, SourceScores: map[string]float64{"mover": .8}}})
	snapshot := recorder.candidates["TEST"]
	if strings.Join(snapshot.Sources, ",") != "mover" || snapshot.FailureStage != "" ||
		!strings.Contains(string(snapshot.PromotionEvidence), `"political_disclosure"`) {
		t.Fatalf("independent source did not replace rejected political lineage: %+v", snapshot)
	}
}

func TestScanRecorderPreservesPromotionSuppressionReason(t *testing.T) {
	recorder := &scanRecorder{candidates: make(map[string]*store.ScannerCandidateSnapshot)}
	candidate := SwingCandidate{Ticker: "  SMALL  "}
	recorder.register([]SwingCandidate{candidate})
	recorder.scored(candidate, SwingScoreResult{Action: "add", Conviction: .9}, nil, .55)
	recorder.promotionRejected(candidate.Ticker, "industry_cap")
	recorder.selected(nil)

	got := recorder.candidates[candidate.Ticker]
	if got.FinalStage != "promotion_suppressed" || got.FailureStage != "promotion" ||
		got.FailureReason != "industry_cap" {
		t.Fatalf("promotion suppression was not preserved: %+v", got)
	}
}

func TestScanRecorderMergesCorrelationAvailabilityEvidence(t *testing.T) {
	recorder := &scanRecorder{candidates: make(map[string]*store.ScannerCandidateSnapshot)}
	candidate := SwingCandidate{Ticker: "MERGE"}
	recorder.register([]SwingCandidate{candidate})
	recorder.promotionCorrelationUnavailable(candidate.Ticker, map[string]string{
		"MISSING": "provider timeout",
	})
	recorder.promotionRejectedWithEvidence(candidate.Ticker, PromotionRejection{
		Reason: "correlation_cap", ConflictKey: "HELD", ConflictCorrelation: .91,
	})

	var got map[string]any
	if err := json.Unmarshal(recorder.candidates[candidate.Ticker].PromotionEvidence, &got); err != nil {
		t.Fatalf("promotion evidence: %v", err)
	}
	if got["reason"] != "correlation_cap" || got["conflict_key"] != "HELD" {
		t.Fatalf("rejection evidence = %+v", got)
	}
	missing, ok := got["correlation_unavailable"].(map[string]any)
	if !ok || missing["MISSING"] != "provider timeout" {
		t.Fatalf("availability evidence = %+v", got["correlation_unavailable"])
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
		Ticker:  "NVDA",
		Sources: []string{"active", "mover", "weekly_leader", "potent_industry"},
		SourceScores: map[string]float64{
			"active": 0.73, "weekly_leader": 0.91, "potent_industry": 0.82,
		},
		WeeklyLeaderEvidence:   "rank 4, composite score 0.910 across the full eligible universe",
		PotentIndustryEvidence: "Semiconductors broad_strong; member rank 1",
	}})

	got := recorder.candidates["NVDA"]
	if want := []string{
		"mover", "active", "weekly_leader", "potent_industry",
	}; !reflect.DeepEqual(got.Sources, want) {
		t.Fatalf("sources = %v, want %v", got.Sources, want)
	}
	if want := map[string]float64{
		"mover": 0, "active": 0.73, "weekly_leader": 0.91,
		"potent_industry": 0.82,
	}; !reflect.DeepEqual(got.SourceScores, want) {
		t.Fatalf("source scores = %v, want %v", got.SourceScores, want)
	}
	if got.WeeklyLeaderEvidence == "" || got.PotentIndustryEvidence == "" ||
		got.GraphContext != "" {
		t.Fatalf("discovery evidence was not stored independently: %+v", got)
	}
	if got.UniverseMember == nil || !*got.UniverseMember {
		t.Fatal("universe membership lineage was not preserved")
	}
	if got.Industry != "Semiconductors" || got.TaxonomyStatus != "known" {
		t.Fatalf("taxonomy lineage was not preserved: %+v", got)
	}
}

func TestScanRecorderValidatesExplicitUniverseLineage(t *testing.T) {
	startedAt := time.Date(2026, 7, 26, 14, 0, 0, 0, time.UTC)
	storedAsOf := startedAt.Add(-time.Hour)
	callerAsOf := startedAt.Add(-24 * time.Hour)
	recorder := &scanRecorder{
		run: store.ScannerRun{
			StartedAt:         startedAt,
			UniverseVersionID: "completed-version",
			UniverseAsOf:      &callerAsOf,
		},
		resolveUniverseByID: func(
			_ context.Context, id string, asOf time.Time,
		) (*store.SwingUniverseVersion, error) {
			if id != "completed-version" || !asOf.Equal(startedAt) {
				t.Fatalf("resolver called with id=%q asOf=%s", id, asOf)
			}
			return &store.SwingUniverseVersion{
				ID: "completed-version", AsOf: storedAsOf, Status: "completed",
			}, nil
		},
	}

	if err := recorder.resolveUniverseLineage(context.Background()); err != nil {
		t.Fatalf("resolveUniverseLineage: %v", err)
	}
	if recorder.run.UniverseVersionID != "completed-version" ||
		recorder.run.UniverseAsOf == nil || !recorder.run.UniverseAsOf.Equal(storedAsOf) {
		t.Fatalf("validated lineage = %+v", recorder.run)
	}
}

func TestScanRecorderClearsInvalidExplicitUniverseLineage(t *testing.T) {
	startedAt := time.Date(2026, 7, 26, 14, 0, 0, 0, time.UTC)
	untrustedAsOf := startedAt.Add(time.Hour)
	tests := []struct {
		name    string
		resolve func(context.Context, string, time.Time) (*store.SwingUniverseVersion, error)
		wantErr bool
	}{
		{
			name: "unfinished failed or future version",
			resolve: func(context.Context, string, time.Time) (*store.SwingUniverseVersion, error) {
				return nil, nil
			},
		},
		{
			name: "resolver returns mismatched version",
			resolve: func(context.Context, string, time.Time) (*store.SwingUniverseVersion, error) {
				return &store.SwingUniverseVersion{
					ID: "different-version", AsOf: startedAt.Add(-time.Hour),
				}, nil
			},
		},
		{
			name: "lookup failure",
			resolve: func(context.Context, string, time.Time) (*store.SwingUniverseVersion, error) {
				return nil, errors.New("database unavailable")
			},
			wantErr: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			recorder := &scanRecorder{
				run: store.ScannerRun{
					StartedAt:         startedAt,
					UniverseVersionID: "untrusted-version",
					UniverseAsOf:      &untrustedAsOf,
				},
				universeEnabled:     true,
				resolveUniverseByID: tc.resolve,
				resolveUsableUniverse: func(
					context.Context, time.Time, time.Duration,
				) (*store.SwingUniverseVersion, error) {
					t.Fatal("invalid explicit lineage must not silently fall back")
					return nil, nil
				},
			}

			err := recorder.resolveUniverseLineage(context.Background())
			if (err != nil) != tc.wantErr {
				t.Fatalf("error = %v, wantErr=%v", err, tc.wantErr)
			}
			if recorder.run.UniverseVersionID != "" || recorder.run.UniverseAsOf != nil {
				t.Fatalf("invalid lineage retained: %+v", recorder.run)
			}
		})
	}
}
