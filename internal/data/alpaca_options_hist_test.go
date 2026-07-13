package data

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestListContracts_QueriesBothStatuses proves the historical fix: the listing must
// request BOTH status=active AND status=inactive (expired 2024 contracts are inactive
// now) and merge them, deduped by symbol.
func TestListContracts_QueriesBothStatuses(t *testing.T) {
	var mu sync.Mutex
	statuses := map[string]bool{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		st := r.URL.Query().Get("status")
		mu.Lock()
		statuses[st] = true
		mu.Unlock()
		// Return one status-specific contract plus a shared one (to prove dedup).
		body := `{"option_contracts":[
			{"symbol":"SPY240607C00530000","underlying_symbol":"SPY","expiration_date":"2024-06-07","strike_price":"530","type":"call"},
			{"symbol":"SPY240607C00999000","underlying_symbol":"SPY","expiration_date":"2024-06-07","strike_price":"999","type":"call"}]}`
		if st == "inactive" {
			body = `{"option_contracts":[
				{"symbol":"SPY240607C00530000","underlying_symbol":"SPY","expiration_date":"2024-06-07","strike_price":"530","type":"call"},
				{"symbol":"SPY240607C00540000","underlying_symbol":"SPY","expiration_date":"2024-06-07","strike_price":"540","type":"call"}]}`
		}
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	a := NewAlpacaOptionsHist("k", "s", srv.URL, srv.URL, 0.03)
	from := time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2024, 6, 8, 0, 0, 0, 0, time.UTC)
	cs, err := a.ListContracts(context.Background(), "SPY", "call", from, to)
	if err != nil {
		t.Fatalf("ListContracts: %v", err)
	}
	mu.Lock()
	gotBoth := statuses["active"] && statuses["inactive"]
	mu.Unlock()
	if !gotBoth {
		t.Errorf("expected both active+inactive statuses queried, got %v", statuses)
	}
	// 530 (shared, deduped) + 999 (active) + 540 (inactive) = 3 unique.
	if len(cs) != 3 {
		t.Fatalf("expected 3 merged unique contracts, got %d: %+v", len(cs), cs)
	}
	seen := map[string]bool{}
	for _, c := range cs {
		if seen[c.OCCSymbol] {
			t.Errorf("duplicate contract %s not deduped", c.OCCSymbol)
		}
		seen[c.OCCSymbol] = true
	}
}

// TestQuoteAt_ModeledFromBars: with no NBBO quotes, QuoteAt takes the newest bar
// close as the last-trade price and brackets it with the modeled spread.
func TestQuoteAt_ModeledFromBars(t *testing.T) {
	var gotPath, gotSort, gotTF string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotSort = r.URL.Query().Get("sort")
		gotTF = r.URL.Query().Get("timeframe")
		_, _ = w.Write([]byte(`{"bars":{"SPY240607C00530000":[{"t":"2024-06-07T13:45:00Z","c":4.00}]}}`))
	}))
	defer srv.Close()

	a := NewAlpacaOptionsHist("k", "s", srv.URL, srv.URL, 0.10) // 10% modeled spread
	asOf := time.Date(2024, 6, 7, 13, 46, 0, 0, time.UTC)
	bid, ask, ok := a.QuoteAt(context.Background(), "SPY240607C00530000", asOf)
	if !ok {
		t.Fatal("expected a modeled quote from the bar close")
	}
	// close 4.00, 10% full spread → half 5% → bid 3.80, ask 4.20, mid 4.00.
	if bid != 3.80 || ask != 4.20 {
		t.Errorf("modeled (bid,ask) = (%v,%v); want (3.80,4.20)", bid, ask)
	}
	if gotPath != "/v1beta1/options/bars" || gotTF != "1Min" || gotSort != "desc" {
		t.Errorf("expected bars endpoint 1Min desc; got path=%q tf=%q sort=%q", gotPath, gotTF, gotSort)
	}
}

