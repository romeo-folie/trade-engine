package universe

import (
	"context"
	"encoding/json"
	"errors"
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

func (f fakeFundamentals) FetchCompanyFundamentals(
	_ context.Context, ticker string,
) (data.CompanyFundamentals, error) {
	value, ok := f[ticker]
	if !ok {
		return data.CompanyFundamentals{}, errors.New("profile unavailable")
	}
	return value, nil
}

type fakeRepository struct {
	taxonomy map[string]*store.CompanyTaxonomyObservation
	version  store.SwingUniverseVersion
	members  []store.SwingUniverseMember
	failed   error
}

func (f *fakeRepository) Begin(_ context.Context, v store.SwingUniverseVersion) (string, error) {
	f.version = v
	return "version-1", nil
}
func (f *fakeRepository) ReplaceMembers(
	_ context.Context, _ string, members []store.SwingUniverseMember,
) error {
	f.members = members
	return nil
}
func (f *fakeRepository) Complete(context.Context, string, time.Time) error { return nil }
func (f *fakeRepository) Fail(_ context.Context, _ string, _ time.Time, err error) error {
	f.failed = err
	return nil
}
func (f *fakeRepository) TaxonomyAsOf(
	_ context.Context, ticker string, _ time.Time, _ time.Duration,
) (*store.CompanyTaxonomyObservation, error) {
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
			if repo.version.ID != "" || len(repo.members) != 0 {
				t.Fatalf("outage published state: version=%+v members=%+v", repo.version, repo.members)
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
		MinMedianDollarVolume: 1_000_000, DollarVolumeLookbackDays: 20,
		AllowedSecurityTypes: []string{"us_equity"},
		AllowedExchanges:     []string{"NYSE", "NASDAQ", "AMEX"},
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
	for i := range bars {
		bars[i] = data.Candle{Ticker: symbol, Close: close, Volume: volume}
	}
	return bars
}
