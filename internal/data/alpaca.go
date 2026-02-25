package data

import (
	"context"
	"encoding/json"
	"fmt"
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
	Timeframe1Day:  "1Day",
}

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

func (c *AlpacaClient) throttle() {
	c.mu.Lock()
	defer c.mu.Unlock()
	since := time.Since(c.lastRequest)
	if since < c.minInterval {
		time.Sleep(c.minInterval - since)
	}
	c.lastRequest = time.Now()
}

// ── HTTP helper ───────────────────────────────────────────────────────────────

func (c *AlpacaClient) get(ctx context.Context, path string, params url.Values) (*http.Response, error) {
	c.throttle()
	u := c.dataURL + path
	if len(params) > 0 {
		u += "?" + params.Encode()
	}
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
		resp.Body.Close()
		return nil, fmt.Errorf("alpaca API %s %d", path, resp.StatusCode)
	}
	return resp, nil
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
		params.Set("adjustment", "raw")
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
	params.Set("adjustment", "raw")
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
