package data

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

const (
	defaultAlpacaTradingURL = "https://paper-api.alpaca.markets"
	alpacaProviderName      = "alpaca"
	// Delayed SIP snapshots trail the real-time feed by 15 minutes. The extra
	// minute keeps after-close requests outside the boundary despite clock skew.
	AlpacaDelayedSIPSnapshotDelay = 16 * time.Minute
)

var errAlpacaSymbolMissing = errors.New("alpaca response omitted symbol")
var errAlpacaSymbolInvalidFormat = errors.New("alpaca symbol has invalid format")

// AlpacaUniverseClient adds trading-host asset discovery and bulk market-data
// capabilities to an AlpacaClient. Wrapping the existing client intentionally
// shares its authentication, retry policy, and rate limiter.
type AlpacaUniverseClient struct {
	client             *AlpacaClient
	tradingURL         string
	snapshotFeed       string
	historicalFeed     string
	sipRecentDataDelay time.Duration
	now                func() time.Time
}

type AlpacaUniverseFeeds struct {
	Snapshots          string
	Historical         string
	SIPRecentDataDelay time.Duration
}

func NewAlpacaUniverseClient(
	client *AlpacaClient, tradingURL string, feeds AlpacaUniverseFeeds,
) *AlpacaUniverseClient {
	if tradingURL == "" {
		tradingURL = defaultAlpacaTradingURL
	}
	return &AlpacaUniverseClient{
		client:             client,
		tradingURL:         strings.TrimRight(tradingURL, "/"),
		snapshotFeed:       strings.ToLower(strings.TrimSpace(feeds.Snapshots)),
		historicalFeed:     strings.ToLower(strings.TrimSpace(feeds.Historical)),
		sipRecentDataDelay: max(feeds.SIPRecentDataDelay, 0),
		now:                time.Now,
	}
}

type alpacaAsset struct {
	ID           string   `json:"id"`
	Class        string   `json:"class"`
	Exchange     string   `json:"exchange"`
	Symbol       string   `json:"symbol"`
	Name         string   `json:"name"`
	Status       string   `json:"status"`
	Tradable     bool     `json:"tradable"`
	Marginable   bool     `json:"marginable"`
	Shortable    bool     `json:"shortable"`
	Fractionable bool     `json:"fractionable"`
	Attributes   []string `json:"attributes"`
}

// FetchStockAssets returns Alpaca's complete US-equity asset master, sorted and
// deduplicated by normalized symbol. Active/tradable status is evidence for the
// caller's configurable filter, so this provider must not pre-filter it.
func (c *AlpacaUniverseClient) FetchStockAssets(ctx context.Context) ([]StockAsset, error) {
	if err := c.readyClient(); err != nil {
		return nil, err
	}
	params := url.Values{}
	params.Set("asset_class", "us_equity")
	resp, err := c.client.getFrom(ctx, c.tradingURL, "/v2/assets", params)
	if err != nil {
		return nil, fmt.Errorf("fetch alpaca stock assets: %w", err)
	}
	defer resp.Body.Close()

	var payload []alpacaAsset
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("decode alpaca stock assets: %w", err)
	}
	sort.Slice(payload, func(i, j int) bool {
		left, right := normalizeStockSymbol(payload[i].Symbol), normalizeStockSymbol(payload[j].Symbol)
		if left == right {
			leftActive := payload[i].Status == "active"
			rightActive := payload[j].Status == "active"
			if leftActive != rightActive {
				return leftActive
			}
			if payload[i].Tradable != payload[j].Tradable {
				return payload[i].Tradable
			}
			return payload[i].ID < payload[j].ID
		}
		return left < right
	})

	observedAt := c.now().UTC()
	assets := make([]StockAsset, 0, len(payload))
	seen := make(map[string]struct{}, len(payload))
	for _, raw := range payload {
		symbol := normalizeStockSymbol(raw.Symbol)
		if symbol == "" || raw.Class != "us_equity" {
			continue
		}
		if _, duplicate := seen[symbol]; duplicate {
			continue
		}
		seen[symbol] = struct{}{}
		attributes := append([]string(nil), raw.Attributes...)
		sort.Strings(attributes)
		assets = append(assets, StockAsset{
			AssetID:      raw.ID,
			Symbol:       symbol,
			Name:         strings.TrimSpace(raw.Name),
			Status:       raw.Status,
			AssetClass:   raw.Class,
			Exchange:     strings.ToUpper(strings.TrimSpace(raw.Exchange)),
			Tradable:     raw.Tradable,
			Marginable:   raw.Marginable,
			Shortable:    raw.Shortable,
			Fractionable: raw.Fractionable,
			Attributes:   attributes,
			Provider:     alpacaProviderName,
			ObservedAt:   observedAt,
		})
	}
	return assets, nil
}

