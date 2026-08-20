package tradeprofile

import (
	"encoding/json"
	"testing"
)

func TestMergeDiscoveryContextsRetainsRepeatedSourceInstances(t *testing.T) {
	makeContext := func(date string) *DiscoveryContext {
		return &DiscoveryContext{Version: DiscoveryContextVersion, Sources: []string{"earnings_scheduled"}, Thesis: date,
			EvidenceBySource: map[string]json.RawMessage{"earnings_scheduled": json.RawMessage(`{"date":"` + date + `"}`)}}
	}
	merged := MergeDiscoveryContexts(makeContext("2026-08-24"), makeContext("2026-08-25"))
	if merged == nil || len(merged.Sources) != 2 || merged.Sources[0] != "earnings_scheduled" || merged.Sources[1] != "earnings_scheduled_2" {
		t.Fatalf("merged=%+v", merged)
	}
	if string(merged.EvidenceBySource["earnings_scheduled_2"]) != `{"date":"2026-08-25"}` {
		t.Fatalf("second evidence=%s", merged.EvidenceBySource["earnings_scheduled_2"])
	}
}
