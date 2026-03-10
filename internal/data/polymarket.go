package data

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

const polymarketGammaBase = "https://gamma-api.polymarket.com"

// PolymarketClient implements PredictionDataProvider for Polymarket.
// Market-data endpoints are public; no API key is required for reading.
type PolymarketClient struct {
	httpClient *http.Client
}

// NewPolymarketClient creates a PolymarketClient with a sensible timeout.
func NewPolymarketClient() *PolymarketClient {
	return &PolymarketClient{
		httpClient: &http.Client{Timeout: 15 * time.Second},
	}
}

// ── PredictionDataProvider ────────────────────────────────────────────────────

// FetchContracts retrieves open Polymarket markets matching the given categories
// (mapped to tag slugs), filtered by minimum 24h volume, limited to maxContracts.
func (c *PolymarketClient) FetchContracts(ctx context.Context, categories []string, minVolume float64, maxContracts int) ([]Contract, error) {
	var all []polymarketMarket

	for _, cat := range categories {
		markets, err := c.fetchMarketsByTag(ctx, cat, minVolume, maxContracts*2)
		if err != nil {
			return nil, fmt.Errorf("polymarket fetchContracts(%s): %w", cat, err)
		}
		all = append(all, markets...)
	}

	// Deduplicate by market ID.
	seen := make(map[string]struct{})
	var unique []polymarketMarket
	for _, m := range all {
		if _, ok := seen[m.ID]; !ok {
			seen[m.ID] = struct{}{}
			unique = append(unique, m)
		}
	}

	// Sort by 24h volume descending, take top maxContracts.
	sort.Slice(unique, func(i, j int) bool {
		return unique[i].Volume24hr > unique[j].Volume24hr
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

// FetchSnapshot retrieves the current state of a single Polymarket market.
func (c *PolymarketClient) FetchSnapshot(ctx context.Context, contractID string) (*ContractSnapshot, error) {
	endpoint := fmt.Sprintf("%s/markets/%s", polymarketGammaBase, url.PathEscape(contractID))
	var m polymarketMarket
	if err := c.get(ctx, endpoint, &m); err != nil {
		return nil, fmt.Errorf("polymarket FetchSnapshot(%s): %w", contractID, err)
	}
	s := m.toSnapshot()
	return &s, nil
}

// ── Internal helpers ──────────────────────────────────────────────────────────

func (c *PolymarketClient) fetchMarketsByTag(ctx context.Context, tagSlug string, minVolume float64, limit int) ([]polymarketMarket, error) {
	params := url.Values{}
	params.Set("closed", "false")
	params.Set("archived", "false")
	params.Set("tag_slug", tagSlug)
	params.Set("volume_num_min", strconv.FormatFloat(minVolume, 'f', 0, 64))
	params.Set("limit", strconv.Itoa(limit))

	endpoint := polymarketGammaBase + "/markets?" + params.Encode()

	var markets []polymarketMarket
	if err := c.get(ctx, endpoint, &markets); err != nil {
		return nil, err
	}
	return markets, nil
}

func (c *PolymarketClient) get(ctx context.Context, endpoint string, out interface{}) error {
	resp, err := withRetry(ctx, func() (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", "application/json")
		r, err := c.httpClient.Do(req)
		if err != nil {
			return nil, err
		}
		if r.StatusCode >= 500 && r.StatusCode <= 504 {
			body, _ := io.ReadAll(io.LimitReader(r.Body, 512))
			r.Body.Close()
			return nil, &httpError{status: r.StatusCode, msg: fmt.Sprintf("polymarket HTTP %d: %s", r.StatusCode, truncate(string(body), 200))}
		}
		return r, nil
	})
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("polymarket HTTP %d: %s", resp.StatusCode, truncate(string(body), 200))
	}
	return json.Unmarshal(body, out)
}

// ── Polymarket API response types ─────────────────────────────────────────────

type polymarketTag struct {
	Label string `json:"label"`
	Slug  string `json:"slug"`
}

type polymarketMarket struct {
	ID            string          `json:"id"`
	Question      string          `json:"question"`
	Slug          string          `json:"slug"`
	EndDate       string          `json:"endDate"`
	OutcomePrices string          `json:"outcomePrices"` // JSON array string: "[\"0.65\",\"0.35\"]"
	Volume        string          `json:"volume"`
	Volume24hr    float64         `json:"volume24hr"`
	Liquidity     string          `json:"liquidity"`
	Spread        float64         `json:"spread"`
	Active        bool            `json:"active"`
	Closed        bool            `json:"closed"`
	Tags          []polymarketTag `json:"tags"`
}

// prices unpacks the JSON-encoded outcomePrices string into [yes, no].
func (m polymarketMarket) prices() (yes, no float64) {
	var arr []string
	if err := json.Unmarshal([]byte(m.OutcomePrices), &arr); err != nil || len(arr) < 2 {
		return 0, 0
	}
	y, _ := strconv.ParseFloat(arr[0], 64)
	n, _ := strconv.ParseFloat(arr[1], 64)
	return y, n
}

func (m polymarketMarket) deadline() time.Time {
	if m.EndDate == "" {
		return time.Time{}
	}
	// Polymarket uses RFC3339 or "2006-01-02T15:04:05Z" format.
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05Z", "2006-01-02"} {
		if t, err := time.Parse(layout, m.EndDate); err == nil {
			return t
		}
	}
	return time.Time{}
}

func (m polymarketMarket) categoryNames() []string {
	names := make([]string, 0, len(m.Tags))
	for _, t := range m.Tags {
		names = append(names, t.Label)
	}
	return names
}

func (m polymarketMarket) toContract() Contract {
	yes, no := m.prices()
	return Contract{
		ID:                 m.ID,
		Platform:           "polymarket",
		Title:              m.Question,
		Categories:         m.categoryNames(),
		Price:              yes, // "price" = Yes probability
		YesPrice:           yes,
		NoPrice:            no,
		Volume24h:          m.Volume24hr,
		Bid:                yes - m.Spread/2,
		Ask:                yes + m.Spread/2,
		ResolutionDeadline: m.deadline(),
		Resolved:           m.Closed,
	}
}

func (m polymarketMarket) toSnapshot() ContractSnapshot {
	yes, no := m.prices()
	return ContractSnapshot{
		Platform:           "polymarket",
		ContractID:         m.ID,
		ContractTitle:      m.Question,
		Price:              yes,
		YesPrice:           yes,
		NoPrice:            no,
		Volume24h:          m.Volume24hr,
		Bid:                yes - m.Spread/2,
		Ask:                yes + m.Spread/2,
		ResolutionDeadline: m.deadline(),
		Resolved:           m.Closed,
		Categories:         m.categoryNames(),
		CreatedAt:          time.Now().UTC(),
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// Platform label used in watchlist / snapshots.
const PolymarketPlatform = "polymarket"

// CategoryToTagSlug maps common human-readable category names to Polymarket
// tag slugs. Unrecognised categories are lowercased and used as-is.
func CategoryToTagSlug(category string) string {
	m := map[string]string{
		"politics":       "politics",
		"crypto":         "crypto",
		"sports":         "sports",
		"finance":        "finance",
		"entertainment":  "entertainment",
		"science":        "science",
		"world":          "world",
		"economics":      "economics",
	}
	if slug, ok := m[strings.ToLower(category)]; ok {
		return slug
	}
	return strings.ToLower(category)
}
