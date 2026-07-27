package data

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"
)

// ErrPremarketEntitlement identifies a provider response that cannot supply
// consolidated real-time premarket evidence. Callers must surface this instead
// of silently downgrading to a partial feed.
var ErrPremarketEntitlement = errors.New("consolidated premarket data entitlement unavailable")

// PremarketBar retains the trade count omitted from the general Candle type.
type PremarketBar struct {
	Candle
	TradeCount int64
}

// PremarketEvidence is the complete provider-neutral input for one checkpoint.
// HistoricalVolumes contain prior-session volume accumulated through the same
// exchange-clock minute, which prevents early checkpoints from being compared
// with full-day volume.
type PremarketEvidence struct {
	Symbol            string
	PreviousClose     float64
	Quote             Quote
	Bars              []PremarketBar
	HistoricalVolumes []int64
	DailyBars         []Candle
	Feed              string
	ObservedAt        time.Time
}

type PremarketBatchResult struct {
	Evidence map[string]PremarketEvidence
	Failures map[string]error
}

type alpacaQuotesResponse struct {
	Quotes        []alpacaQuote `json:"quotes"`
	NextPageToken string        `json:"next_page_token"`
}

// PremarketDataProvider supplies consolidated snapshots and bounded detailed
// evidence. Implementations must not substitute another feed.
type PremarketDataProvider interface {
	FetchPremarketSnapshots(
		ctx context.Context, symbols []string, batchSize int, feed string,
	) (StockSnapshotBatchResult, error)
	FetchPremarketEvidence(
		ctx context.Context, symbols []string, sessionDate, checkpoint time.Time,
		historySessions, batchSize int, feed string,
	) (PremarketBatchResult, error)
}

// AlpacaPremarketClient wraps the shared authenticated/rate-limited client.
type AlpacaPremarketClient struct {
	client *AlpacaClient
	now    func() time.Time
}

func NewAlpacaPremarketClient(client *AlpacaClient) *AlpacaPremarketClient {
	return &AlpacaPremarketClient{client: client, now: time.Now}
}

func (c *AlpacaPremarketClient) FetchPremarketSnapshots(
	ctx context.Context, symbols []string, batchSize int, feed string,
) (StockSnapshotBatchResult, error) {
	result := StockSnapshotBatchResult{
		Snapshots: make(map[string]StockSnapshotEvidence),
		Failures:  make(map[string]error),
	}
	if err := c.ready(feed); err != nil {
		return result, err
	}
	normalized, err := normalizedSymbolList(symbols, batchSize)
	if err != nil {
		return result, err
	}
	for start := 0; start < len(normalized); start += batchSize {
		end := min(start+batchSize, len(normalized))
		batch := normalized[start:end]
		params := url.Values{"symbols": {strings.Join(batch, ",")}, "feed": {feed}}
		resp, fetchErr := c.client.get(ctx, "/v2/stocks/snapshots", params)
		if fetchErr != nil {
			if isEntitlementError(fetchErr) {
				return result, fmt.Errorf("%w: Alpaca %s snapshots: %v",
					ErrPremarketEntitlement, feed, fetchErr)
			}
			markSymbolFailures(result.Failures, batch, fetchErr)
			if ctx.Err() != nil {
				return result, ctx.Err()
			}
			continue
		}
		var payload map[string]alpacaSnapshot
		decodeErr := json.NewDecoder(resp.Body).Decode(&payload)
		resp.Body.Close()
		if decodeErr != nil {
			markSymbolFailures(result.Failures, batch, decodeErr)
			continue
		}
		observedAt := c.now().UTC()
		for _, symbol := range batch {
			raw, ok := snapshotForSymbol(payload, symbol)
			if !ok {
				result.Failures[symbol] = errAlpacaSymbolMissing
				continue
			}
			evidence := stockSnapshotEvidence(symbol, raw, observedAt)
			evidence.Provider = alpacaProviderName + ":" + feed
			result.Snapshots[symbol] = evidence
		}
	}
	return result, nil
}

