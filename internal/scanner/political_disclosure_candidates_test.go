package scanner

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/config"
)

func TestMergePoliticalDisclosureCandidateCarriesEvidenceIntoPrompt(t *testing.T) {
	existing := []SwingCandidate{{Ticker: "MSFT", Sources: []string{"mover"}}}
	disclosure := SwingCandidate{
		Ticker: "MSFT", Sources: []string{"political_disclosure"},
		SourceScores:                  map[string]float64{"political_disclosure": 1},
		PoliticalDisclosureClaimToken: "claim",
		PoliticalDisclosureEvidence:   "House filing by Hon. Example; purchase $15,001-$50,000",
	}
	merged := mergePoliticalDisclosureCandidates(existing, []SwingCandidate{disclosure})
	if len(merged) != 1 || !hasSource(merged[0].Sources, "political_disclosure") {
		t.Fatalf("merged = %+v", merged)
	}
	_, prompt := BuildSwingPrompt(merged[0], 0.65)
	if !strings.Contains(prompt, "Political disclosure evidence") ||
		!strings.Contains(prompt, "Hon. Example") {
		t.Fatalf("prompt omitted disclosure evidence: %s", prompt)
	}
	fallback, ok := candidateAfterPromotionClaimLoss(merged[0], "political_disclosure")
	if !ok || len(fallback.Sources) != 1 || fallback.Sources[0] != "mover" {
		t.Fatalf("fallback = %+v, ok=%v", fallback, ok)
	}
}

func TestStandaloneDisclosurePromptDoesNotInventMarketData(t *testing.T) {
	_, prompt := BuildSwingPrompt(SwingCandidate{Ticker: "MSFT", Sources: []string{"political_disclosure"}}, .65)
	if strings.Contains(prompt, "Price:        0.00") || !strings.Contains(prompt, "Market data:  unavailable") {
		t.Fatalf("prompt = %s", prompt)
	}
}

func TestPoliticalDisclosureScoreCountsPurchasesBeyondPreview(t *testing.T) {
	now := time.Date(2026, 8, 9, 0, 0, 0, 0, time.UTC)
	got := scorePoliticalDisclosure(now, 7, now, 20, 20, 1)
	want := .45 + .35 + .20/3
	if math.Abs(got-want) > 1e-9 {
		t.Fatalf("score = %f, want %f", got, want)
	}
}

func TestPoliticalDisclosureTTL(t *testing.T) {
	scanner := MarketScanner{Cfg: config.ScannerConfig{
		TTLDays:              3,
		PoliticalDisclosures: config.PoliticalDisclosureScannerConfig{TTLDays: 7},
	}}
	if got := scanner.candidateTTLDays(SwingCandidate{Sources: []string{"political_disclosure"}}); got != 7 {
		t.Fatalf("TTL = %d, want 7", got)
	}
}

func TestPoliticalDisclosurePriorityBreaksCloseConvictionTies(t *testing.T) {
	items := []scoredCandidate{
		{candidate: SwingCandidate{Ticker: "A", PoliticalDisclosureScore: 1}, score: SwingScoreResult{Conviction: .80}},
		{candidate: SwingCandidate{Ticker: "B"}, score: SwingScoreResult{Conviction: .91}},
	}
	sortByConviction(items)
	if items[0].candidate.Ticker != "B" {
		t.Fatalf("disclosure signal overrode conviction: %#v", items)
	}
	items[0].score.Conviction = .80
	items[1].score.Conviction = .79
	sortByConviction(items)
	if items[0].candidate.Ticker != "A" {
		t.Fatalf("disclosure signal did not prioritize close score: %#v", items)
	}
}
