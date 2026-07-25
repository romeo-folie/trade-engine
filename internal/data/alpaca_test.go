package data

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// newTestAlpacaClient creates an AlpacaClient pointed at a test server URL.
func newTestAlpacaClient(serverURL string) *AlpacaClient {
	c := NewAlpacaClient("test-key", "test-secret", serverURL)
	c.minInterval = 0 // disable rate limiting in tests
	return c
}

func TestAlpacaFetchCandles_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("APCA-API-KEY-ID") != "test-key" {
			t.Errorf("missing or wrong API key header")
		}
		if got := r.URL.Query().Get("adjustment"); got != "split" {
			t.Errorf("adjustment = %q, want split", got)
		}
		resp := alpacaBarsResponse{
			Symbol: "AAPL",
			Bars: []alpacaBar{
				{Timestamp: "2024-01-02T05:00:00Z", Open: 185.0, High: 188.0, Low: 183.0, Close: 187.0, Volume: 1000000, VWAP: 186.5},
				{Timestamp: "2024-01-03T05:00:00Z", Open: 187.0, High: 190.0, Low: 185.0, Close: 189.0, Volume: 1200000, VWAP: 188.2},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client := newTestAlpacaClient(srv.URL)
	from := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2024, 1, 5, 0, 0, 0, 0, time.UTC)

	candles, err := client.FetchCandles(context.Background(), "AAPL", Timeframe1Day, from, to)
	if err != nil {
		t.Fatalf("FetchCandles error: %v", err)
	}
	if len(candles) != 2 {
		t.Errorf("expected 2 candles, got %d", len(candles))
	}
	if candles[0].Ticker != "AAPL" {
		t.Errorf("expected ticker AAPL, got %s", candles[0].Ticker)
	}
	if candles[0].Close != 187.0 {
		t.Errorf("expected close 187.0, got %f", candles[0].Close)
	}
}

func TestAlpacaFetchCandles_Pagination(t *testing.T) {
	var requestCount int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&requestCount, 1)
		w.Header().Set("Content-Type", "application/json")

		if n == 1 {
			// first page — return a next_page_token
			resp := alpacaBarsResponse{
				Symbol:        "MSFT",
				NextPageToken: "page2token",
				Bars: []alpacaBar{
					{Timestamp: "2024-01-02T05:00:00Z", Open: 370.0, High: 375.0, Low: 368.0, Close: 373.0, Volume: 500000},
				},
			}
			_ = json.NewEncoder(w).Encode(resp)
			return
		}
		// second page — no token, done
		if r.URL.Query().Get("page_token") != "page2token" {
			t.Errorf("expected page_token=page2token, got %s", r.URL.Query().Get("page_token"))
		}
		resp := alpacaBarsResponse{
			Symbol: "MSFT",
			Bars: []alpacaBar{
				{Timestamp: "2024-01-03T05:00:00Z", Open: 373.0, High: 378.0, Low: 371.0, Close: 376.0, Volume: 600000},
			},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client := newTestAlpacaClient(srv.URL)
	from := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2024, 1, 5, 0, 0, 0, 0, time.UTC)

	candles, err := client.FetchCandles(context.Background(), "MSFT", Timeframe1Day, from, to)
	if err != nil {
		t.Fatalf("FetchCandles pagination error: %v", err)
	}
	if len(candles) != 2 {
		t.Errorf("expected 2 candles (from 2 pages), got %d", len(candles))
	}
	if atomic.LoadInt32(&requestCount) != 2 {
		t.Errorf("expected 2 HTTP requests (pagination), got %d", requestCount)
	}
}

func TestAlpacaFetchCandles_InvalidTimeframe(t *testing.T) {
	client := newTestAlpacaClient("http://unused")
	_, err := client.FetchCandles(context.Background(), "AAPL", Timeframe("invalid"), time.Now(), time.Now())
	if err == nil {
		t.Fatal("expected error for invalid timeframe")
	}
}

func TestAlpacaFetchCandles_4HourTimeframe(t *testing.T) {
	var gotTF string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotTF = r.URL.Query().Get("timeframe")
		resp := alpacaBarsResponse{Symbol: "AAPL", Bars: []alpacaBar{}}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client := newTestAlpacaClient(srv.URL)
	from := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2024, 1, 5, 0, 0, 0, 0, time.UTC)
	_, err := client.FetchCandles(context.Background(), "AAPL", Timeframe4Hour, from, to)
	if err != nil {
		t.Fatalf("FetchCandles error: %v", err)
	}
	if gotTF != "4Hour" {
		t.Errorf("expected timeframe=4Hour, got %q", gotTF)
	}
}