func (c *AlpacaPremarketClient) FetchPremarketEvidence(
	ctx context.Context, symbols []string, sessionDate, checkpoint time.Time,
	historySessions, batchSize int, feed string,
) (PremarketBatchResult, error) {
	result := PremarketBatchResult{
		Evidence: make(map[string]PremarketEvidence),
		Failures: make(map[string]error),
	}
	if err := c.ready(feed); err != nil {
		return result, err
	}
	normalized, err := normalizedSymbolList(symbols, batchSize)
	if err != nil {
		return result, err
	}
	if historySessions <= 0 {
		return result, fmt.Errorf("history sessions must be positive")
	}
	// Fetch enough calendar time to cover weekends and ordinary exchange
	// holidays. The evaluator still requires the exact requested session count.
	from := sessionDate.AddDate(0, 0, -(historySessions*2 + 10))
	for _, symbol := range normalized {
		evidence, fetchErr := c.fetchSymbolEvidence(
			ctx, symbol, sessionDate, checkpoint, from, historySessions, feed,
		)
		if fetchErr != nil {
			if isEntitlementError(fetchErr) {
				return result, fmt.Errorf("%w: Alpaca %s bars: %v",
					ErrPremarketEntitlement, feed, fetchErr)
			}
			result.Failures[symbol] = fetchErr
			if ctx.Err() != nil {
				return result, ctx.Err()
			}
			continue
		}
		result.Evidence[symbol] = evidence
	}
	return result, nil
}

func (c *AlpacaPremarketClient) fetchSymbolEvidence(
	ctx context.Context, symbol string, sessionDate, checkpoint, from time.Time,
	historySessions int, feed string,
) (PremarketEvidence, error) {
	minuteBars, err := c.fetchBars(ctx, symbol, "1Min", from, checkpoint, feed)
	if err != nil {
		return PremarketEvidence{}, err
	}
	rawDailyBars, err := c.fetchBars(ctx, symbol, "1Day",
		from.AddDate(0, 0, -30), sessionDate, feed)
	if err != nil {
		return PremarketEvidence{}, err
	}
	current, historical := splitPremarketBars(
		minuteBars, sessionDate, checkpoint, historySessions,
	)
	if len(rawDailyBars) == 0 {
		return PremarketEvidence{}, errors.New("previous daily close unavailable")
	}
	previousClose := 0.0
	dailyBars := make([]Candle, 0, len(rawDailyBars))
	for _, bar := range rawDailyBars {
		if bar.Timestamp.Before(sessionDate) {
			previousClose = bar.Close
			dailyBars = append(dailyBars, bar.Candle)
		}
	}
	if previousClose <= 0 {
		return PremarketEvidence{}, errors.New("previous daily close unavailable")
	}
	quote, err := c.fetchQuoteAt(ctx, symbol, checkpoint, feed)
	if err != nil {
		return PremarketEvidence{}, err
	}
	return PremarketEvidence{
		Symbol: symbol, PreviousClose: previousClose, Quote: quote,
		Bars: current, HistoricalVolumes: historical,
		DailyBars: dailyBars, Feed: feed, ObservedAt: c.now().UTC(),
	}, nil
}

