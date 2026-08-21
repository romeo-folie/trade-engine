package store

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/tradeprofile"
)

func TestMergedDiscoveryThesisPreservesUnrelatedLiveSource(t *testing.T) {
	evidence := map[string]json.RawMessage{
		"political_disclosure": json.RawMessage(`{"source_thesis":"political_disclosure: filing aggregate"}`),
		"mover":                json.RawMessage(`{"source_thesis":"mover: recent news catalyst"}`),
	}
	if got := mergedDiscoveryThesis("old row thesis", "mover: recent news catalyst", []string{"political_disclosure", "mover"}, evidence); got != "political_disclosure: filing aggregate | mover: recent news catalyst" {
		t.Fatalf("source-attributed thesis = %q", got)
	}

	// Legacy rows lacked source_thesis. A mover refresh must still retain the
	// unrelated political thesis rather than replacing it wholesale.
	evidence["political_disclosure"] = json.RawMessage(`{"report_count":2}`)
	if got := mergedDiscoveryThesis("political_disclosure: filing aggregate", "mover: recent news catalyst", []string{"political_disclosure", "mover"}, evidence); got != "political_disclosure: filing aggregate | mover: recent news catalyst" {
		t.Fatalf("legacy thesis preservation = %q", got)
	}
	if got := mergedDiscoveryThesis("political_disclosure: filing aggregate | mover: recent news catalyst", "mover: recent news catalyst", []string{"political_disclosure", "mover"}, evidence); got != "political_disclosure: filing aggregate | mover: recent news catalyst" {
		t.Fatalf("legacy thesis duplicated a refreshed source = %q", got)
	}
}

func TestFilterDiscoveryContextSourcesPreservesLiveSourceThesis(t *testing.T) {
	context := &tradeprofile.DiscoveryContext{
		Version: tradeprofile.DiscoveryContextVersion,
		Sources: []string{"mover", "weekly_leader"}, Thesis: "mover: short-lived | weekly_leader: durable leadership",
		EvidenceBySource: map[string]json.RawMessage{
			"mover":         json.RawMessage(`{"source_thesis":"mover: short-lived"}`),
			"weekly_leader": json.RawMessage(`{"source_thesis":"weekly_leader: durable leadership"}`),
		},
	}
	filtered, pruned := filterDiscoveryContextSources(context, map[string]bool{"weekly_leader": true})
	if !pruned || filtered == nil || filtered.Thesis != "weekly_leader: durable leadership" {
		t.Fatalf("filtered context = %+v, pruned=%v", filtered, pruned)
	}
}

func TestRefreshActivityResultPreservesDatabaseErrors(t *testing.T) {
	queryErr := errors.New("database connection dropped")
	if got := refreshActivityResult(false, queryErr); !errors.Is(got, queryErr) {
		t.Fatalf("query error = %v, want original error", got)
	}
	if got := refreshActivityResult(false, nil); !errors.Is(got, ErrWatchlistRefreshNotActive) {
		t.Fatalf("inactive result = %v, want refresh sentinel", got)
	}
}

func TestClearExpiredWatchlistContractKeepsOnlyIndependentContextRouting(t *testing.T) {
	expired := time.Now().Add(-time.Second)
	item := WatchlistItem{
		ExpiresAt:         &expired,
		Source:            "scanner",
		Strategies:        []string{"scanner_only"},
		Timeframes:        []string{"5m"},
		MinConfidence:     0.9,
		ScannerRunID:      "scanner-run",
		DiscoveryContext:  &tradeprofile.DiscoveryContext{Version: tradeprofile.DiscoveryContextVersion},
		ExecutionProfile:  &tradeprofile.ExecutionProfile{Version: tradeprofile.ExecutionProfileVersion},
		DiscoveryContexts: []WatchlistDiscoveryContext{{Key: "earnings_scheduled:2026-08-24"}},
	}
	clearExpiredWatchlistContract(&item)
	if item.DiscoveryContext != nil || item.ExecutionProfile != nil {
		t.Fatalf("expired legacy contract remained: %+v", item)
	}
	if len(item.DiscoveryContexts) != 1 {
		t.Fatalf("independent contexts were changed: %+v", item.DiscoveryContexts)
	}
	if item.Source != "earnings_watchlist" || item.ScannerRunID != "" || item.MinConfidence != 0 ||
		len(item.Strategies) != 3 || item.Strategies[0] != "trend_following" ||
		len(item.Timeframes) != 3 || item.Timeframes[0] != "1D" {
		t.Fatalf("expired scanner routing remained: %+v", item)
	}
}
