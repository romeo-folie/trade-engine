package universe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/data"
	"github.com/romeo-folie/trade-engine/internal/store"
)

type fakeAssets struct {
	assets []data.StockAsset
	err    error
}

func (f fakeAssets) FetchStockAssets(context.Context) ([]data.StockAsset, error) {
	return f.assets, f.err
}

type fakeMarketData struct {
	snapshots data.StockSnapshotBatchResult
	bars      data.DailyBarsBatchResult
}

type recordingMarketData struct {
	snapshotSymbols []string
	barSymbols      []string
}

func (r *recordingMarketData) FetchStockSnapshots(
	_ context.Context, symbols []string, _ int,
) (data.StockSnapshotBatchResult, error) {
	r.snapshotSymbols = append([]string(nil), symbols...)
	return completeMarkets(symbols, 20).snapshots, nil
}

func (r *recordingMarketData) FetchDailyBars(
	_ context.Context, symbols []string, _ time.Time, _ time.Time, _ int,
) (data.DailyBarsBatchResult, error) {
	r.barSymbols = append([]string(nil), symbols...)
	return completeMarkets(symbols, 20).bars, nil
}

func (f fakeMarketData) FetchStockSnapshots(
	context.Context, []string, int,
) (data.StockSnapshotBatchResult, error) {
	return f.snapshots, nil
}
func (f fakeMarketData) FetchDailyBars(
	context.Context, []string, time.Time, time.Time, int,
) (data.DailyBarsBatchResult, error) {
	return f.bars, nil
}

type fakeFundamentals map[string]data.CompanyFundamentals

func (f fakeFundamentals) ProviderName() string { return "fake-fundamentals" }

func (f fakeFundamentals) FetchCompanyFundamentals(
	_ context.Context, ticker string,
) (data.CompanyFundamentals, error) {
	value, ok := f[ticker]
	if !ok {
		return data.CompanyFundamentals{}, errors.New("profile unavailable")
	}
	return value, nil
}

type timeoutFundamentals struct{}

func (timeoutFundamentals) ProviderName() string { return "timeout-fundamentals" }

func (timeoutFundamentals) FetchCompanyFundamentals(
	context.Context, string,
) (data.CompanyFundamentals, error) {
	return data.CompanyFundamentals{}, context.DeadlineExceeded
}

type fakeRepository struct {
	taxonomy          map[string]*store.CompanyTaxonomyObservation
	taxonomyLookupErr error
	version           store.SwingUniverseVersion
	members           []store.SwingUniverseMember
	failed            error
	completedQuality  store.SwingUniverseQuality
	failedQuality     store.SwingUniverseQuality
}

func (f *fakeRepository) Begin(_ context.Context, v store.SwingUniverseVersion) (string, error) {
	f.version = v
	return "version-1", nil
}
func (f *fakeRepository) UpdateMetadata(
	_ context.Context, _ string, v store.SwingUniverseVersion,
) error {
	f.version = v
	return nil
}
func (f *fakeRepository) ReplaceMembers(
	_ context.Context, _ string, members []store.SwingUniverseMember,
) error {
	f.members = members
	return nil
}
func (f *fakeRepository) Complete(
	_ context.Context, _ string, _ time.Time, quality store.SwingUniverseQuality,
) error {
	f.completedQuality = quality
	return nil
}
func (f *fakeRepository) Fail(
	_ context.Context, _ string, _ time.Time, quality store.SwingUniverseQuality, err error,
) error {
	f.failed = err
	f.failedQuality = quality
	return nil
}
func (f *fakeRepository) TaxonomyAsOf(
	_ context.Context, ticker string, _ time.Time, _ time.Duration,
) (*store.CompanyTaxonomyObservation, error) {
	if f.taxonomyLookupErr != nil {
		return nil, f.taxonomyLookupErr
	}
	return f.taxonomy[ticker], nil
}
func (f *fakeRepository) InsertTaxonomy(
	_ context.Context, observation store.CompanyTaxonomyObservation,
) (string, error) {
	copy := observation
	f.taxonomy[observation.Ticker] = &copy
	return "taxonomy-1", nil
}

