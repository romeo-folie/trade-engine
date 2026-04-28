package execution

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// ─── FetchTradeRecords ────────────────────────────────────────────────────────

func TestFetchTradeRecords(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/api/iserver/account/trades" {
			t.Errorf("unexpected path: %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		days := r.URL.Query().Get("days")
		if days != "7" {
			t.Errorf("days query param = %q; want 7", days)
		}
		w.Header().Set("Content-Type", "application/json")
		// Two rows: one EUR/USD BUY fill and one unfilled order that should be skipped.
		w.Write([]byte(`[
			{
				"orderId": 123456789,
				"symbol": "EUR.USD",
				"side": "B",
				"size": 5000,
				"price": 1.1750,
				"status": "Filled",
				"trade_time": "20260428-08:17:02",
				"conid": 12087792
			},
			{
				"orderId": 999999999,
				"symbol": "GBP.USD",
				"side": "S",
				"size": 5000,
				"price": 1.3500,
				"status": "Submitted",
				"trade_time": "20260428-09:00:00",
				"conid": 12087797
			}
		]`))
	}))
	defer srv.Close()

	b := newTestIBKRBroker(srv.URL)
	records, err := b.FetchTradeRecords(t.Context(), 7)
	if err != nil {
		t.Fatalf("FetchTradeRecords error: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("len(records) = %d; want 1 (unfilled row should be skipped)", len(records))
	}
	r := records[0]
	if r.OrderID != "123456789" {
		t.Errorf("OrderID = %q; want 123456789", r.OrderID)
	}
	// conid 12087792 reverse-resolves to "EUR/USD" via the hardcoded map.
	if r.Symbol != "EUR/USD" {
		t.Errorf("Symbol = %q; want EUR/USD", r.Symbol)
	}
	if r.Side != "buy" {
		t.Errorf("Side = %q; want buy", r.Side)
	}
	if r.Qty != 5000 {
		t.Errorf("Qty = %v; want 5000", r.Qty)
	}
	if r.Price != 1.1750 {
		t.Errorf("Price = %v; want 1.1750", r.Price)
	}
	if r.FilledAt.IsZero() {
		t.Error("FilledAt should not be zero")
	}
}

func TestFetchTradeRecords_DaysClamped(t *testing.T) {
	var gotDays string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotDays = r.URL.Query().Get("days")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	b := newTestIBKRBroker(srv.URL)

	// Over the max — should clamp to 7.
	if _, err := b.FetchTradeRecords(t.Context(), 99); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotDays != "7" {
		t.Errorf("days (clamped from 99) = %q; want 7", gotDays)
	}

	// Under the min — should clamp to 1.
	if _, err := b.FetchTradeRecords(t.Context(), 0); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotDays != "1" {
		t.Errorf("days (clamped from 0) = %q; want 1", gotDays)
	}
}
