package scanner

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/data"
	"github.com/romeo-folie/trade-engine/internal/store"
	"github.com/romeo-folie/trade-engine/internal/tradeprofile"
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
		PoliticalDisclosureClaimToken: "claim",
		PoliticalDisclosureEvidence:   "House filing by Hon. Example; purchase $15,001-$50,000",
		PoliticalDisclosureFacts:      json.RawMessage(`{"version":1,"ticker":"MSFT"}`),
	}
	merged := mergePoliticalDisclosureCandidates(existing, []SwingCandidate{disclosure})
	if len(merged) != 1 || !hasSource(merged[0].Sources, "political_disclosure") {
		t.Fatalf("merged = %+v", merged)
	}
	_, prompt := BuildSwingPrompt(merged[0], 0.65)
	if !strings.Contains(prompt, "Political disclosure evidence") ||
		!strings.Contains(prompt, "Hon. Example") ||
		strings.Contains(strings.ToLower(prompt), "political disclosure priority") {
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
		PoliticalDisclosureClaimToken:     "claim",
		PoliticalDisclosureClaimReportIDs: []string{"report-1", "report-2"},
		PoliticalDisclosureEvidence:       "two filings",
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
		PoliticalDisclosureClaimToken:     "claim",
		PoliticalDisclosureClaimReportIDs: []string{"report-1"},
		PoliticalDisclosureEvidence:       "filing",
	}
	got := scanner.hydratePoliticalDisclosureMarketData(
		context.Background(), []SwingCandidate{disclosure},
	)
	if len(got) != 0 {
		t.Fatalf("unhydrated candidates = %+v", got)
	}
}

func TestPoliticalDisclosureScoringFailureRequeuesClaim(t *testing.T) {
	if !politicalDisclosureScoringFailureRequeues("failed") ||
		politicalDisclosureScoringFailureRequeues("rejected") ||
		politicalDisclosureScoringFailureRequeues("pending") {
		t.Fatal("scoring-failure requeue must apply only to transient failed status")
	}
}

func TestPoliticalDisclosureFactsDistinguishRepeatedAndIndependentFilers(t *testing.T) {
	filing := time.Date(2026, 8, 9, 0, 0, 0, 0, time.UTC)
	firstTransaction := filing.AddDate(0, 0, -10)
	secondTransaction := filing.AddDate(0, 0, -4)
	minimum, maximum := int64(1_001), int64(15_000)
	items := []store.PoliticalDisclosureCandidate{
		politicalCandidateFixture(t, "db-1", "house-1", "house", "house", "Hon. Jane Doe", "CA12", filing, []store.PoliticalDisclosureTransaction{
			{Owner: "Self", TransactionType: "purchase", TransactionDate: firstTransaction, AmountMin: &minimum, AmountMax: &maximum},
			{Owner: "Spouse", TransactionType: "sale", TransactionDate: secondTransaction, AmountMin: &minimum, AmountMax: &maximum},
		}),
		politicalCandidateFixture(t, "db-2", "house-2", "house", "house", "Doe, Jane", "CA12", filing.AddDate(0, 0, 1), []store.PoliticalDisclosureTransaction{
			{Owner: "Self", TransactionType: "purchase", TransactionDate: secondTransaction},
		}),
		politicalCandidateFixture(t, "db-3", "senate-1", "senate", "senate", "Senator John Roe", "OH", filing, []store.PoliticalDisclosureTransaction{
			{Owner: "Joint", TransactionType: "purchase", TransactionDate: secondTransaction, AmountMin: &minimum, AmountMax: &maximum},
			{Owner: "Joint", TransactionType: "exchange", TransactionDate: secondTransaction},
		}),
	}
	items[1].ReportType = "periodic_transaction_amendment"
	candidate := buildPoliticalDisclosureCandidate("TEST", "claim", items)
	if _, ok := candidate.SourceScores["political_disclosure"]; ok {
		t.Fatalf("disclosure priority leaked into source scores: %+v", candidate.SourceScores)
	}
	var facts tradeprofile.PoliticalDisclosureEvidence
	if err := json.Unmarshal(candidate.PoliticalDisclosureFacts, &facts); err != nil {
		t.Fatal(err)
	}
	if facts.DistinctFilerCount != 2 || facts.DistinctBuyerCount != 2 ||
		facts.ReportCount != 3 || facts.AmendmentReportCount != 1 || facts.IndependentReportCount != 2 || facts.IndependentTransactionCount != 4 || facts.AmendmentTransactionCount != 1 || facts.TransactionCount != 5 ||
		facts.PurchaseCount != 2 || facts.SaleCount != 1 || facts.ExchangeCount != 1 ||
		!facts.RepeatedFilerActivity || !facts.IndependentBuyerActivity ||
		!facts.ContradictoryActivity {
		t.Fatalf("aggregate facts = %+v", facts)
	}
	if facts.PurchaseAmounts.KnownTransactionCount != 2 ||
		facts.PurchaseAmounts.UnknownTransactionCount != 0 ||
		facts.PurchaseAmounts.MinimumTotal != 2_002 ||
		facts.PurchaseAmounts.MaximumTotal != 30_000 {
		t.Fatalf("purchase amounts = %+v", facts.PurchaseAmounts)
	}
	if facts.DisclosureLagDays.LatestTransactionDays != 4 {
		t.Fatalf("latest transaction disclosure lag = %+v", facts.DisclosureLagDays)
	}
	if len(facts.Filers) != 2 || facts.Filers[0].CanonicalFilerID == facts.Filers[1].CanonicalFilerID {
		t.Fatalf("filer facts = %+v", facts.Filers)
	}
	var jane *tradeprofile.PoliticalDisclosureFilerEvidence
	for index := range facts.Filers {
		if strings.Contains(facts.Filers[index].CanonicalFilerID, "doe-jane") {
			jane = &facts.Filers[index]
		}
	}
	if jane == nil || jane.ReportCount != 2 || jane.AmendmentReportCount != 1 || jane.IndependentReportCount != 1 || !jane.RepeatedActivity ||
		!jane.ContradictoryActivity || len(jane.ReportedOwners) != 2 ||
		jane.DisclosureLagDays.LatestTransactionDays != 4 {
		t.Fatalf("canonical Jane evidence = %+v", jane)
	}
}

