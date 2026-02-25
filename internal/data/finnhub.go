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

const finnhubBaseURL = "https://finnhub.io/api/v1"

// FinnhubClient implements NewsProvider using the Finnhub company news API.
// Free tier: 60 API calls/minute.
type FinnhubClient struct {
	apiKey     string
	httpClient *http.Client

	// Simple rate limiter: min 1s between requests on free tier.
	mu          sync.Mutex
	lastRequest time.Time
	minInterval time.Duration
}

// NewFinnhubClient creates a new Finnhub news client.
func NewFinnhubClient(apiKey string) *FinnhubClient {
	return &FinnhubClient{
		apiKey:      apiKey,
		httpClient:  &http.Client{Timeout: 15 * time.Second},
		minInterval: time.Second, // 60 req/min → ~1s between requests (conservative)
	}
}

// throttle enforces the per-client rate limit. Returns ctx.Err() if the context
// is cancelled during the wait rather than blocking until the interval elapses.
func (c *FinnhubClient) throttle(ctx context.Context) error {
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

// finnhubArticle is the raw JSON article shape from Finnhub's /company-news endpoint.
type finnhubArticle struct {
	ID       int64   `json:"id"`
	Category string  `json:"category"`
	DateTime int64   `json:"datetime"` // Unix timestamp
	Headline string  `json:"headline"`
	Source   string  `json:"source"`
	Summary  string  `json:"summary"`
	URL      string  `json:"url"`
	Related  string  `json:"related"` // comma-separated tickers
	Image    string  `json:"image"`
	Sentiment struct {
		BullishPercent float64 `json:"bullishPercent"`
		BearishPercent float64 `json:"bearishPercent"`
	} `json:"sentiment"`
}

// FetchNews retrieves company news from Finnhub for a single ticker published after 'since'.
// Finnhub returns articles in reverse-chronological order.
func (c *FinnhubClient) FetchNews(ctx context.Context, ticker string, since time.Time) ([]NewsItem, error) {
	if err := c.throttle(ctx); err != nil {
		return nil, err
	}

	now := time.Now()
	params := url.Values{}
	params.Set("symbol", ticker)
	params.Set("from", since.Format("2006-01-02"))
	params.Set("to", now.Format("2006-01-02"))
	params.Set("token", c.apiKey)

	reqURL := finnhubBaseURL + "/company-news?" + params.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("finnhub request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("finnhub get: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == 429 {
		return nil, fmt.Errorf("finnhub rate limit exceeded")
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("finnhub API error %d for %s", resp.StatusCode, ticker)
	}

	var articles []finnhubArticle
	if err := json.NewDecoder(resp.Body).Decode(&articles); err != nil {
		return nil, fmt.Errorf("finnhub decode: %w", err)
	}

	items := make([]NewsItem, 0, len(articles))
	for _, a := range articles {
		if a.Headline == "" || a.URL == "" {
			continue
		}
		publishedAt := time.Unix(a.DateTime, 0).UTC()
		if publishedAt.Before(since) {
			continue
		}

		// Compute a simple sentiment score: bullish% - bearish%, normalised to [-1, 1].
		sentiment := (a.Sentiment.BullishPercent - a.Sentiment.BearishPercent) / 100.0

		items = append(items, NewsItem{
			ExternalID:     fmt.Sprintf("finnhub-%d", a.ID),
			Headline:       a.Headline,
			Summary:        a.Summary,
			Source:         a.Source,
			URL:            a.URL,
			PublishedAt:    publishedAt,
			Tickers:        []string{ticker},
			SentimentScore: sentiment,
		})
	}
	return items, nil
}
