package data

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// newTestIBKRClient creates an IBKRClient pointed at a test server URL with
// rate limiting disabled.
func newTestIBKRClient(serverURL string) *IBKRClient {
	c := NewIBKRClient(serverURL, false)
	c.minInterval = 0 // disable rate limiting in tests
	return c
}

// ── FetchCandles ──────────────────────────────────────────────────────────────

func TestIBKRFetchCandles(t *testing.T) {
	// t=1712000000000 ms → 2024-04-01 20:53:20 UTC
	// t=1712003600000 ms → 2024-04-01 21:53:20 UTC
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/api/iserver/marketdata/history" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.URL.Query().Get("conid") != "12087792" {
			t.Errorf("expected conid 12087792, got %s", r.URL.Query().Get("conid"))
		}
		if r.URL.Query().Get("outsideRth") != "true" {
			t.Errorf("expected outsideRth=true")
		}
		// startTime must anchor the END of the window (to) in UTC, so historical
		// ranges are fetched rather than the most-recent period.
		wantStart := time.Unix(1712100000, 0).UTC().Format("20060102-15:04:05")
		if got := r.URL.Query().Get("startTime"); got != wantStart {
			t.Errorf("expected startTime=%s, got %s", wantStart, got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"data": [
				{"o": 1.0900, "h": 1.0950, "l": 1.0850, "c": 1.0920, "v": 0, "t": 1712000000000},
				{"o": 1.0920, "h": 1.0960, "l": 1.0900, "c": 1.0940, "v": 0, "t": 1712003600000}
			],
			"mdAvailability": "S"
		}`))
	}))
	defer srv.Close()

	client := newTestIBKRClient(srv.URL)
	from := time.Unix(1711900000, 0).UTC()
	to := time.Unix(1712100000, 0).UTC()

	candles, err := client.FetchCandles(context.Background(), "EUR/USD", Timeframe1Hour, from, to)
	if err != nil {
		t.Fatalf("FetchCandles error: %v", err)
	}
	if len(candles) != 2 {
		t.Fatalf("expected 2 candles, got %d", len(candles))
	}

	c0 := candles[0]
	if c0.Ticker != "EUR/USD" {
		t.Errorf("expected ticker EUR/USD, got %s", c0.Ticker)
	}
	if c0.Timeframe != Timeframe1Hour {
		t.Errorf("expected timeframe %s, got %s", Timeframe1Hour, c0.Timeframe)
	}
	// Verify millisecond epoch timestamp parsing.
	expectedTS := time.UnixMilli(1712000000000).UTC()
	if !c0.Timestamp.Equal(expectedTS) {
		t.Errorf("expected timestamp %v, got %v", expectedTS, c0.Timestamp)
	}
	if c0.Open != 1.0900 {
		t.Errorf("expected open 1.0900, got %f", c0.Open)
	}
	if c0.High != 1.0950 {
		t.Errorf("expected high 1.0950, got %f", c0.High)
	}
	if c0.Low != 1.0850 {
		t.Errorf("expected low 1.0850, got %f", c0.Low)
	}
	if c0.Close != 1.0920 {
		t.Errorf("expected close 1.0920, got %f", c0.Close)
	}
	// Volume must always be 0 for IBKR forex candles.
	if c0.Volume != 0 {
		t.Errorf("expected volume 0, got %d", c0.Volume)
	}
	if c0.VWAP != 0 {
		t.Errorf("expected VWAP 0, got %f", c0.VWAP)
	}
}

func TestIBKRResolveConid_StaticCurrencyStrengthCrosses(t *testing.T) {
	client := newTestIBKRClient("http://127.0.0.1:0")
	for pair, want := range map[string]string{
		"EUR/JPY": "14321016",
		"GBP/JPY": "14321015",
	} {
		got, err := client.resolveConid(context.Background(), pair)
		if err != nil {
			t.Fatalf("resolveConid(%q): %v", pair, err)
		}
		if got != want {
			t.Errorf("resolveConid(%q) = %q, want %q", pair, got, want)
		}
	}
}

func TestIBKRFetchCandles_FiltersOutOfRange(t *testing.T) {
	// Server returns three candles; the first is before `from` and the last is
	// after `to`. Only the middle candle should survive.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"data": [
				{"o":1.08,"h":1.09,"l":1.07,"c":1.085,"v":0,"t":1711900000000},
				{"o":1.09,"h":1.10,"l":1.08,"c":1.092,"v":0,"t":1712000000000},
				{"o":1.09,"h":1.10,"l":1.08,"c":1.095,"v":0,"t":1712200000000}
			]
		}`))
	}))
	defer srv.Close()

	client := newTestIBKRClient(srv.URL)
	from := time.UnixMilli(1711950000000).UTC()
	to := time.UnixMilli(1712100000000).UTC()

	candles, err := client.FetchCandles(context.Background(), "EUR/USD", Timeframe1Hour, from, to)
	if err != nil {
		t.Fatalf("FetchCandles error: %v", err)
	}
	if len(candles) != 1 {
		t.Errorf("expected 1 candle after range filtering, got %d", len(candles))
	}
}

