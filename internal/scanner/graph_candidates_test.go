package scanner

import (
	"reflect"
	"strings"
	"testing"

	"github.com/romeo-folie/trade-engine/internal/store"
)

// graphLinkPhrase orients the explanation by edge direction relative to the
// watched seed ticker, and humanizes the relationship type.
func TestGraphLinkPhrase_Direction(t *testing.T) {
	outbound := graphLinkPhrase(store.NeighborCandidate{
		SeedTicker: "NVDA", RelationshipType: "supplies_to", Direction: "outbound",
	})
	if !strings.Contains(outbound, "NVDA (on watchlist) supplies to this company") {
		t.Errorf("outbound phrase = %q", outbound)
	}

	inbound := graphLinkPhrase(store.NeighborCandidate{
		SeedTicker: "NVDA", RelationshipType: "manufactures_for", Direction: "inbound",
	})
	if !strings.Contains(inbound, "this company manufactures for NVDA (on watchlist)") {
		t.Errorf("inbound phrase = %q", inbound)
	}
}

func TestPreferGraphNeighborCanonicalizesEqualStrength(t *testing.T) {
	// These model the qualifying LHX/LUNR graph edges that previously alternated
	// according to PostgreSQL's unspecified order for equal-strength rows.
	edges := []store.NeighborCandidate{
		{SeedTicker: "LHX", Ticker: "LUNR", RelationshipType: "supplies_to", Direction: "inbound", Strength: 0.90},
		{SeedTicker: "LHX", Ticker: "LUNR", RelationshipType: "partner_of", Direction: "outbound", Strength: 0.90},
		{SeedTicker: "LHX", Ticker: "LUNR", RelationshipType: "partner_of", Direction: "inbound", Strength: 0.90},
	}

	orders := [][]int{{0, 1, 2}, {0, 2, 1}, {1, 0, 2}, {1, 2, 0}, {2, 0, 1}, {2, 1, 0}}
	const want = "this company partner of LHX (on watchlist)"
	for _, order := range orders {
		winner := edges[order[0]]
		for _, index := range order[1:] {
			if preferGraphNeighbor(edges[index], winner) {
				winner = edges[index]
			}
		}
		if got := graphLinkPhrase(winner); got != want {
			t.Errorf("order %v selected %q, want %q", order, got, want)
		}
	}
}

func TestPreferGraphNeighborKeepsStrengthPriority(t *testing.T) {
	canonicalTieWinner := store.NeighborCandidate{
		SeedTicker: "LHX", Ticker: "LUNR", RelationshipType: "partner_of", Direction: "inbound", Strength: 0.90,
	}
	stronger := store.NeighborCandidate{
		SeedTicker: "LHX", Ticker: "LUNR", RelationshipType: "supplies_to", Direction: "inbound", Strength: 0.91,
	}
	if !preferGraphNeighbor(stronger, canonicalTieWinner) {
		t.Fatal("stronger graph edge lost to the equal-strength canonical tie-break")
	}
}

func TestRankGraphNeighborsPreservesTickerTieBreak(t *testing.T) {
	edges := []store.NeighborCandidate{
		{SeedTicker: "AAA", Ticker: "ZZZ", RelationshipType: "partner_of", Strength: 0.90},
		{SeedTicker: "ZZZ", Ticker: "AAA", RelationshipType: "supplies_to", Strength: 0.90},
		{SeedTicker: "AAA", Ticker: "MMM", RelationshipType: "partner_of", Strength: 0.91},
	}

	rankGraphNeighbors(edges)
	got := []string{edges[0].Ticker, edges[1].Ticker, edges[2].Ticker}
	want := []string{"MMM", "AAA", "ZZZ"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ranked tickers = %v, want %v", got, want)
	}
}

// BuildSwingPrompt surfaces the graph link so the LLM can weigh the relational
// thesis; screener candidates omit the line.
func TestBuildSwingPrompt_RendersGraphContext(t *testing.T) {
	withCtx := SwingCandidate{
		Ticker: "ASML", Price: 900, Sources: []string{"graph_neighbor"},
		GraphContext: "TSMC (on watchlist) supplies to this company",
	}
	_, user := BuildSwingPrompt(withCtx, 0.65)
	if !strings.Contains(user, "Graph link:") || !strings.Contains(user, "TSMC (on watchlist)") {
		t.Errorf("graph context not rendered in prompt:\n%s", user)
	}

	plain := SwingCandidate{Ticker: "AAPL", Price: 200, Sources: []string{"mover"}}
	_, user2 := BuildSwingPrompt(plain, 0.65)
	if strings.Contains(user2, "Graph link:") {
		t.Errorf("screener candidate should not render a Graph link line:\n%s", user2)
	}
}
