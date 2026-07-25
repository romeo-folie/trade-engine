package data

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// alpacaTimeframes maps our internal Timeframe constants to Alpaca's API format.
var alpacaTimeframes = map[Timeframe]string{
	Timeframe1Min:  "1Min",
	Timeframe5Min:  "5Min",
	Timeframe15Min: "15Min",
	Timeframe1Hour: "1Hour",
	Timeframe4Hour: "4Hour",
	Timeframe1Day:  "1Day",
}

// alpacaStockAdjustment keeps pre/post-split history on the same price scale
// as the current quote. Raw history fabricates price collapses at split dates
// and corrupts every rolling indicator that spans the event.
const alpacaStockAdjustment = "split"

// AlpacaClient implements MarketDataProvider using the Alpaca REST market data API.
// Alpaca free tier: 200 requests/minute.
type AlpacaClient struct {
	apiKey  string
	secret  string
	dataURL string // e.g. https://data.alpaca.markets

	// rate limiter: 200 req/min → 1 req per 300ms
	mu          sync.Mutex
	lastRequest time.Time
	minInterval time.Duration

	httpClient *http.Client
}

// NewAlpacaClient creates a new Alpaca market data client.
// dataURL defaults to https://data.alpaca.markets if empty.
func NewAlpacaClient(apiKey, secret, dataURL string) *AlpacaClient {
	if dataURL == "" {
		dataURL = "https://data.alpaca.markets"
	}
	return &AlpacaClient{
		apiKey:      apiKey,
		secret:      secret,
		dataURL:     dataURL,
		minInterval: 300 * time.Millisecond,
		httpClient:  &http.Client{Timeout: 30 * time.Second},
	}
}

// ── Rate limiter ─────────────────────────────────────────────────────────────

// throttle enforces the per-client rate limit. It returns ctx.Err() immediately
// if the context is cancelled while waiting, so callers can honour graceful shutdown
// without being blocked for up to minInterval by a plain time.Sleep.
func (c *AlpacaClient) throttle(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	since := time.Since(c.lastRequest)
	if since < c.minInterval {
		remaining := c.minInterval - since
		select {
		case <-time.After(remaining):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	c.lastRequest = time.Now()
	return nil
}

// ── HTTP helper ───────────────────────────────────────────────────────────────

func (c *AlpacaClient) get(ctx context.Context, path string, params url.Values) (*http.Response, error) {
	return c.getFrom(ctx, c.dataURL, path, params)
}

// getFrom is get against an arbitrary base host (the data API by default, or the
// trading API for the historical-options client), sharing the throttle, auth headers,
// retry/backoff, and error envelope so callers don't re-implement them.
func (c *AlpacaClient) getFrom(ctx context.Context, baseURL, path string, params url.Values) (*http.Response, error) {
	if err := c.throttle(ctx); err != nil {
		return nil, err
	}
	u := baseURL + path
	if len(params) > 0 {
		u += "?" + params.Encode()
	}
	return withRetry(ctx, func() (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("APCA-API-KEY-ID", c.apiKey)
		req.Header.Set("APCA-API-SECRET-KEY", c.secret)
		req.Header.Set("Accept", "application/json")
		resp, err := c.httpClient.Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode >= 400 {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
			retryAfter, _ := parseRetryAfterHeader(resp.Header.Get("Retry-After"), time.Now())
			resp.Body.Close()
			return nil, &httpError{
				status:     resp.StatusCode,
				msg:        fmt.Sprintf("alpaca API %s %d: %s", path, resp.StatusCode, bytes.TrimSpace(body)),
				retryAfter: retryAfter,
			}
		}
		return resp, nil
	})
}

// ── Alpaca response types ─────────────────────────────────────────────────────

type alpacaBar struct {
	Timestamp string  `json:"t"`
	Open      float64 `json:"o"`
	High      float64 `json:"h"`
	Low       float64 `json:"l"`
	Close     float64 `json:"c"`
	Volume    int64   `json:"v"`
	VWAP      float64 `json:"vw"`
}

type alpacaBarsResponse struct {
	Bars          []alpacaBar `json:"bars"`
	Symbol        string      `json:"symbol"`
	NextPageToken string      `json:"next_page_token"`
}

type alpacaQuote struct {
	Timestamp string  `json:"t"`
	BidPrice  float64 `json:"bp"`
	AskPrice  float64 `json:"ap"`
}

type alpacaQuoteResponse struct {
	Quote  alpacaQuote `json:"quote"`
	Symbol string      `json:"symbol"`
}

type alpacaTrade struct {
	Timestamp string  `json:"t"`
	Price     float64 `json:"p"`
	Size      int64   `json:"s"`
}

type alpacaTradesResponse struct {
	Trades        []alpacaTrade `json:"trades"`
	Symbol        string        `json:"symbol"`
	NextPageToken string        `json:"next_page_token"`
}

// ── MarketDataProvider implementation ────────────────────────────────────────

// FetchCandles retrieves OHLCV bars for the given ticker, timeframe, and date range.
// Handles pagination automatically (Alpaca returns up to 1000 bars per request).
func (c *AlpacaClient) FetchCandles(ctx context.Context, ticker string, tf Timeframe, from, to time.Time) ([]Candle, error) {
	alpacaTF, ok := alpacaTimeframes[tf]
	if !ok {
		return nil, fmt.Errorf("unsupported timeframe: %s", tf)
	}

	var all []Candle
	var nextPageToken string

	for {
		params := url.Values{}
		params.Set("timeframe", alpacaTF)
		params.Set("start", from.UTC().Format(time.RFC3339))
		params.Set("end", to.UTC().Format(time.RFC3339))
		params.Set("limit", "1000")
		params.Set("adjustment", alpacaStockAdjustment)
		params.Set("feed", "iex")
		if nextPageToken != "" {
			params.Set("page_token", nextPageToken)
		}

		resp, err := c.get(ctx, fmt.Sprintf("/v2/stocks/%s/bars", ticker), params)
		if err != nil {
			return nil, fmt.Errorf("fetch candles %s: %w", ticker, err)
		}

		var data alpacaBarsResponse
		if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
			resp.Body.Close()
			return nil, fmt.Errorf("decode bars response: %w", err)
		}
		resp.Body.Close()

		for _, bar := range data.Bars {
			ts, err := time.Parse(time.RFC3339, bar.Timestamp)
			if err != nil {
				continue
			}
			all = append(all, Candle{
				Ticker:    ticker,
				Timeframe: tf,
				Timestamp: ts,
				Open:      bar.Open,
				High:      bar.High,
				Low:       bar.Low,
				Close:     bar.Close,
				Volume:    bar.Volume,
				VWAP:      bar.VWAP,
			})
		}

		if data.NextPageToken == "" {
			break
		}
		nextPageToken = data.NextPageToken
	}

	return all, nil
}

// FetchHistory retrieves the most recent N candles for a ticker.
func (c *AlpacaClient) FetchHistory(ctx context.Context, ticker string, tf Timeframe, limit int) ([]Candle, error) {
	alpacaTF, ok := alpacaTimeframes[tf]
	if !ok {
		return nil, fmt.Errorf("unsupported timeframe: %s", tf)
	}

	params := url.Values{}
	params.Set("timeframe", alpacaTF)
	params.Set("limit", fmt.Sprintf("%d", limit))
	params.Set("adjustment", alpacaStockAdjustment)
	params.Set("feed", "iex")

	resp, err := c.get(ctx, fmt.Sprintf("/v2/stocks/%s/bars", ticker), params)
	if err != nil {
		return nil, fmt.Errorf("fetch history %s: %w", ticker, err)
	}
	defer resp.Body.Close()

	var data alpacaBarsResponse
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, fmt.Errorf("decode bars response: %w", err)
	}

	candles := make([]Candle, 0, len(data.Bars))
	for _, bar := range data.Bars {
		ts, err := time.Parse(time.RFC3339, bar.Timestamp)
		if err != nil {
			continue
		}
		candles = append(candles, Candle{
			Ticker:    ticker,
			Timeframe: tf,
			Timestamp: ts,
			Open:      bar.Open,
			High:      bar.High,
			Low:       bar.Low,
			Close:     bar.Close,
			Volume:    bar.Volume,
			VWAP:      bar.VWAP,
		})
	}
	return candles, nil
}

