package universe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/data"
	"github.com/romeo-folie/trade-engine/internal/engine"
	"github.com/romeo-folie/trade-engine/internal/store"
)

// FundamentalsProvider is deliberately narrow so the universe builder can use
// a cached/profile provider without coupling discovery to the news interface.
type FundamentalsProvider interface {
	FetchCompanyFundamentals(context.Context, string) (data.CompanyFundamentals, error)
}

// Repository isolates orchestration from the package-level database adapter and
// makes partial-provider and no-lookahead behavior deterministic in tests.
type Repository interface {
	Begin(context.Context, store.SwingUniverseVersion) (string, error)
	UpdateMetadata(context.Context, string, store.SwingUniverseVersion) error
	ReplaceMembers(context.Context, string, []store.SwingUniverseMember) error
	Complete(context.Context, string, time.Time, store.SwingUniverseQuality) error
	Fail(context.Context, string, time.Time, store.SwingUniverseQuality, error) error
	TaxonomyAsOf(context.Context, string, time.Time, time.Duration) (*store.CompanyTaxonomyObservation, error)
	InsertTaxonomy(context.Context, store.CompanyTaxonomyObservation) (string, error)
}

type Builder struct {
	Config               config.UniverseConfig
	Assets               data.AssetUniverseProvider
	Snapshots            data.BulkStockSnapshotProvider
	Bars                 data.BulkDailyBarsProvider
	Fundamentals         FundamentalsProvider
	Repository           Repository
	Now                  func() time.Time
	excludedNamePatterns []*regexp.Regexp
}

type RefreshResult struct {
	VersionID           string
	AsOf                time.Time
	MemberCount         int
	EligibleCount       int
	PartialFailureCount int
	TaxonomyRefreshes   int
	Quality             store.SwingUniverseQuality
}

type fundamentalsEvidence struct {
	Name        string  `json:"name,omitempty"`
	MarketCapMM float64 `json:"market_cap_mm,omitempty"`
	Country     string  `json:"country,omitempty"`
	Exchange    string  `json:"exchange,omitempty"`
}