func TestRefreshEnumeratesFullAssetMasterAndRecordsPartialFailures(t *testing.T) {
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	cfg := testConfig()
	cfg.MinMarketDataCoverage = 0.50
	repo := &fakeRepository{taxonomy: make(map[string]*store.CompanyTaxonomyObservation)}
	bars := dailyBars("GOOD", 20, 100, 20_000)
	market := fakeMarketData{
		snapshots: data.StockSnapshotBatchResult{
			Snapshots: map[string]data.StockSnapshotEvidence{
				"GOOD": {Symbol: "GOOD", LatestTradePrice: 101},
			},
			Failures: map[string]error{"MISS": errors.New("snapshot timeout")},
		},
		bars: data.DailyBarsBatchResult{
			Bars:     map[string][]data.Candle{"GOOD": bars},
			Failures: map[string]error{"MISS": errors.New("bars timeout")},
		},
	}
	builder := Builder{
		Config: cfg,
		Assets: fakeAssets{assets: []data.StockAsset{
			{Symbol: "GOOD", Status: "active", AssetClass: "us_equity", Exchange: "NASDAQ", Tradable: true},
			{Symbol: "MISS", Status: "active", AssetClass: "us_equity", Exchange: "NYSE", Tradable: true},
		}},
		Snapshots: market, Bars: market,
		Fundamentals: fakeFundamentals{
			"GOOD": {MarketCapitalization: 500, Industry: "Technology", Provider: "finnhub"},
		},
		Repository: repo,
		Now:        func() time.Time { return now },
	}

	result, err := builder.Refresh(context.Background())
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if result.MemberCount != 2 || len(repo.members) != 2 {
		t.Fatalf("result/members = %+v / %+v", result, repo.members)
	}
	if !repo.members[0].Eligible || repo.members[0].Industry != "Technology" {
		t.Fatalf("GOOD member = %+v", repo.members[0])
	}
	if repo.members[1].Eligible || repo.members[1].FailureStage != "provider_evidence" ||
		repo.members[1].RemovalStage != "market_data" {
		t.Fatalf("MISS member = %+v", repo.members[1])
	}
	if !repo.version.AsOf.Equal(now) || repo.version.AsOf.Before(repo.version.CapturedAt) {
		t.Fatalf("invalid point-in-time version = %+v", repo.version)
	}
	if repo.completedQuality.EvidenceRequiredCount != 2 ||
		repo.completedQuality.CoherentMarketDataCount != 1 ||
		repo.completedQuality.MarketDataCoverage != 0.50 ||
		repo.completedQuality.ProviderFailureCount != 1 {
		t.Fatalf("persisted completion quality = %+v", repo.completedQuality)
	}
}

func TestRefreshNarrowsEvidenceRequestsToAssetMasterEligibleSymbols(t *testing.T) {
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	repo := &fakeRepository{taxonomy: make(map[string]*store.CompanyTaxonomyObservation)}
	repo.taxonomy["OTC"] = &store.CompanyTaxonomyObservation{
		Ticker: "OTC", Industry: "Cached Industry", Sector: "Cached Sector", Status: "known",
		ObservedAt: now,
	}
	market := &recordingMarketData{}
	builder := Builder{
		Config: testConfig(),
		Assets: fakeAssets{assets: []data.StockAsset{
			{Symbol: "GOOD", Status: "active", AssetClass: "us_equity", Exchange: "NASDAQ", Tradable: true},
			{Symbol: "OTC", Status: "active", AssetClass: "us_equity", Exchange: "OTC", Tradable: true},
		}},
		Snapshots: market, Bars: market,
		Fundamentals: fakeFundamentals{
			"GOOD": {MarketCapitalization: 500, Industry: "Technology", Provider: "finnhub"},
		},
		Repository: repo,
		Now:        func() time.Time { return now },
	}
	if _, err := builder.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if strings.Join(market.snapshotSymbols, ",") != "GOOD" ||
		strings.Join(market.barSymbols, ",") != "GOOD" {
		t.Fatalf("evidence requests = snapshots %v bars %v, want only GOOD",
			market.snapshotSymbols, market.barSymbols)
	}
	if len(repo.members) != 2 || repo.members[1].Eligible || repo.members[1].RemovalStage != "exchange" ||
		repo.members[1].Industry != "Cached Industry" {
		t.Fatalf("members = %+v, want GOOD eligible and OTC excluded at exchange stage", repo.members)
	}
}

func TestRefreshPublishesAllIneligibleSnapshotWithoutEvidenceRequests(t *testing.T) {
	repo := &fakeRepository{taxonomy: make(map[string]*store.CompanyTaxonomyObservation)}
	market := &recordingMarketData{}
	builder := Builder{
		Config: testConfig(),
		Assets: fakeAssets{assets: []data.StockAsset{{
			Symbol: "OTC", Status: "active", AssetClass: "us_equity", Exchange: "OTC", Tradable: true,
		}}},
		Snapshots: market, Bars: market, Repository: repo,
		Now: func() time.Time { return time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC) },
	}
	result, err := builder.Refresh(context.Background())
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if result.MemberCount != 1 || len(repo.members) != 1 || repo.members[0].Eligible ||
		repo.members[0].RemovalStage != "exchange" {
		t.Fatalf("result/members = %+v / %+v", result, repo.members)
	}
	if len(market.snapshotSymbols) != 0 || len(market.barSymbols) != 0 {
		t.Fatalf("evidence requests = snapshots %v bars %v, want none",
			market.snapshotSymbols, market.barSymbols)
	}
}