// FetchQuote retrieves the latest bid/ask quote for a ticker.
// Last price is computed as (bid + ask) / 2.
func (c *AlpacaClient) FetchQuote(ctx context.Context, ticker string) (*Quote, error) {
	resp, err := c.get(ctx, fmt.Sprintf("/v2/stocks/%s/quotes/latest", ticker), nil)
	if err != nil {
		return nil, fmt.Errorf("fetch quote %s: %w", ticker, err)
	}
	defer resp.Body.Close()

	var data alpacaQuoteResponse
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, fmt.Errorf("decode quote response: %w", err)
	}

	ts, _ := time.Parse(time.RFC3339Nano, data.Quote.Timestamp)
	mid := (data.Quote.BidPrice + data.Quote.AskPrice) / 2.0

	return &Quote{
		Ticker:    ticker,
		Bid:       data.Quote.BidPrice,
		Ask:       data.Quote.AskPrice,
		Last:      mid,
		Timestamp: ts,
	}, nil
}

// FetchTrades returns exact IEX trade prints in [from, to]. Unlike one-minute
// OHLC, these timestamps can distinguish a qualifying pre-signal flush from a
// genuine post-signal stop breach in the same minute.
func (c *AlpacaClient) FetchTrades(
	ctx context.Context, ticker string, from, to time.Time,
) ([]MarketTrade, error) {
	var all []MarketTrade
	var nextPageToken string
	for {
		params := url.Values{}
		params.Set("start", from.UTC().Format(time.RFC3339Nano))
		params.Set("end", to.UTC().Format(time.RFC3339Nano))
		params.Set("limit", "10000")
		params.Set("feed", "iex")
		params.Set("sort", "asc")
		if nextPageToken != "" {
			params.Set("page_token", nextPageToken)
		}

		resp, err := c.get(ctx, fmt.Sprintf("/v2/stocks/%s/trades", ticker), params)
		if err != nil {
			return nil, fmt.Errorf("fetch trades %s: %w", ticker, err)
		}
		var payload alpacaTradesResponse
		if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
			resp.Body.Close()
			return nil, fmt.Errorf("decode trades response: %w", err)
		}
		resp.Body.Close()

		for _, trade := range payload.Trades {
			ts, err := time.Parse(time.RFC3339Nano, trade.Timestamp)
			if err != nil || ts.Before(from) || ts.After(to) || trade.Price <= 0 {
				continue
			}
			all = append(all, MarketTrade{
				Ticker: ticker, Price: trade.Price, Size: trade.Size, Timestamp: ts,
			})
		}
		if payload.NextPageToken == "" {
			break
		}
		nextPageToken = payload.NextPageToken
	}
	return all, nil
}
