package data

import (
	"context"
	"errors"
	"testing"
	"time"
)

type mockNewsProvider struct {
	items []NewsItem
	err  error
}

func (m mockNewsProvider) FetchNews(_ context.Context, _ string, _ time.Time) ([]NewsItem, error) {
	if m.err != nil {
		return nil, m.err
	}
	return m.items, nil
}

func TestNewsAggregator_MultiProviderSuccess(t *testing.T) {
	now := time.Now()
	p1 := mockNewsProvider{items: []NewsItem{{ExternalID: "1", Headline: "A", URL: "https://a", PublishedAt: now.Add(-2 * time.Minute)}}}
	p2 := mockNewsProvider{items: []NewsItem{{ExternalID: "2", Headline: "B", URL: "https://b", PublishedAt: now.Add(-1 * time.Minute)}}}

	agg := NewNewsAggregator(p1, p2)
	got, err := agg.FetchNews(context.Background(), "AAPL", now.Add(-1*time.Hour))
	if err != nil {
		t.Fatalf("FetchNews returned error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len(got)=%d want 2", len(got))
	}
	if got[0].URL != "https://b" || got[1].URL != "https://a" {
		t.Fatalf("expected newest-first ordering, got %+v", got)
	}
}

func TestNewsAggregator_DedupByURLThenTitle(t *testing.T) {
	now := time.Now()
	p1 := mockNewsProvider{items: []NewsItem{
		{ExternalID: "1", Headline: "Same story", URL: "https://dup", PublishedAt: now},
		{ExternalID: "2", Headline: "Title only dup", URL: "", PublishedAt: now.Add(-1 * time.Minute)},
	}}
	p2 := mockNewsProvider{items: []NewsItem{
		{ExternalID: "3", Headline: "Different title", URL: "https://dup", PublishedAt: now.Add(-2 * time.Minute)}, // dup by URL
		{ExternalID: "4", Headline: "  title only DUP  ", URL: "", PublishedAt: now.Add(-3 * time.Minute)},         // dup by title
	}}

	agg := NewNewsAggregator(p1, p2)
	got, err := agg.FetchNews(context.Background(), "AAPL", now.Add(-1*time.Hour))
	if err != nil {
		t.Fatalf("FetchNews returned error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len(got)=%d want 2 (dedup expected)", len(got))
	}
}

func TestNewsAggregator_ProviderErrorIsolation(t *testing.T) {
	now := time.Now()
	pErr := mockNewsProvider{err: errors.New("provider down")}
	pOK := mockNewsProvider{items: []NewsItem{{ExternalID: "ok1", Headline: "OK", URL: "https://ok", PublishedAt: now}}}

	agg := NewNewsAggregator(pErr, pOK)
	got, err := agg.FetchNews(context.Background(), "AAPL", now.Add(-1*time.Hour))
	if err != nil {
		t.Fatalf("expected no error when at least one provider succeeds, got %v", err)
	}
	if len(got) != 1 || got[0].URL != "https://ok" {
		t.Fatalf("unexpected results: %+v", got)
	}
}

func TestNewsAggregator_EmptyProviderList(t *testing.T) {
	agg := NewNewsAggregator()
	got, err := agg.FetchNews(context.Background(), "AAPL", time.Now().Add(-1*time.Hour))
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("len(got)=%d want 0", len(got))
	}
}

func TestNewsAggregator_AllProvidersFail(t *testing.T) {
	agg := NewNewsAggregator(
		mockNewsProvider{err: errors.New("p1 fail")},
		mockNewsProvider{err: errors.New("p2 fail")},
	)
	got, err := agg.FetchNews(context.Background(), "AAPL", time.Now().Add(-1*time.Hour))
	if err == nil {
		t.Fatal("expected error when all providers fail")
	}
	if got != nil {
		t.Fatalf("expected nil results on all-provider failure, got %+v", got)
	}
}