func TestRefreshExcludesNonCommonInstrumentsBeforeEvidenceRequests(t *testing.T) {
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	repo := &fakeRepository{taxonomy: make(map[string]*store.CompanyTaxonomyObservation)}
	market := &recordingMarketData{}
	builder := Builder{
		Config: testConfig(),
		Assets: fakeAssets{assets: []data.StockAsset{
			{Symbol: "GOOD", Name: "Common Stock", Status: "active", AssetClass: "us_equity", Exchange: "NASDAQ", Tradable: true},
			{Symbol: "WARRANT", Name: "Example Acquisition Corp. Warrants", Status: "active", AssetClass: "us_equity", Exchange: "NASDAQ", Tradable: true},
			{Symbol: "ETF", Name: "Example Trust ETF", Status: "active", AssetClass: "us_equity", Exchange: "NASDAQ", Tradable: true},
		}},
		Snapshots: market, Bars: market, Repository: repo,
		Fundamentals: fakeFundamentals{
			"GOOD": {MarketCapitalization: 500, Industry: "Technology", Provider: "finnhub"},
			"ETF":  {MarketCapitalization: 500, Industry: "Funds", Provider: "finnhub"},
		},
		Now: func() time.Time { return now },
	}
	if _, err := builder.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if strings.Join(market.snapshotSymbols, ",") != "ETF,GOOD" ||
		strings.Join(market.barSymbols, ",") != "ETF,GOOD" {
		t.Fatalf("evidence requests = snapshots %v bars %v, want ETF and GOOD only", market.snapshotSymbols, market.barSymbols)
	}
	var warrant store.SwingUniverseMember
	for _, member := range repo.members {
		if member.Ticker == "WARRANT" {
			warrant = member
		}
	}
	if len(repo.members) != 3 || warrant.RemovalStage != "asset_name" || warrant.Eligible {
		t.Fatalf("members = %+v, want Warrant excluded at asset_name stage", repo.members)
	}
}

func TestExcludedNamePatternsPreserveIssuerAndETFNames(t *testing.T) {
	patterns, err := compileExcludedNamePatterns(testConfig().ExcludedNamePatterns)
	if err != nil {
		t.Fatalf("compileExcludedNamePatterns: %v", err)
	}
	for _, name := range []string{
		"Preferred Bank Common Stock",
		"iShares Preferred and Income Securities ETF",
		"Preferred Stock ETF",
		"Notes Live, Inc. Common Stock",
		"Unit Corporation Common Stock",
	} {
		if matchesExcludedAssetName(name, patterns) {
			t.Errorf("name %q was excluded, want preserved", name)
		}
	}
	for _, name := range []string{
		"Example Acquisition Corp. Warrants",
		"Example Acquisition Corp. Warrants Class A Common Stock",
		"Example Acquisition Corp. Units (1 Ord & 1/2 Wt)",
		"Example Corp. Preferred Stock",
		"Example Corp. Preferred Stock, Series A",
		"MicroSectors FANG+ Index 3X Leveraged ETN",
		"Senior Notes due 2030",
		"6.35% Fixed-to-Floating Rate Perpetual Non-Cumulative Preference Share, Series A",
	} {
		if !matchesExcludedAssetName(name, patterns) {
			t.Errorf("name %q was preserved, want excluded", name)
		}
	}
	explicit, err := compileExcludedNamePatterns([]string{`(?i)acme`})
	if err != nil || !matchesExcludedAssetName("Acme Corp. Common Stock", explicit) {
		t.Fatalf("explicit exclusion pattern did not override common-stock naming: err=%v", err)
	}
}

func TestRefreshRejectsInvalidExcludedNamePattern(t *testing.T) {
	cfg := testConfig()
	cfg.ExcludedNamePatterns = []string{"["}
	builder := Builder{Config: cfg}
	if _, err := builder.Refresh(context.Background()); err == nil || !strings.Contains(err.Error(), "excluded_name_patterns") {
		t.Fatalf("Refresh error = %v, want invalid excluded_name_patterns error", err)
	}
}

func TestRefreshUnknownTaxonomyDoesNotInventClassification(t *testing.T) {
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	cfg := testConfig()
	cfg.MissingFundamentalsPolicy = "include"
	cfg.TaxonomyRefreshLimit = 0
	repo := &fakeRepository{taxonomy: make(map[string]*store.CompanyTaxonomyObservation)}
	market := completeMarket("NEW", 20)
	builder := Builder{
		Config: cfg,
		Assets: fakeAssets{assets: []data.StockAsset{{
			Symbol: "NEW", Status: "active", AssetClass: "us_equity", Exchange: "NASDAQ", Tradable: true,
		}}},
		Snapshots: market, Bars: market, Repository: repo,
		Now: func() time.Time { return now },
	}

	if _, err := builder.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	got := repo.members[0]
	if got.Sector != store.UnknownTaxonomy || got.Industry != store.UnknownTaxonomy ||
		got.TaxonomyStatus != "unknown" {
		t.Fatalf("unknown taxonomy was not explicit: %+v", got)
	}
}

func TestMedianDollarVolumeRequiresCompleteLookback(t *testing.T) {
	asOf := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	if _, ok := medianDollarVolume(dailyBars("A", 19, 10, 100), 20, asOf); ok {
		t.Fatal("short history unexpectedly qualified")
	}
	got, ok := medianDollarVolume([]data.Candle{
		{Close: 10, Volume: 10}, {Close: 20, Volume: 10},
		{Close: 30, Volume: 10}, {Close: 40, Volume: 10},
	}, 4, asOf)
	if !ok || got != 250 {
		t.Fatalf("median = %v, %v; want 250, true", got, ok)
	}
}

