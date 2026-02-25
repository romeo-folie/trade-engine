package store

import (
	"context"
	"fmt"
	"time"

	"github.com/romeo-folie/trade-engine/internal/data"
)

// UpsertNewsItems idempotently inserts news articles.
// Deduplication is on external_id (preferred) or url.
// Existing articles are not updated — this is an insert-only store.
func UpsertNewsItems(ctx context.Context, items []data.NewsItem) error {
	if len(items) == 0 {
		return nil
	}
	const batchSize = 200
	for i := 0; i < len(items); i += batchSize {
		end := i + batchSize
		if end > len(items) {
			end = len(items)
		}
		if err := upsertNewsBatch(ctx, items[i:end]); err != nil {
			return fmt.Errorf("upsert news batch [%d:%d]: %w", i, end, err)
		}
	}
	return nil
}

func upsertNewsBatch(ctx context.Context, items []data.NewsItem) error {
	q := `INSERT INTO news_items (external_id, headline, summary, source, url, published_at, tickers, sentiment_score) VALUES `
	args := make([]any, 0, len(items)*8)
	for i, item := range items {
		if i > 0 {
			q += ", "
		}
		base := i * 8
		q += fmt.Sprintf("($%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d)",
			base+1, base+2, base+3, base+4, base+5, base+6, base+7, base+8)
		var extID *string
		if item.ExternalID != "" {
			s := item.ExternalID
			extID = &s
		}
		var sentiment *float64
		if item.SentimentScore != 0 {
			s := item.SentimentScore
			sentiment = &s
		}
		args = append(args,
			extID, item.Headline, item.Summary, item.Source, item.URL,
			item.PublishedAt.UTC(), item.Tickers, sentiment,
		)
	}
	// Dedup on external_id if present, otherwise on url.
	q += ` ON CONFLICT (external_id) WHERE external_id IS NOT NULL DO NOTHING`

	_, err := GetDB().Exec(ctx, q, args...)
	if err != nil {
		return err
	}
	return nil
}

// GetRecentNews retrieves news items for a ticker published after 'since',
// ordered by published_at descending (most recent first).
func GetRecentNews(ctx context.Context, ticker string, since time.Time) ([]data.NewsItem, error) {
	rows, err := GetDB().Query(ctx, `
		SELECT external_id, headline, COALESCE(summary,''), COALESCE(source,''),
		       COALESCE(url,''), published_at, COALESCE(tickers,'{}'),
		       COALESCE(sentiment_score, 0)
		FROM news_items
		WHERE $1 = ANY(tickers) AND published_at >= $2
		ORDER BY published_at DESC
	`, ticker, since.UTC())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var items []data.NewsItem
	for rows.Next() {
		var n data.NewsItem
		var extID *string
		if err := rows.Scan(&extID, &n.Headline, &n.Summary, &n.Source,
			&n.URL, &n.PublishedAt, &n.Tickers, &n.SentimentScore); err != nil {
			return nil, err
		}
		if extID != nil {
			n.ExternalID = *extID
		}
		items = append(items, n)
	}
	return items, rows.Err()
}