func TestPoliticalDisclosureFactsReachScannerSnapshotProfile(t *testing.T) {
	filing := time.Date(2026, 8, 15, 0, 0, 0, 0, time.UTC)
	transaction := filing.AddDate(0, 0, -5)
	items := []store.PoliticalDisclosureCandidate{
		politicalCandidateFixture(t, "db-1", "house-1", "house", "house", "Alex Example", "CA12", filing, []store.PoliticalDisclosureTransaction{{
			Owner: "Self", TransactionType: "purchase", TransactionDate: transaction,
		}}),
	}
	candidate := buildPoliticalDisclosureCandidate("TEST", "claim", items)
	discovery, profile, err := buildScannerTradeProfile(candidate, SwingScoreResult{Strategy: "momentum"})
	if err != nil {
		t.Fatal(err)
	}
	recorder := &scanRecorder{candidates: make(map[string]*store.ScannerCandidateSnapshot)}
	recorder.register([]SwingCandidate{candidate})
	if err := recorder.setTradeProfile(candidate, discovery, profile); err != nil {
		t.Fatal(err)
	}
	snapshot := recorder.candidates["TEST"]
	if snapshot == nil || snapshot.DiscoveryContext == nil || snapshot.ExecutionProfile == nil {
		t.Fatalf("snapshot profile = %+v", snapshot)
	}
	var facts tradeprofile.PoliticalDisclosureEvidence
	if err := json.Unmarshal(snapshot.DiscoveryContext.EvidenceBySource["political_disclosure"], &facts); err != nil {
		t.Fatal(err)
	}
	if facts.ReportCount != 1 || facts.DistinctBuyerCount != 1 ||
		snapshot.ExecutionProfile.PrimaryTimeframe != "4H" {
		t.Fatalf("snapshot facts=%+v profile=%+v", facts, snapshot.ExecutionProfile)
	}
}