func TestMedianDollarVolumeExcludesInProgressSession(t *testing.T) {
	location, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("load New York timezone: %v", err)
	}
	asOf := time.Date(2026, 7, 24, 15, 0, 0, 0, location)
	bars := []data.Candle{
		{
			Timestamp: time.Date(2026, 7, 22, 0, 0, 0, 0, location),
			Close:     10, Volume: 10,
		},
		{
			Timestamp: time.Date(2026, 7, 23, 0, 0, 0, 0, location),
			Close:     20, Volume: 10,
		},
		{
			Timestamp: time.Date(2026, 7, 24, 0, 0, 0, 0, location),
			Close:     1, Volume: 1,
		},
	}
	got, ok := medianDollarVolume(bars, 2, asOf)
	if !ok || got != 150 {
		t.Fatalf("pre-close median = %v, %v; want 150, true", got, ok)
	}
	got, ok = medianDollarVolume(bars, 2, asOf.Add(time.Hour))
	if !ok || got != 100.5 {
		t.Fatalf("post-close median = %v, %v; want 100.5, true", got, ok)
	}
}

func TestRefreshRejectsTotalMarketDataOutage(t *testing.T) {
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name   string
		market fakeMarketData
	}{
		{
			name: "all snapshot batches fail",
			market: fakeMarketData{
				snapshots: data.StockSnapshotBatchResult{
					Snapshots: map[string]data.StockSnapshotEvidence{},
					Failures:  map[string]error{"A": errors.New("snapshot outage")},
				},
				bars: data.DailyBarsBatchResult{
					Bars:     map[string][]data.Candle{"A": dailyBars("A", 20, 100, 20_000)},
					Failures: map[string]error{},
				},
			},
		},
		{
			name: "all daily bar batches fail",
			market: fakeMarketData{
				snapshots: data.StockSnapshotBatchResult{
					Snapshots: map[string]data.StockSnapshotEvidence{
						"A": {Symbol: "A", LatestTradePrice: 100},
					},
					Failures: map[string]error{},
				},
				bars: data.DailyBarsBatchResult{
					Bars:     map[string][]data.Candle{},
					Failures: map[string]error{"A": errors.New("bars outage")},
				},
			},
		},
		{
			name: "daily bars are truncated below completed lookback",
			market: fakeMarketData{
				snapshots: data.StockSnapshotBatchResult{
					Snapshots: map[string]data.StockSnapshotEvidence{
						"A": {Symbol: "A", LatestTradePrice: 100},
					},
					Failures: map[string]error{},
				},
				bars: data.DailyBarsBatchResult{
					Bars:     map[string][]data.Candle{"A": dailyBars("A", 19, 100, 20_000)},
					Failures: map[string]error{},
				},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			repo := &fakeRepository{taxonomy: make(map[string]*store.CompanyTaxonomyObservation)}
			builder := Builder{
				Config: testConfig(),
				Assets: fakeAssets{assets: []data.StockAsset{{
					Symbol: "A", Status: "active", AssetClass: "us_equity",
					Exchange: "NASDAQ", Tradable: true,
				}}},
				Snapshots: test.market, Bars: test.market,
				Fundamentals: fakeFundamentals{
					"A": {MarketCapitalization: 500, Industry: "Technology", Provider: "finnhub"},
				},
				Repository: repo,
				Now:        func() time.Time { return now },
			}

			if _, err := builder.Refresh(context.Background()); err == nil {
				t.Fatal("total market-data outage unexpectedly published a universe")
			}
			if repo.version.Provider == "" || len(repo.members) != 1 || repo.failed == nil {
				t.Fatalf("outage was not persisted for audit: version=%+v members=%+v failed=%v",
					repo.version, repo.members, repo.failed)
			}
			if repo.failedQuality.EvidenceRequiredCount != 1 ||
				repo.failedQuality.CoherentMarketDataCount != 0 ||
				repo.failedQuality.MarketDataCoverage != 0 {
				t.Fatalf("persisted rejection quality = %+v", repo.failedQuality)
			}
			if strings.Contains(repo.members[0].FailureReason, "fundamentals") ||
				repo.members[0].RemovalStage != "market_data" {
				t.Fatalf("unattempted fundamentals leaked into rejection audit: %+v", repo.members[0])
			}
		})
	}
}

func TestTaxonomyRefreshPrioritizesUnattemptedSymbols(t *testing.T) {
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	cfg := testConfig()
	cfg.TaxonomyRefreshLimit = 1
	unknownEvidence, _ := json.Marshal(fundamentalsEvidence{MarketCapMM: 500})
	repo := &fakeRepository{taxonomy: map[string]*store.CompanyTaxonomyObservation{
		"A": {
			Ticker: "A", Provider: "finnhub", EffectiveAt: now.Add(-time.Hour),
			ObservedAt: now.Add(-time.Hour), Sector: store.UnknownTaxonomy,
			Industry: store.UnknownTaxonomy, Status: "unknown", Evidence: unknownEvidence,
		},
	}}
	market := completeMarkets([]string{"A", "B"}, 20)
	builder := Builder{
		Config: cfg,
		Assets: fakeAssets{assets: []data.StockAsset{
			{Symbol: "A", Status: "active", AssetClass: "us_equity", Exchange: "NASDAQ", Tradable: true},
			{Symbol: "B", Status: "active", AssetClass: "us_equity", Exchange: "NASDAQ", Tradable: true},
		}},
		Snapshots: market, Bars: market,
		Fundamentals: fakeFundamentals{
			"B": {MarketCapitalization: 600, Industry: "Software", Provider: "finnhub"},
		},
		Repository: repo,
		Now:        func() time.Time { return now },
	}

	result, err := builder.Refresh(context.Background())
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if result.TaxonomyRefreshes != 1 || repo.taxonomy["B"] == nil ||
		repo.taxonomy["B"].Industry != "Software" {
		t.Fatalf("unattempted symbol was starved: result=%+v taxonomy=%+v", result, repo.taxonomy)
	}
}

