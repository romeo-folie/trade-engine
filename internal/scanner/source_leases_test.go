package scanner

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/tradeprofile"
)

func TestScannerSourceLeasesAreSourceSpecificAndRetryStable(t *testing.T) {
	now := time.Date(2026, time.August, 21, 14, 0, 0, 0, time.UTC)
	scanner := &MarketScanner{Cfg: config.ScannerConfig{
		TTLDays: 3, WeeklyLeader: config.WeeklyLeaderConfig{TTLDays: 10},
	}}
	candidate := SwingCandidate{
		Ticker: "lease", Sources: []string{"mover", "weekly_leader"}, Price: 12.5, Volume: 100,
		WeeklyLeaderRunID: "weekly-run-1", WeeklyLeaderEvidence: "ranked first",
	}
	discovery, _, err := buildScannerTradeProfile(candidate, SwingScoreResult{})
	if err != nil {
		t.Fatalf("buildScannerTradeProfile: %v", err)
	}
	first := scanner.scannerSourceLeases(candidate, discovery, now)
	second := scanner.scannerSourceLeases(candidate, discovery, now.Add(time.Hour))
	if len(first) != 2 || len(second) != 2 {
		t.Fatalf("leases = %d/%d, want two", len(first), len(second))
	}
	if first[0].EvidenceFingerprint != second[0].EvidenceFingerprint ||
		first[1].EvidenceFingerprint != second[1].EvidenceFingerprint {
		t.Fatalf("identical observation changed fingerprint: %#v vs %#v", first, second)
	}
	if got := first[0].ExpiresAt.Sub(now); got != 3*24*time.Hour {
		t.Fatalf("mover lease = %s, want 3d", got)
	}
	if got := first[1].ExpiresAt.Sub(now); got != 10*24*time.Hour {
		t.Fatalf("weekly lease = %s, want 10d", got)
	}

	candidate.Volume++
	changedDiscovery, _, err := buildScannerTradeProfile(candidate, SwingScoreResult{})
	if err != nil {
		t.Fatalf("build changed profile: %v", err)
	}
	changed := scanner.scannerSourceLeases(candidate, changedDiscovery, now.Add(time.Hour))
	if first[0].EvidenceFingerprint == changed[0].EvidenceFingerprint {
		t.Fatal("material mover observation did not change its fingerprint")
	}
	if first[1].EvidenceFingerprint != changed[1].EvidenceFingerprint {
		t.Fatal("unrelated mover observation changed weekly-leader identity")
	}
}

func TestPoliticalDisclosureLeaseFingerprintUsesCanonicalReportIdentity(t *testing.T) {
	now := time.Date(2026, time.August, 21, 14, 0, 0, 0, time.UTC)
	scanner := &MarketScanner{Cfg: config.ScannerConfig{TTLDays: 3}}
	facts := json.RawMessage(`{"version":1,"ticker":"DISC","report_count":2}`)
	candidate := SwingCandidate{
		Ticker: "DISC", Sources: []string{"political_disclosure"},
		PoliticalDisclosureClaimReportIDs: []string{"b-report", "a-report"},
		PoliticalDisclosureFacts:          facts,
	}
	discovery := &tradeprofile.DiscoveryContext{
		Version: 1, Sources: []string{"political_disclosure"}, Thesis: "filing evidence",
		EvidenceBySource: map[string]json.RawMessage{"political_disclosure": facts},
	}
	first := scanner.scannerSourceLeases(candidate, discovery, now)
	candidate.PoliticalDisclosureClaimReportIDs = []string{"a-report", "b-report"}
	second := scanner.scannerSourceLeases(candidate, discovery, now.Add(time.Hour))
	if first[0].EvidenceFingerprint != second[0].EvidenceFingerprint {
		t.Fatal("report ordering changed political-disclosure fingerprint")
	}
	candidate.PoliticalDisclosureClaimReportIDs = append(candidate.PoliticalDisclosureClaimReportIDs, "c-report")
	third := scanner.scannerSourceLeases(candidate, discovery, now.Add(2*time.Hour))
	if first[0].EvidenceFingerprint == third[0].EvidenceFingerprint {
		t.Fatal("new filing did not change political-disclosure fingerprint")
	}
}
