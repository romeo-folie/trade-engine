package data

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func newTestKalshiClient(serverURL string) *KalshiClient {
	c := NewKalshiClient("test-key")
	c.httpClient = newHTTPClientWithTransport(serverURL)
	return c
}

func makeKalshiMarket(ticker string, volume24h int) kalshiMarket {
	return kalshiMarket{
		Ticker:       ticker,
		EventTicker:  "EVT-" + ticker,
		Title:        "Will " + ticker + " happen?",
		Category:     "Finance",
		Status:       "open",
		YesBid:       45,
		YesAsk:       50,
		NoBid:        50,
		NoAsk:        55,
		LastPrice:    48,
		Volume24h:    volume24h,
		OpenInterest: 1000,
		CloseTime:    "2024-12-31T00:00:00Z",
	}
}

func TestKalshiFetchContracts_Success(t *testing.T) {
	markets := []kalshiMarket{
		makeKalshiMarket("TICKER-A", 5000),
		makeKalshiMarket("TICKER-B", 3000),
	}
	page := kalshiMarketsPage{Markets: markets}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(page)
	}))
	defer srv.Close()

	client := newTestKalshiClient(srv.URL)
	contracts, err := client.FetchContracts(context.Background(), []string{"finance"}, 0, 10)
	if err != nil {
		t.Fatalf("FetchContracts error: %v", err)
	}
	if len(contracts) != 2 {
		t.Errorf("expected 2 contracts, got %d", len(contracts))
	}
	// Should be sorted by volume descending.
	if contracts[0].ID != "TICKER-A" {
		t.Errorf("expected TICKER-A first (higher volume), got %s", contracts[0].ID)
	}
	if contracts[0].Platform != KalshiPlatform {
		t.Errorf("expected platform=%s, got %s", KalshiPlatform, contracts[0].Platform)
	}
}

func TestKalshiFetchContracts_VolumeFilter(t *testing.T) {
	markets := []kalshiMarket{
		makeKalshiMarket("HIGH-VOL", 5000),
		makeKalshiMarket("LOW-VOL", 100),
	}
	page := kalshiMarketsPage{Markets: markets}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(page)
	}))
	defer srv.Close()

	client := newTestKalshiClient(srv.URL)
	// minVolume = 1000 should exclude LOW-VOL.
	contracts, err := client.FetchContracts(context.Background(), []string{"finance"}, 1000, 10)
	if err != nil {
		t.Fatalf("FetchContracts error: %v", err)
	}
	if len(contracts) != 1 {
		t.Errorf("expected 1 contract (volume filter), got %d", len(contracts))
	}
	if contracts[0].ID != "HIGH-VOL" {
		t.Errorf("expected HIGH-VOL, got %s", contracts[0].ID)
	}
}

func TestKalshiFetchContracts_MaxContracts(t *testing.T) {
	markets := make([]kalshiMarket, 5)
	for i := range markets {
		markets[i] = makeKalshiMarket("TICKER-"+string(rune('A'+i)), 1000-i*100)
	}
	page := kalshiMarketsPage{Markets: markets}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(page)
	}))
	defer srv.Close()

	client := newTestKalshiClient(srv.URL)
	// maxContracts = 3 should limit results.
	contracts, err := client.FetchContracts(context.Background(), []string{"finance"}, 0, 3)
	if err != nil {
		t.Fatalf("FetchContracts error: %v", err)
	}
	if len(contracts) != 3 {
		t.Errorf("expected 3 contracts (max limit), got %d", len(contracts))
	}
}

func TestKalshiFetchContracts_Deduplication(t *testing.T) {
	// Same ticker returned twice (across two categories).
	markets := []kalshiMarket{
		makeKalshiMarket("DUP-TICKER", 5000),
		makeKalshiMarket("DUP-TICKER", 5000),
	}
	page := kalshiMarketsPage{Markets: markets}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(page)
	}))
	defer srv.Close()

	client := newTestKalshiClient(srv.URL)
	contracts, err := client.FetchContracts(context.Background(), []string{"finance"}, 0, 10)
	if err != nil {
		t.Fatalf("FetchContracts error: %v", err)
	}
	if len(contracts) != 1 {
		t.Errorf("expected 1 contract (deduplication), got %d", len(contracts))
	}
}