func TestTaxonomyRefreshPrioritizesOldestStaleObservation(t *testing.T) {
	now := time.Date(2026, 7, 26, 21, 0, 0, 0, time.UTC)
	cfg := testConfig()
	cfg.TaxonomyRefreshLimit = 1
	evidence, _ := json.Marshal(fundamentalsEvidence{MarketCapMM: 500})
	repo := &fakeRepository{taxonomy: map[string]*store.CompanyTaxonomyObservation{
		"A": {
			Ticker: "A", Provider: "finnhub", EffectiveAt: now.Add(-8 * 24 * time.Hour),
			ObservedAt: now.Add(-8 * 24 * time.Hour), Sector: store.UnknownTaxonomy,
			Industry: "Technology", Status: "stale", Evidence: evidence,
		},
		"B": {
			Ticker: "B", Provider: "finnhub", EffectiveAt: now.Add(-9 * 24 * time.Hour),
			ObservedAt: now.Add(-9 * 24 * time.Hour), Sector: store.UnknownTaxonomy,
			Industry: "Banking", Status: "stale", Evidence: evidence,
		},
	}}
	market := completeMarkets([]string{"A", "B"}, 20)
	builder := Builder{
		Config: cfg,
		Assets: fakeAssets{assets: []data.StockAsset{
			{Symbol: "A", Status: "active", AssetClass: "us_equity", Exchange: "NASDAQ", Tradable: true},
			{Symbol: "B", Status: "active", AssetClass: "us_equity", Exchange: "NASDAQ", Tradable: true},
		}},
		Snapshots: market, Bars: market,
		Fundamentals: fakeFundamentals{
			"A": {MarketCapitalization: 500, Industry: "Technology", Provider: "finnhub"},
			"B": {MarketCapitalization: 500, Industry: "Banking", Provider: "finnhub"},
		},
		Repository: repo, Now: func() time.Time { return now },
	}
	if _, err := builder.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := repo.taxonomy["B"].ObservedAt; !got.Equal(now) {
		t.Fatalf("oldest stale observation was not refreshed first: B observed at %s", got)
	}
	if _, err := builder.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := repo.taxonomy["A"].ObservedAt; !got.Equal(now) {
		t.Fatalf("later stale observation was not refreshed on the next run: A observed at %s", got)
	}
}

func TestTaxonomyObservationMapsCuratedFinnhubIndustries(t *testing.T) {
	now := time.Date(2026, 8, 17, 21, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name, industry, wantSector string
	}{
		{"banking", "Banking", "Financials"},
		{"normalized semiconductor", "  Semiconductors ", "Technology"},
		{"health care", "Health Care", "Health Care"},
		{"unmapped", "N/A", store.UnknownTaxonomy},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := taxonomyObservation("TEST", data.CompanyFundamentals{
				Industry: tc.industry, Provider: "finnhub",
			}, now)
			if got.Industry != strings.TrimSpace(tc.industry) || got.Sector != tc.wantSector {
				t.Fatalf("taxonomy = %+v, want raw industry %q and sector %q", got,
					strings.TrimSpace(tc.industry), tc.wantSector)
			}
			if tc.wantSector == store.UnknownTaxonomy && strings.Contains(string(got.Evidence), finnhubIndustrySectorMappingVersion) {
				t.Fatalf("unmapped industry unexpectedly recorded a mapping: %s", got.Evidence)
			}
		})
	}
	otherProvider := taxonomyObservation("TEST", data.CompanyFundamentals{
		Industry: "Banking", Provider: "other",
	}, now)
	if otherProvider.Sector != store.UnknownTaxonomy {
		t.Fatalf("non-Finnhub provider was mapped: %+v", otherProvider)
	}
}

func TestTaxonomyLookupFailureAbortsRefresh(t *testing.T) {
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	lookupErr := errors.New("taxonomy database unavailable")
	repo := &fakeRepository{
		taxonomy:          make(map[string]*store.CompanyTaxonomyObservation),
		taxonomyLookupErr: lookupErr,
	}
	market := completeMarket("LOOKUP", 20)
	builder := Builder{
		Config: testConfig(),
		Assets: fakeAssets{assets: []data.StockAsset{{
			Symbol: "LOOKUP", Status: "active", AssetClass: "us_equity",
			Exchange: "NASDAQ", Tradable: true,
		}}},
		Snapshots: market,
		Bars:      market,
		Fundamentals: fakeFundamentals{
			"LOOKUP": {
				MarketCapitalization: 600,
				Industry:             "Software",
				Provider:             "fake-fundamentals",
			},
		},
		Repository: repo,
		Now:        func() time.Time { return now },
	}

	if _, err := builder.Refresh(context.Background()); !errors.Is(err, lookupErr) {
		t.Fatalf("Refresh error = %v, want taxonomy lookup failure", err)
	}
	if repo.version.ID != "" || len(repo.members) != 0 {
		t.Fatalf("taxonomy lookup failure published state: version=%+v members=%+v", repo.version, repo.members)
	}
}

