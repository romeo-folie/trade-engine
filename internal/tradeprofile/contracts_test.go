package tradeprofile

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestVersionedContractsRoundTrip(t *testing.T) {
	context := &DiscoveryContext{Version: DiscoveryContextVersion, Sources: []string{"weekly_leader"}, Thesis: "Relative leader", EvidenceBySource: map[string]json.RawMessage{"weekly_leader": json.RawMessage(`{"rank":1}`)}}
	profile := &ExecutionProfile{Version: ExecutionProfileVersion, RecommendedLens: "momentum", AdmissibleLenses: []string{"momentum", "trend_following"}, PrimaryTimeframe: "1D", EntryTimingTimeframe: "4H", ScorerRationale: "Leadership needs entry timing."}
	rawContext, err := MarshalDiscoveryContext(context)
	if err != nil {
		t.Fatalf("MarshalDiscoveryContext: %v", err)
	}
	gotContext, err := DecodeDiscoveryContext(rawContext)
	if err != nil || gotContext.Thesis != context.Thesis {
		t.Fatalf("context round trip = %#v, %v", gotContext, err)
	}
	rawProfile, err := MarshalExecutionProfile(profile)
	if err != nil {
		t.Fatalf("MarshalExecutionProfile: %v", err)
	}
	gotProfile, err := DecodeExecutionProfile(rawProfile)
	if err != nil || gotProfile.RecommendedLens != profile.RecommendedLens {
		t.Fatalf("profile round trip = %#v, %v", gotProfile, err)
	}
}

func TestExecutionProfileRejectsSourceLabels(t *testing.T) {
	err := (ExecutionProfile{Version: ExecutionProfileVersion, RecommendedLens: "weekly_leader", AdmissibleLenses: []string{"weekly_leader"}, PrimaryTimeframe: "1D", ScorerRationale: "invalid"}).Validate()
	if err == nil || !strings.Contains(err.Error(), "recognised") {
		t.Fatalf("Validate = %v, want source label rejection", err)
	}
}

func TestNullableDecodePreservesLegacyRows(t *testing.T) {
	context, err := DecodeDiscoveryContext(nil)
	if err != nil || context != nil {
		t.Fatalf("DecodeDiscoveryContext(nil) = %#v, %v", context, err)
	}
	profile, err := DecodeExecutionProfile([]byte("null"))
	if err != nil || profile != nil {
		t.Fatalf("DecodeExecutionProfile(null) = %#v, %v", profile, err)
	}
}

func TestContractsCanonicalizeStorageValues(t *testing.T) {
	context := &DiscoveryContext{
		Version: DiscoveryContextVersion,
		Sources: []string{" Weekly_Leader "},
		Thesis:  "  Relative leader  ",
		EvidenceBySource: map[string]json.RawMessage{
			" WEEKLY_LEADER ": json.RawMessage(`{"rank":1}`),
		},
	}
	profile := &ExecutionProfile{
		Version:              ExecutionProfileVersion,
		RecommendedLens:      " Momentum ",
		AdmissibleLenses:     []string{" Momentum ", " TREND_FOLLOWING "},
		PrimaryTimeframe:     " 1d ",
		EntryTimingTimeframe: " 4h ",
		ScorerRationale:      "  Leadership needs a timed entry.  ",
	}

	rawContext, err := MarshalDiscoveryContext(context)
	if err != nil {
		t.Fatalf("MarshalDiscoveryContext: %v", err)
	}
	rawProfile, err := MarshalExecutionProfile(profile)
	if err != nil {
		t.Fatalf("MarshalExecutionProfile: %v", err)
	}
	gotContext, err := DecodeDiscoveryContext(rawContext)
	if err != nil {
		t.Fatalf("DecodeDiscoveryContext: %v", err)
	}
	gotProfile, err := DecodeExecutionProfile(rawProfile)
	if err != nil {
		t.Fatalf("DecodeExecutionProfile: %v", err)
	}
	if gotContext.Sources[0] != "weekly_leader" || gotContext.Thesis != "Relative leader" ||
		string(gotContext.EvidenceBySource["weekly_leader"]) != `{"rank":1}` {
		t.Fatalf("canonical discovery context = %#v", gotContext)
	}
	if gotProfile.RecommendedLens != "momentum" ||
		strings.Join(gotProfile.AdmissibleLenses, ",") != "momentum,trend_following" ||
		gotProfile.PrimaryTimeframe != "1D" || gotProfile.EntryTimingTimeframe != "4H" ||
		gotProfile.ScorerRationale != "Leadership needs a timed entry." {
		t.Fatalf("canonical execution profile = %#v", gotProfile)
	}
}

func TestDiscoveryContextRejectsInvalidCanonicalSource(t *testing.T) {
	err := (DiscoveryContext{
		Version: DiscoveryContextVersion,
		Sources: []string{"weekly-leader"}, Thesis: "Relative leader",
		EvidenceBySource: map[string]json.RawMessage{
			"weekly-leader": json.RawMessage(`{"rank":1}`),
		},
	}).Validate()
	if err == nil || !strings.Contains(err.Error(), "snake_case") {
		t.Fatalf("Validate = %v, want invalid source rejection", err)
	}
}

func TestExecutionProfileRejectsDuplicateCanonicalLens(t *testing.T) {
	err := (ExecutionProfile{
		Version: ExecutionProfileVersion, RecommendedLens: "momentum",
		AdmissibleLenses: []string{"momentum", " MOMENTUM "},
		PrimaryTimeframe: "1D", ScorerRationale: "invalid duplicate",
	}).Validate()
	if err == nil || !strings.Contains(err.Error(), "duplicated") {
		t.Fatalf("Validate = %v, want duplicate lens rejection", err)
	}
}