func TestPoliticalDisclosureAmendmentDoesNotInflateIndependentBreadthOrRepeatedActivity(t *testing.T) {
	filing := time.Date(2026, 8, 15, 0, 0, 0, 0, time.UTC)
	transaction := filing.AddDate(0, 0, -5)
	items := []store.PoliticalDisclosureCandidate{
		politicalCandidateFixture(t, "base", "base", "oge", "executive", "Alex Example", "Office", filing, []store.PoliticalDisclosureTransaction{{TransactionType: "purchase", TransactionDate: transaction}}),
		politicalCandidateFixture(t, "amendment", "amendment", "oge", "executive", "Alex Example", "Office", filing.AddDate(0, 0, 1), []store.PoliticalDisclosureTransaction{{TransactionType: "purchase", TransactionDate: transaction}}),
	}
	items[1].IsAmendment = true
	newerAmendment := filing.AddDate(0, 0, 3)
	items[1].LatestTransaction = newerAmendment
	items[1].Evidence, _ = json.Marshal([]store.PoliticalDisclosureTransaction{{TransactionType: "sale", TransactionDate: newerAmendment}})
	candidate := buildPoliticalDisclosureCandidate("TEST", "claim", items)
	var facts tradeprofile.PoliticalDisclosureEvidence
	if err := json.Unmarshal(candidate.PoliticalDisclosureFacts, &facts); err != nil {
		t.Fatal(err)
	}
	if facts.ReportCount != 2 || facts.AmendmentReportCount != 1 || facts.IndependentReportCount != 1 || facts.RepeatedFilerActivity || facts.IndependentBuyerActivity || facts.DistinctBuyerCount != 1 {
		t.Fatalf("amendment inflated independent evidence: %+v", facts)
	}
	if len(facts.Filers) != 1 || facts.Filers[0].IndependentReportCount != 1 || facts.Filers[0].RepeatedActivity {
		t.Fatalf("filer evidence=%+v", facts.Filers)
	}
	if facts.LatestTransactionDate != transaction.Format("2006-01-02") || facts.Filers[0].LatestTransactionDate != transaction.Format("2006-01-02") || facts.SaleCount != 0 {
		t.Fatalf("newer amendment changed independent semantics: %+v", facts)
	}
	limitation := strings.Join(facts.DataQuality.Limitations, " ")
	if !strings.Contains(limitation, "report detail and raw transaction counts") ||
		!strings.Contains(limitation, "independent semantic action, amount, owner, timing, repetition, and contradiction") ||
		!strings.Contains(limitation, "no authoritative supersession relationship is available") {
		t.Fatalf("amendment data-quality limitation missing: %+v", facts.DataQuality)
	}
}

func TestPoliticalDisclosureCandidateAggregatesHistoryButOwnsOnlyClaimedRows(t *testing.T) {
	filing := time.Date(2026, 8, 15, 0, 0, 0, 0, time.UTC)
	items := []store.PoliticalDisclosureCandidate{
		politicalCandidateFixture(t, "history", "history", "house", "house", "Alex Example", "CA12", filing, []store.PoliticalDisclosureTransaction{{TransactionType: "purchase", TransactionDate: filing}}),
		politicalCandidateFixture(t, "pending", "pending", "house", "house", "Alex Example", "CA12", filing.AddDate(0, 0, 1), []store.PoliticalDisclosureTransaction{{TransactionType: "sale", TransactionDate: filing}}),
	}
	items[1].Claimed = true
	items[1].PromotionClaimToken = "claim"
	candidate := buildPoliticalDisclosureCandidate("TEST", "claim", items)
	if len(candidate.PoliticalDisclosureClaimReportIDs) != 1 || candidate.PoliticalDisclosureClaimReportIDs[0] != "pending" {
		t.Fatalf("claim report IDs = %v, want only pending claim", candidate.PoliticalDisclosureClaimReportIDs)
	}
	var facts tradeprofile.PoliticalDisclosureEvidence
	if err := json.Unmarshal(candidate.PoliticalDisclosureFacts, &facts); err != nil {
		t.Fatal(err)
	}
	if facts.ReportCount != 2 || facts.TransactionCount != 2 || facts.PurchaseCount != 1 || facts.SaleCount != 1 {
		t.Fatalf("history was not fully aggregated: %+v", facts)
	}
	if !strings.Contains(candidate.PoliticalDisclosureEvidence, "2 raw transaction row(s) (2 independent; 0 amendment)") {
		t.Fatalf("summary does not label raw transaction rows: %q", candidate.PoliticalDisclosureEvidence)
	}
}

