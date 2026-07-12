package data

import (
	"context"
	"net/http"
	"net/http/httptest"
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

	a := NewAlpacaOptionsHist("k", "s", srv.URL, srv.URL)
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

// TestQuoteAt_RequestsNewestQuote proves QuoteAt asks for the single newest quote at
// or before asOf (sort=desc, limit=1) rather than the first ascending page.
func TestQuoteAt_RequestsNewestQuote(t *testing.T) {
	var gotSort, gotLimit, gotEnd string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		gotSort, gotLimit, gotEnd = q.Get("sort"), q.Get("limit"), q.Get("end")
		_, _ = w.Write([]byte(`{"quotes":{"SPY240607C00530000":[{"t":"2024-06-07T13:45:00Z","bp":1.20,"ap":1.24}]}}`))
	}))
	defer srv.Close()

	a := NewAlpacaOptionsHist("k", "s", srv.URL, srv.URL)
	asOf := time.Date(2024, 6, 7, 13, 46, 0, 0, time.UTC)
	bid, ask, ok := a.QuoteAt(context.Background(), "SPY240607C00530000", asOf)
	if !ok || bid != 1.20 || ask != 1.24 {
		t.Fatalf("QuoteAt = (%v,%v,%v); want (1.20,1.24,true)", bid, ask, ok)
	}
	if gotSort != "desc" || gotLimit != "1" {
		t.Errorf("expected sort=desc limit=1, got sort=%q limit=%q", gotSort, gotLimit)
	}
	if gotEnd == "" {
		t.Errorf("expected end bound set to asOf, got empty")
	}
}

// TestQuoteAt_RejectsFutureQuote defends against a server ignoring the end bound: a
// quote stamped after asOf must not be used.
func TestQuoteAt_RejectsFutureQuote(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"quotes":{"SPY240607C00530000":[{"t":"2024-06-07T14:00:00Z","bp":1.20,"ap":1.24}]}}`))
	}))
	defer srv.Close()

	a := NewAlpacaOptionsHist("k", "s", srv.URL, srv.URL)
	asOf := time.Date(2024, 6, 7, 13, 46, 0, 0, time.UTC) // before the returned 14:00 quote
	if _, _, ok := a.QuoteAt(context.Background(), "SPY240607C00530000", asOf); ok {
		t.Errorf("expected ok=false for a quote stamped after asOf")
	}
}
