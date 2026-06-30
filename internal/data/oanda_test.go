package data

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestOANDAInstrument(t *testing.T) {
	if got := oandaInstrument("EUR/USD"); got != "EUR_USD" {
		t.Errorf("oandaInstrument(EUR/USD) = %q; want EUR_USD", got)
	}
	if got := oandaInstrument("XAU/USD"); got != "XAU_USD" {
		t.Errorf("oandaInstrument(XAU/USD) = %q; want XAU_USD", got)
	}
}

func TestOANDAFetchCandles(t *testing.T) {
	to := time.Date(2026, 4, 13, 8, 3, 0, 0, time.UTC)
	all := []struct {
		ts         string
		o, h, l, c string
		vol        int
		complete   bool
	}{
		{"2026-04-13T08:00:00Z", "1.1000", "1.1010", "1.0995", "1.1005", 50, true},
		{"2026-04-13T08:01:00Z", "1.1005", "1.1020", "1.1000", "1.1018", 60, true},
		{"2026-04-13T08:03:00Z", "1.1018", "1.1025", "1.1012", "1.1020", 70, true},  // == to → excluded
		{"2026-04-13T08:04:00Z", "1.1020", "1.1021", "1.1019", "1.1020", 10, false}, // incomplete → dropped
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v3/instruments/EUR_USD/candles" {
			t.Errorf("unexpected path: %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		if got := r.URL.Query().Get("granularity"); got != "M1" {
			t.Errorf("granularity = %q; want M1", got)
		}
		// The P1 fix: never send `count` when both from and to are present (OANDA 400s).
		if got := r.URL.Query().Get("count"); got != "" {
			t.Errorf("count = %q; want empty when from+to are set", got)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("Authorization = %q; want Bearer test-token", got)
		}
		// Emulate OANDA: return only candles at/after the `from` cursor.
		from, _ := time.Parse(time.RFC3339, r.URL.Query().Get("from"))
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"instrument":"EUR_USD","granularity":"M1","candles":[`))
		first := true
		for _, c := range all {
			ts, _ := time.Parse(time.RFC3339, c.ts)
			if ts.Before(from) {
				continue
			}
			if !first {
				w.Write([]byte(","))
			}
			first = false
			fmt.Fprintf(w, `{"time":%q,"volume":%d,"complete":%t,"mid":{"o":%q,"h":%q,"l":%q,"c":%q}}`,
				c.ts, c.vol, c.complete, c.o, c.h, c.l, c.c)
		}
		w.Write([]byte(`]}`))
	}))
	defer srv.Close()

	c := NewOANDAClient("test-token", "", srv.URL)
	candles, err := c.FetchCandles(t.Context(), "EUR/USD", Timeframe1Min,
		time.Date(2026, 4, 13, 8, 0, 0, 0, time.UTC), to)
	if err != nil {
		t.Fatalf("FetchCandles: %v", err)
	}
	// 08:00 and 08:01 kept; 08:03 == `to` excluded (half-open); 08:04 incomplete dropped.
	if len(candles) != 2 {
		t.Fatalf("len(candles) = %d; want 2 (to-exclusive + incomplete dropped)", len(candles))
	}
	c0 := candles[0]
	if c0.Open != 1.1000 || c0.High != 1.1010 || c0.Low != 1.0995 || c0.Close != 1.1005 {
		t.Errorf("candle[0] OHLC = %v/%v/%v/%v; want 1.1000/1.1010/1.0995/1.1005", c0.Open, c0.High, c0.Low, c0.Close)
	}
	if c0.Ticker != "EUR/USD" || c0.Timeframe != Timeframe1Min {
		t.Errorf("candle[0] ticker/tf = %q/%q; want EUR/USD/1m", c0.Ticker, c0.Timeframe)
	}
	if c0.Volume != 50 {
		t.Errorf("candle[0] volume = %d; want 50", c0.Volume)
	}
}

func TestOANDAFetchCandles_UnsupportedTimeframe(t *testing.T) {
	c := NewOANDAClient("t", "", "http://unused")
	if _, err := c.FetchCandles(t.Context(), "EUR/USD", Timeframe("3m"),
		time.Now().Add(-time.Hour), time.Now()); err == nil {
		t.Error("expected error for unsupported timeframe, got nil")
	}
}

func TestOANDAFetchQuote(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v3/accounts/acc-1/pricing" {
			t.Errorf("unexpected path: %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(`{"prices":[{"instrument":"EUR_USD","time":"2026-04-13T08:00:00Z",
			"bids":[{"price":"1.1000"}],"asks":[{"price":"1.1002"}]}]}`))
	}))
	defer srv.Close()

	c := NewOANDAClient("t", "acc-1", srv.URL)
	q, err := c.FetchQuote(t.Context(), "EUR/USD")
	if err != nil {
		t.Fatalf("FetchQuote: %v", err)
	}
	if q.Bid != 1.1000 || q.Ask != 1.1002 {
		t.Errorf("bid/ask = %v/%v; want 1.1000/1.1002", q.Bid, q.Ask)
	}
	if q.Last != (1.1000+1.1002)/2 {
		t.Errorf("last = %v; want midpoint", q.Last)
	}
}

func TestOANDAFetchQuote_NoAccountID(t *testing.T) {
	c := NewOANDAClient("t", "", "http://unused")
	if _, err := c.FetchQuote(t.Context(), "EUR/USD"); err == nil {
		t.Error("expected error when account ID is unset, got nil")
	}
}