// Refresh builds a current snapshot. It intentionally has no historical as-of
// argument: calling live providers to fill a past date would introduce
// lookahead. Historical callers must select an already-recorded version.
func (b *Builder) Refresh(ctx context.Context) (result RefreshResult, err error) {
	patterns, err := compileExcludedNamePatterns(b.Config.ExcludedNamePatterns)
	if err != nil {
		return result, err
	}
	b.excludedNamePatterns = patterns
	if err := b.ready(); err != nil {
		return result, err
	}
	startedAt := b.now()
	filters, _ := json.Marshal(b.Config)
	versionID, err := b.Repository.Begin(ctx, store.SwingUniverseVersion{
		Provider: b.Config.Provider, AsOf: startedAt, CapturedAt: startedAt,
		Filters: filters, SourceEvidence: json.RawMessage(`{"build_stage":"started"}`),
	})
	if err != nil {
		return result, err
	}
	result.VersionID = versionID
	result.Quality.MinMarketDataCoverage = b.Config.MinMarketDataCoverage
	if result.Quality.MinMarketDataCoverage <= 0 {
		result.Quality.MinMarketDataCoverage = 0.90
	}
	defer func() {
		if err != nil {
			_ = b.Repository.Fail(
				context.Background(), versionID, b.now(), result.Quality, err,
			)
		}
	}()
	assets, err := b.Assets.FetchStockAssets(ctx)
	if err != nil {
		return result, fmt.Errorf("fetch stock asset master: %w", err)
	}
	if len(assets) == 0 {
		return result, errors.New("stock asset master returned no members")
	}
	sort.Slice(assets, func(i, j int) bool {
		left, right := normalizeSymbol(assets[i].Symbol), normalizeSymbol(assets[j].Symbol)
		if left == right {
			return assets[i].AssetID < assets[j].AssetID
		}
		return left < right
	})
	uniqueAssets := assets[:0]
	symbols := make([]string, 0, len(assets))
	seenSymbols := make(map[string]struct{}, len(assets))
	for _, asset := range assets {
		symbol := normalizeSymbol(asset.Symbol)
		if symbol == "" {
			continue
		}
		if _, duplicate := seenSymbols[symbol]; duplicate {
			continue
		}
		seenSymbols[symbol] = struct{}{}
		asset.Symbol = symbol
		uniqueAssets = append(uniqueAssets, asset)
		symbols = append(symbols, symbol)
	}
	assets = uniqueAssets
	result.MemberCount = len(assets)
	if len(assets) == 0 {
		return result, errors.New("stock asset master contained no valid symbols")
	}
	// Do not spend bulk market-data and taxonomy requests on assets that the
	// configured universe would reject from the asset-master filters anyway.
	// The full asset list is still retained for the snapshot audit and member
	// counts; only the evidence workload is narrowed here.
	marketSymbols := make([]string, 0, len(symbols))
	for _, asset := range assets {
		if b.passesAssetMasterFilters(asset) {
			marketSymbols = append(marketSymbols, asset.Symbol)
		}
	}
	result.Quality.EvidenceRequiredCount = len(marketSymbols)

	from := startedAt.AddDate(0, 0, -(b.Config.DollarVolumeLookbackDays*2 + 10))
	var snapshots data.StockSnapshotBatchResult
	var bars data.DailyBarsBatchResult
	var snapshotErr, barsErr error
	fetchSnapshots := func() {
		snapshots, snapshotErr = b.Snapshots.FetchStockSnapshots(ctx, marketSymbols, b.Config.BatchSize)
	}
	fetchBars := func() {
		bars, barsErr = b.Bars.FetchDailyBars(ctx, marketSymbols, from, startedAt, b.Config.BatchSize)
	}
	if b.Config.MaxConcurrency <= 1 {
		fetchSnapshots()
		fetchBars()
	} else {
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			fetchSnapshots()
		}()
		go func() {
			defer wg.Done()
			fetchBars()
		}()
		wg.Wait()
	}
	if err := contextFailure(ctx, snapshotErr, barsErr); err != nil {
		return result, err
	}
	if snapshots.Snapshots == nil {
		snapshots.Snapshots = make(map[string]data.StockSnapshotEvidence)
	}
	if snapshots.Failures == nil {
		snapshots.Failures = make(map[string]error)
	}
	if bars.Bars == nil {
		bars.Bars = make(map[string][]data.Candle)
	}
	if bars.Failures == nil {
		bars.Failures = make(map[string]error)
	}
	for _, symbol := range marketSymbols {
		if snapshotErr != nil && snapshots.Failures[symbol] == nil {
			snapshots.Failures[symbol] = snapshotErr
		}
		if barsErr != nil && bars.Failures[symbol] == nil {
			var batchErr *data.DailyBarsBatchError
			if !errors.As(barsErr, &batchErr) {
				bars.Failures[symbol] = barsErr
			}
		}
	}
	result.Quality = marketDataQuality(
		marketSymbols, snapshots, bars, b.Config.DollarVolumeLookbackDays,
		startedAt, result.Quality.MinMarketDataCoverage,
	)
	if result.Quality.MarketDataCoverage < result.Quality.MinMarketDataCoverage {
		members := make([]store.SwingUniverseMember, 0, len(assets))
		for _, asset := range assets {
			member := b.evaluate(asset, snapshots, bars, nil, nil, startedAt, false)
			member.UniverseVersionID = versionID
			members = append(members, member)
			if member.FailureStage != "" {
				result.PartialFailureCount++
			}
		}
		result.Quality.PartialFailureCount = result.PartialFailureCount
		if replaceErr := b.Repository.ReplaceMembers(ctx, versionID, members); replaceErr != nil {
			return result, replaceErr
		}
		return result, fmt.Errorf(
			"market-data coverage %.2f%% is below configured minimum %.2f%%",
			result.Quality.MarketDataCoverage*100,
			result.Quality.MinMarketDataCoverage*100,
		)
	}

	taxonomy := make(map[string]*store.CompanyTaxonomyObservation, len(symbols))
	taxonomyErrors := make(map[string]error)
	lookupAt := b.now()
	maxTaxonomyAge := time.Duration(b.Config.TaxonomyMaxAgeHours) * time.Hour
	assetsBySymbol := make(map[string]data.StockAsset, len(assets))
	for _, asset := range assets {
		assetsBySymbol[asset.Symbol] = asset
	}
	fundamentalsRequiredSymbols := make([]string, 0, len(marketSymbols))
	type refreshCandidate struct {
		symbol   string
		priority int
	}
	refreshCandidates := make([]refreshCandidate, 0, len(marketSymbols))
	marketSymbolSet := make(map[string]struct{}, len(marketSymbols))
	for _, symbol := range marketSymbols {
		marketSymbolSet[symbol] = struct{}{}
	}
	for _, symbol := range symbols {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		observation, lookupErr := b.Repository.TaxonomyAsOf(ctx, symbol, lookupAt, maxTaxonomyAge)
		if lookupErr != nil {
			return result, fmt.Errorf("load taxonomy for %s: %w", symbol, lookupErr)
		}
		taxonomy[symbol] = observation
		// Cached taxonomy is retained for every audited asset, including ones
		// excluded by asset-master filters. Only eligible assets can consume
		// market evidence or enter the external taxonomy-refresh workload.
		if _, eligibleForEvidence := marketSymbolSet[symbol]; !eligibleForEvidence {
			continue
		}
		if !b.passesNonFundamentalFilters(
			assetsBySymbol[symbol], snapshots, bars, lookupAt,
		) {
			continue
		}
		fundamentalsRequiredSymbols = append(fundamentalsRequiredSymbols, symbol)
		if priority := taxonomyRefreshPriority(observation, lookupAt, maxTaxonomyAge); priority > 0 {
			refreshCandidates = append(refreshCandidates, refreshCandidate{
				symbol: symbol, priority: priority,
			})
		}
	}
	sort.SliceStable(refreshCandidates, func(i, j int) bool {
		if refreshCandidates[i].priority == refreshCandidates[j].priority {
			return refreshCandidates[i].symbol < refreshCandidates[j].symbol
		}
		return refreshCandidates[i].priority < refreshCandidates[j].priority
	})
	refreshes := 0
	for _, candidate := range refreshCandidates {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		switch {
		case b.Fundamentals == nil:
			taxonomyErrors[candidate.symbol] = errors.New("fundamentals provider unavailable")
		case refreshes >= b.Config.TaxonomyRefreshLimit:
			taxonomyErrors[candidate.symbol] = errors.New("taxonomy refresh limit reached")
		default:
			refreshes++
			fundamentals, fetchErr := b.Fundamentals.FetchCompanyFundamentals(
				ctx, candidate.symbol,
			)
			if fetchErr != nil {
				if ctxErr := ctx.Err(); ctxErr != nil {
					return result, ctxErr
				}
				observation := taxonomyUnavailableObservation(
					candidate.symbol,
					fundamentalsProviderName(b.Fundamentals),
					fetchErr,
					b.now(),
				)
				if _, insertErr := b.Repository.InsertTaxonomy(
					ctx, *observation,
				); insertErr != nil {
					return result, fmt.Errorf(
						"persist unavailable taxonomy for %s: %w",
						candidate.symbol, insertErr,
					)
				}
				taxonomy[candidate.symbol] = observation
				taxonomyErrors[candidate.symbol] = fetchErr
				continue
			}
			observation := taxonomyObservation(candidate.symbol, fundamentals, b.now())
			taxonomy[candidate.symbol] = observation
			if _, insertErr := b.Repository.InsertTaxonomy(ctx, *observation); insertErr != nil {
				return result, fmt.Errorf(
					"persist taxonomy for %s: %w", candidate.symbol, insertErr,
				)
			}
		}
	}
	result.Quality.FundamentalsRequiredCount = len(fundamentalsRequiredSymbols)
	for _, symbol := range fundamentalsRequiredSymbols {
		observation := taxonomy[symbol]
		if observation != nil && marketCapFromEvidence(observation.Evidence) > 0 {
			result.Quality.UsableFundamentalsCount++
		} else {
			result.Quality.FundamentalsFailureCount++
			// Fundamentals are required only after coherent market evidence
			// passes, so this symbol is not already in the market-data failure
			// union calculated above.
			result.Quality.ProviderFailureCount++
		}
	}
	if result.Quality.FundamentalsRequiredCount == 0 {
		result.Quality.FundamentalsCoverage = 1
	} else {
		result.Quality.FundamentalsCoverage = float64(
			result.Quality.UsableFundamentalsCount,
		) / float64(result.Quality.FundamentalsRequiredCount)
	}
	var qualityErr error
	if b.Config.MinMarketCapMM > 0 &&
		b.Config.MissingFundamentalsPolicy != "include" &&
		result.Quality.FundamentalsRequiredCount > 0 &&
		result.Quality.UsableFundamentalsCount == 0 {
		qualityErr = errors.New(
			"fundamentals refresh produced no usable market-cap evidence",
		)
	}

	asOf := b.now()
	sourceEvidence, _ := json.Marshal(map[string]any{
		"asset_provider": b.Config.Provider,
		"started_at":     startedAt,
		"snapshot_error": errorString(snapshotErr),
		"bars_error":     errorString(barsErr),
	})
	if updateErr := b.Repository.UpdateMetadata(ctx, versionID, store.SwingUniverseVersion{
		Provider:       b.Config.Provider,
		AsOf:           asOf,
		CapturedAt:     startedAt,
		Filters:        filters,
		SourceEvidence: sourceEvidence,
	}); updateErr != nil {
		return result, updateErr
	}

	members := make([]store.SwingUniverseMember, 0, len(assets))
	for _, asset := range assets {
		symbol := normalizeSymbol(asset.Symbol)
		if symbol == "" {
			continue
		}
		member := b.evaluate(
			asset, snapshots, bars, taxonomy[symbol], taxonomyErrors[symbol], asOf, true,
		)
		member.UniverseVersionID = versionID
		members = append(members, member)
		if member.Eligible {
			result.EligibleCount++
		}
		if member.FailureStage != "" {
			result.PartialFailureCount++
		}
	}
	result.Quality.PartialFailureCount = result.PartialFailureCount
	if err = b.Repository.ReplaceMembers(ctx, versionID, members); err != nil {
		return result, err
	}
	if qualityErr != nil {
		return result, qualityErr
	}
	completedAt := b.now()
	if err = b.Repository.Complete(ctx, versionID, completedAt, result.Quality); err != nil {
		return result, err
	}
	result.AsOf = asOf
	result.MemberCount = len(members)
	result.TaxonomyRefreshes = refreshes
	return result, nil
}

