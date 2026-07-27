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
				"order_ref": "currency-strength-2026-04-28-EURUSD",
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
	if r.TradeID != "currency-strength-2026-04-28-EURUSD" {
		t.Errorf("TradeID = %q; want propagated order_ref", r.TradeID)
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

// ─── GetWorkingOrders ───────────────────────────────────────────────────────

// TestGetWorkingOrders verifies the live-orders lookup: only working orders are
// returned (terminal states dropped), conid reverse-resolves to a ticker, side is
// normalized, and orderId decodes whether the gateway sends it as a number or a
// quoted string (the ibkrID two-branch decode the time-exit's cancel-by-id relies on).
func TestGetWorkingOrders(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/api/iserver/account/orders" {
			t.Errorf("unexpected path: %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		// Four rows: a working TP (number id, number price), a working SL (string id,
		// string stop_price), a filled order and a cancelled order (both dropped).
		w.Write([]byte(`{"orders":[
			{"orderId": 111, "order_ref": "currency-strength-exit-2026-07-27-EURUSD", "conid": 12087792, "ticker": "EUR", "side": "SELL", "status": "PreSubmitted", "remainingQuantity": 5000, "price": 1.12},
			{"orderId": "222", "conid": 12087792, "ticker": "EUR", "side": "SELL", "status": "Submitted", "remainingQuantity": "5000", "stop_price": "1.09"},
			{"orderId": 333, "conid": 12087792, "ticker": "EUR", "side": "SELL", "status": "Filled", "remainingQuantity": 0},
			{"orderId": 444, "conid": 12087797, "ticker": "GBP", "side": "BUY", "status": "Cancelled", "remainingQuantity": 5000}
		]}`))
	}))
	defer srv.Close()

	b := newTestIBKRBroker(srv.URL)
	orders, err := b.GetWorkingOrders(t.Context())
	if err != nil {
		t.Fatalf("GetWorkingOrders error: %v", err)
	}
	if len(orders) != 2 {
		t.Fatalf("len(orders) = %d; want 2 (filled + cancelled dropped)", len(orders))
	}
	// Both working orders are EUR/USD (conid 12087792 reverse-resolves via the map).
	for _, o := range orders {
		if o.Ticker != "EUR/USD" {
			t.Errorf("Ticker = %q; want EUR/USD", o.Ticker)
		}
		if o.Side != "sell" {
			t.Errorf("Side = %q; want sell", o.Side)
		}
		if o.RemainingQty != 5000 {
			t.Errorf("RemainingQty = %v; want 5000", o.RemainingQty)
		}
	}
	if orders[0].OrderID != "111" {
		t.Errorf("orders[0].OrderID = %q; want 111 (numeric id)", orders[0].OrderID)
	}
	if orders[0].TradeID != "currency-strength-exit-2026-07-27-EURUSD" {
		t.Errorf("orders[0].TradeID = %q; want propagated order_ref", orders[0].TradeID)
	}
	if orders[1].OrderID != "222" {
		t.Errorf("orders[1].OrderID = %q; want 222 (string id)", orders[1].OrderID)
	}
	// Leg prices decode from both number (price) and string (stop_price) encodings.
	if orders[0].Price != 1.12 {
		t.Errorf("orders[0].Price = %v; want 1.12 (numeric)", orders[0].Price)
	}
	if orders[1].StopPrice != 1.09 {
		t.Errorf("orders[1].StopPrice = %v; want 1.09 (string-encoded)", orders[1].StopPrice)
	}
}