func TestIBKRFetchCandles_UnknownPair(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/api/iserver/secdef/search" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	client := newTestIBKRClient(srv.URL)
	_, err := client.FetchCandles(context.Background(), "BTC/USD", Timeframe1Hour, time.Now(), time.Now())
	if err == nil {
		t.Fatal("expected error for unknown pair")
	}
}

func TestIBKRResolveConid_ForexDynamic(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/api/iserver/secdef/search" && r.URL.Query().Get("symbol") == "GBP" && r.URL.Query().Get("secType") == "CASH" {
			hits++
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`[
				{"conid":11111111,"companyName":"GBP.USD","description":"GBP/USD","exchange":"IDEALPRO"},
				{"conid":22222222,"companyName":"GBP.SEK","description":"GBP/SEK","exchange":"IDEALPRO"},
				{"conid":99999999,"companyName":"GBP.SEK","description":"GBP/SEK","exchange":"SMART"}
			]`))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	client := newTestIBKRClient(srv.URL)
	conid, err := client.resolveConid(context.Background(), "GBP/SEK")
	if err != nil {
		t.Fatalf("resolveConid forex: %v", err)
	}
	if conid != "22222222" {
		t.Errorf("conid = %q; want 22222222", conid)
	}
	if _, err := client.resolveConid(context.Background(), "GBP/SEK"); err != nil {
		t.Fatalf("resolveConid (cached): %v", err)
	}
	if hits != 1 {
		t.Errorf("secdef/search hit %d times; want 1 (second call cached)", hits)
	}
}

func TestIBKRResolveConid_ForexDynamic_StringConid(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/api/iserver/secdef/search" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[
			{"conid":"12087799","companyName":"EUR.SEK","description":"EUR/SEK","exchange":"IDEALPRO"}
		]`))
	}))
	defer srv.Close()

	client := newTestIBKRClient(srv.URL)
	conid, err := client.resolveConid(context.Background(), "EUR/SEK")
	if err != nil {
		t.Fatalf("resolveConid forex with string conid: %v", err)
	}
	if conid != "12087799" {
		t.Errorf("conid = %q; want 12087799", conid)
	}
}

func TestIBKRResolveConid_ForexDynamic_NullConid(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[
			{"conid":null,"companyName":"EUR.SEK","description":"EUR/SEK","exchange":"SMART"},
			{"conid":"12087799","companyName":"EUR.SEK","description":"EUR/SEK","exchange":"IDEALPRO"}
		]`))
	}))
	defer srv.Close()

	client := newTestIBKRClient(srv.URL)
	conid, err := client.resolveConid(context.Background(), "EUR/SEK")
	if err != nil {
		t.Fatalf("resolveConid forex with null plus valid conid: %v", err)
	}
	if conid != "12087799" {
		t.Errorf("conid = %q; want 12087799", conid)
	}
}

func TestIBKRResolveConid_ForexDynamic_MalformedConid(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[
			{"conid":"not-a-number","companyName":"EUR.SEK","description":"EUR/SEK","exchange":"IDEALPRO"},
			{"conid":12087799,"companyName":"EUR.SEK","description":"EUR/SEK","exchange":"IDEALPRO"}
		]`))
	}))
	defer srv.Close()

	client := newTestIBKRClient(srv.URL)
	conid, err := client.resolveConid(context.Background(), "EUR/SEK")
	if err != nil {
		t.Fatalf("resolveConid forex with malformed plus valid conid: %v", err)
	}
	if conid != "12087799" {
		t.Errorf("conid = %q; want 12087799", conid)
	}
}

func TestIBKRResolveConid_FuturesDynamic(t *testing.T) {
	// A futures symbol absent from the static maps must resolve via /trsrv/futures
	// (nearest expiry >= today) and then serve from cache — mirroring the broker so
	// the market-data path can fetch candles for it.
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/api/trsrv/futures" && r.URL.Query().Get("symbols") == "ZZ" {
			hits++
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"ZZ":[
				{"conid":222,"expirationDate":20991231,"ltd":20991231},
				{"conid":111,"expirationDate":20990601,"ltd":20990601},
				{"conid":999,"expirationDate":20000101,"ltd":20000101}
			]}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	client := newTestIBKRClient(srv.URL)
	conid, err := client.resolveConid(context.Background(), "ZZ")
	if err != nil {
		t.Fatalf("resolveConid futures: %v", err)
	}
	if conid != "111" { // nearest expiry >= today
		t.Errorf("conid = %q; want 111 (nearest front-month)", conid)
	}
	if _, err := client.resolveConid(context.Background(), "ZZ"); err != nil {
		t.Fatalf("resolveConid (cached): %v", err)
	}
	if hits != 1 {
		t.Errorf("trsrv hit %d times; want 1 (second call cached)", hits)
	}
}

func TestIBKRFetchCandles_UnsupportedTimeframe(t *testing.T) {
	client := newTestIBKRClient("http://unused")
	_, err := client.FetchCandles(context.Background(), "EUR/USD", Timeframe("3m"), time.Now(), time.Now())
	if err == nil {
		t.Fatal("expected error for unsupported timeframe")
	}
}

func TestIBKRFetchCandles_MalformedJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{not valid json`))
	}))
	defer srv.Close()

	client := newTestIBKRClient(srv.URL)
	from := time.Now().Add(-time.Hour)
	to := time.Now()

	_, err := client.FetchCandles(context.Background(), "EUR/USD", Timeframe1Hour, from, to)
	if err == nil {
		t.Fatal("expected error on malformed JSON")
	}
}