func TestFailedTaxonomyAttemptAdvancesBoundedRefresh(t *testing.T) {
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	cfg := testConfig()
	cfg.TaxonomyRefreshLimit = 1
	repo := &fakeRepository{taxonomy: make(map[string]*store.CompanyTaxonomyObservation)}
	market := completeMarkets([]string{"A", "B"}, 20)
	builder := Builder{
		Config: cfg,
		Assets: fakeAssets{assets: []data.StockAsset{
			{Symbol: "A", Status: "active", AssetClass: "us_equity", Exchange: "NASDAQ", Tradable: true},
			{Symbol: "B", Status: "active", AssetClass: "us_equity", Exchange: "NASDAQ", Tradable: true},
		}},
		Snapshots: market, Bars: market,
		Fundamentals: fakeFundamentals{
			"B": {MarketCapitalization: 600, Industry: "Software", Provider: "fake-fundamentals"},
		},
		Repository: repo,
		Now:        func() time.Time { return now },
	}

	if _, err := builder.Refresh(context.Background()); err == nil {
		t.Fatal("first refresh without any market-cap evidence unexpectedly completed")
	}
	failedAttempt := repo.taxonomy["A"]
	if failedAttempt == nil || failedAttempt.Status != "unavailable" ||
		failedAttempt.Provider != "fake-fundamentals" ||
		!strings.Contains(string(failedAttempt.Evidence), "profile unavailable") {
		t.Fatalf("failed attempt was not persisted: %+v", failedAttempt)
	}

	result, err := builder.Refresh(context.Background())
	if err != nil {
		t.Fatalf("second Refresh: %v", err)
	}
	if result.TaxonomyRefreshes != 1 || repo.taxonomy["B"] == nil ||
		repo.taxonomy["B"].Industry != "Software" {
		t.Fatalf("later symbol remained starved: result=%+v taxonomy=%+v", result, repo.taxonomy)
	}
}

func TestProviderLocalTimeoutPersistsTaxonomyAttempt(t *testing.T) {
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	cfg := testConfig()
	repo := &fakeRepository{taxonomy: make(map[string]*store.CompanyTaxonomyObservation)}
	market := completeMarket("TIMEOUT", 20)
	builder := Builder{
		Config: cfg,
		Assets: fakeAssets{assets: []data.StockAsset{{
			Symbol: "TIMEOUT", Status: "active", AssetClass: "us_equity",
			Exchange: "NASDAQ", Tradable: true,
		}}},
		Snapshots:    market,
		Bars:         market,
		Fundamentals: timeoutFundamentals{},
		Repository:   repo,
		Now:          func() time.Time { return now },
	}

	if _, err := builder.Refresh(context.Background()); err == nil {
		t.Fatal("refresh without any market-cap evidence unexpectedly completed")
	}
	failedAttempt := repo.taxonomy["TIMEOUT"]
	if failedAttempt == nil || failedAttempt.Status != "unavailable" ||
		failedAttempt.Provider != "timeout-fundamentals" ||
		!strings.Contains(string(failedAttempt.Evidence), context.DeadlineExceeded.Error()) {
		t.Fatalf("provider-local timeout was not persisted: %+v", failedAttempt)
	}
}

func TestRefreshAppliesSpreadCeiling(t *testing.T) {
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	cfg := testConfig()
	cfg.MaxSpreadBPS = 10
	repo := &fakeRepository{taxonomy: make(map[string]*store.CompanyTaxonomyObservation)}
	market := completeMarket("WIDE", 20)
	snapshot := market.snapshots.Snapshots["WIDE"]
	snapshot.LatestBidPrice = 99
	snapshot.LatestAskPrice = 101
	market.snapshots.Snapshots["WIDE"] = snapshot
	builder := Builder{
		Config: cfg,
		Assets: fakeAssets{assets: []data.StockAsset{{
			Symbol: "WIDE", Status: "active", AssetClass: "us_equity", Exchange: "NASDAQ", Tradable: true,
		}}},
		Snapshots: market, Bars: market,
		Fundamentals: fakeFundamentals{
			"WIDE": {MarketCapitalization: 500, Industry: "Technology", Provider: "finnhub"},
		},
		Repository: repo,
		Now:        func() time.Time { return now },
	}
	if _, err := builder.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if repo.members[0].Eligible || repo.members[0].RemovalStage != "spread" ||
		repo.members[0].SpreadBPS == nil || *repo.members[0].SpreadBPS != 200 {
		t.Fatalf("spread member = %+v", repo.members[0])
	}
}