type alpacaBulkBarsResponse struct {
	Bars          map[string][]alpacaBar `json:"bars"`
	NextPageToken string                 `json:"next_page_token"`
}

// FetchStockSnapshots fetches snapshot evidence from the configured feed in
// deterministic, caller-bounded batches. Non-context batch failures are
// attributed to every symbol in that batch and later batches continue.
func (c *AlpacaUniverseClient) FetchStockSnapshots(
	ctx context.Context, symbols []string, batchSize int,
) (StockSnapshotBatchResult, error) {
	result := StockSnapshotBatchResult{
		Snapshots: make(map[string]StockSnapshotEvidence),
		Failures:  make(map[string]error),
	}
	if err := c.readySnapshots(); err != nil {
		return result, err
	}
	normalized, err := normalizedSymbolList(symbols, batchSize)
	if err != nil {
		return result, err
	}
	normalized, invalid := requestableAlpacaSymbols(normalized)
	mergeFailures(result.Failures, invalid)
	for start := 0; start < len(normalized); start += batchSize {
		if err := ctx.Err(); err != nil {
			markSymbolFailures(result.Failures, normalized[start:], err)
			return result, err
		}
		end := min(start+batchSize, len(normalized))
		batch := normalized[start:end]
		snapshots, failures, fetchErr := c.fetchStockSnapshotsBatch(ctx, batch)
		for symbol, snapshot := range snapshots {
			result.Snapshots[symbol] = snapshot
		}
		for symbol, failure := range failures {
			result.Failures[symbol] = failure
		}
		if fetchErr != nil {
			markSymbolFailures(result.Failures, batch, fetchErr)
			markSymbolFailures(result.Failures, normalized[end:], fetchErr)
			return result, fetchErr
		}
	}
	return result, nil
}

func (c *AlpacaUniverseClient) fetchStockSnapshotsBatch(
	ctx context.Context, batch []string,
) (map[string]StockSnapshotEvidence, map[string]error, error) {
	snapshots := make(map[string]StockSnapshotEvidence)
	failures := make(map[string]error)
	params := url.Values{}
	params.Set("symbols", strings.Join(batch, ","))
	params.Set("feed", c.snapshotFeed)
	resp, fetchErr := c.client.get(ctx, "/v2/stocks/snapshots", params)
	if fetchErr != nil {
		if ctx.Err() != nil {
			return snapshots, failures, ctx.Err()
		}
		if shouldIsolateInvalidSymbolBatch(fetchErr) && len(batch) > 1 {
			left, right := splitSymbolBatch(batch)
			leftSnapshots, leftFailures, err := c.fetchStockSnapshotsBatch(ctx, left)
			if err != nil {
				return snapshots, failures, err
			}
			rightSnapshots, rightFailures, err := c.fetchStockSnapshotsBatch(ctx, right)
			if err != nil {
				return snapshots, failures, err
			}
			mergeSnapshots(snapshots, leftSnapshots)
			mergeSnapshots(snapshots, rightSnapshots)
			mergeFailures(failures, leftFailures)
			mergeFailures(failures, rightFailures)
			return snapshots, failures, nil
		}
		markSymbolFailures(failures, batch, fetchErr)
		return snapshots, failures, nil
	}
	var payload map[string]alpacaSnapshot
	decodeErr := json.NewDecoder(resp.Body).Decode(&payload)
	resp.Body.Close()
	if decodeErr != nil {
		markSymbolFailures(failures, batch, fmt.Errorf("decode alpaca snapshots: %w", decodeErr))
		return snapshots, failures, nil
	}
	observedAt := c.now().UTC()
	for _, symbol := range batch {
		raw, ok := snapshotForSymbol(payload, symbol)
		if !ok {
			failures[symbol] = errAlpacaSymbolMissing
			continue
		}
		evidence := stockSnapshotEvidence(symbol, raw, observedAt)
		evidence.Provider = alpacaProviderName + ":" + c.snapshotFeed
		snapshots[symbol] = evidence
	}
	return snapshots, failures, nil
}

