package data

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFetchGapMovers(t *testing.T) {
	moversResp := alpacaMoversResponse{
		Gainers: []alpacaMover{
			{Symbol: "AAPL", ChangePerc: 5.2, Price: 180.0, Volume: 100000, PrevClose: 171.1},
			{Symbol: "TSLA", ChangePerc: 2.1, Price: 250.0, Volume: 80000, PrevClose: 244.9},
			{Symbol: "NVDA", ChangePerc: 4.0, Price: 500.0, Volume: 120000, PrevClose: 480.8},
		},
		Losers: []alpacaMover{
			{Symbol: "META", ChangePerc: -6.0, Price: 300.0, Volume: 90000, PrevClose: 319.1},
			{Symbol: "AMZN", ChangePerc: -1.5, Price: 180.0, Volume: 70000, PrevClose: 182.7},
		},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1beta1/screener/stocks/movers" {
			t.Errorf("unexpected path: %s", r.URL.Path)
			http.Error(w, "not found", 404)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(moversResp)
	}))
	defer srv.Close()

	client := NewAlpacaClient("test-key", "test-secret", srv.URL)
	client.minInterval = 0 // disable throttle for tests

	candidates, err := FetchGapMovers(context.Background(), client, 3.0)
	if err != nil {
		t.Fatalf("FetchGapMovers: %v", err)
	}

	// Should include AAPL (5.2%), META (-6.0%), NVDA (4.0%) — not TSLA (2.1%) or AMZN (-1.5%)
	if len(candidates) != 3 {
		t.Fatalf("expected 3 candidates, got %d", len(candidates))
	}

	// Sorted by abs gap descending: META (6.0), AAPL (5.2), NVDA (4.0)
	if candidates[0].Ticker != "META" {
		t.Errorf("candidates[0].Ticker = %q, want META", candidates[0].Ticker)
	}
	if candidates[1].Ticker != "AAPL" {
		t.Errorf("candidates[1].Ticker = %q, want AAPL", candidates[1].Ticker)
	}
	if candidates[2].Ticker != "NVDA" {
		t.Errorf("candidates[2].Ticker = %q, want NVDA", candidates[2].Ticker)
	}

	// Verify gap values
	if math.Abs(candidates[0].GapPct-(-6.0)) > 0.01 {
		t.Errorf("candidates[0].GapPct = %f, want -6.0", candidates[0].GapPct)
	}
}

func TestFetchGapMoversHighThreshold(t *testing.T) {
	moversResp := alpacaMoversResponse{
		Gainers: []alpacaMover{
			{Symbol: "AAPL", ChangePerc: 2.0, Price: 180.0, Volume: 100000, PrevClose: 176.5},
		},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(moversResp)
	}))
	defer srv.Close()

	client := NewAlpacaClient("test-key", "test-secret", srv.URL)
	client.minInterval = 0

	candidates, err := FetchGapMovers(context.Background(), client, 5.0)
	if err != nil {
		t.Fatalf("FetchGapMovers: %v", err)
	}
	if len(candidates) != 0 {
		t.Errorf("expected 0 candidates with 5%% threshold, got %d", len(candidates))
	}
}