func (b *Builder) passesAssetMasterFilters(asset data.StockAsset) bool {
	securityType := strings.ToLower(asset.AssetClass)
	exchange := strings.ToUpper(asset.Exchange)
	return allowedValue(securityType, b.Config.AllowedSecurityTypes) &&
		allowedValue(exchange, b.Config.AllowedExchanges) &&
		!matchesExcludedAssetName(asset.Name, b.excludedNamePatterns) &&
		(!b.Config.RequireActive || strings.EqualFold(asset.Status, "active")) &&
		(!b.Config.RequireTradable || asset.Tradable)
}

func (b *Builder) passesNonFundamentalFilters(
	asset data.StockAsset,
	snapshots data.StockSnapshotBatchResult,
	bars data.DailyBarsBatchResult,
	decisionAt time.Time,
) bool {
	securityType := strings.ToLower(asset.AssetClass)
	exchange := strings.ToUpper(asset.Exchange)
	if !allowedValue(securityType, b.Config.AllowedSecurityTypes) ||
		!allowedValue(exchange, b.Config.AllowedExchanges) ||
		matchesExcludedAssetName(asset.Name, b.excludedNamePatterns) ||
		(b.Config.RequireActive && !strings.EqualFold(asset.Status, "active")) ||
		(b.Config.RequireTradable && !asset.Tradable) {
		return false
	}
	snapshot, snapshotOK := snapshots.Snapshots[asset.Symbol]
	price := stockSnapshotPrice(snapshot)
	history, barsOK := bars.Bars[asset.Symbol]
	_, volumeOK := usableDailyBarHistory(
		history, b.Config.DollarVolumeLookbackDays, decisionAt,
	)
	available := snapshotOK && price > 0 && barsOK && volumeOK
	if !available && b.Config.RequireMarketData &&
		b.Config.MissingMarketDataPolicy != "include" {
		return false
	}
	if price > 0 && price < b.Config.MinPrice {
		return false
	}
	return true
}