// TestQuoteAt_ExcludesInProgressBar: the bar starting exactly at asOf covers
// [asOf, asOf+1m) — its close peeks past asOf — so it must be skipped in favour of
// the newest COMPLETED bar (no lookahead).
func TestQuoteAt_ExcludesInProgressBar(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// desc: in-progress 13:46 bar first (close 9.99 = post-breakout), then completed 13:45.
		_, _ = w.Write([]byte(`{"bars":{"SPY240607C00530000":[{"t":"2024-06-07T13:46:00Z","c":9.99},{"t":"2024-06-07T13:45:00Z","c":4.00}]}}`))
	}))
	defer srv.Close()

	a := NewAlpacaOptionsHist("k", "s", srv.URL, srv.URL, 0.0)
	asOf := time.Date(2024, 6, 7, 13, 46, 0, 0, time.UTC) // == the in-progress bar's start
	bid, _, ok := a.QuoteAt(context.Background(), "SPY240607C00530000", asOf)
	if !ok {
		t.Fatal("expected the completed 13:45 bar to be used")
	}
	if bid != 4.00 { // the completed bar's close, NOT the in-progress 9.99
		t.Errorf("price = %v; want 4.00 (completed bar, not the in-progress 9.99 lookahead)", bid)
	}
}

// TestQuoteAt_RejectsFutureBar: a bar stamped after asOf must not be used.
func TestQuoteAt_RejectsFutureBar(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"bars":{"SPY240607C00530000":[{"t":"2024-06-07T14:00:00Z","c":4.00}]}}`))
	}))
	defer srv.Close()

	a := NewAlpacaOptionsHist("k", "s", srv.URL, srv.URL, 0.03)
	asOf := time.Date(2024, 6, 7, 13, 46, 0, 0, time.UTC) // before the returned 14:00 bar
	if _, _, ok := a.QuoteAt(context.Background(), "SPY240607C00530000", asOf); ok {
		t.Errorf("expected ok=false for a bar stamped after asOf")
	}
}

// TestQuotesAt_BatchNewestPerSymbol: QuotesAt takes each symbol's newest bar close
// (desc → first) and brackets it with the modeled spread.
func TestQuotesAt_BatchNewestPerSymbol(t *testing.T) {
	var gotSymbols, gotSort string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSymbols = r.URL.Query().Get("symbols")
		gotSort = r.URL.Query().Get("sort")
		// desc → newest first per symbol; A has a later bar (close 4.00) then older.
		_, _ = w.Write([]byte(`{"bars":{
			"SPY240607C00530000":[{"t":"2024-06-07T13:45:00Z","c":4.00},{"t":"2024-06-07T13:40:00Z","c":3.50}],
			"SPY240607C00540000":[{"t":"2024-06-07T13:44:00Z","c":2.00}]}}`))
	}))
	defer srv.Close()

	a := NewAlpacaOptionsHist("k", "s", srv.URL, srv.URL, 0.0) // 0 spread → bid=ask=close
	asOf := time.Date(2024, 6, 7, 13, 46, 0, 0, time.UTC)
	out, err := a.QuotesAt(context.Background(), []string{"SPY240607C00530000", "SPY240607C00540000"}, asOf)
	if err != nil {
		t.Fatalf("QuotesAt: %v", err)
	}
	if gotSort != "desc" {
		t.Errorf("expected sort=desc, got %q", gotSort)
	}
	if gotSymbols == "" || len(out) != 2 {
		t.Fatalf("symbols=%q, out=%v", gotSymbols, out)
	}
	if out["SPY240607C00530000"].Bid != 4.00 { // newest bar close, not the older 3.50
		t.Errorf("A newest = %v; want 4.00", out["SPY240607C00530000"].Bid)
	}
	if out["SPY240607C00540000"].Ask != 2.00 {
		t.Errorf("B = %v; want 2.00", out["SPY240607C00540000"].Ask)
	}
}

// TestQuotesAt_PagesUntilAllSymbolsResolved: Alpaca caps `limit` across ALL symbols,
// so a busy contract can fill page 1 and push another symbol to page 2. QuotesAt must
// page until every requested symbol resolves.
func TestQuotesAt_PagesUntilAllSymbolsResolved(t *testing.T) {
	var pages int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page_token") == "" {
			pages++
			_, _ = w.Write([]byte(`{"bars":{"AAA240607C00530000":[{"t":"2024-06-07T13:45:00Z","c":4.00}]},"next_page_token":"p2"}`))
			return
		}
		pages++
		_, _ = w.Write([]byte(`{"bars":{"BBB240607C00540000":[{"t":"2024-06-07T13:44:00Z","c":2.00}]},"next_page_token":""}`))
	}))
	defer srv.Close()

	a := NewAlpacaOptionsHist("k", "s", srv.URL, srv.URL, 0.0)
	asOf := time.Date(2024, 6, 7, 13, 46, 0, 0, time.UTC)
	out, err := a.QuotesAt(context.Background(), []string{"AAA240607C00530000", "BBB240607C00540000"}, asOf)
	if err != nil {
		t.Fatalf("QuotesAt: %v", err)
	}
	if pages != 2 {
		t.Errorf("expected 2 pages walked, got %d", pages)
	}
	if len(out) != 2 || out["AAA240607C00530000"].Bid != 4.00 || out["BBB240607C00540000"].Ask != 2.00 {
		t.Fatalf("both symbols must resolve across pages, got %v", out)
	}
}

// TestQuotesAt_ChunksOver100Symbols guards the Alpaca 100-symbol cap: a >100-symbol
// request must be split into multiple ≤100 batches (one over-cap request 400s).
func TestQuotesAt_ChunksOver100Symbols(t *testing.T) {
	var maxBatch, requests int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		syms := strings.Split(r.URL.Query().Get("symbols"), ",")
		if len(syms) > maxBatch {
			maxBatch = len(syms)
		}
		if len(syms) > 100 {
			http.Error(w, `{"message":"too many symbols"}`, http.StatusBadRequest)
			return
		}
		var b strings.Builder
		b.WriteString(`{"bars":{`)
		for i, s := range syms {
			if i > 0 {
				b.WriteString(",")
			}
			fmt.Fprintf(&b, `%q:[{"t":"2024-06-07T13:45:00Z","c":4.00}]`, s)
		}
		b.WriteString(`}}`)
		_, _ = w.Write([]byte(b.String()))
	}))
	defer srv.Close()

	a := NewAlpacaOptionsHist("k", "s", srv.URL, srv.URL, 0.0)
	syms := make([]string, 250)
	for i := range syms {
		syms[i] = fmt.Sprintf("SPY240607C%08d", 500000+i*1000)
	}
	out, err := a.QuotesAt(context.Background(), syms, time.Date(2024, 6, 7, 13, 46, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("QuotesAt: %v", err)
	}
	if maxBatch > 100 {
		t.Errorf("a batch requested %d symbols; Alpaca caps at 100", maxBatch)
	}
	if requests < 3 { // 250 → 100+100+50
		t.Errorf("expected ≥3 chunked requests, got %d", requests)
	}
	if len(out) != 250 {
		t.Errorf("expected all 250 symbols resolved across chunks, got %d", len(out))
	}
}

// TestAlpacaOptionHistoryStart_ETAnchored: the data floor must be ET midnight, so the
// backtest's ET-day iteration lands on the intended day. A UTC-midnight value would
// convert to the prior ET evening → floor to Jan 14, one day early.
func TestAlpacaOptionHistoryStart_ETAnchored(t *testing.T) {
	et, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("no tzdata")
	}
	got := AlpacaOptionHistoryStart.In(et)
	if got.Year() != 2024 || got.Month() != time.January || got.Day() != 15 || got.Hour() != 0 {
		t.Errorf("floor in ET = %v; want 2024-01-15 00:00 ET (a UTC-midnight floor would land on Jan 14)", got)
	}
}
