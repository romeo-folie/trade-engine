package data

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

const kalshiAPIBase = "https://trading-api.kalshi.com/trade-api/v2"

// KalshiClient implements PredictionDataProvider for Kalshi.
// Public market-data endpoints do not require authentication.
// An API key can optionally be set for higher rate limits.
type KalshiClient struct {
	httpClient *http.Client
	apiKey     string // optional; leave empty for unauthenticated reads
}

// NewKalshiClient creates a KalshiClient.  Pass an empty apiKey for read-only
// unauthenticated access (rate limits apply).
func NewKalshiClient(apiKey string) *KalshiClient {
	return &KalshiClient{
		httpClient: &http.Client{Timeout: 15 * time.Second},
		apiKey:     apiKey,
	}
}

// ── PredictionDataProvider ────────────────────────────────────────────────────

// FetchContracts retrieves open Kalshi markets matching the given categories,
// filtered by minimum 24h volume, limited to maxContracts results.
func (c *KalshiClient) FetchContracts(ctx context.Context, categories []string, minVolume float64, maxContracts int) ([]Contract, error) {
	var all []kalshiMarket

	for _, cat := range categories {
		markets, err := c.fetchByCategory(ctx, cat)
		if err != nil {
			return nil, fmt.Errorf("kalshi fetchContracts(%s): %w", cat, err)
		}
		all = append(all, markets...)
	}

	// Deduplicate by ticker.
	seen := make(map[string]struct{})
	var unique []kalshiMarket
	for _, m := range all {
		if _, ok := seen[m.Ticker]; !ok {
			seen[m.Ticker] = struct{}{}
			// Apply minimum volume filter.
			if float64(m.Volume24h) >= minVolume {
				unique = append(unique, m)
			}
		}
	}

	// Sort by 24h volume descending.
	sort.Slice(unique, func(i, j int) bool {
		return unique[i].Volume24h > unique[j].Volume24h
	})
	if len(unique) > maxContracts {
		unique = unique[:maxContracts]
	}

	contracts := make([]Contract, 0, len(unique))
	for _, m := range unique {
		contracts = append(contracts, m.toContract())
	}
	return contracts, nil
}

// FetchSnapshot retrieves the current state of a single Kalshi market by ticker.
func (c *KalshiClient) FetchSnapshot(ctx context.Context, contractID string) (*ContractSnapshot, error) {
	endpoint := fmt.Sprintf("%s/markets/%s", kalshiAPIBase, url.PathEscape(contractID))

	var resp kalshiMarketResponse
	if err := c.get(ctx, endpoint, &resp); err != nil {
		return nil, fmt.Errorf("kalshi FetchSnapshot(%s): %w", contractID, err)
	}
	s := resp.Market.toSnapshot()
	return &s, nil
}

// ── Internal helpers ──────────────────────────────────────────────────────────

func (c *KalshiClient) fetchByCategory(ctx context.Context, category string) ([]kalshiMarket, error) {
	var all []kalshiMarket
	cursor := ""
	for {
		params := url.Values{}
		params.Set("status", "open")
		params.Set("category", kalshiCategorySlug(category))
		params.Set("limit", "200")
		if cursor != "" {
			params.Set("cursor", cursor)
		}

		endpoint := kalshiAPIBase + "/markets?" + params.Encode()
		var page kalshiMarketsPage
		if err := c.get(ctx, endpoint, &page); err != nil {
			return nil, err
		}
		all = append(all, page.Markets...)
		if page.Cursor == "" || len(page.Markets) == 0 {
			break
		}
		cursor = page.Cursor
	}
	return all, nil
}

func (c *KalshiClient) get(ctx context.Context, endpoint string, out interface{}) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(string(body), 200))
	}
	return json.Unmarshal(body, out)
}

// ── Kalshi API response types ─────────────────────────────────────────────────

type kalshiMarketsPage struct {
	Markets []kalshiMarket `json:"markets"`
	Cursor  string         `json:"cursor"`
}

