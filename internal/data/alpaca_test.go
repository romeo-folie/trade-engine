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

func TestAlpacaFetchHistory_DailyUsesBufferedRangeAndChronologicalOrder(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("timeframe") != "1Day" {
			t.Errorf("timeframe = %q, want 1Day", q.Get("timeframe"))
		}
		if q.Get("sort") != "desc" {
			t.Errorf("sort = %q, want desc", q.Get("sort"))
		}
		start, err := time.Parse(time.RFC3339, q.Get("start"))
		if err != nil {
			t.Fatalf("start = %q: %v", q.Get("start"), err)
		}
		end, err := time.Parse(time.RFC3339, q.Get("end"))
		if err != nil {
			t.Fatalf("end = %q: %v", q.Get("end"), err)
		}
		if !start.Before(end) || end.Sub(start) < 30*24*time.Hour {
			t.Fatalf("daily history range = %v..%v, want at least 30 calendar days", start, end)
		}

		// Alpaca returns newest-first when sort=desc. The client must restore
		// oldest-to-newest ordering for EMA/indicator consumers.
		resp := alpacaBarsResponse{
			Symbol: "TSLA",
			Bars: []alpacaBar{
				{Timestamp: "2024-01-03T05:00:00Z", Close: 103},
				{Timestamp: "2024-01-02T05:00:00Z", Close: 102},
				{Timestamp: "2024-01-01T05:00:00Z", Close: 101},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client := newTestAlpacaClient(srv.URL)
	candles, err := client.FetchHistory(context.Background(), "TSLA", Timeframe1Day, 3)
	if err != nil {
		t.Fatalf("FetchHistory error: %v", err)
	}
	if len(candles) != 3 {
		t.Fatalf("got %d candles, want 3", len(candles))
	}
	for i, want := range []float64{101, 102, 103} {
		if candles[i].Close != want {
			t.Errorf("candles[%d].Close = %v, want %v", i, candles[i].Close, want)
		}
	}
}

// TestAlpacaFetchHistory_IntradayUsesBufferedRangeAndChronologicalOrder pins
// the fix for the trailing stop's dead ATR. Alpaca's bars endpoint defaults to
// the CURRENT SESSION when no start/end is supplied, so an unranged request for
// 50 1h bars returned only the handful of hours elapsed that day — four during
// the session, zero before the open. ComputeATR needs period+1 bars and returns
// 0 below that, so the trail's stop candidate collapsed to the current stop and
// never ratcheted.
func TestAlpacaFetchHistory_IntradayUsesBufferedRangeAndChronologicalOrder(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("timeframe") != "1Hour" {
			t.Errorf("timeframe = %q, want 1Hour", q.Get("timeframe"))
		}
		if q.Get("sort") != "desc" {
			t.Errorf("sort = %q, want desc", q.Get("sort"))
		}
		start, err := time.Parse(time.RFC3339, q.Get("start"))
		if err != nil {
			t.Fatalf("start = %q: %v", q.Get("start"), err)
		}
		end, err := time.Parse(time.RFC3339, q.Get("end"))
		if err != nil {
			t.Fatalf("end = %q: %v", q.Get("end"), err)
		}
		// The window must reach well past today, or ATR(14) starves again.
		if !start.Before(end) || end.Sub(start) < 30*24*time.Hour {
			t.Fatalf("intraday history range = %v..%v, want at least 30 calendar days", start, end)
		}

		resp := alpacaBarsResponse{
			Symbol: "TSLA",
			Bars: []alpacaBar{
				{Timestamp: "2024-01-02T20:00:00Z", Close: 103},
				{Timestamp: "2024-01-02T19:00:00Z", Close: 102},
				{Timestamp: "2024-01-02T18:00:00Z", Close: 101},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client := newTestAlpacaClient(srv.URL)
	candles, err := client.FetchHistory(context.Background(), "TSLA", Timeframe1Hour, 50)
	if err != nil {
		t.Fatalf("FetchHistory error: %v", err)
	}
	if len(candles) != 3 {
		t.Fatalf("got %d candles, want 3", len(candles))
	}
	// sort=desc returns newest-first; indicator consumers need oldest-first so
	// the final element is the current bar.
	for i, want := range []float64{101, 102, 103} {
		if candles[i].Close != want {
			t.Errorf("candles[%d].Close = %v, want %v", i, candles[i].Close, want)
		}
	}
}

// TestAlpacaFetchHistory_FollowsPaginationUntilWindowFull covers the other way
// a rolling indicator can starve: Alpaca may answer with fewer bars than the
// requested limit and hand back a next_page_token instead. A single request
// would leave ATR(14) below its period+1 minimum and reproduce the zero-ATR
// stall the range fix exists to prevent.
func TestAlpacaFetchHistory_FollowsPaginationUntilWindowFull(t *testing.T) {
	var pages int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&pages, 1)
		token := r.URL.Query().Get("page_token")
		var resp alpacaBarsResponse
		switch n {
		case 1:
			if token != "" {
				t.Errorf("first page sent page_token %q, want empty", token)
			}
			// Short first page plus a continuation token.
			resp = alpacaBarsResponse{Symbol: "TSLA", Bars: []alpacaBar{
				{Timestamp: "2024-01-02T20:00:00Z", Close: 103},
				{Timestamp: "2024-01-02T19:00:00Z", Close: 102},
			}, NextPageToken: "page-2"}
		case 2:
			if token != "page-2" {
				t.Errorf("second page sent page_token %q, want page-2", token)
			}
			if got := r.URL.Query().Get("limit"); got != "2" {
				t.Errorf("continuation limit = %q, want the remaining 2", got)
			}
			resp = alpacaBarsResponse{Symbol: "TSLA", Bars: []alpacaBar{
				{Timestamp: "2024-01-02T18:00:00Z", Close: 101},
				{Timestamp: "2024-01-02T17:00:00Z", Close: 100},
			}}
		default:
			t.Errorf("unexpected request #%d after the window was filled", n)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client := newTestAlpacaClient(srv.URL)
	candles, err := client.FetchHistory(context.Background(), "TSLA", Timeframe1Hour, 4)
	if err != nil {
		t.Fatalf("FetchHistory error: %v", err)
	}
	if len(candles) != 4 {
		t.Fatalf("got %d candles, want 4 across two pages", len(candles))
	}
	// Pages arrive newest-first and continue older; the reversal must still
	// leave one chronological run.
	for i, want := range []float64{100, 101, 102, 103} {
		if candles[i].Close != want {
			t.Errorf("candles[%d].Close = %v, want %v", i, candles[i].Close, want)
		}
	}
	if got := atomic.LoadInt32(&pages); got != 2 {
		t.Errorf("requests = %d, want 2", got)
	}
}

// TestAlpacaFetchHistory_PaginationTerminatesWithoutProgress guards the loop
// against a provider that keeps handing back a token it cannot make progress
// on. Each sub-case is a distinct way to spin forever or duplicate the window,
// so all three exits are pinned.
func TestAlpacaFetchHistory_PaginationTerminatesWithoutProgress(t *testing.T) {
	cases := []struct {
		name string
		// page returns the response for request n (1-indexed).
		page func(n int32) alpacaBarsResponse
		// wantCandles and wantCalls after FetchHistory returns.
		wantCandles int
		wantCalls   int32
	}{
		{
			name: "token with an empty page",
			page: func(n int32) alpacaBarsResponse {
				r := alpacaBarsResponse{Symbol: "TSLA", NextPageToken: "more"}
				if n == 1 {
					r.Bars = []alpacaBar{{Timestamp: "2024-01-02T20:00:00Z", Close: 103}}
				}
				return r
			},
			wantCandles: 1, wantCalls: 2,
		},
		{
			// Bars present but every timestamp is unparseable, so they are
			// skipped and the page adds nothing: len(data.Bars) alone would
			// not catch this.
			name: "token with only unparseable bars",
			page: func(n int32) alpacaBarsResponse {
				r := alpacaBarsResponse{Symbol: "TSLA", NextPageToken: fmt.Sprintf("more-%d", n)}
				if n == 1 {
					r.Bars = []alpacaBar{{Timestamp: "2024-01-02T20:00:00Z", Close: 103}}
				} else {
					r.Bars = []alpacaBar{{Timestamp: "not-a-timestamp", Close: 99}}
				}
				return r
			},
			wantCandles: 1, wantCalls: 2,
		},
		{
			// A token that never advances would re-request the same page and
			// fill the window with duplicates.
			name: "token that does not advance",
			page: func(n int32) alpacaBarsResponse {
				return alpacaBarsResponse{
					Symbol:        "TSLA",
					NextPageToken: "stuck",
					Bars: []alpacaBar{
						{Timestamp: "2024-01-02T20:00:00Z", Close: 103},
					},
				}
			},
			wantCandles: 2, wantCalls: 2,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var calls int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				n := atomic.AddInt32(&calls, 1)
				if n > 5 {
					t.Errorf("pagination did not terminate: %d requests", n)
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(tc.page(n))
			}))
			defer srv.Close()

			client := newTestAlpacaClient(srv.URL)
			candles, err := client.FetchHistory(context.Background(), "TSLA", Timeframe1Hour, 50)
			if err != nil {
				t.Fatalf("FetchHistory error: %v", err)
			}
			if len(candles) != tc.wantCandles {
				t.Errorf("got %d candles, want %d", len(candles), tc.wantCandles)
			}
			if got := atomic.LoadInt32(&calls); got != tc.wantCalls {
				t.Errorf("requests = %d, want %d", got, tc.wantCalls)
			}
		})
	}
}

func TestHistoryLookbackDays(t *testing.T) {
	cases := []struct {
		name  string
		tf    Timeframe
		limit int
		want  int
	}{
		// Short requests floor at 30 days so a long weekend can't starve them.
		{"1h short request floors at 30d", Timeframe1Hour, 50, 30},
		{"1d short request floors at 30d", Timeframe1Day, 3, 30},
		// ~7 hourly bars per session: 200 bars ≈ 29 sessions ≈ 87 calendar days.
		{"1h engine window", Timeframe1Hour, 200, 87},
		// Daily keeps its original limit*3 behaviour.
		{"1d scanner window", Timeframe1Day, 130, 390},
		// 390 one-minute bars per session.
		{"1m one session", Timeframe1Min, 390, 30},
		{"1m two sessions", Timeframe1Min, 400, 30},
		// Unknown timeframes fall back to the widest window.
		{"unknown timeframe", Timeframe("2Hour"), 40, 120},
		// Degenerate limits must not produce a zero-width window.
		{"zero limit", Timeframe1Hour, 0, 30},
		{"negative limit", Timeframe1Day, -5, 30},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := historyLookbackDays(tc.tf, tc.limit); got != tc.want {
				t.Errorf("historyLookbackDays(%s, %d) = %d, want %d", tc.tf, tc.limit, got, tc.want)
			}
		})
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