func compileExcludedNamePatterns(patterns []string) ([]*regexp.Regexp, error) {
	compiled := make([]*regexp.Regexp, 0, len(patterns))
	for _, pattern := range patterns {
		if strings.TrimSpace(pattern) == "" {
			return nil, errors.New("scanner.universe.excluded_name_patterns must not contain blank values")
		}
		re, err := regexp.Compile(pattern)
		if err != nil {
			return nil, fmt.Errorf("invalid scanner.universe.excluded_name_patterns pattern %q: %w", pattern, err)
		}
		compiled = append(compiled, re)
	}
	return compiled, nil
}

func matchesExcludedAssetName(name string, patterns []*regexp.Regexp) bool {
	for _, pattern := range patterns {
		if pattern.MatchString(name) {
			return true
		}
	}
	return false
}

func (b *Builder) evaluate(
	asset data.StockAsset,
	snapshots data.StockSnapshotBatchResult,
	bars data.DailyBarsBatchResult,
	taxonomy *store.CompanyTaxonomyObservation,
	taxonomyErr error,
	decisionAt time.Time,
	fundamentalsEvaluated bool,
) store.SwingUniverseMember {
	symbol := normalizeSymbol(asset.Symbol)
	member := store.SwingUniverseMember{
		AssetID:      asset.AssetID,
		Name:         asset.Name,
		Ticker:       symbol,
		SecurityType: strings.ToLower(asset.AssetClass),
		Exchange:     strings.ToUpper(asset.Exchange),
	}
	active := strings.EqualFold(asset.Status, "active")
	tradable := asset.Tradable
	member.Active = &active
	member.Tradable = &tradable
	sourceEvidence, _ := json.Marshal(asset)
	member.SourceEvidence = sourceEvidence

	if taxonomy != nil {
		member.Sector = taxonomy.Sector
		member.Industry = taxonomy.Industry
		member.TaxonomyStatus = taxonomy.Status
		member.TaxonomyAsOf = &taxonomy.ObservedAt
		member.TaxonomyProvider = taxonomy.Provider
		member.TaxonomyEvidence = taxonomy.Evidence
		if capMM := marketCapFromEvidence(taxonomy.Evidence); capMM > 0 {
			member.MarketCapMM = &capMM
		}
	} else {
		member.Sector = store.UnknownTaxonomy
		member.Industry = store.UnknownTaxonomy
		member.TaxonomyStatus = "unknown"
	}

	snapshot, snapshotOK := snapshots.Snapshots[symbol]
	history, barsOK := bars.Bars[symbol]
	price := stockSnapshotPrice(snapshot)
	if price > 0 {
		member.Price = &price
	}
	if snapshot.LatestBidPrice > 0 && snapshot.LatestAskPrice >= snapshot.LatestBidPrice {
		mid := (snapshot.LatestBidPrice + snapshot.LatestAskPrice) / 2
		if mid > 0 {
			spreadBPS := (snapshot.LatestAskPrice - snapshot.LatestBidPrice) / mid * 10_000
			member.SpreadBPS = &spreadBPS
		}
	}
	if barsOK {
		if median, ok := usableDailyBarHistory(
			history, b.Config.DollarVolumeLookbackDays, decisionAt,
		); ok {
			member.DollarVolume = &median
		}
	}
	available := snapshotOK && member.Price != nil && member.DollarVolume != nil
	member.DataAvailable = &available

	var failures []string
	requiresMarketEvidence := b.passesAssetMasterFilters(asset)
	if failure := snapshots.Failures[symbol]; failure != nil {
		failures = append(failures, "snapshot: "+failure.Error())
	} else if requiresMarketEvidence && (!snapshotOK || member.Price == nil) {
		failures = append(failures, "snapshot: usable price evidence unavailable")
	}
	if failure := bars.Failures[symbol]; failure != nil {
		failures = append(failures, "daily_bars: "+failure.Error())
	} else if requiresMarketEvidence && member.DollarVolume == nil {
		failures = append(failures, "daily_bars: completed lookback is unavailable")
	}
	if taxonomyErr != nil {
		failures = append(failures, "fundamentals: "+taxonomyErr.Error())
	} else if fundamentalsEvaluated && requiresMarketEvidence && b.Config.MinMarketCapMM > 0 &&
		member.MarketCapMM == nil {
		failures = append(failures, "fundamentals: market-cap evidence unavailable")
	}
	if len(failures) > 0 {
		member.FailureStage = "provider_evidence"
		member.FailureReason = strings.Join(failures, "; ")
	}

	switch {
	case !allowedValue(member.SecurityType, b.Config.AllowedSecurityTypes):
		member.RemovalStage = "security_type"
	case !allowedValue(member.Exchange, b.Config.AllowedExchanges):
		member.RemovalStage = "exchange"
	case matchesExcludedAssetName(asset.Name, b.excludedNamePatterns):
		member.RemovalStage = "asset_name"
	case b.Config.RequireActive && !strings.EqualFold(asset.Status, "active"):
		member.RemovalStage = "active"
	case b.Config.RequireTradable && !asset.Tradable:
		member.RemovalStage = "tradable"
	case !available && b.Config.RequireMarketData &&
		b.Config.MissingMarketDataPolicy != "include":
		member.RemovalStage = "market_data"
	case member.Price != nil && *member.Price < b.Config.MinPrice:
		member.RemovalStage = "price"
	case fundamentalsEvaluated && b.Config.MinMarketCapMM > 0 && member.MarketCapMM == nil &&
		b.Config.MissingFundamentalsPolicy != "include":
		member.RemovalStage = "fundamentals"
	case member.MarketCapMM != nil && *member.MarketCapMM < b.Config.MinMarketCapMM:
		member.RemovalStage = "market_cap"
	case member.DollarVolume != nil && *member.DollarVolume < b.Config.MinMedianDollarVolume:
		member.RemovalStage = "median_dollar_volume"
	case b.Config.MaxSpreadBPS > 0 && member.SpreadBPS == nil &&
		b.Config.MissingMarketDataPolicy != "include":
		member.RemovalStage = "spread"
	case b.Config.MaxSpreadBPS > 0 && member.SpreadBPS != nil &&
		*member.SpreadBPS > b.Config.MaxSpreadBPS:
		member.RemovalStage = "spread"
	default:
		member.Eligible = true
	}
	return member
}

