package data

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// newTestFinnhubClient creates a FinnhubClient that redirects all HTTP calls
// to the given test server.
func newTestFinnhubClient(serverURL string) *FinnhubClient {
	c := NewFinnhubClient("test-token")
	c.minInterval = 0
	c.httpClient = newHTTPClientWithTransport(serverURL)
	return c
}

func makeFinnhubArticles(count int, baseTime time.Time) []finnhubArticle {
	articles := make([]finnhubArticle, count)
	for i := 0; i < count; i++ {
		articles[i] = finnhubArticle{
			ID:       int64(1000 + i),
			Category: "company",
			DateTime: baseTime.Add(time.Duration(i) * time.Hour).Unix(),
			Headline: "Test headline",
			Source:   "Reuters",
			URL:      "https://example.com/news",
			Related:  "AAPL",
		}
		articles[i].Sentiment.BullishPercent = 60.0
		articles[i].Sentiment.BearishPercent = 40.0
	}
	return articles
}

func TestFinnhubFetchNews_Success(t *testing.T) {
	baseTime := time.Date(2024, 1, 2, 10, 0, 0, 0, time.UTC)
	articles := makeFinnhubArticles(3, baseTime)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("symbol") != "AAPL" {
			t.Errorf("expected symbol=AAPL, got %s", r.URL.Query().Get("symbol"))
		}
		if r.URL.Query().Get("token") != "test-token" {
			t.Errorf("expected token=test-token, got %s", r.URL.Query().Get("token"))
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(articles)
	}))
	defer srv.Close()

	client := newTestFinnhubClient(srv.URL)
	since := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

	items, err := client.FetchNews(context.Background(), "AAPL", since)
	if err != nil {
		t.Fatalf("FetchNews error: %v", err)
	}
	if len(items) != 3 {
		t.Errorf("expected 3 news items, got %d", len(items))
	}
	// Sentiment: (60 - 40) / 100 = 0.2
	if items[0].SentimentScore != 0.2 {
		t.Errorf("expected sentiment 0.2, got %f", items[0].SentimentScore)
	}
	if items[0].ExternalID != "finnhub-1000" {
		t.Errorf("unexpected ExternalID: %s", items[0].ExternalID)
	}
}

func TestFinnhubFetchNews_FiltersOldArticles(t *testing.T) {
	since := time.Date(2024, 1, 5, 0, 0, 0, 0, time.UTC)
	articles := []finnhubArticle{
		{
			ID:       1001,
			Headline: "Old article",
			URL:      "https://example.com/old",
			DateTime: time.Date(2024, 1, 3, 0, 0, 0, 0, time.UTC).Unix(),
		},
		{
			ID:       1002,
			Headline: "New article",
			URL:      "https://example.com/new",
			DateTime: time.Date(2024, 1, 6, 0, 0, 0, 0, time.UTC).Unix(),
		},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(articles)
	}))
	defer srv.Close()

	client := newTestFinnhubClient(srv.URL)
	items, err := client.FetchNews(context.Background(), "AAPL", since)
	if err != nil {
		t.Fatalf("FetchNews error: %v", err)
	}
	if len(items) != 1 {
		t.Errorf("expected 1 item (filtered), got %d", len(items))
	}
}

func TestFinnhubFetchNews_FiltersIncompleteArticles(t *testing.T) {
	since := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	articles := []finnhubArticle{
		{ID: 1001, Headline: "", URL: "https://example.com/a",
			DateTime: time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC).Unix()},
		{ID: 1002, Headline: "Good", URL: "",
			DateTime: time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC).Unix()},
		{ID: 1003, Headline: "Complete", URL: "https://example.com/c",
			DateTime: time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC).Unix()},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(articles)
	}))
	defer srv.Close()

	client := newTestFinnhubClient(srv.URL)
	items, err := client.FetchNews(context.Background(), "AAPL", since)
	if err != nil {
		t.Fatalf("FetchNews error: %v", err)
	}
	if len(items) != 1 {
		t.Errorf("expected 1 complete item, got %d", len(items))
	}
}

func TestFinnhubFetchNews_EmptyResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	client := newTestFinnhubClient(srv.URL)
	items, err := client.FetchNews(context.Background(), "AAPL", time.Now().Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("FetchNews empty response error: %v", err)
	}
	if len(items) != 0 {
		t.Errorf("expected 0 items, got %d", len(items))
	}
}

func TestFinnhubFetchNews_RetriesOn5xx(t *testing.T) {
	var calls int32
	baseTime := time.Date(2024, 1, 2, 10, 0, 0, 0, time.UTC)
	articles := makeFinnhubArticles(1, baseTime)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n < 2 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(articles)
	}))
	defer srv.Close()

	client := newTestFinnhubClient(srv.URL)
	_, err := client.FetchNews(context.Background(), "AAPL", time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("unexpected error after retry: %v", err)
	}
	if atomic.LoadInt32(&calls) != 2 {
		t.Errorf("expected 2 calls (1 retry), got %d", calls)
	}
}

func TestFinnhubFetchNews_NoRetryOn4xx(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	client := newTestFinnhubClient(srv.URL)
	_, err := client.FetchNews(context.Background(), "AAPL", time.Now().Add(-24*time.Hour))
	if err == nil {
		t.Fatal("expected error on 403")
	}
	if atomic.LoadInt32(&calls) != 1 {
		t.Errorf("expected 1 call (no retry on 4xx), got %d", calls)
	}
}

func TestFinnhubFetchNews_MalformedJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`not-json`))
	}))
	defer srv.Close()

	client := newTestFinnhubClient(srv.URL)
	_, err := client.FetchNews(context.Background(), "AAPL", time.Now().Add(-24*time.Hour))
	if err == nil {
		t.Fatal("expected error on malformed JSON")
	}
}