type kalshiMarketResponse struct {
	Market kalshiMarket `json:"market"`
}

// kalshiMarket represents a Kalshi market object from the API.
// Prices (YesBid, YesAsk, NoBid, NoAsk, LastPrice) are in cents (0–99).
type kalshiMarket struct {
	Ticker       string `json:"ticker"`
	EventTicker  string `json:"event_ticker"`
	Title        string `json:"title"`
	Subtitle     string `json:"subtitle"`
	Category     string `json:"category"`
	Status       string `json:"status"`
	YesBid       int    `json:"yes_bid"`
	YesAsk       int    `json:"yes_ask"`
	NoBid        int    `json:"no_bid"`
	NoAsk        int    `json:"no_ask"`
	LastPrice    int    `json:"last_price"`
	Volume       int    `json:"volume"`
	Volume24h    int    `json:"volume_24h"`
	OpenInterest int    `json:"open_interest"`
	CloseTime    string `json:"close_time"`
}

// centToProb converts a Kalshi price in cents (0–99) to a probability (0–1).
func centToProb(cents int) float64 { return float64(cents) / 100.0 }

func (m kalshiMarket) deadline() time.Time {
	if m.CloseTime == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05Z"} {
		if t, err := time.Parse(layout, m.CloseTime); err == nil {
			return t
		}
	}
	return time.Time{}
}

func (m kalshiMarket) toContract() Contract {
	yes := centToProb(m.YesAsk+m.YesBid) / 2.0 // mid
	no := centToProb(m.NoAsk+m.NoBid) / 2.0
	return Contract{
		ID:                 m.Ticker,
		Platform:           KalshiPlatform,
		Title:              m.Title,
		Categories:         []string{m.Category},
		Price:              yes,
		YesPrice:           yes,
		NoPrice:            no,
		Volume24h:          float64(m.Volume24h),
		OpenInterest:       float64(m.OpenInterest),
		Bid:                centToProb(m.YesBid),
		Ask:                centToProb(m.YesAsk),
		ResolutionDeadline: m.deadline(),
		Resolved:           m.Status == "settled" || m.Status == "closed",
	}
}

func (m kalshiMarket) toSnapshot() ContractSnapshot {
	yes := centToProb(m.YesAsk+m.YesBid) / 2.0
	no := centToProb(m.NoAsk+m.NoBid) / 2.0
	return ContractSnapshot{
		Platform:           KalshiPlatform,
		ContractID:         m.Ticker,
		ContractTitle:      m.Title,
		Price:              yes,
		YesPrice:           yes,
		NoPrice:            no,
		Volume24h:          float64(m.Volume24h),
		OpenInterest:       float64(m.OpenInterest),
		Bid:                centToProb(m.YesBid),
		Ask:                centToProb(m.YesAsk),
		ResolutionDeadline: m.deadline(),
		Resolved:           m.Status == "settled" || m.Status == "closed",
		Categories:         []string{m.Category},
		CreatedAt:          time.Now().UTC(),
	}
}

// KalshiPlatform is the platform identifier used in watchlist / snapshots.
const KalshiPlatform = "kalshi"

// kalshiCategorySlug normalises a category name to Kalshi's expected format.
// Kalshi uses title-cased category names (e.g. "Finance", "Politics").
func kalshiCategorySlug(category string) string {
	known := map[string]string{
		"politics":      "Politics",
		"finance":       "Finance",
		"crypto":        "Crypto",
		"sports":        "Sports",
		"economics":     "Economics",
		"science":       "Science",
		"climate":       "Climate",
		"entertainment": "Entertainment",
		"us":            "US",
		"world":         "World",
	}
	if v, ok := known[strings.ToLower(category)]; ok {
		return v
	}
	// Title-case the category as a fallback.
	if len(category) == 0 {
		return category
	}
	return strings.ToUpper(category[:1]) + strings.ToLower(category[1:])
}

