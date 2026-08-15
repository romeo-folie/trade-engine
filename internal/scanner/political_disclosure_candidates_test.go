package scanner

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/data"
)

type politicalAssetProvider struct {
	assets []data.StockAsset
	err    error
}

func (p politicalAssetProvider) FetchStockAssets(context.Context) ([]data.StockAsset, error) {
	return p.assets, p.err
}

type politicalSnapshotProvider struct {
	result data.StockSnapshotBatchResult
	err    error
}

func (p politicalSnapshotProvider) FetchStockSnapshots(
	context.Context, []string, int,
) (data.StockSnapshotBatchResult, error) {
	return p.result, p.err
}

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

func TestHydratePoliticalDisclosureMarketData(t *testing.T) {
	// DailyBar is the still-open session and must not be used for a terminal
	// liquidity decision; PreviousDailyBar is the completed-session evidence.
	daily := data.Candle{Close: 25, Volume: 100}
	previous := data.Candle{Close: 20, Volume: 90_000}
	scanner := &MarketScanner{
		StockAssets: politicalAssetProvider{assets: []data.StockAsset{{
			Symbol: "GOOD", Status: "active", AssetClass: "us_equity", Tradable: true,
		}}},
		StockSnapshots: politicalSnapshotProvider{result: data.StockSnapshotBatchResult{
			Snapshots: map[string]data.StockSnapshotEvidence{
				"GOOD": {Symbol: "GOOD", LatestTradePrice: 25, DailyBar: &daily, PreviousDailyBar: &previous},
			},
			Failures: map[string]error{},
		}},
	}
	candidate := SwingCandidate{
		Ticker: "GOOD", Sources: []string{"political_disclosure"},
		SourceScores:                      map[string]float64{"political_disclosure": .8},
		PoliticalDisclosureClaimToken:     "claim",
		PoliticalDisclosureClaimReportIDs: []string{"report-1", "report-2"},
		PoliticalDisclosureEvidence:       "two filings", PoliticalDisclosureScore: .8,
	}
	got := scanner.hydratePoliticalDisclosureMarketData(context.Background(), []SwingCandidate{candidate})
	if len(got) != 1 || got[0].Price != 25 || got[0].Volume != 90_000 ||
		got[0].DollarVolume != 1_800_000 || got[0].ChangePct != 25 {
		t.Fatalf("hydrated = %+v", got)
	}
	claims := scannerSourcePromotionClaims(got[0])
	if claims.PoliticalDisclosure == nil || len(claims.PoliticalDisclosure.ReportIDs) != 2 {
		t.Fatalf("promotion claims = %+v", claims.PoliticalDisclosure)
	}

	bad := candidate
	bad.Ticker = "IRA"
	if got := scanner.hydratePoliticalDisclosureMarketData(context.Background(), []SwingCandidate{bad}); len(got) != 0 {
		t.Fatalf("non-tradeable standalone candidate survived: %+v", got)
	}

	missingCompletedSession := candidate
	scanner.StockSnapshots = politicalSnapshotProvider{result: data.StockSnapshotBatchResult{
		Snapshots: map[string]data.StockSnapshotEvidence{
			"GOOD": {Symbol: "GOOD", LatestTradePrice: 25, DailyBar: &daily},
		},
		Failures: map[string]error{},
	}}
	if got := scanner.hydratePoliticalDisclosureMarketData(
		context.Background(), []SwingCandidate{missingCompletedSession},
	); len(got) != 0 {
		t.Fatalf("candidate without completed-session liquidity survived: %+v", got)
	}
}

func TestPoliticalMarketProviderFailureDoesNotReturnUnhydratedCandidate(t *testing.T) {
	scanner := &MarketScanner{
		StockAssets:    politicalAssetProvider{err: errors.New("asset service unavailable")},
		StockSnapshots: politicalSnapshotProvider{},
	}
	disclosure := SwingCandidate{
		Ticker: "GOOD", Sources: []string{"political_disclosure"},
		SourceScores:                      map[string]float64{"political_disclosure": .8},
		PoliticalDisclosureClaimToken:     "claim",
		PoliticalDisclosureClaimReportIDs: []string{"report-1"},
		PoliticalDisclosureEvidence:       "filing", PoliticalDisclosureScore: .8,
	}
	got := scanner.hydratePoliticalDisclosureMarketData(
		context.Background(), []SwingCandidate{disclosure},
	)
	if len(got) != 0 {
		t.Fatalf("unhydrated candidates = %+v", got)
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

func TestPoliticalClaimLossPreservesPremarketCandidate(t *testing.T) {
	premarket := SwingCandidate{
		Ticker: "MSFT", Price: 425, ChangePct: 3.2,
		Sources:                     []string{"premarket_episodic"},
		SourceScores:                map[string]float64{"premarket_episodic": .82},
		PremarketEpisodicRunID:      "premarket-run",
		PremarketEpisodicClaimToken: "premarket-claim",
		PremarketEpisodicEvidence:   "fresh catalyst",
	}
	disclosure := SwingCandidate{
		Ticker: "MSFT", Sources: []string{"political_disclosure"},
		SourceScores:                  map[string]float64{"political_disclosure": .9},
		PoliticalDisclosureClaimToken: "political-claim",
		PoliticalDisclosureEvidence:   "recent purchase",
		PoliticalDisclosureScore:      .9,
	}
	merged := mergePoliticalDisclosureCandidates([]SwingCandidate{premarket}, []SwingCandidate{disclosure})
	fallback, ok := candidateAfterPromotionClaimLoss(merged[0], "political_disclosure")
	if !ok || !hasSource(fallback.Sources, "premarket_episodic") || hasSource(fallback.Sources, "political_disclosure") {
		t.Fatalf("fallback = %+v, ok=%v", fallback, ok)
	}
	if fallback.PremarketEpisodicRunID != "premarket-run" || fallback.Price != 425 || fallback.PoliticalDisclosureClaimToken != "" {
		t.Fatalf("premarket state was not preserved: %+v", fallback)
	}
}
