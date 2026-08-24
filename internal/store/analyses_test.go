package store

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestAnalysisAuditValuesLeavesUnsetTimingSnapshotAsSQLNull(t *testing.T) {
	_, _, _, _, _, timingSnapshot, _, err := analysisAuditValues(AnalysisAudit{})
	if err != nil {
		t.Fatalf("analysisAuditValues: %v", err)
	}
	if timingSnapshot != nil {
		t.Fatalf("unset timing snapshot = %#v, want SQL NULL", timingSnapshot)
	}
}

func TestEntryGateDecisionOmitsUnsetObservedAt(t *testing.T) {
	payload, err := json.Marshal(EntryGateDecision{Name: "trend_alignment", Status: "passed"})
	if err != nil {
		t.Fatalf("marshal gate decision: %v", err)
	}
	if strings.Contains(string(payload), "observed_at") {
		t.Fatalf("zero observation timestamp leaked into audit JSON: %s", payload)
	}
}
