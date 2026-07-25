package data

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
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
	ID        int64  `json:"id"`
	Category  string `json:"category"`
	DateTime  int64  `json:"datetime"` // Unix timestamp
	Headline  string `json:"headline"`
	Source    string `json:"source"`
	Summary   string `json:"summary"`
	URL       string `json:"url"`
	Related   string `json:"related"` // comma-separated tickers
	Image     string `json:"image"`
	Sentiment struct {
		BullishPercent float64 `json:"bullishPercent"`
		BearishPercent float64 `json:"bearishPercent"`
	} `json:"sentiment"`
}

// finnhubProfile is the raw JSON shape from Finnhub's /stock/profile2 endpoint.
type finnhubProfile struct {
	Name                 string  `json:"name"`
	MarketCapitalization float64 `json:"marketCapitalization"` // USD millions
}

// FetchCompanyProfile returns the company's full name for a ticker via
// Finnhub's /stock/profile2 endpoint (available on the free tier).
// Finnhub answers unknown symbols with an empty JSON object and HTTP 200
// rather than a 404, so "no profile" is returned as ("", nil) — only
// transport/API failures produce an error.
func (c *FinnhubClient) FetchCompanyProfile(ctx context.Context, ticker string) (string, error) {
	p, err := c.FetchCompanyFundamentals(ctx, ticker)
	return p.Name, err
}

// CompanyFundamentals contains the free profile fields used by deterministic
// universe filters. Finnhub reports MarketCapitalization in USD millions.
type CompanyFundamentals struct {
	Name                 string
	MarketCapitalization float64
}

// FetchCompanyFundamentals returns company identity and market capitalization
// from Finnhub's free /stock/profile2 endpoint.
func (c *FinnhubClient) FetchCompanyFundamentals(ctx context.Context, ticker string) (CompanyFundamentals, error) {
	if err := c.throttle(ctx); err != nil {
		return CompanyFundamentals{}, err
	}

	params := url.Values{}
	params.Set("symbol", ticker)
	params.Set("token", c.apiKey)

	reqURL := finnhubBaseURL + "/stock/profile2?" + params.Encode()
	resp, err := withRetry(ctx, func() (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
		if err != nil {
			return nil, err
		}
		resp, err := c.httpClient.Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode >= 400 {
			retryAfter, _ := parseRetryAfterHeader(resp.Header.Get("Retry-After"), time.Now())
			resp.Body.Close()
			return nil, &httpError{
				status:     resp.StatusCode,
				msg:        fmt.Sprintf("finnhub profile API error %d for %s", resp.StatusCode, ticker),
				retryAfter: retryAfter,
			}
		}
		return resp, nil
	})
	if err != nil {
		return CompanyFundamentals{}, fmt.Errorf("finnhub profile get: %w", err)
	}
	defer resp.Body.Close()

	var p finnhubProfile
	if err := json.NewDecoder(resp.Body).Decode(&p); err != nil {
		return CompanyFundamentals{}, fmt.Errorf("finnhub profile decode: %w", err)
	}
	return CompanyFundamentals{
		Name:                 strings.TrimSpace(p.Name),
		MarketCapitalization: p.MarketCapitalization,
	}, nil
}

// EarningsRelease is one scheduled/historical earnings date from Finnhub's calendar.
type EarningsRelease struct {
	Symbol string
	Date   time.Time // announcement day (parsed from the "YYYY-MM-DD" date field)
	Hour   string    // 'bmo' | 'amc' | 'dmh' | '' (before/after market, during, unknown)
}

// finnhubEarningsResp is the raw shape of /calendar/earnings.
type finnhubEarningsResp struct {
	EarningsCalendar []struct {
		Symbol string `json:"symbol"`
		Date   string `json:"date"` // "2026-08-25"
		Hour   string `json:"hour"` // "bmo" | "amc" | "dmh" | ""
	} `json:"earningsCalendar"`
}

// FetchEarningsCalendar returns earnings releases for symbol within [from, to].
//
// FREE-TIER LIMIT: Finnhub serves ~1 month of history plus ALL upcoming dates, so a
// historical (backtest) window returns an empty list rather than an error — seed those
// dates separately (they're public record). Live use only needs the next upcoming report,
// which is always covered. Malformed date strings are skipped, not fatal.
func (c *FinnhubClient) FetchEarningsCalendar(ctx context.Context, symbol string, from, to time.Time) ([]EarningsRelease, error) {
	if err := c.throttle(ctx); err != nil {
		return nil, err
	}

	params := url.Values{}
	params.Set("symbol", symbol)
	params.Set("from", from.Format("2006-01-02"))
	params.Set("to", to.Format("2006-01-02"))
	params.Set("token", c.apiKey)

	reqURL := finnhubBaseURL + "/calendar/earnings?" + params.Encode()
	resp, err := withRetry(ctx, func() (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
		if err != nil {
			return nil, err
		}
		resp, err := c.httpClient.Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode >= 400 {
			retryAfter, _ := parseRetryAfterHeader(resp.Header.Get("Retry-After"), time.Now())
			resp.Body.Close()
			return nil, &httpError{
				status:     resp.StatusCode,
				msg:        fmt.Sprintf("finnhub earnings API error %d for %s", resp.StatusCode, symbol),
				retryAfter: retryAfter,
			}
		}
		return resp, nil
	})
	if err != nil {
		return nil, fmt.Errorf("finnhub earnings get: %w", err)
	}
	defer resp.Body.Close()

	var raw finnhubEarningsResp
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("finnhub earnings decode: %w", err)
	}

	out := make([]EarningsRelease, 0, len(raw.EarningsCalendar))
	for _, e := range raw.EarningsCalendar {
		d, perr := time.Parse("2006-01-02", e.Date)
		if perr != nil {
			continue // skip an unparseable date rather than fail the whole sync
		}
		out = append(out, EarningsRelease{Symbol: e.Symbol, Date: d, Hour: e.Hour})
	}
	return out, nil
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
	resp, err := withRetry(ctx, func() (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
		if err != nil {
			return nil, err
		}
		resp, err := c.httpClient.Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode >= 400 {
			retryAfter, _ := parseRetryAfterHeader(resp.Header.Get("Retry-After"), time.Now())
			resp.Body.Close()
			return nil, &httpError{
				status:     resp.StatusCode,
				msg:        fmt.Sprintf("finnhub API error %d for %s", resp.StatusCode, ticker),
				retryAfter: retryAfter,
			}
		}
		return resp, nil
	})
	if err != nil {
		return nil, fmt.Errorf("finnhub get: %w", err)
	}
	defer resp.Body.Close()

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