func politicalCandidateFixture(
	t *testing.T, reportID, externalID, source, chamber, filer, office string,
	filingDate time.Time, transactions []store.PoliticalDisclosureTransaction,
) store.PoliticalDisclosureCandidate {
	t.Helper()
	evidence, err := json.Marshal(transactions)
	if err != nil {
		t.Fatal(err)
	}
	latest := time.Time{}
	for _, transaction := range transactions {
		if transaction.TransactionDate.After(latest) {
			latest = transaction.TransactionDate
		}
	}
	return store.PoliticalDisclosureCandidate{
		ReportID: reportID, ExternalID: externalID, Ticker: "TEST",
		Source: source, Chamber: chamber, FilerName: filer, Office: office,
		ReportType: "periodic_transaction",
		FilingDate: filingDate, DocumentURL: "https://example.test/" + externalID,
		ExtractionStatus: "completed", TransactionCount: len(transactions),
		LatestTransaction: latest, Evidence: evidence,
	}
}

func TestCanonicalPoliticalFilerIdentityNormalizesTitlesButKeepsSuffixes(t *testing.T) {
	first := canonicalPoliticalFilerID("house", "house", "Hon. Jane Doe")
	second := canonicalPoliticalFilerID("house", "house", "Doe, Jane")
	if first != second {
		t.Fatalf("equivalent filer IDs differ: %q != %q", first, second)
	}
	junior := canonicalPoliticalFilerID("house", "house", "John Smith Jr.")
	senior := canonicalPoliticalFilerID("house", "house", "John Smith Sr.")
	if junior == senior {
		t.Fatalf("distinct suffix identities collapsed to %q", junior)
	}
}

func TestPoliticalDisclosureTTL(t *testing.T) {
	scanner := MarketScanner{Cfg: config.ScannerConfig{
		TTLDays:              3,
		PoliticalDisclosures: config.PoliticalDisclosureScannerConfig{TTLDays: 7},
	}}
	if got := scanner.candidateTTLDays(SwingCandidate{Sources: []string{"political_disclosure"}}); got != 3 {
		t.Fatalf("disclosure provenance changed watchlist scheduling: TTL = %d, want 3", got)
	}
}

func TestPoliticalDisclosureBreadthDoesNotChangeConvictionOrder(t *testing.T) {
	items := []scoredCandidate{
		{candidate: SwingCandidate{Ticker: "A", PoliticalDisclosureFacts: json.RawMessage(`{"distinct_buyer_count":20}`)}, score: SwingScoreResult{Conviction: .80}},
		{candidate: SwingCandidate{Ticker: "B"}, score: SwingScoreResult{Conviction: .81}},
	}
	sortByConviction(items)
	if items[0].candidate.Ticker != "B" {
		t.Fatalf("disclosure signal overrode conviction: %#v", items)
	}
	items = []scoredCandidate{
		{candidate: SwingCandidate{Ticker: "B"}, score: SwingScoreResult{Conviction: .80}},
		{candidate: SwingCandidate{Ticker: "A", PoliticalDisclosureFacts: json.RawMessage(`{"distinct_buyer_count":20}`)}, score: SwingScoreResult{Conviction: .80}},
	}
	sortByConviction(items)
	if items[0].candidate.Ticker != "B" {
		t.Fatalf("equal-conviction order changed due to disclosure breadth: %#v", items)
	}
}

func TestPoliticalDisclosureIsNotAnExecutionStrategy(t *testing.T) {
	candidate := SwingCandidate{
		Sources: []string{"mover", "political_disclosure"}, PullbackQualified: true,
	}
	strategies := scannerCandidateStrategies(candidate, SwingScoreResult{Strategy: "momentum"})
	if strings.Contains(strings.Join(strategies, ","), "political_disclosure") {
		t.Fatalf("discovery source leaked into execution strategies: %v", strategies)
	}
	if got := strings.Join(strategies, ","); got != "momentum,pullback_strength" {
		t.Fatalf("strategies = %q", got)
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
		PoliticalDisclosureClaimToken: "political-claim",
		PoliticalDisclosureEvidence:   "recent purchase",
		PoliticalDisclosureFacts:      json.RawMessage(`{"version":1,"ticker":"MSFT"}`),
	}
	merged := mergePoliticalDisclosureCandidates([]SwingCandidate{premarket}, []SwingCandidate{disclosure})
	fallback, ok := candidateAfterPromotionClaimLoss(merged[0], "political_disclosure")
	if !ok || !hasSource(fallback.Sources, "premarket_episodic") || hasSource(fallback.Sources, "political_disclosure") {
		t.Fatalf("fallback = %+v, ok=%v", fallback, ok)
	}
	if fallback.PremarketEpisodicRunID != "premarket-run" || fallback.Price != 425 || fallback.PoliticalDisclosureClaimToken != "" {
		t.Fatalf("premarket state was not preserved: %+v", fallback)
	}
	if fallback.PoliticalDisclosureEvidence != "" || len(fallback.PoliticalDisclosureFacts) != 0 ||
		strings.Contains(strings.Join(scannerCandidateStrategies(fallback, SwingScoreResult{Strategy: "momentum"}), ","), "political_disclosure") {
		t.Fatalf("fallback leaked political evidence into execution: %+v", fallback)
	}
}