func TestRefreshAppliesAssetMasterEligibilityFlags(t *testing.T) {
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name  string
		asset data.StockAsset
		stage string
	}{
		{
			name: "security type",
			asset: data.StockAsset{
				Symbol: "TYPE", Status: "active", AssetClass: "crypto", Exchange: "NASDAQ", Tradable: true,
			},
			stage: "security_type",
		},
		{
			name: "exchange",
			asset: data.StockAsset{
				Symbol: "OTC", Status: "active", AssetClass: "us_equity", Exchange: "OTC", Tradable: true,
			},
			stage: "exchange",
		},
		{
			name: "inactive",
			asset: data.StockAsset{
				Symbol: "OLD", Status: "inactive", AssetClass: "us_equity", Exchange: "NYSE", Tradable: true,
			},
			stage: "active",
		},
		{
			name: "untradable",
			asset: data.StockAsset{
				Symbol: "HALT", Status: "active", AssetClass: "us_equity", Exchange: "NYSE",
			},
			stage: "tradable",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			repo := &fakeRepository{taxonomy: make(map[string]*store.CompanyTaxonomyObservation)}
			market := completeMarket(test.asset.Symbol, 20)
			builder := Builder{
				Config: testConfig(), Assets: fakeAssets{assets: []data.StockAsset{test.asset}},
				Snapshots: market, Bars: market,
				Fundamentals: fakeFundamentals{
					test.asset.Symbol: {
						MarketCapitalization: 500, Industry: "Technology", Provider: "finnhub",
					},
				},
				Repository: repo, Now: func() time.Time { return now },
			}
			result, err := builder.Refresh(context.Background())
			if err != nil {
				t.Fatalf("Refresh: %v", err)
			}
			if got := repo.members[0]; got.Eligible || got.RemovalStage != test.stage {
				t.Fatalf("member = %+v, want removal stage %q", got, test.stage)
			}
			if result.TaxonomyRefreshes != 0 {
				t.Fatalf("objectively excluded asset consumed taxonomy quota: %+v", result)
			}
		})
	}
}

func testConfig() config.UniverseConfig {
	return config.UniverseConfig{
		Enabled: true, Provider: "alpaca", MinPrice: 5, MinMarketCapMM: 100,
		MinMarketDataCoverage: 0.90,
		MinMedianDollarVolume: 1_000_000, DollarVolumeLookbackDays: 20,
		AllowedSecurityTypes: []string{"us_equity"},
		AllowedExchanges:     []string{"NYSE", "NASDAQ", "AMEX"},
		ExcludedNamePatterns: []string{`(?i)\bwarrants?\b`, `(?i)\brights?\b`, `(?i)\bunits?(?:\s*(?:[,(]|$))`, `(?i)\bnotes?\s+due\b`, `(?i)\betns?\b`, `(?i)\bdebentures?\b`, `(?i)\bpreferred\s+(?:stock|shares?)(?:,\s*series\b.*)?\s*$`, `(?i)\bpreference\s+shares?(?:,\s*series\b.*)?\s*$`},
		RequireActive:        true, RequireTradable: true, RequireMarketData: true,
		BatchSize: 200, TaxonomyMaxAgeHours: 168, TaxonomyRefreshLimit: 200,
		MissingFundamentalsPolicy: "exclude", MissingMarketDataPolicy: "exclude",
	}
}

func completeMarket(symbol string, days int) fakeMarketData {
	return completeMarkets([]string{symbol}, days)
}

func completeMarkets(symbols []string, days int) fakeMarketData {
	snapshots := make(map[string]data.StockSnapshotEvidence, len(symbols))
	bars := make(map[string][]data.Candle, len(symbols))
	for _, symbol := range symbols {
		snapshots[symbol] = data.StockSnapshotEvidence{Symbol: symbol, LatestTradePrice: 100}
		bars[symbol] = dailyBars(symbol, days, 100, 20_000)
	}
	return fakeMarketData{
		snapshots: data.StockSnapshotBatchResult{
			Snapshots: snapshots,
			Failures:  map[string]error{},
		},
		bars: data.DailyBarsBatchResult{
			Bars:     bars,
			Failures: map[string]error{},
		},
	}
}

func dailyBars(symbol string, count int, close float64, volume int64) []data.Candle {
	bars := make([]data.Candle, count)
	location, _ := time.LoadLocation("America/New_York")
	last := time.Date(2026, 7, 24, 0, 0, 0, 0, location)
	for i := range bars {
		bars[i] = data.Candle{
			Ticker: symbol, Close: close, Volume: volume,
			Timestamp: last.AddDate(0, 0, i-count+1),
		}
	}
	return bars
}

func TestMarketDataQualityUsesCoherentIntersection(t *testing.T) {
	asOf := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	quality := marketDataQuality(
		[]string{"A", "B", "C"},
		data.StockSnapshotBatchResult{Snapshots: map[string]data.StockSnapshotEvidence{
			"A": {LatestTradePrice: 10}, "B": {LatestTradePrice: 20},
		}},
		data.DailyBarsBatchResult{Bars: map[string][]data.Candle{
			"B": dailyBars("B", 20, 20, 1_000), "C": dailyBars("C", 20, 30, 1_000),
		}},
		20, asOf, 0.90,
	)
	if quality.UsableSnapshotCount != 2 || quality.UsableBarHistoryCount != 2 ||
		quality.CoherentMarketDataCount != 1 || quality.ProviderFailureCount != 2 ||
		quality.MarketDataCoverage != 1.0/3.0 {
		t.Fatalf("quality = %+v", quality)
	}
}