// FetchDailyBars retrieves split-adjusted daily bars from the configured feed
// using Alpaca's multi-symbol endpoint. Pagination is completed independently
// for each batch; a failed batch never makes a partial page look complete.
func (c *AlpacaUniverseClient) FetchDailyBars(
	ctx context.Context,
	symbols []string,
	from, to time.Time,
	batchSize int,
) (DailyBarsBatchResult, error) {
	result := DailyBarsBatchResult{
		Bars:     make(map[string][]Candle),
		Failures: make(map[string]error),
	}
	if err := c.readyHistorical(); err != nil {
		return result, err
	}
	if c.historicalFeed == "sip" && c.sipRecentDataDelay > 0 {
		entitlementCutoff := c.now().UTC().Add(-c.sipRecentDataDelay)
		if to.After(entitlementCutoff) {
			to = entitlementCutoff
		}
	}
	if !to.After(from) {
		return result, fmt.Errorf("daily bars end must be after start")
	}
	normalized, err := normalizedSymbolList(symbols, batchSize)
	if err != nil {
		return result, err
	}
	normalized, invalid := requestableAlpacaSymbols(normalized)
	mergeFailures(result.Failures, invalid)
	for start := 0; start < len(normalized); start += batchSize {
		if err := ctx.Err(); err != nil {
			markSymbolFailures(result.Failures, normalized[start:], err)
			return result, err
		}
		end := min(start+batchSize, len(normalized))
		batch := normalized[start:end]
		batchBars, batchFailures, fetchErr := c.fetchDailyBarsIsolated(ctx, batch, from, to)
		for symbol, bars := range batchBars {
			result.Bars[symbol] = bars
		}
		for symbol, failure := range batchFailures {
			result.Failures[symbol] = failure
		}
		if fetchErr != nil {
			if ctx.Err() != nil {
				markSymbolFailures(result.Failures, normalized[start:], ctx.Err())
				return result, ctx.Err()
			}
			// The current and all later batches were not successfully
			// retrieved; preserve that denominator for the universe audit.
			markSymbolFailures(result.Failures, normalized[start:], fetchErr)
			return result, &DailyBarsBatchError{
				Symbols: append([]string(nil), batch...), Err: fetchErr,
			}
		}
		for _, symbol := range batch {
			if _, failed := batchFailures[symbol]; failed {
				continue
			}
			bars := batchBars[symbol]
			if len(bars) == 0 {
				result.Failures[symbol] = errAlpacaSymbolMissing
				continue
			}
			result.Bars[symbol] = bars
		}
	}
	return result, nil
}

func (c *AlpacaUniverseClient) fetchDailyBarsIsolated(
	ctx context.Context, batch []string, from, to time.Time,
) (map[string][]Candle, map[string]error, error) {
	failures := make(map[string]error)
	batchBars, fetchErr := c.fetchDailyBarsBatch(ctx, batch, from, to)
	if fetchErr == nil {
		return batchBars, failures, nil
	}
	if ctx.Err() != nil {
		return batchBars, failures, ctx.Err()
	}
	if !shouldIsolateInvalidSymbolBatch(fetchErr) {
		return batchBars, failures, fetchErr
	}
	if len(batch) == 1 {
		failures[batch[0]] = fetchErr
		return batchBars, failures, nil
	}
	left, right := splitSymbolBatch(batch)
	leftBars, leftFailures, err := c.fetchDailyBarsIsolated(ctx, left, from, to)
	if err != nil {
		return batchBars, failures, err
	}
	rightBars, rightFailures, err := c.fetchDailyBarsIsolated(ctx, right, from, to)
	if err != nil {
		return batchBars, failures, err
	}
	if leftBars == nil {
		leftBars = make(map[string][]Candle)
	}
	mergeBars(leftBars, rightBars)
	mergeFailures(leftFailures, rightFailures)
	return leftBars, leftFailures, nil
}