func (b *Builder) ready() error {
	if !b.Config.Enabled {
		return errors.New("scanner.universe.enabled is false")
	}
	if b.Assets == nil || b.Snapshots == nil || b.Bars == nil || b.Repository == nil {
		return errors.New("universe builder dependencies are incomplete")
	}
	if b.Now == nil {
		b.Now = time.Now
	}
	return nil
}

func (b *Builder) now() time.Time {
	return b.Now().UTC()
}

func taxonomyObservation(
	ticker string, fundamentals data.CompanyFundamentals, observedAt time.Time,
) *store.CompanyTaxonomyObservation {
	evidence, _ := json.Marshal(fundamentalsEvidence{
		Name: fundamentals.Name, MarketCapMM: fundamentals.MarketCapitalization,
		Country: fundamentals.Country, Exchange: fundamentals.Exchange,
	})
	industry := strings.TrimSpace(fundamentals.Industry)
	status := "known"
	if industry == "" {
		industry = store.UnknownTaxonomy
		status = "unknown"
	}
	if fundamentals.Provider == "" {
		fundamentals.Provider = "unknown"
	}
	return &store.CompanyTaxonomyObservation{
		Ticker: ticker, Provider: fundamentals.Provider,
		EffectiveAt: observedAt, ObservedAt: observedAt,
		Sector: store.UnknownTaxonomy, Industry: industry, Status: status,
		Evidence: evidence,
	}
}