func TestMarketDataQualityThresholdBoundaryAndEmptyDenominator(t *testing.T) {
	asOf := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	var symbols []string
	snapshots := make(map[string]data.StockSnapshotEvidence)
	bars := make(map[string][]data.Candle)
	for i := 0; i < 10; i++ {
		symbol := fmt.Sprintf("S%02d", i)
		symbols = append(symbols, symbol)
		snapshots[symbol] = data.StockSnapshotEvidence{LatestTradePrice: 10}
		if i < 9 {
			bars[symbol] = dailyBars(symbol, 20, 10, 1_000)
		}
	}
	quality := marketDataQuality(symbols,
		data.StockSnapshotBatchResult{Snapshots: snapshots},
		data.DailyBarsBatchResult{Bars: bars}, 20, asOf, 0.90)
	if quality.CoherentMarketDataCount != 9 || quality.MarketDataCoverage != 0.90 ||
		quality.MarketDataCoverage < quality.MinMarketDataCoverage {
		t.Fatalf("boundary quality = %+v", quality)
	}
	empty := marketDataQuality(nil, data.StockSnapshotBatchResult{}, data.DailyBarsBatchResult{}, 20, asOf, 0.90)
	if empty.EvidenceRequiredCount != 0 || empty.MarketDataCoverage != 1 {
		t.Fatalf("empty-denominator quality = %+v", empty)
	}
}

func TestCompletedDailyBarsUsesEarlyClose(t *testing.T) {
	location, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	bar := data.Candle{Timestamp: time.Date(2024, 11, 29, 0, 0, 0, 0, location), Close: 100, Volume: 1_000}
	before := time.Date(2024, 11, 29, 12, 59, 59, 0, location)
	atClose := time.Date(2024, 11, 29, 13, 0, 0, 0, location)
	if got := completedDailyBars([]data.Candle{bar}, before); len(got) != 0 {
		t.Fatalf("pre-close bar included: %+v", got)
	}
	if got := completedDailyBars([]data.Candle{bar}, atClose); len(got) != 1 {
		t.Fatalf("early-close bar excluded: %+v", got)
	}
}

func TestMarketDataQualityRejectsHistoryMissingLatestSession(t *testing.T) {
	asOf := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	history := dailyBars("STALE", 20, 10, 1_000)
	for i := range history {
		history[i].Timestamp = history[i].Timestamp.AddDate(0, 0, -1)
	}
	quality := marketDataQuality(
		[]string{"STALE"},
		data.StockSnapshotBatchResult{Snapshots: map[string]data.StockSnapshotEvidence{
			"STALE": {LatestTradePrice: 10},
		}},
		data.DailyBarsBatchResult{Bars: map[string][]data.Candle{"STALE": history}},
		20, asOf, 0.90,
	)
	if quality.UsableBarHistoryCount != 0 || quality.MarketDataCoverage != 0 {
		t.Fatalf("stale history approved: %+v", quality)
	}
}

func TestRefreshApprovesBoundaryButExcludesStaleSessionMember(t *testing.T) {
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	assets := make([]data.StockAsset, 0, 10)
	symbols := make([]string, 0, 10)
	fundamentals := make(fakeFundamentals, 10)
	for i := 0; i < 10; i++ {
		symbol := fmt.Sprintf("B%02d", i)
		symbols = append(symbols, symbol)
		assets = append(assets, data.StockAsset{
			Symbol: symbol, Status: "active", AssetClass: "us_equity",
			Exchange: "NASDAQ", Tradable: true,
		})
		fundamentals[symbol] = data.CompanyFundamentals{
			MarketCapitalization: 500, Industry: "Technology", Provider: "finnhub",
		}
	}
	market := completeMarkets(symbols, 20)
	stale := market.bars.Bars["B09"]
	for i := range stale {
		stale[i].Timestamp = stale[i].Timestamp.AddDate(0, 0, -1)
	}
	market.bars.Bars["B09"] = stale
	repository := &fakeRepository{taxonomy: make(map[string]*store.CompanyTaxonomyObservation)}
	result, err := (&Builder{
		Config: testConfig(), Assets: fakeAssets{assets: assets},
		Snapshots: market, Bars: market, Fundamentals: fundamentals,
		Repository: repository, Now: func() time.Time { return now },
	}).Refresh(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.Quality.MarketDataCoverage != 0.90 || result.EligibleCount != 9 {
		t.Fatalf("boundary result = %+v", result)
	}
	for _, member := range repository.members {
		if member.Ticker == "B09" {
			if member.Eligible || member.RemovalStage != "market_data" ||
				member.FailureStage != "provider_evidence" {
				t.Fatalf("stale boundary member = %+v", member)
			}
			return
		}
	}
	t.Fatal("stale boundary member was not audited")
}