func TestKalshiFetchContracts_Pagination(t *testing.T) {
	var requestCount int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		w.Header().Set("Content-Type", "application/json")
		if requestCount == 1 {
			page := kalshiMarketsPage{
				Markets: []kalshiMarket{makeKalshiMarket("PAGE1-A", 1000)},
				Cursor:  "next-cursor",
			}
			_ = json.NewEncoder(w).Encode(page)
			return
		}
		// Page 2 — no cursor.
		if r.URL.Query().Get("cursor") != "next-cursor" {
			t.Errorf("expected cursor=next-cursor, got %s", r.URL.Query().Get("cursor"))
		}
		page := kalshiMarketsPage{
			Markets: []kalshiMarket{makeKalshiMarket("PAGE2-A", 800)},
		}
		_ = json.NewEncoder(w).Encode(page)
	}))
	defer srv.Close()

	client := newTestKalshiClient(srv.URL)
	contracts, err := client.FetchContracts(context.Background(), []string{"finance"}, 0, 10)
	if err != nil {
		t.Fatalf("FetchContracts error: %v", err)
	}
	if len(contracts) != 2 {
		t.Errorf("expected 2 contracts (paginated), got %d", len(contracts))
	}
	if requestCount != 2 {
		t.Errorf("expected 2 HTTP requests, got %d", requestCount)
	}
}

func TestKalshiFetchSnapshot_Success(t *testing.T) {
	market := makeKalshiMarket("MY-TICKER", 2000)
	resp := kalshiMarketResponse{Market: market}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client := newTestKalshiClient(srv.URL)
	snapshot, err := client.FetchSnapshot(context.Background(), "MY-TICKER")
	if err != nil {
		t.Fatalf("FetchSnapshot error: %v", err)
	}
	if snapshot.ContractID != "MY-TICKER" {
		t.Errorf("expected ContractID=MY-TICKER, got %s", snapshot.ContractID)
	}
	if snapshot.Platform != KalshiPlatform {
		t.Errorf("expected platform=%s, got %s", KalshiPlatform, snapshot.Platform)
	}
	if snapshot.Volume24h != 2000 {
		t.Errorf("expected Volume24h=2000, got %f", snapshot.Volume24h)
	}
}

func TestKalshiFetchSnapshot_Error(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"not found"}`))
	}))
	defer srv.Close()

	client := newTestKalshiClient(srv.URL)
	_, err := client.FetchSnapshot(context.Background(), "MISSING-TICKER")
	if err == nil {
		t.Fatal("expected error on 404")
	}
}

func TestKalshiCategorySlug(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"politics", "Politics"},
		{"finance", "Finance"},
		{"crypto", "Crypto"},
		{"FINANCE", "Finance"},
		{"unknown", "Unknown"},
	}
	for _, tc := range tests {
		got := kalshiCategorySlug(tc.input)
		if got != tc.want {
			t.Errorf("kalshiCategorySlug(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestCentToProb(t *testing.T) {
	tests := []struct {
		cents int
		want  float64
	}{
		{0, 0.0},
		{50, 0.5},
		{99, 0.99},
		{100, 1.0},
	}
	for _, tc := range tests {
		got := centToProb(tc.cents)
		if got != tc.want {
			t.Errorf("centToProb(%d) = %f, want %f", tc.cents, got, tc.want)
		}
	}
}

func TestKalshiGet_RetriesOn5xx(t *testing.T) {
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts < 3 {
			w.WriteHeader(http.StatusServiceUnavailable) // 503 — retryable
			return
		}
		page := kalshiMarketsPage{Markets: []kalshiMarket{makeKalshiMarket("RETRY-A", 1000)}}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(page)
	}))
	defer srv.Close()

	c := newTestKalshiClient(srv.URL)
	// categories=["finance"] drives at least one HTTP call; minVolume=0 keeps the contract
	contracts, err := c.FetchContracts(context.Background(), []string{"finance"}, 0, 10)
	if err != nil {
		t.Fatalf("expected success after retries, got: %v", err)
	}
	if len(contracts) != 1 || contracts[0].ID != "RETRY-A" {
		t.Fatalf("unexpected contracts: %+v", contracts)
	}
	if attempts != 3 {
		t.Fatalf("expected 3 attempts (2 retries), got %d", attempts)
	}
}

func TestKalshiGet_NoRetryOn4xx(t *testing.T) {
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.WriteHeader(http.StatusUnauthorized) // 401 — not retryable
	}))
	defer srv.Close()

	c := newTestKalshiClient(srv.URL)
	_, err := c.FetchContracts(context.Background(), []string{"finance"}, 0, 10)
	if err == nil {
		t.Fatal("expected error on 401")
	}
	if attempts != 1 {
		t.Fatalf("expected exactly 1 attempt (no retry on 4xx), got %d", attempts)
	}
}
