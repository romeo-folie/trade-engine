package orb

import (
	"reflect"
	"testing"
)

func TestFindOrphanOptionPositions(t *testing.T) {
	managed := map[string]float64{
		"SPY260717C00551000": 2, // broker holds exactly 2 → fully explained
		"QQQ260717P00500000": 1, // broker holds 3 → 2-contract surplus
	}
	broker := map[string]float64{
		"SPY260717C00551000":  2, // matches managed → not an orphan
		"QQQ260717P00500000":  3, // 3 held vs 1 managed → surplus
		"NVDA260717C00120000": 4, // no managed row → fully unexplained
	}
	got := findOrphanOptionPositions(broker, managed)
	want := []optionSurplus{
		{OCC: "NVDA260717C00120000", BrokerQty: 4, ManagedQty: 0},
		{OCC: "QQQ260717P00500000", BrokerQty: 3, ManagedQty: 1},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("orphans = %+v; want %+v", got, want)
	}

	// Broker qty equal to managed → not an orphan (the qty-mismatch guard).
	if got := findOrphanOptionPositions(map[string]float64{"SPY260717C00551000": 2}, managed); len(got) != 0 {
		t.Errorf("expected no orphan when broker qty == managed, got %+v", got)
	}
	// Empty broker → no orphans.
	if got := findOrphanOptionPositions(nil, managed); len(got) != 0 {
		t.Errorf("expected no orphans for empty broker, got %+v", got)
	}
}
