package events

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestPortfolioSnapshotPayload_DistinguishesRefreshOnlyEvent(t *testing.T) {
	refreshOnly, err := json.Marshal(PortfolioSnapshotPayload{})
	if err != nil {
		t.Fatalf("marshal refresh-only payload: %v", err)
	}
	if !strings.Contains(string(refreshOnly), `"available":false`) {
		t.Fatalf("refresh-only payload does not identify itself as unavailable: %s", refreshOnly)
	}

	snapshot, err := json.Marshal(PortfolioSnapshotPayload{
		Available:              true,
		PositionStatsAvailable: true,
	})
	if err != nil {
		t.Fatalf("marshal available payload: %v", err)
	}
	for _, want := range []string{`"available":true`, `"position_stats_available":true`} {
		if !strings.Contains(string(snapshot), want) {
			t.Fatalf("available payload %s missing %s", snapshot, want)
		}
	}
}
