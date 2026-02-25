package data

import (
	"context"
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