func TestAlpacaFetchHistory_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		limit := r.URL.Query().Get("limit")
		if limit != "10" {
			t.Errorf("expected limit=10, got %s", limit)
		}
		if got := r.URL.Query().Get("adjustment"); got != "split" {
			t.Errorf("adjustment = %q, want split", got)
		}
		resp := alpacaBarsResponse{
			Symbol: "TSLA",
			Bars: []alpacaBar{
				{Timestamp: "2024-01-02T05:00:00Z", Open: 248.0, High: 253.0, Low: 245.0, Close: 251.0, Volume: 800000},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client := newTestAlpacaClient(srv.URL)
	candles, err := client.FetchHistory(context.Background(), "TSLA", Timeframe1Day, 10)
	if err != nil {
		t.Fatalf("FetchHistory error: %v", err)
	}
	if len(candles) != 1 {
		t.Errorf("expected 1 candle, got %d", len(candles))
	}
	if candles[0].Volume != 800000 {
		t.Errorf("expected volume 800000, got %d", candles[0].Volume)
	}
}

func TestAlpacaFetchHistory_InvalidTimeframe(t *testing.T) {
	client := newTestAlpacaClient("http://unused")
	_, err := client.FetchHistory(context.Background(), "AAPL", Timeframe("bad"), 5)
	if err == nil {
		t.Fatal("expected error for invalid timeframe")
	}
}

func TestAlpacaFetchQuote_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := alpacaQuoteResponse{
			Symbol: "AAPL",
			Quote: alpacaQuote{
				Timestamp: "2024-01-02T15:30:00Z",
				BidPrice:  184.50,
				AskPrice:  184.55,
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client := newTestAlpacaClient(srv.URL)
	quote, err := client.FetchQuote(context.Background(), "AAPL")
	if err != nil {
		t.Fatalf("FetchQuote error: %v", err)
	}
	if quote.Ticker != "AAPL" {
		t.Errorf("expected ticker AAPL, got %s", quote.Ticker)
	}
	expectedMid := (184.50 + 184.55) / 2
	if quote.Last != expectedMid {
		t.Errorf("expected mid price %.4f, got %.4f", expectedMid, quote.Last)
	}
	if quote.Bid != 184.50 {
		t.Errorf("expected bid 184.50, got %f", quote.Bid)
	}
}

func TestAlpacaFetchTradesUsesExactBoundsAndPaginates(t *testing.T) {
	from := time.Date(2026, 7, 25, 14, 0, 30, 123000000, time.UTC)
	to := from.Add(2 * time.Minute)
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Path != "/v2/stocks/AAPL/trades" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if r.URL.Query().Get("start") != from.Format(time.RFC3339Nano) ||
			r.URL.Query().Get("end") != to.Format(time.RFC3339Nano) {
			t.Errorf("bounds = %q..%q", r.URL.Query().Get("start"), r.URL.Query().Get("end"))
		}
		if r.URL.Query().Get("feed") != "iex" ||
			r.URL.Query().Get("limit") != "10000" ||
			r.URL.Query().Get("sort") != "asc" {
			t.Errorf("query = %v", r.URL.Query())
		}
		payload := alpacaTradesResponse{
			Symbol: "AAPL",
			Trades: []alpacaTrade{{
				Timestamp: from.Add(time.Second).Format(time.RFC3339Nano),
				Price:     99, Size: 10,
			}},
		}
		if requests == 1 {
			payload.NextPageToken = "next"
		} else if r.URL.Query().Get("page_token") != "next" {
			t.Errorf("page token = %q", r.URL.Query().Get("page_token"))
		}
		_ = json.NewEncoder(w).Encode(payload)
	}))
	defer srv.Close()

	client := newTestAlpacaClient(srv.URL)
	trades, err := client.FetchTrades(context.Background(), "AAPL", from, to)
	if err != nil {
		t.Fatalf("FetchTrades: %v", err)
	}
	if requests != 2 || len(trades) != 2 || trades[0].Price != 99 {
		t.Fatalf("requests=%d trades=%+v", requests, trades)
	}
}

func TestAlpacaGet_RetriesOn5xx(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n < 3 {
			w.WriteHeader(http.StatusServiceUnavailable) // 503 — should retry
			return
		}
		// Third attempt succeeds.
		resp := alpacaQuoteResponse{
			Symbol: "AAPL",
			Quote:  alpacaQuote{Timestamp: "2024-01-02T15:30:00Z", BidPrice: 184.50, AskPrice: 184.55},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client := newTestAlpacaClient(srv.URL)
	_, err := client.FetchQuote(context.Background(), "AAPL")
	if err != nil {
		t.Fatalf("unexpected error after retries: %v", err)
	}
	if atomic.LoadInt32(&calls) != 3 {
		t.Errorf("expected 3 calls (2 retries), got %d", calls)
	}
}

func TestAlpacaGet_NoRetryOn4xx(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusUnauthorized) // 401 — not retryable
		fmt.Fprintln(w, `{"message":"forbidden"}`)
	}))
	defer srv.Close()

	client := newTestAlpacaClient(srv.URL)
	_, err := client.FetchQuote(context.Background(), "AAPL")
	if err == nil {
		t.Fatal("expected error on 401, got nil")
	}
	if atomic.LoadInt32(&calls) != 1 {
		t.Errorf("expected 1 call (no retry on 4xx), got %d", calls)
	}
}

func TestAlpacaFetchCandles_MalformedJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{not valid json`))
	}))
	defer srv.Close()

	client := newTestAlpacaClient(srv.URL)
	from := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2024, 1, 5, 0, 0, 0, 0, time.UTC)

	_, err := client.FetchCandles(context.Background(), "AAPL", Timeframe1Day, from, to)
	if err == nil {
		t.Fatal("expected error on malformed JSON")
	}
}