// ── FetchQuote ────────────────────────────────────────────────────────────────

func TestIBKRFetchQuote(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/api/iserver/marketdata/snapshot" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.URL.Query().Get("conids") != "12087792" {
			t.Errorf("expected conids=12087792, got %s", r.URL.Query().Get("conids"))
		}
		if r.URL.Query().Get("fields") != "84,85,86" {
			t.Errorf("expected fields=84,85,86, got %s", r.URL.Query().Get("fields"))
		}
		w.Header().Set("Content-Type", "application/json")
		// Field 84=bid, 85=ask, 86=last — all string values as IBKR returns them.
		_, _ = w.Write([]byte(`[{"conid":12087792,"84":"1.0950","85":"1.0952","86":"1.0951"}]`))
	}))
	defer srv.Close()

	client := newTestIBKRClient(srv.URL)
	quote, err := client.FetchQuote(context.Background(), "EUR/USD")
	if err != nil {
		t.Fatalf("FetchQuote error: %v", err)
	}
	if quote.Ticker != "EUR/USD" {
		t.Errorf("expected ticker EUR/USD, got %s", quote.Ticker)
	}
	if quote.Bid != 1.0950 {
		t.Errorf("expected bid 1.0950, got %f", quote.Bid)
	}
	if quote.Ask != 1.0952 {
		t.Errorf("expected ask 1.0952, got %f", quote.Ask)
	}
	if quote.Last != 1.0951 {
		t.Errorf("expected last 1.0951, got %f", quote.Last)
	}
	if quote.Timestamp.IsZero() {
		t.Error("expected non-zero timestamp")
	}
}

func TestIBKRFetchQuote_UnknownPair(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/api/iserver/secdef/search" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	client := newTestIBKRClient(srv.URL)
	_, err := client.FetchQuote(context.Background(), "BTC/ETH")
	if err == nil {
		t.Fatal("expected error for unknown pair")
	}
}

func TestIBKRFetchQuote_EmptyResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	client := newTestIBKRClient(srv.URL)
	_, err := client.FetchQuote(context.Background(), "EUR/USD")
	if err == nil {
		t.Fatal("expected error for empty snapshot response")
	}
}

// ── FetchQuote priming retry ──────────────────────────────────────────────────

func TestIBKRFetchQuotePriming(t *testing.T) {
	// IBKR snapshot quirk: first call returns empty/zero bid+ask (priming),
	// second call returns real data. Verify the client retries automatically.
	var callCount int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&callCount, 1)
		w.Header().Set("Content-Type", "application/json")
		if n == 1 {
			// Priming response — bid and ask both absent (parse to 0).
			_, _ = w.Write([]byte(`[{"conid":12087792,"86":"1.0951"}]`))
			return
		}
		// Real data on second call.
		_, _ = w.Write([]byte(`[{"conid":12087792,"84":"1.0950","85":"1.0952","86":"1.0951"}]`))
	}))
	defer srv.Close()

	client := newTestIBKRClient(srv.URL)
	// Speed up the 300ms wait in tests.
	before := time.Now()
	quote, err := client.FetchQuote(context.Background(), "EUR/USD")
	elapsed := time.Since(before)

	if err != nil {
		t.Fatalf("FetchQuote (priming) error: %v", err)
	}
	if atomic.LoadInt32(&callCount) != 2 {
		t.Errorf("expected 2 HTTP calls (priming + retry), got %d", callCount)
	}
	if quote.Bid != 1.0950 {
		t.Errorf("expected bid 1.0950 after retry, got %f", quote.Bid)
	}
	if quote.Ask != 1.0952 {
		t.Errorf("expected ask 1.0952 after retry, got %f", quote.Ask)
	}
	// Verify the 300ms wait actually happened.
	if elapsed < 300*time.Millisecond {
		t.Errorf("expected at least 300ms wait for priming retry, elapsed %v", elapsed)
	}
}

