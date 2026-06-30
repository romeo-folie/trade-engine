package orb

import (
	"slices"
	"testing"

	"github.com/romeo-folie/trade-engine/internal/store"
)

// orphanedSplitEntries flags a still-pending split leg for cancellation once a sibling in
// its logical group has already exited (status='closed') — but never flags a leg whose group
// is still live, nor an ordinary (non-split) pending entry.
func TestOrphanedSplitEntries(t *testing.T) {
	parent := "runner"
	trades := []store.ORBTrade{
		// Group A: runner filled then exited (closed); its partial entry never filled.
		{ID: "runner", OrderID: "runner-ord", Status: "closed", ScaleOutRole: "runner"},
		{ID: "partA", OrderID: "partA-ord", Status: "pending", ScaleOutRole: "partial", ScaleOutParentID: &parent},
		// Group B: both legs still pending (no exit yet) — must NOT be flagged.
		{ID: "runnerB", OrderID: "runnerB-ord", Status: "pending", ScaleOutRole: "runner"},
		// Ordinary pending trade — not a split leg, must NOT be flagged.
		{ID: "plain", OrderID: "plain-ord", Status: "pending"},
	}
	pending := map[string]string{
		"partA-ord":   "EUR/USD", // orphan: sibling runner exited
		"runnerB-ord": "GBP/USD", // group still open
		"plain-ord":   "USD/JPY", // ordinary
	}

	got := orphanedSplitEntries(pending, trades)
	if len(got) != 1 || got[0] != "partA-ord" {
		t.Errorf("orphanedSplitEntries = %v; want [partA-ord] only (sibling already exited)", got)
	}

	// Symmetric: a runner entry left pending after its PARTIAL leg filled and exited.
	partParent := "runner2"
	trades2 := []store.ORBTrade{
		{ID: "runner2", OrderID: "runner2-ord", Status: "pending", ScaleOutRole: "runner"},
		{ID: "part2", OrderID: "part2-ord", Status: "closed", ScaleOutRole: "partial", ScaleOutParentID: &partParent},
	}
	got2 := orphanedSplitEntries(map[string]string{"runner2-ord": "EUR/USD"}, trades2)
	if !slices.Contains(got2, "runner2-ord") {
		t.Errorf("runner entry not flagged after its partial exited; got %v", got2)
	}
}
