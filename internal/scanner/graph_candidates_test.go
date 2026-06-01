package scanner

import (
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
