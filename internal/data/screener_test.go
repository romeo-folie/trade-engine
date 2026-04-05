package data

import (
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFetchGapMovers(t *testing.T) {
	// Use raw JSON matching the real Alpaca API field names to catch
	// deserialization mismatches (the previous Go-struct-based test
	// silently masked a wrong json tag).
	const moversJSON = `{
		"gainers": [
			{"symbol":"AAPL","percent_change":5.2,"change":8.93,"price":180.0},
			{"symbol":"TSLA","percent_change":2.1,"change":5.15,"price":250.0},
			{"symbol":"NVDA","percent_change":4.0,"change":19.23,"price":500.0}
		],
		"losers": [
			{"symbol":"META","percent_change":-6.0,"change":-19.15,"price":300.0},
			{"symbol":"AMZN","percent_change":-1.5,"change":-2.74,"price":180.0}
		]
	}`

	const snapshotsJSON = `{
		"AAPL": {"dailyBar":{"t":"2026-04-05T04:00:00Z","o":171.0,"h":181.0,"l":170.0,"c":180.0,"v":1500000,"vw":176.0}},
		"META": {"dailyBar":{"t":"2026-04-05T04:00:00Z","o":318.0,"h":320.0,"l":298.0,"c":300.0,"v":2200000,"vw":310.0}},
		"NVDA": {"dailyBar":{"t":"2026-04-05T04:00:00Z","o":482.0,"h":505.0,"l":480.0,"c":500.0,"v":3100000,"vw":495.0}}
	}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1beta1/screener/stocks/movers":
			w.Write([]byte(moversJSON))
		case "/v2/stocks/snapshots":
			w.Write([]byte(snapshotsJSON))
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
			http.Error(w, "not found", 404)
		}
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

	// Verify PriorClose is derived correctly (price - change)
	if math.Abs(candidates[1].PriorClose-171.07) > 0.01 {
		t.Errorf("candidates[1].PriorClose = %f, want ~171.07", candidates[1].PriorClose)
	}

	// Verify PreMarketVol is enriched from snapshots
	if candidates[0].PreMarketVol != 2200000 { // META
		t.Errorf("candidates[0].PreMarketVol = %d, want 2200000", candidates[0].PreMarketVol)
	}
	if candidates[1].PreMarketVol != 1500000 { // AAPL
		t.Errorf("candidates[1].PreMarketVol = %d, want 1500000", candidates[1].PreMarketVol)
	}
}

func TestFetchGapMoversHighThreshold(t *testing.T) {
	const moversJSON = `{
		"gainers": [
			{"symbol":"AAPL","percent_change":2.0,"change":3.53,"price":180.0}
		],
		"losers": []
	}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(moversJSON))
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
