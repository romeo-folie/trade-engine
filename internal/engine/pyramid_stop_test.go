package engine

import "testing"

// pyramidTightenedStop is invoked by generateSignal AFTER the risk gate
// confirms the pyramid is eligible (#324). These tests pin its behaviour
// because the engine's previous in-line implementation copied the existing
// canonical stop onto the new lot unconditionally, which could produce a
// long stop above the current signal price (e.g. after a gap down before
// monitorPositions runs) — closing the new trade immediately on the next
// monitor pass.

func TestPyramidTightenedStop_PicksAvgEntryWhenItDominates(t *testing.T) {
	// Existing stop is below the weighted avg entry; tighten to avg entry
	// so the combined cost basis is at worst breakeven on the original lot.
	got := pyramidTightenedStop(
		90,  // analyst stop
		95,  // existing max stop
		100, // avg entry
		110, // signal price
	)
	if got != 100 {
		t.Fatalf("expected 100 (avg entry), got %v", got)
	}
}

func TestPyramidTightenedStop_PicksExistingStopWhenItDominates(t *testing.T) {
	// Existing protective stop has already been ratcheted up beyond the
	// avg entry — keep that protection on the new lot too.
	got := pyramidTightenedStop(
		95,  // analyst stop
		103, // existing max stop (well above avg entry)
		100, // avg entry
		110, // signal price
	)
	if got != 103 {
		t.Fatalf("expected 103 (existing stop), got %v", got)
	}
}

func TestPyramidTightenedStop_KeepsAnalystStopWhenItIsTighter(t *testing.T) {
	// Analyst stop is the most aggressive of the three — leave it alone
	// rather than loosening it.
	got := pyramidTightenedStop(
		108, // analyst stop (very tight)
		95,  // existing max stop
		100, // avg entry
		110, // signal price
	)
	if got != 108 {
		t.Fatalf("expected 108 (analyst stop), got %v", got)
	}
}

func TestPyramidTightenedStop_DefenceInDepth_StopAtOrAbovePrice(t *testing.T) {
	// Pathological input: existing stop has drifted to or above the
	// current signal price. checkPyramidPolicy should already have rejected
	// this signal, but if anything ever lets it through, fall back to the
	// analyst stop rather than producing an immediately-out-of-range long
	// stop.
	got := pyramidTightenedStop(
		100, // analyst stop
		115, // existing max stop above signal price
		100, // avg entry
		110, // signal price
	)
	if got != 100 {
		t.Fatalf("expected fallback to analyst stop (100), got %v", got)
	}

	// Equality boundary should also fall back.
	got = pyramidTightenedStop(99, 110, 100, 110)
	if got != 99 {
		t.Fatalf("expected fallback to analyst stop (99) at equality, got %v", got)
	}
}

func TestPyramidTightenedStop_ZeroSignalPriceLeavesGuardOff(t *testing.T) {
	// signalPrice == 0 means "unknown" — skip the defence-in-depth cap so
	// callers without price data still get the tightened value. (In the
	// engine, missing signalPrice would have already failed earlier checks.)
	got := pyramidTightenedStop(95, 105, 100, 0)
	if got != 105 {
		t.Fatalf("expected 105 when signalPrice=0, got %v", got)
	}
}
