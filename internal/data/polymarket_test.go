package data

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func newTestPolymarketClient(serverURL string) *PolymarketClient {
	c := NewPolymarketClient()
	c.httpClient = newHTTPClientWithTransport(serverURL)
	return c
}

func makePolymarketMarket(id string, volume24h float64) polymarketMarket {
	return polymarketMarket{
		ID:            id,
		Question:      "Will " + id + " happen?",
		Slug:          id + "-slug",
		EndDate:       "2024-12-31T00:00:00Z",
		OutcomePrices: `["0.65","0.35"]`,
		Volume:        "100000",
		Volume24hr:    volume24h,
		Spread:        0.02,
		Active:        true,
		Closed:        false,
		Tags:          []polymarketTag{{Label: "Finance", Slug: "finance"}},
	}
}

func TestPolymarketFetchContracts_Success(t *testing.T) {
	markets := []polymarketMarket{
		makePolymarketMarket("market-a", 10000),
		makePolymarketMarket("market-b", 5000),
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("tag_slug") == "" {
			t.Errorf("expected tag_slug query param")
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(markets)
	}))
	defer srv.Close()

	client := newTestPolymarketClient(srv.URL)
	contracts, err := client.FetchContracts(context.Background(), []string{"finance"}, 0, 10)
	if err != nil {
		t.Fatalf("FetchContracts error: %v", err)
	}
	if len(contracts) != 2 {
		t.Errorf("expected 2 contracts, got %d", len(contracts))
	}
	// Should be sorted by volume descending.
	if contracts[0].ID != "market-a" {
		t.Errorf("expected market-a first (higher volume), got %s", contracts[0].ID)
	}
	if contracts[0].Platform != PolymarketPlatform {
		t.Errorf("expected platform=%s, got %s", PolymarketPlatform, contracts[0].Platform)
	}
	// Price should be 0.65 (yes price from OutcomePrices).
	if contracts[0].YesPrice != 0.65 {
		t.Errorf("expected YesPrice=0.65, got %f", contracts[0].YesPrice)
	}
}

func TestPolymarketFetchContracts_MaxContracts(t *testing.T) {
	markets := make([]polymarketMarket, 5)
	for i := range markets {
		markets[i] = makePolymarketMarket("market-"+string(rune('a'+i)), float64(5000-i*100))
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(markets)
	}))
	defer srv.Close()

	client := newTestPolymarketClient(srv.URL)
	contracts, err := client.FetchContracts(context.Background(), []string{"finance"}, 0, 3)
	if err != nil {
		t.Fatalf("FetchContracts error: %v", err)
	}
	if len(contracts) != 3 {
		t.Errorf("expected 3 contracts (max limit), got %d", len(contracts))
	}
}

func TestPolymarketFetchContracts_Deduplication(t *testing.T) {
	// Same market ID from two category requests.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		markets := []polymarketMarket{makePolymarketMarket("dup-market", 5000)}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(markets)
	}))
	defer srv.Close()

	client := newTestPolymarketClient(srv.URL)
	contracts, err := client.FetchContracts(context.Background(), []string{"finance", "crypto"}, 0, 10)
	if err != nil {
		t.Fatalf("FetchContracts error: %v", err)
	}
	if len(contracts) != 1 {
		t.Errorf("expected 1 contract (deduplication), got %d", len(contracts))
	}
}

func TestPolymarketFetchSnapshot_Success(t *testing.T) {
	market := makePolymarketMarket("test-market-id", 8000)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(market)
	}))
	defer srv.Close()

	client := newTestPolymarketClient(srv.URL)
	snapshot, err := client.FetchSnapshot(context.Background(), "test-market-id")
	if err != nil {
		t.Fatalf("FetchSnapshot error: %v", err)
	}
	if snapshot.ContractID != "test-market-id" {
		t.Errorf("expected ContractID=test-market-id, got %s", snapshot.ContractID)
	}
	if snapshot.Platform != PolymarketPlatform {
		t.Errorf("expected platform=%s, got %s", PolymarketPlatform, snapshot.Platform)
	}
	if snapshot.Volume24h != 8000 {
		t.Errorf("expected Volume24h=8000, got %f", snapshot.Volume24h)
	}
	if snapshot.CreatedAt.IsZero() {
		t.Error("expected non-zero CreatedAt")
	}
}

func TestPolymarketFetchSnapshot_Error(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"not found"}`))
	}))
	defer srv.Close()

	client := newTestPolymarketClient(srv.URL)
	_, err := client.FetchSnapshot(context.Background(), "missing-id")
	if err == nil {
		t.Fatal("expected error on 404")
	}
}

func TestPolymarketMarketPrices(t *testing.T) {
	m := polymarketMarket{OutcomePrices: `["0.72","0.28"]`}
	yes, no := m.prices()
	if yes != 0.72 {
		t.Errorf("expected yes=0.72, got %f", yes)
	}
	if no != 0.28 {
		t.Errorf("expected no=0.28, got %f", no)
	}
}

func TestPolymarketMarketPrices_Malformed(t *testing.T) {
	m := polymarketMarket{OutcomePrices: `not json`}
	yes, no := m.prices()
	if yes != 0 || no != 0 {
		t.Errorf("expected 0,0 on malformed prices, got %f,%f", yes, no)
	}
}

func TestPolymarketMarketDeadline(t *testing.T) {
	m := polymarketMarket{EndDate: "2024-12-31T00:00:00Z"}
	deadline := m.deadline()
	expected := time.Date(2024, 12, 31, 0, 0, 0, 0, time.UTC)
	if !deadline.Equal(expected) {
		t.Errorf("expected deadline %v, got %v", expected, deadline)
	}
}

func TestPolymarketMarketDeadline_Empty(t *testing.T) {
	m := polymarketMarket{EndDate: ""}
	deadline := m.deadline()
	if !deadline.IsZero() {
		t.Errorf("expected zero deadline for empty endDate, got %v", deadline)
	}
}

func TestCategoryToTagSlug(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"politics", "politics"},
		{"CRYPTO", "crypto"},
		{"Finance", "finance"},
		{"unknown-tag", "unknown-tag"},
	}
	for _, tc := range tests {
		got := CategoryToTagSlug(tc.input)
		if got != tc.want {
			t.Errorf("CategoryToTagSlug(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}
