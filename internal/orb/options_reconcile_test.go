package orb

import (
	"reflect"
	"testing"
)

func TestFindOrphanOptionPositions(t *testing.T) {
	managed := map[string]bool{
		"SPY260717C00551000": true,
		"QQQ260717P00500000": true,
	}
	broker := []string{
		"SPY260717C00551000",  // managed → not an orphan
		"NVDA260717C00120000", // NOT managed → orphan
		"NVDA260717C00120000", // duplicate → collapsed
		"",                    // ignored
		"AAPL260717P00200000", // NOT managed → orphan
	}
	got := findOrphanOptionPositions(broker, managed)
	want := []string{"AAPL260717P00200000", "NVDA260717C00120000"} // sorted, de-duped
	if !reflect.DeepEqual(got, want) {
		t.Errorf("orphans = %v; want %v", got, want)
	}

	// All managed → no orphans.
	if got := findOrphanOptionPositions([]string{"SPY260717C00551000"}, managed); len(got) != 0 {
		t.Errorf("expected no orphans, got %v", got)
	}
	// Empty broker → no orphans.
	if got := findOrphanOptionPositions(nil, managed); len(got) != 0 {
		t.Errorf("expected no orphans for empty broker, got %v", got)
	}
}