func (c *AlpacaUniverseClient) fetchDailyBarsBatch(
	ctx context.Context, batch []string, from, to time.Time,
) (map[string][]Candle, error) {
	collected := make(map[string][]Candle, len(batch))
	nextPageToken := ""
	for {
		params := url.Values{}
		params.Set("symbols", strings.Join(batch, ","))
		params.Set("timeframe", "1Day")
		params.Set("start", from.UTC().Format(time.RFC3339))
		params.Set("end", to.UTC().Format(time.RFC3339))
		params.Set("limit", "10000")
		params.Set("adjustment", alpacaStockAdjustment)
		params.Set("feed", c.historicalFeed)
		params.Set("sort", "asc")
		if nextPageToken != "" {
			params.Set("page_token", nextPageToken)
		}
		resp, err := c.client.get(ctx, "/v2/stocks/bars", params)
		if err != nil {
			return nil, fmt.Errorf("fetch alpaca daily bars: %w", err)
		}
		var payload alpacaBulkBarsResponse
		decodeErr := json.NewDecoder(resp.Body).Decode(&payload)
		resp.Body.Close()
		if decodeErr != nil {
			return nil, fmt.Errorf("decode alpaca daily bars: %w", decodeErr)
		}
		for rawSymbol, rawBars := range payload.Bars {
			symbol := normalizeStockSymbol(rawSymbol)
			for _, rawBar := range rawBars {
				if candle, ok := alpacaCandle(symbol, Timeframe1Day, rawBar); ok {
					collected[symbol] = append(collected[symbol], candle)
				}
			}
		}
		if payload.NextPageToken == "" {
			break
		}
		nextPageToken = payload.NextPageToken
	}
	for symbol, bars := range collected {
		collected[symbol] = dedupeAndSortCandles(bars)
	}
	return collected, nil
}

func (c *AlpacaUniverseClient) readyClient() error {
	if c == nil || c.client == nil {
		return fmt.Errorf("alpaca universe client is nil")
	}
	return nil
}

func (c *AlpacaUniverseClient) readySnapshots() error {
	if err := c.readyClient(); err != nil {
		return err
	}
	if c.snapshotFeed == "" {
		return fmt.Errorf("alpaca universe snapshot feed is required")
	}
	return nil
}

func (c *AlpacaUniverseClient) readyHistorical() error {
	if err := c.readyClient(); err != nil {
		return err
	}
	if c.historicalFeed == "" {
		return fmt.Errorf("alpaca universe historical feed is required")
	}
	return nil
}

func normalizedSymbolList(symbols []string, batchSize int) ([]string, error) {
	if batchSize <= 0 {
		return nil, fmt.Errorf("batch size must be positive")
	}
	seen := make(map[string]struct{}, len(symbols))
	normalized := make([]string, 0, len(symbols))
	for _, raw := range symbols {
		symbol := normalizeStockSymbol(raw)
		if symbol == "" {
			continue
		}
		if _, ok := seen[symbol]; ok {
			continue
		}
		seen[symbol] = struct{}{}
		normalized = append(normalized, symbol)
	}
	sort.Strings(normalized)
	return normalized, nil
}

// requestableAlpacaSymbols quarantines asset-master identifiers that cannot be
// sent to Alpaca's stock endpoints. The provider's equity-symbol contract is
// ASCII alphanumeric text with optional dots/hyphens and a maximum length of
// eight characters. Quarantined symbols remain explicit failures so the
// universe audit retains the original denominator.
func requestableAlpacaSymbols(symbols []string) ([]string, map[string]error) {
	requestable := make([]string, 0, len(symbols))
	failures := make(map[string]error)
	for _, symbol := range symbols {
		if !validAlpacaSymbol(symbol) {
			failures[symbol] = errAlpacaSymbolInvalidFormat
			continue
		}
		requestable = append(requestable, symbol)
	}
	return requestable, failures
}

func validAlpacaSymbol(symbol string) bool {
	if symbol == "" || len(symbol) > 8 {
		return false
	}
	for _, char := range symbol {
		if (char < 'A' || char > 'Z') &&
			(char < '0' || char > '9') && char != '.' && char != '-' {
			return false
		}
	}
	return true
}

func normalizeStockSymbol(symbol string) string {
	return strings.ToUpper(strings.TrimSpace(symbol))
}

func markSymbolFailures(failures map[string]error, symbols []string, err error) {
	for _, symbol := range symbols {
		if _, alreadySet := failures[symbol]; !alreadySet {
			failures[symbol] = err
		}
	}
}