func taxonomyUnavailableObservation(
	ticker, provider string,
	cause error,
	observedAt time.Time,
) *store.CompanyTaxonomyObservation {
	if strings.TrimSpace(provider) == "" {
		provider = "unknown"
	}
	evidence, _ := json.Marshal(map[string]string{
		"provider_error": errorString(cause),
	})
	return &store.CompanyTaxonomyObservation{
		Ticker: ticker, Provider: provider,
		EffectiveAt: observedAt, ObservedAt: observedAt,
		Sector: store.UnknownTaxonomy, Industry: store.UnknownTaxonomy,
		Status: "unavailable", Evidence: evidence,
	}
}

func fundamentalsProviderName(provider FundamentalsProvider) string {
	named, ok := provider.(interface{ ProviderName() string })
	if !ok {
		return "unknown"
	}
	return strings.TrimSpace(named.ProviderName())
}

func taxonomyRefreshPriority(
	observation *store.CompanyTaxonomyObservation,
	asOf time.Time,
	retryAfter time.Duration,
) int {
	if observation == nil {
		return 1
	}
	if observation.Status == "stale" {
		return 2
	}
	if observation.Status != "unknown" && observation.Status != "unavailable" {
		return 0
	}
	if retryAfter <= 0 || asOf.Sub(observation.ObservedAt) >= retryAfter {
		return 3
	}
	return 0
}