func (c *AlpacaPremarketClient) fetchBars(
	ctx context.Context, symbol, timeframe string, from, to time.Time, feed string,
) ([]PremarketBar, error) {
	var out []PremarketBar
	token := ""
	for {
		params := url.Values{
			"timeframe":  {timeframe},
			"start":      {from.UTC().Format(time.RFC3339Nano)},
			"end":        {to.UTC().Format(time.RFC3339Nano)},
			"limit":      {"10000"},
			"adjustment": {alpacaStockAdjustment},
			"feed":       {feed},
			"sort":       {"asc"},
		}
		if token != "" {
			params.Set("page_token", token)
		}
		resp, err := c.client.get(ctx, fmt.Sprintf("/v2/stocks/%s/bars", symbol), params)
		if err != nil {
			return nil, err
		}
		var payload alpacaBarsResponse
		decodeErr := json.NewDecoder(resp.Body).Decode(&payload)
		resp.Body.Close()
		if decodeErr != nil {
			return nil, decodeErr
		}
		tf := Timeframe1Min
		if timeframe == "1Day" {
			tf = Timeframe1Day
		}
		for _, raw := range payload.Bars {
			candle, ok := alpacaCandle(symbol, tf, raw)
			if ok {
				out = append(out, PremarketBar{Candle: candle, TradeCount: raw.TradeCount})
			}
		}
		if payload.NextPageToken == "" {
			break
		}
		token = payload.NextPageToken
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Timestamp.Before(out[j].Timestamp)
	})
	return out, nil
}

func (c *AlpacaPremarketClient) fetchQuoteAt(
	ctx context.Context, symbol string, checkpoint time.Time, feed string,
) (Quote, error) {
	params := url.Values{
		"start": {checkpoint.Add(-8 * time.Hour).UTC().Format(time.RFC3339Nano)},
		"end":   {checkpoint.UTC().Format(time.RFC3339Nano)},
		"limit": {"1"},
		"feed":  {feed},
		"sort":  {"desc"},
	}
	resp, err := c.client.get(ctx, fmt.Sprintf("/v2/stocks/%s/quotes", symbol), params)
	if err != nil {
		return Quote{}, err
	}
	defer resp.Body.Close()
	var payload alpacaQuotesResponse
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return Quote{}, err
	}
	if len(payload.Quotes) == 0 {
		return Quote{}, errors.New("consolidated quote unavailable at checkpoint")
	}
	raw := payload.Quotes[0]
	ts, err := time.Parse(time.RFC3339Nano, raw.Timestamp)
	if err != nil {
		return Quote{}, fmt.Errorf("invalid quote timestamp: %w", err)
	}
	return Quote{
		Ticker: symbol, Bid: raw.BidPrice, Ask: raw.AskPrice,
		Last:      (raw.BidPrice + raw.AskPrice) / 2,
		Timestamp: ts,
	}, nil
}

func splitPremarketBars(
	bars []PremarketBar, sessionDate, checkpoint time.Time, historySessions int,
) ([]PremarketBar, []int64) {
	loc := checkpoint.Location()
	targetY, targetM, targetD := sessionDate.In(loc).Date()
	current := make([]PremarketBar, 0)
	volumeByDate := make(map[string]int64)
	for _, bar := range bars {
		local := bar.Timestamp.In(loc)
		hour, minute, _ := local.Clock()
		if hour < 4 || hour > checkpoint.Hour() ||
			(hour == checkpoint.Hour() && minute >= checkpoint.Minute()) ||
			hour >= 9 && (hour > 9 || minute >= 30) {
			continue
		}
		y, m, d := local.Date()
		if y == targetY && m == targetM && d == targetD {
			current = append(current, bar)
			continue
		}
		volumeByDate[local.Format("2006-01-02")] += bar.Volume
	}
	keys := make([]string, 0, len(volumeByDate))
	for key := range volumeByDate {
		keys = append(keys, key)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(keys)))
	if len(keys) > historySessions {
		keys = keys[:historySessions]
	}
	historical := make([]int64, 0, len(keys))
	for _, key := range keys {
		historical = append(historical, volumeByDate[key])
	}
	return current, historical
}

func (c *AlpacaPremarketClient) ready(feed string) error {
	if c == nil || c.client == nil {
		return errors.New("alpaca premarket client is nil")
	}
	if strings.ToLower(strings.TrimSpace(feed)) != "sip" {
		return fmt.Errorf("%w: feed %q is not consolidated", ErrPremarketEntitlement, feed)
	}
	return nil
}

func isEntitlementError(err error) bool {
	var apiErr *httpError
	return errors.As(err, &apiErr) && (apiErr.status == 401 || apiErr.status == 403)
}
