package scanner

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/store"
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

func TestOrdinaryLeaseFingerprintsIgnoreMergedSourceFields(t *testing.T) {
	now := time.Date(2026, time.August, 21, 14, 0, 0, 0, time.UTC)
	scanner := &MarketScanner{Cfg: config.ScannerConfig{TTLDays: 3}}
	candidate := SwingCandidate{
		Ticker: "OWNED", Sources: []string{"mover", "active", "weekly_leader"},
		Price: 12.5, ChangePct: 4.2, Volume: 500, TradeCount: 25,
		Return1M: 20, Return3M: 40, Return6M: 80, DollarVolume: 1_000_000,
		MarketCapMM: 500, Sector: "Tech", Industry: "Semis",
		WeeklyLeaderRunID: "weekly-one", WeeklyLeaderEvidence: "ranked first",
		sourceMarketObservations: map[string]map[string]any{
			"mover":  moverMarketObservation(10, 2.5, 100),
			"active": activeMarketObservation(11, 200, 10),
		},
	}
	discovery, _, err := buildScannerTradeProfile(candidate, SwingScoreResult{})
	if err != nil {
		t.Fatalf("build profile: %v", err)
	}
	first := scanner.scannerSourceLeases(candidate, discovery, now)

	// This models a later weekly/premarket merge changing the shared scorer
	// fields. Neither ordinary screener observation changed.
	candidate.Price, candidate.ChangePct, candidate.Volume, candidate.TradeCount = 30, 9, 9_000, 900
	candidate.Return1M, candidate.Return3M, candidate.Return6M = 5, 10, 15
	candidate.DollarVolume, candidate.MarketCapMM = 9_000_000, 9_000
	candidate.Sector, candidate.Industry = "Energy", "Oil"
	changedDiscovery, _, err := buildScannerTradeProfile(candidate, SwingScoreResult{})
	if err != nil {
		t.Fatalf("build merged profile: %v", err)
	}
	second := scanner.scannerSourceLeases(candidate, changedDiscovery, now.Add(time.Hour))
	if first[0].EvidenceFingerprint != second[0].EvidenceFingerprint || first[1].EvidenceFingerprint != second[1].EvidenceFingerprint {
		t.Fatalf("merged fields renewed ordinary leases: %#v vs %#v", first[:2], second[:2])
	}

	candidate.sourceMarketObservations["mover"] = moverMarketObservation(10, 3.5, 100)
	thirdDiscovery, _, err := buildScannerTradeProfile(candidate, SwingScoreResult{})
	if err != nil {
		t.Fatalf("build mover change profile: %v", err)
	}
	third := scanner.scannerSourceLeases(candidate, thirdDiscovery, now.Add(2*time.Hour))
	if first[0].EvidenceFingerprint == third[0].EvidenceFingerprint {
		t.Fatal("changed mover observation did not renew mover lease")
	}
	if first[1].EvidenceFingerprint != third[1].EvidenceFingerprint {
		t.Fatal("changed mover observation renewed active lease")
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

func TestScannerSourceLeaseFingerprintIgnoresNewsOrdering(t *testing.T) {
	now := time.Date(2026, time.August, 21, 14, 0, 0, 0, time.UTC)
	scanner := &MarketScanner{Cfg: config.ScannerConfig{TTLDays: 3}}
	candidate := SwingCandidate{Ticker: "NEWS", Sources: []string{"mover"}, Price: 12.5, Volume: 100,
		News: []SwingNewsItem{
			{Headline: "Later", Sentiment: "POSITIVE", PublishedAt: now.Add(time.Hour)},
			{Headline: "Earlier", Sentiment: "NEUTRAL", PublishedAt: now},
		},
	}
	firstDiscovery, _, err := buildScannerTradeProfile(candidate, SwingScoreResult{})
	if err != nil {
		t.Fatalf("build first profile: %v", err)
	}
	first := scanner.scannerSourceLeases(candidate, firstDiscovery, now)
	candidate.News[0], candidate.News[1] = candidate.News[1], candidate.News[0]
	secondDiscovery, _, err := buildScannerTradeProfile(candidate, SwingScoreResult{})
	if err != nil {
		t.Fatalf("build reordered profile: %v", err)
	}
	second := scanner.scannerSourceLeases(candidate, secondDiscovery, now.Add(time.Hour))
	if first[0].EvidenceFingerprint != second[0].EvidenceFingerprint {
		t.Fatalf("news ordering changed fingerprint: %q != %q", first[0].EvidenceFingerprint, second[0].EvidenceFingerprint)
	}
}

func TestDeduplicateScannerCandidatesPreservesSourceOwnedGraphAndSympathyIdentity(t *testing.T) {
	now := time.Date(2026, time.August, 21, 14, 0, 0, 0, time.UTC)
	trigger := SwingNewsItem{Headline: "Seed announces new fab", Sentiment: "POSITIVE", PublishedAt: now}
	merged := deduplicateScannerCandidates([]SwingCandidate{
		{Ticker: "OVERLAP", Sources: []string{"mover"}, Price: 12, Volume: 100},
		{Ticker: "overlap", Sources: []string{"graph_neighbor"}, GraphContext: "Seed supplies_to OVERLAP"},
		{
			Ticker: "Overlap", Sources: []string{"news_sympathy"},
			GraphContext: "Seed news sympathy through customer_of OVERLAP",
			SympathySeed: "SEED", SympathyRelationship: "customer_of", News: []SwingNewsItem{trigger},
		},
	})
	if len(merged) != 1 {
		t.Fatalf("deduplicated candidates = %d, want 1", len(merged))
	}
	candidate := merged[0]
	if got := sourceGraphContext(candidate, "graph_neighbor"); got != "Seed supplies_to OVERLAP" {
		t.Fatalf("graph relationship = %q", got)
	}
	if got := sourceGraphContext(candidate, "news_sympathy"); got != "Seed news sympathy through customer_of OVERLAP" {
		t.Fatalf("sympathy relationship prose = %q", got)
	}
	if candidate.SympathySeed != "SEED" || candidate.SympathyRelationship != "customer_of" {
		t.Fatalf("sympathy identity = %q/%q", candidate.SympathySeed, candidate.SympathyRelationship)
	}
	if len(candidate.News) == 0 || candidate.News[0] != trigger {
		t.Fatalf("sympathy trigger was not retained first: %#v", candidate.News)
	}
	graphIdentity := scannerSourceIdentity(candidate, "graph_neighbor")
	if graphIdentity["relationship"] != "Seed supplies_to OVERLAP" {
		t.Fatalf("graph lease identity = %#v", graphIdentity)
	}
	sympathyIdentity := scannerSourceIdentity(candidate, "news_sympathy")
	if sympathyIdentity["seed"] != "SEED" || sympathyIdentity["relationship"] != "customer_of" {
		t.Fatalf("sympathy lease identity = %#v", sympathyIdentity)
	}
}

func TestMaxSourceLeaseExpiryUsesLongestGeneratedLease(t *testing.T) {
	now := time.Date(2026, time.August, 21, 14, 0, 0, 0, time.UTC)
	short := now.AddDate(0, 0, 3)
	political := now.AddDate(0, 0, 7)
	if got := maxSourceLeaseExpiry([]store.WatchlistSourceLease{{ExpiresAt: short}, {ExpiresAt: political}}, short); !got.Equal(political) {
		t.Fatalf("max source lease expiry = %s, want %s", got, political)
	}
	if got := maxSourceLeaseExpiry([]store.WatchlistSourceLease{{ExpiresAt: short}}, short); !got.Equal(short) {
		t.Fatalf("single source expiry = %s, want %s", got, short)
	}
}