func marketCapFromEvidence(raw json.RawMessage) float64 {
	var evidence fundamentalsEvidence
	if json.Unmarshal(raw, &evidence) != nil {
		return 0
	}
	return evidence.MarketCapMM
}

func medianDollarVolume(
	bars []data.Candle, lookback int, asOf time.Time,
) (float64, bool) {
	completed := completedDailyBars(bars, asOf)
	if lookback <= 0 || len(completed) < lookback {
		return 0, false
	}
	values := make([]float64, 0, lookback)
	for _, bar := range completed[len(completed)-lookback:] {
		if bar.Close <= 0 || bar.Volume <= 0 {
			return 0, false
		}
		values = append(values, bar.Close*float64(bar.Volume))
	}
	sort.Float64s(values)
	mid := len(values) / 2
	if len(values)%2 == 1 {
		return values[mid], true
	}
	return (values[mid-1] + values[mid]) / 2, true
}

func completedDailyBars(bars []data.Candle, asOf time.Time) []data.Candle {
	location, err := time.LoadLocation("America/New_York")
	if err != nil {
		// Conservative fallback: only prior UTC dates are unambiguously
		// complete. Production binaries embed tzdata.
		cutoff := time.Date(asOf.UTC().Year(), asOf.UTC().Month(), asOf.UTC().Day(), 0, 0, 0, 0, time.UTC)
		completed := make([]data.Candle, 0, len(bars))
		for _, bar := range bars {
			if bar.Timestamp.Before(cutoff) {
				completed = append(completed, bar)
			}
		}
		return completed
	}
	decision := asOf.In(location)
	decisionDate := time.Date(
		decision.Year(), decision.Month(), decision.Day(), 0, 0, 0, 0, location,
	)
	marketClosed := decision.Hour() >= 16
	if session, ok := engine.MarketSessionForDate(decision); ok {
		marketClosed = !decision.Before(session.Close)
	}
	completed := make([]data.Candle, 0, len(bars))
	for _, bar := range bars {
		session := bar.Timestamp.In(location)
		sessionDate := time.Date(
			session.Year(), session.Month(), session.Day(), 0, 0, 0, 0, location,
		)
		if sessionDate.Before(decisionDate) ||
			(sessionDate.Equal(decisionDate) && marketClosed) {
			completed = append(completed, bar)
		}
	}
	return completed
}