func shouldIsolateInvalidSymbolBatch(err error) bool {
	// Alpaca rejects an entire bulk request when any member is malformed. Only
	// its explicit client-side invalid-symbol responses are safe to bisect;
	// auth, rate-limit, and transient provider failures must remain batch errors.
	var apiErr *httpError
	if !errors.As(err, &apiErr) || (apiErr.status != http.StatusBadRequest && apiErr.status != http.StatusUnprocessableEntity) {
		return false
	}
	return strings.Contains(strings.ToLower(apiErr.msg), "invalid symbol")
}

func splitSymbolBatch(batch []string) ([]string, []string) {
	mid := len(batch) / 2
	return batch[:mid], batch[mid:]
}

func mergeSnapshots(dst, src map[string]StockSnapshotEvidence) {
	for symbol, snapshot := range src {
		dst[symbol] = snapshot
	}
}

func mergeFailures(dst, src map[string]error) {
	for symbol, failure := range src {
		if _, exists := dst[symbol]; !exists {
			dst[symbol] = failure
		}
	}
}

func mergeBars(dst, src map[string][]Candle) {
	if dst == nil {
		return
	}
	for symbol, bars := range src {
		dst[symbol] = bars
	}
}

func snapshotForSymbol(payload map[string]alpacaSnapshot, symbol string) (alpacaSnapshot, bool) {
	if snap, ok := payload[symbol]; ok {
		return snap, true
	}
	for rawSymbol, snap := range payload {
		if normalizeStockSymbol(rawSymbol) == symbol {
			return snap, true
		}
	}
	return alpacaSnapshot{}, false
}

func stockSnapshotEvidence(symbol string, raw alpacaSnapshot, observedAt time.Time) StockSnapshotEvidence {
	evidence := StockSnapshotEvidence{
		Symbol:     symbol,
		Provider:   alpacaProviderName,
		ObservedAt: observedAt,
	}
	if candle, ok := alpacaCandle(symbol, Timeframe1Min, derefAlpacaBar(raw.MinuteBar)); ok && raw.MinuteBar != nil {
		evidence.MinuteBar = &candle
	}
	if candle, ok := alpacaCandle(symbol, Timeframe1Day, derefAlpacaBar(raw.DailyBar)); ok && raw.DailyBar != nil {
		evidence.DailyBar = &candle
	}
	if candle, ok := alpacaCandle(symbol, Timeframe1Day, derefAlpacaBar(raw.PreviousDailyBar)); ok && raw.PreviousDailyBar != nil {
		evidence.PreviousDailyBar = &candle
	}
	if raw.LatestTrade != nil {
		evidence.LatestTradePrice = raw.LatestTrade.Price
		evidence.LatestTradeAt, _ = time.Parse(time.RFC3339Nano, raw.LatestTrade.Timestamp)
	}
	if raw.LatestQuote != nil {
		evidence.LatestBidPrice = raw.LatestQuote.BidPrice
		evidence.LatestAskPrice = raw.LatestQuote.AskPrice
		evidence.LatestQuoteAt, _ = time.Parse(time.RFC3339Nano, raw.LatestQuote.Timestamp)
	}
	return evidence
}

func derefAlpacaBar(bar *alpacaBar) alpacaBar {
	if bar == nil {
		return alpacaBar{}
	}
	return *bar
}

func alpacaCandle(symbol string, timeframe Timeframe, raw alpacaBar) (Candle, bool) {
	ts, err := time.Parse(time.RFC3339Nano, raw.Timestamp)
	if err != nil {
		return Candle{}, false
	}
	return Candle{
		Ticker: symbol, Timeframe: timeframe, Timestamp: ts,
		Open: raw.Open, High: raw.High, Low: raw.Low, Close: raw.Close,
		Volume: raw.Volume, VWAP: raw.VWAP,
	}, true
}

func dedupeAndSortCandles(bars []Candle) []Candle {
	sort.SliceStable(bars, func(i, j int) bool {
		return bars[i].Timestamp.Before(bars[j].Timestamp)
	})
	out := bars[:0]
	for _, bar := range bars {
		if len(out) > 0 && out[len(out)-1].Timestamp.Equal(bar.Timestamp) {
			out[len(out)-1] = bar
			continue
		}
		out = append(out, bar)
	}
	return out
}
