package execution

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// ─── ibkrFloat ────────────────────────────────────────────────────────────────

func TestIBKRFloat_UnmarshalJSON(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want float64
	}{
		{"quoted string (real trades-endpoint shape)", `"1.1750"`, 1.1750},
		{"bare number", `1.1750`, 1.1750},
		{"quoted integer", `"5000"`, 5000},
		{"empty string", `""`, 0},
		{"null", `null`, 0},
		{"negative quoted", `"-12.5"`, -12.5},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var f ibkrFloat
			if err := json.Unmarshal([]byte(c.in), &f); err != nil {
				t.Fatalf("unmarshal %s: %v", c.in, err)
			}
			if float64(f) != c.want {
				t.Errorf("ibkrFloat(%s) = %v; want %v", c.in, float64(f), c.want)
			}
		})
	}

	// Malformed numeric strings must surface an error rather than silently zero.
	var f ibkrFloat
	if err := json.Unmarshal([]byte(`"not-a-number"`), &f); err == nil {
		t.Error("expected error decoding non-numeric string, got nil")
	}
}

// ─── FetchTradeRecords ────────────────────────────────────────────────────────

// TestFetchTradeRecords exercises the parse/filter/resolve logic with bare-number
// size/price (the ibkrFloat numeric branch). The real IBKR Client Portal sends
// these as quoted strings — that string branch is covered by
// TestFetchTradeRecords_StringEncodedNumerics, which is the regression guard for
// the production bug. Keep both: they cover the two ibkrFloat decode branches.
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

// TestFetchTradeRecords_StringEncodedNumerics mirrors the real IBKR Client Portal
// payload, which serializes size and price as quoted strings. Before ibkrFloat,
// this failed with "cannot unmarshal string into Go struct field .price of type
// float64", which silently broke every ORB exit reconcile in production.
func TestFetchTradeRecords_StringEncodedNumerics(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`[
			{
				"orderId": 123456789,
				"symbol": "EUR.USD",
				"side": "S",
				"size": "5000",
				"price": "1.16030",
				"status": "Filled",
				"trade_time": "20260428-08:17:02",
				"conid": 12087792
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
		t.Fatalf("len(records) = %d; want 1", len(records))
	}
	r := records[0]
	if r.Qty != 5000 {
		t.Errorf("Qty = %v; want 5000 (string-encoded size)", r.Qty)
	}
	if r.Price != 1.16030 {
		t.Errorf("Price = %v; want 1.16030 (string-encoded price)", r.Price)
	}
	if r.Side != "sell" {
		t.Errorf("Side = %q; want sell", r.Side)
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