func marketDataQuality(
	symbols []string,
	snapshots data.StockSnapshotBatchResult,
	bars data.DailyBarsBatchResult,
	lookback int,
	asOf time.Time,
	minimum float64,
) store.SwingUniverseQuality {
	quality := store.SwingUniverseQuality{
		EvidenceRequiredCount: len(symbols), MinMarketDataCoverage: minimum,
	}
	failedSymbols := make(map[string]struct{})
	for _, symbol := range symbols {
		snapshot, snapshotOK := snapshots.Snapshots[symbol]
		snapshotUsable := snapshotOK && stockSnapshotPrice(snapshot) > 0
		if snapshotUsable {
			quality.UsableSnapshotCount++
		} else {
			quality.SnapshotFailureCount++
			failedSymbols[symbol] = struct{}{}
		}
		barsUsable := false
		if history, barsOK := bars.Bars[symbol]; barsOK {
			if _, usable := usableDailyBarHistory(history, lookback, asOf); usable {
				barsUsable = true
			}
		}
		if barsUsable {
			quality.UsableBarHistoryCount++
		} else {
			quality.BarFailureCount++
			failedSymbols[symbol] = struct{}{}
		}
		if snapshotUsable && barsUsable {
			quality.CoherentMarketDataCount++
		}
	}
	quality.ProviderFailureCount = len(failedSymbols)
	if len(symbols) == 0 {
		quality.MarketDataCoverage = 1
	} else {
		quality.MarketDataCoverage = float64(
			quality.CoherentMarketDataCount,
		) / float64(len(symbols))
	}
	return quality
}

func usableDailyBarHistory(
	bars []data.Candle, lookback int, asOf time.Time,
) (float64, bool) {
	median, usable := medianDollarVolume(bars, lookback, asOf)
	return median, usable && hasLatestCompletedSessionBar(bars, asOf)
}

func hasLatestCompletedSessionBar(bars []data.Candle, asOf time.Time) bool {
	completed := completedDailyBars(bars, asOf)
	if len(completed) == 0 {
		return false
	}
	target := engine.MostRecentCompletedMarketSession(asOf, 0)
	latest := completed[0].Timestamp
	for _, bar := range completed[1:] {
		if bar.Timestamp.After(latest) {
			latest = bar.Timestamp
		}
	}
	return latest.In(target.Date.Location()).Format("2006-01-02") ==
		target.Date.Format("2006-01-02")
}

func stockSnapshotPrice(snapshot data.StockSnapshotEvidence) float64 {
	if snapshot.LatestTradePrice > 0 {
		return snapshot.LatestTradePrice
	}
	if snapshot.DailyBar != nil && snapshot.DailyBar.Close > 0 {
		return snapshot.DailyBar.Close
	}
	return 0
}

func hasFundamentalsEvidence(
	symbols []string,
	taxonomy map[string]*store.CompanyTaxonomyObservation,
) bool {
	for _, symbol := range symbols {
		observation := taxonomy[symbol]
		if observation != nil && marketCapFromEvidence(observation.Evidence) > 0 {
			return true
		}
	}
	return false
}

func allowedValue(value string, allowed []string) bool {
	for _, candidate := range allowed {
		if strings.EqualFold(strings.TrimSpace(value), strings.TrimSpace(candidate)) {
			return true
		}
	}
	return false
}

func normalizeSymbol(symbol string) string {
	return strings.ToUpper(strings.TrimSpace(symbol))
}

func contextFailure(ctx context.Context, errs ...error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	for _, err := range errs {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
	}
	return nil
}

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