func TestNonPoliticalClaimLossPreservesDisclosureFactsWithoutStrategyLeak(t *testing.T) {
	ordinary := SwingCandidate{Ticker: "MSFT", Price: 425, Sources: []string{"mover"}}
	weekly := cloneSwingCandidate(ordinary)
	weekly.Sources = []string{"mover", "weekly_leader"}
	weekly.WeeklyLeaderRunID = "weekly-run"
	weekly.WeeklyLeaderClaimToken = "weekly-claim"
	weekly.WeeklyLeaderEvidence = "rank 2"
	weekly.weeklyLeaderFallback = &ordinary
	disclosure := SwingCandidate{
		Ticker: "MSFT", Sources: []string{"political_disclosure"},
		PoliticalDisclosureClaimToken:     "political-claim",
		PoliticalDisclosureClaimReportIDs: []string{"report-1"},
		PoliticalDisclosureEvidence:       "one purchase",
		PoliticalDisclosureFacts:          json.RawMessage(`{"version":1,"ticker":"MSFT","report_count":1,"distinct_buyer_count":1}`),
	}
	merged := mergePoliticalDisclosureCandidates([]SwingCandidate{weekly}, []SwingCandidate{disclosure})
	withoutWeekly, ok := candidateAfterPromotionClaimLoss(merged[0], "weekly_leader")
	if !ok || hasSource(withoutWeekly.Sources, "weekly_leader") ||
		!hasSource(withoutWeekly.Sources, "political_disclosure") ||
		len(withoutWeekly.PoliticalDisclosureFacts) == 0 ||
		withoutWeekly.PoliticalDisclosureClaimToken != "political-claim" {
		t.Fatalf("weekly claim fallback lost disclosure provenance: %+v, ok=%v", withoutWeekly, ok)
	}
	if strings.Contains(strings.Join(scannerCandidateStrategies(
		withoutWeekly, SwingScoreResult{Strategy: "momentum"},
	), ","), "political_disclosure") {
		t.Fatalf("disclosure provenance leaked into fallback strategies: %+v", withoutWeekly)
	}
	ordinaryAgain, ok := candidateAfterPromotionClaimLoss(withoutWeekly, "political_disclosure")
	if !ok || len(ordinaryAgain.Sources) != 1 || ordinaryAgain.Sources[0] != "mover" ||
		len(ordinaryAgain.PoliticalDisclosureFacts) != 0 || ordinaryAgain.PoliticalDisclosureClaimToken != "" {
		t.Fatalf("sequential claim fallback = %+v, ok=%v", ordinaryAgain, ok)
	}
}

func TestSequentialClaimLossDoesNotResurrectPoliticalDisclosure(t *testing.T) {
	disclosure := SwingCandidate{
		Ticker: "MSFT", Sources: []string{"political_disclosure"},
		PoliticalDisclosureClaimToken:     "political-claim",
		PoliticalDisclosureClaimReportIDs: []string{"report-1"},
		PoliticalDisclosureEvidence:       "one purchase",
		PoliticalDisclosureFacts:          json.RawMessage(`{"version":1,"ticker":"MSFT"}`),
	}
	tests := []struct {
		name       string
		merge      func([]SwingCandidate) []SwingCandidate
		secondLoss string
	}{
		{
			name: "weekly leader", secondLoss: "weekly_leader",
			merge: func(existing []SwingCandidate) []SwingCandidate {
				ordinary := SwingCandidate{Ticker: "MSFT", Sources: []string{"mover"}}
				leader := SwingCandidate{
					Ticker: "MSFT", Sources: []string{"weekly_leader"},
					WeeklyLeaderRunID: "weekly-run", WeeklyLeaderClaimToken: "weekly-claim",
				}
				return mergeWeeklyLeaderCandidates([]SwingCandidate{ordinary}, []SwingCandidate{leader})
			},
		},
		{
			name: "potent industry", secondLoss: "potent_industry",
			merge: func(existing []SwingCandidate) []SwingCandidate {
				ordinary := SwingCandidate{Ticker: "MSFT", Sources: []string{"mover"}}
				industry := SwingCandidate{
					Ticker: "MSFT", Sources: []string{"potent_industry"},
					PotentIndustryRunID: "industry-run", PotentIndustryClaimToken: "industry-claim",
					SourceScores: map[string]float64{"potent_industry": 1},
				}
				return mergePotentIndustryCandidates([]SwingCandidate{ordinary}, []SwingCandidate{industry})
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			merged := mergePoliticalDisclosureCandidates(test.merge(nil), []SwingCandidate{disclosure})
			withoutPolitical, ok := candidateAfterPromotionClaimLoss(merged[0], "political_disclosure")
			if !ok {
				t.Fatal("political claim loss did not produce source fallback")
			}
			ordinary, ok := candidateAfterPromotionClaimLoss(withoutPolitical, test.secondLoss)
			if !ok {
				t.Fatalf("%s claim loss did not produce ordinary fallback", test.secondLoss)
			}
			assertNoPoliticalDisclosureProvenance(t, ordinary)
		})
	}
}

