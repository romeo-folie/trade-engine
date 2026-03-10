package data

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"
)

// NewsItem represents a single news article from any news provider.
// Fields map to the news_items table.
type NewsItem struct {
	ExternalID     string    // provider-supplied unique ID (used for deduplication)
	Headline       string
	Summary        string
	Source         string
	URL            string
	PublishedAt    time.Time
	Tickers        []string
	SentimentScore float64 // 0 if provider doesn't supply sentiment
}

// NewsProvider is the interface all news data adapters must satisfy.
// Current implementation: Finnhub. Future: Benzinga, others.
type NewsProvider interface {
	// FetchNews retrieves news items for a specific ticker published after 'since'.
	FetchNews(ctx context.Context, ticker string, since time.Time) ([]NewsItem, error)
}


// NewsAggregator fans out to multiple NewsProvider implementations and
// aggregates their results with deduplication.
type NewsAggregator struct {
	providers []NewsProvider
}

// NewNewsAggregator constructs an aggregator over the provided providers.
func NewNewsAggregator(providers ...NewsProvider) *NewsAggregator {
	return &NewsAggregator{providers: providers}
}

// FetchNews queries all providers and returns a deduplicated, newest-first list.
//
// Dedup key priority:
//   1) URL (if present)
//   2) Headline (case-insensitive, trimmed)
//
// Provider errors are isolated: failures from one provider do not suppress
// successful results from others. If all providers fail, an aggregated error
// is returned.
func (a *NewsAggregator) FetchNews(ctx context.Context, ticker string, since time.Time) ([]NewsItem, error) {
	if len(a.providers) == 0 {
		return []NewsItem{}, nil
	}

	all := make([]NewsItem, 0, 32)
	var errs []error

	for _, p := range a.providers {
		items, err := p.FetchNews(ctx, ticker, since)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		all = append(all, items...)
	}

	deduped := dedupeNewsItems(all)
	sort.Slice(deduped, func(i, j int) bool {
		return deduped[i].PublishedAt.After(deduped[j].PublishedAt)
	})

	if len(deduped) == 0 && len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return deduped, nil
}

func dedupeNewsItems(items []NewsItem) []NewsItem {
	seen := make(map[string]struct{}, len(items))
	out := make([]NewsItem, 0, len(items))
	for _, n := range items {
		key := strings.TrimSpace(n.URL)
		if key == "" {
			key = strings.ToLower(strings.TrimSpace(n.Headline))
		}
		if key == "" {
			// no usable dedupe key; keep item
			out = append(out, n)
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, n)
	}
	return out
}