func TestIBKRFetchQuotePriming_NoRetryWhenDataPresent(t *testing.T) {
	// If the first response has real bid+ask data, no retry should occur.
	var callCount int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&callCount, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"conid":12087792,"84":"1.0950","85":"1.0952","86":"1.0951"}]`))
	}))
	defer srv.Close()

	client := newTestIBKRClient(srv.URL)
	_, err := client.FetchQuote(context.Background(), "EUR/USD")
	if err != nil {
		t.Fatalf("FetchQuote error: %v", err)
	}
	if atomic.LoadInt32(&callCount) != 1 {
		t.Errorf("expected exactly 1 HTTP call (no priming retry needed), got %d", callCount)
	}
}

// ── FetchHistory ──────────────────────────────────────────────────────────────

func TestIBKRFetchHistory(t *testing.T) {
	// The server captures the requested period to verify it is derived correctly.
	var capturedPeriod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedPeriod = r.URL.Query().Get("period")
		w.Header().Set("Content-Type", "application/json")
		// Return 5 candles spaced 1 hour apart ending now-ish.
		now := time.Now().UTC()
		bars := ""
		for i := 4; i >= 0; i-- {
			ts := now.Add(-time.Duration(i) * time.Hour)
			if bars != "" {
				bars += ","
			}
			bars += `{"o":1.09,"h":1.10,"l":1.08,"c":1.092,"v":0,"t":` +
				itoa(ts.UnixMilli()) + `}`
		}
		_, _ = w.Write([]byte(`{"data":[` + bars + `]}`))
	}))
	defer srv.Close()

	client := newTestIBKRClient(srv.URL)
	// Request 3 candles at 1-hour timeframe → period should cover ~3h (well within 24h → "1d").
	candles, err := client.FetchHistory(context.Background(), "EUR/USD", Timeframe1Hour, 3)
	if err != nil {
		t.Fatalf("FetchHistory error: %v", err)
	}
	if capturedPeriod != "1d" {
		t.Errorf("expected period=1d for 3 x 1h, got %q", capturedPeriod)
	}
	// Should return at most 3 candles even if server returned more.
	if len(candles) > 3 {
		t.Errorf("expected at most 3 candles, got %d", len(candles))
	}
}

func TestIBKRFetchHistory_UnsupportedTimeframe(t *testing.T) {
	client := newTestIBKRClient("http://unused")
	_, err := client.FetchHistory(context.Background(), "EUR/USD", Timeframe("bad"), 5)
	if err == nil {
		t.Fatal("expected error for unsupported timeframe")
	}
}

// ── computePeriod ─────────────────────────────────────────────────────────────

func TestComputePeriod(t *testing.T) {
	base := time.Date(2024, 4, 1, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name     string
		hours    float64
		expected string
	}{
		{"exactly 1h", 1, "1d"},
		{"exactly 24h", 24, "1d"},
		{"25h", 25, "2d"},
		{"exactly 48h", 48, "2d"},
		{"49h", 49, "3d"},
		{"exactly 72h", 72, "3d"},
		{"73h", 73, "5d"},
		{"exactly 120h", 120, "5d"},
		{"121h", 121, "1w"},
		{"exactly 168h", 168, "1w"},
		{"169h", 169, "1m"},
		{"exactly 720h", 720, "1m"},
		{"721h", 721, "1y"},
		{"2000h", 2000, "1y"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			to := base.Add(time.Duration(tc.hours * float64(time.Hour)))
			got := computePeriod(base, to)
			if got != tc.expected {
				t.Errorf("computePeriod(%gh) = %q, want %q", tc.hours, got, tc.expected)
			}
		})
	}
}

// ── helpers ───────────────────────────────────────────────────────────────────

// itoa converts an int64 to its decimal string representation without importing
// strconv in the test file (strconv is already used in ibkr.go; this avoids a
// test-only import that would otherwise look redundant).
func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	buf := make([]byte, 0, 20)
	for n > 0 {
		buf = append([]byte{byte('0' + n%10)}, buf...)
		n /= 10
	}
	if neg {
		buf = append([]byte{'-'}, buf...)
	}
	return string(buf)
}