func TestCloneSwingCandidateLineageCopiesSharedCyclesIndependently(t *testing.T) {
	original := &SwingCandidate{Ticker: "MSFT", Sources: []string{"mover"}}
	shared := &SwingCandidate{Ticker: "MSFT", Sources: []string{"weekly_leader"}}
	original.weeklyLeaderFallback = shared
	original.potentIndustryFallback = shared
	shared.politicalDisclosureFallback = original

	clone := cloneSwingCandidateLineage(original)
	if clone == original || clone.weeklyLeaderFallback == shared {
		t.Fatal("lineage clone retained original nodes")
	}
	if clone.weeklyLeaderFallback != clone.potentIndustryFallback {
		t.Fatal("lineage clone did not retain shared fallback identity")
	}
	if clone.weeklyLeaderFallback.politicalDisclosureFallback != clone {
		t.Fatal("lineage clone did not retain cyclic fallback edge")
	}
	clone.weeklyLeaderFallback.Sources = []string{"mutated"}
	if strings.Join(shared.Sources, ",") != "weekly_leader" {
		t.Fatalf("mutating lineage clone changed original fallback: %+v", shared)
	}
}

func assertNoPoliticalDisclosureProvenance(t *testing.T, candidate SwingCandidate) {
	t.Helper()
	seen := make(map[*SwingCandidate]struct{})
	var walk func(*SwingCandidate)
	walk = func(item *SwingCandidate) {
		if item == nil {
			return
		}
		if _, exists := seen[item]; exists {
			return
		}
		seen[item] = struct{}{}
		_, hasPoliticalScore := item.SourceScores["political_disclosure"]
		if hasSource(item.Sources, "political_disclosure") ||
			hasPoliticalScore ||
			item.PoliticalDisclosureClaimToken != "" ||
			len(item.PoliticalDisclosureClaimReportIDs) != 0 ||
			item.PoliticalDisclosureEvidence != "" || len(item.PoliticalDisclosureFacts) != 0 ||
			item.politicalDisclosureFallback != nil || item.politicalDisclosureSource != nil {
			t.Fatalf("political provenance resurrected in %+v", *item)
		}
		walk(item.weeklyLeaderFallback)
		walk(item.potentIndustryFallback)
		walk(item.potentIndustrySource)
		walk(item.politicalDisclosureFallback)
		walk(item.politicalDisclosureSource)
	}
	walk(&candidate)
}

func TestClaimLossFallsBackToDisclosureOnlyCandidate(t *testing.T) {
	weekly := SwingCandidate{
		Ticker: "MSFT", Sources: []string{"weekly_leader"},
		WeeklyLeaderRunID: "weekly-run", WeeklyLeaderClaimToken: "weekly-claim",
		WeeklyLeaderEvidence: "rank 2",
	}
	disclosure := SwingCandidate{
		Ticker: "MSFT", Price: 425, Volume: 100_000, DollarVolume: 42_500_000,
		Sources:                           []string{"political_disclosure"},
		PoliticalDisclosureClaimToken:     "political-claim",
		PoliticalDisclosureClaimReportIDs: []string{"report-1"},
		PoliticalDisclosureEvidence:       "one purchase",
		PoliticalDisclosureFacts:          json.RawMessage(`{"version":1,"ticker":"MSFT","report_count":1,"distinct_buyer_count":1}`),
	}
	merged := mergePoliticalDisclosureCandidates([]SwingCandidate{weekly}, []SwingCandidate{disclosure})
	fallback, ok := candidateAfterPromotionClaimLoss(merged[0], "weekly_leader")
	if !ok || len(fallback.Sources) != 1 || fallback.Sources[0] != "political_disclosure" ||
		fallback.Price != 425 || len(fallback.PoliticalDisclosureFacts) == 0 ||
		fallback.PoliticalDisclosureClaimToken != "political-claim" {
		t.Fatalf("disclosure-only fallback = %+v, ok=%v", fallback, ok)
	}
	if strings.Contains(strings.Join(scannerCandidateStrategies(
		fallback, SwingScoreResult{Strategy: "momentum"},
	), ","), "political_disclosure") {
		t.Fatalf("disclosure-only fallback leaked an execution strategy: %+v", fallback)
	}
}

func TestClaimLossAfterDisclosureHydrationRetainsLiquidityAndTaxonomy(t *testing.T) {
	previous := data.Candle{Close: 20, Volume: 90_000}
	scanner := &MarketScanner{
		StockAssets: politicalAssetProvider{assets: []data.StockAsset{{
			Symbol: "GOOD", Status: "active", AssetClass: "us_equity", Tradable: true,
		}}},
		StockSnapshots: politicalSnapshotProvider{result: data.StockSnapshotBatchResult{
			Snapshots: map[string]data.StockSnapshotEvidence{
				"GOOD": {Symbol: "GOOD", LatestTradePrice: 25, PreviousDailyBar: &previous},
			},
			Failures: map[string]error{},
		}},
	}
	disclosure := scanner.hydratePoliticalDisclosureMarketData(context.Background(), []SwingCandidate{{
		Ticker: "GOOD", Sources: []string{"political_disclosure"},
		PoliticalDisclosureClaimToken: "political", PoliticalDisclosureClaimReportIDs: []string{"report-1"},
	}})
	if len(disclosure) != 1 {
		t.Fatalf("hydrated disclosure = %+v", disclosure)
	}
	weekly := SwingCandidate{Ticker: "GOOD", Sources: []string{"weekly_leader"}, WeeklyLeaderRunID: "weekly", WeeklyLeaderClaimToken: "weekly-token"}
	merged := mergePoliticalDisclosureCandidates([]SwingCandidate{weekly}, disclosure)
	taxonomyAt := time.Date(2026, 8, 17, 0, 0, 0, 0, time.UTC)
	applyUniverseTaxonomyToLineage(&merged[0], &store.SwingUniverseMember{
		Ticker: "GOOD", Sector: "Technology", Industry: "Software", TaxonomyStatus: "known",
		TaxonomyAsOf: &taxonomyAt, TaxonomyProvider: "fixture", TaxonomyEvidence: json.RawMessage(`{"source":"fixture"}`),
	})
	fallback, ok := candidateAfterPromotionClaimLoss(merged[0], "weekly_leader")
	if !ok || fallback.Price != 25 || fallback.Volume != 90_000 || fallback.DollarVolume != 1_800_000 ||
		fallback.Sector != "Technology" || fallback.Industry != "Software" || fallback.TaxonomyStatus != "known" {
		t.Fatalf("claim-loss fallback lost hydrated evidence: %+v, ok=%v", fallback, ok)
	}
}

func TestPoliticalOnlyFallbackFailsClosedWithoutFullLiquidityEvidence(t *testing.T) {
	for _, disclosure := range []SwingCandidate{
		{Ticker: "BAD", Price: 20, Volume: 0, DollarVolume: pullbackMinDollarVolume, Sources: []string{"political_disclosure"}},
		{Ticker: "BAD", Price: 20, Volume: 10, DollarVolume: pullbackMinDollarVolume - 1, Sources: []string{"political_disclosure"}},
	} {
		candidate := SwingCandidate{Ticker: "BAD", Sources: []string{"weekly_leader", "political_disclosure"}, politicalDisclosureSource: &disclosure}
		if fallback, ok := politicalDisclosureOnlyFallback(candidate); ok {
			t.Fatalf("insufficiently liquid fallback survived: %+v", fallback)
		}
	}
}
