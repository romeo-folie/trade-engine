package execution

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/romeo-folie/trade-engine/internal/config"
)

// newTestIBKRBroker creates an IBKRBroker pointed at the supplied test server URL.
func newTestIBKRBroker(serverURL string) *IBKRBroker {
	return NewIBKRBroker(config.IBKRConfig{
		AccountID:   "U123456",
		GatewayURL:  serverURL,
		InsecureTLS: false, // test server uses plain HTTP
	})
}

// ─── PlaceOrder ───────────────────────────────────────────────────────────────

func TestIBKRPlaceOrder(t *testing.T) {
	var capturedBody map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/iserver/account/U123456/orders":
			if err := json.NewDecoder(r.Body).Decode(&capturedBody); err != nil {
				t.Errorf("decode request body: %v", err)
			}
			w.Header().Set("Content-Type", "application/json")
			// Direct success — no confirmation prompt.
			w.Write([]byte(`[{"order_id":"ORD001","order_status":"Submitted"}]`))
		default:
			t.Errorf("unexpected request to %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	b := newTestIBKRBroker(srv.URL)
	result, err := b.PlaceOrder(t.Context(), OrderRequest{
		Ticker:    "EUR/USD",
		Side:      OrderSideBuy,
		Type:      OrderTypeStop,
		Quantity:  100000,
		StopPrice: 1.095,
		TradeID:   "trade-abc",
	})
	if err != nil {
		t.Fatalf("PlaceOrder error: %v", err)
	}

	if result.BrokerOrderID != "ORD001" {
		t.Errorf("BrokerOrderID = %q; want ORD001", result.BrokerOrderID)
	}
	if result.Status != "pending" {
		t.Errorf("Status = %q; want pending", result.Status)
	}

	// Verify the JSON body sent to the gateway.
	orders, ok := capturedBody["orders"].([]any)
	if !ok || len(orders) != 1 {
		t.Fatalf("expected 1 order in body, got %v", capturedBody)
	}
	o := orders[0].(map[string]any)
	// conid is sent as a JSON number (int) — after JSON round-trip it's float64.
	if o["conid"] != float64(12087792) {
		t.Errorf("conid = %v; want 12087792", o["conid"])
	}
	if o["side"] != "BUY" {
		t.Errorf("side = %v; want BUY", o["side"])
	}
	if o["orderType"] != "STP" {
		t.Errorf("orderType = %v; want STP", o["orderType"])
	}
	if o["price"] != 1.095 {
		t.Errorf("price = %v; want 1.095", o["price"])
	}
	if o["cOID"] != "trade-abc" {
		t.Errorf("cOID = %v; want trade-abc", o["cOID"])
	}
	if o["tif"] != "DAY" {
		t.Errorf("tif = %v; want DAY", o["tif"])
	}
	if o["secType"] != "CASH" {
		t.Errorf("secType = %v; want CASH", o["secType"])
	}
}

// ─── PlaceBracketOrder ────────────────────────────────────────────────────────

func TestIBKRPlaceBracketOrder(t *testing.T) {
	var capturedBody map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/iserver/account/U123456/orders" {
			if err := json.NewDecoder(r.Body).Decode(&capturedBody); err != nil {
				t.Errorf("decode request body: %v", err)
			}
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`[{"order_id":"ORD002","order_status":"PreSubmitted"}]`))
		}
	}))
	defer srv.Close()

	b := newTestIBKRBroker(srv.URL)
	result, err := b.PlaceOrder(t.Context(), OrderRequest{
		Ticker:          "GBP/USD",
		Side:            OrderSideBuy,
		Type:            OrderTypeStop,
		Quantity:        50000,
		StopPrice:       1.265,
		TradeID:         "trade-bracket",
		OrderClass:      "bracket",
		TakeProfitPrice: 1.275,
		StopLossPrice:   1.255,
	})
	if err != nil {
		t.Fatalf("PlaceOrder (bracket) error: %v", err)
	}
	if result.BrokerOrderID != "ORD002" {
		t.Errorf("BrokerOrderID = %q; want ORD002", result.BrokerOrderID)
	}

	orders, ok := capturedBody["orders"].([]any)
	if !ok || len(orders) != 3 {
		t.Fatalf("expected 3 orders in bracket, got %d: %v", len(orders), capturedBody)
	}

	parent := orders[0].(map[string]any)
	tp := orders[1].(map[string]any)
	sl := orders[2].(map[string]any)

	// Parent
	if parent["conid"] != float64(12087797) { // GBP/USD
		t.Errorf("parent conid = %v; want 12087797", parent["conid"])
	}
	if parent["orderType"] != "STP" {
		t.Errorf("parent orderType = %v; want STP", parent["orderType"])
	}
	if parent["price"] != 1.265 {
		t.Errorf("parent price = %v; want 1.265", parent["price"])
	}
	if parent["cOID"] != "trade-bracket" {
		t.Errorf("parent cOID = %v; want trade-bracket", parent["cOID"])
	}

	// Take-profit child
	if tp["orderType"] != "LMT" {
		t.Errorf("tp orderType = %v; want LMT", tp["orderType"])
	}
	if tp["price"] != 1.275 {
		t.Errorf("tp price = %v; want 1.275", tp["price"])
	}
	if tp["parentId"] != "trade-bracket" {
		t.Errorf("tp parentId = %v; want trade-bracket", tp["parentId"])
	}
	if tp["side"] != "SELL" {
		t.Errorf("tp side = %v; want SELL", tp["side"])
	}

	// Stop-loss child
	if sl["orderType"] != "STP" {
		t.Errorf("sl orderType = %v; want STP", sl["orderType"])
	}
	if sl["price"] != 1.255 {
		t.Errorf("sl price = %v; want 1.255", sl["price"])
	}
	if sl["parentId"] != "trade-bracket" {
		t.Errorf("sl parentId = %v; want trade-bracket", sl["parentId"])
	}
	if sl["side"] != "SELL" {
		t.Errorf("sl side = %v; want SELL", sl["side"])
	}
}

// ─── Order confirmation flow ──────────────────────────────────────────────────

func TestIBKROrderConfirmation(t *testing.T) {
	confirmCalled := false

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/iserver/account/U123456/orders":
			// First response: confirmation prompt (no order_id, has id).
			w.Write([]byte(`[{"id":"REPLY-99","message":["Order requires confirmation"]}]`))

		case "/iserver/reply/REPLY-99":
			// Verify the confirmation body.
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode confirm body: %v", err)
			}
			if body["confirmed"] != true {
				t.Errorf("confirmed = %v; want true", body["confirmed"])
			}
			confirmCalled = true
			// Return actual order result after confirmation.
			w.Write([]byte(`[{"order_id":"ORD003","order_status":"Submitted"}]`))

		default:
			t.Errorf("unexpected request to %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	b := newTestIBKRBroker(srv.URL)
	result, err := b.PlaceOrder(t.Context(), OrderRequest{
		Ticker:    "EUR/USD",
		Side:      OrderSideBuy,
		Type:      OrderTypeMarket,
		Quantity:  100000,
		TradeID:   "trade-confirm",
	})
	if err != nil {
		t.Fatalf("PlaceOrder (confirmation) error: %v", err)
	}
	if !confirmCalled {
		t.Error("expected confirmation reply to be called, but it was not")
	}
	if result.BrokerOrderID != "ORD003" {
		t.Errorf("BrokerOrderID = %q; want ORD003", result.BrokerOrderID)
	}
	if result.Status != "pending" {
		t.Errorf("Status = %q; want pending", result.Status)
	}
}

// ─── NormalizeIBKROrderStatus ─────────────────────────────────────────────────

func TestNormalizeIBKROrderStatus(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"Filled", "filled"},
		{"Cancelled", "cancelled"},
		{"ApiCancelled", "cancelled"},
		{"Inactive", "rejected"},
		{"Submitted", "pending"},
		{"PreSubmitted", "pending"},
		{"PendingSubmit", "pending"},
		{"PendingCancel", "pending"},
		{"", "pending"},
		{"Unknown", "pending"},
	}
	for _, tc := range cases {
		got := NormalizeIBKROrderStatus(tc.input)
		if got != tc.want {
			t.Errorf("NormalizeIBKROrderStatus(%q) = %q; want %q", tc.input, got, tc.want)
		}
	}
}

// ─── resolveConid ─────────────────────────────────────────────────────────────

func TestResolveConid_HardcodedMap(t *testing.T) {
	// No HTTP server needed — all lookups come from the hardcoded map.
	b := newTestIBKRBroker("http://unused")

	cases := map[string]string{
		// Forex (CASH)
		"EUR/USD": "12087792",
		"GBP/USD": "12087797",
		"USD/JPY": "12087802",
		"AUD/USD": "15016059",
		"USD/CHF": "12087820",
		"USD/CAD": "12087817",
		"NZD/USD": "15016065",
		"EUR/GBP": "12087811",
		// Metals (CFD)
		"XAU/USD": "69067924",
		"XAG/USD": "69067925",
	}
	for pair, wantConid := range cases {
		got, err := b.resolveConid(t.Context(), pair, "")
		if err != nil {
			t.Errorf("resolveConid(%q) error: %v", pair, err)
			continue
		}
		if got != wantConid {
			t.Errorf("resolveConid(%q) = %q; want %q", pair, got, wantConid)
		}
	}
}

func TestResolveConid_APIFallback(t *testing.T) {
	// XPT/USD is not in the hardcoded map, so it falls through to the API.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/iserver/secdef/search" && r.URL.Query().Get("symbol") == "XPT" {
			w.Header().Set("Content-Type", "application/json")
			// Simulate IBKR secdef search response.
			w.Write([]byte(`[
				{"conid": 12345678, "companyName": "XPT.USD", "description": "XPT/USD", "exchange": "IDEALPRO"},
				{"conid": 99999999, "companyName": "XPT.CHF", "description": "XPT/CHF", "exchange": "IDEALPRO"}
			]`))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	b := newTestIBKRBroker(srv.URL)

	conid, err := b.resolveConid(t.Context(), "XPT/USD", "CFD")
	if err != nil {
		t.Fatalf("resolveConid API fallback error: %v", err)
	}
	if conid != "12345678" {
		t.Errorf("conid = %q; want 12345678", conid)
	}

	// Second call must come from cache (server would 404 on repeat).
	conid2, err := b.resolveConid(t.Context(), "XPT/USD", "CFD")
	if err != nil {
		t.Fatalf("resolveConid (cached) error: %v", err)
	}
	if conid2 != conid {
		t.Errorf("cached conid = %q; want %q", conid2, conid)
	}
}

func TestResolveConid_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`[]`)) // empty result set
	}))
	defer srv.Close()

	b := newTestIBKRBroker(srv.URL)
	_, err := b.resolveConid(t.Context(), "ZZZ/YYY", "CASH")
	if err == nil {
		t.Error("expected error for unknown pair, got nil")
	}
}

// ─── CheckAuth ────────────────────────────────────────────────────────────────

func TestIBKRCheckAuth_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/iserver/auth/status" || r.Method != http.MethodGet {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"authenticated":true,"connected":true}`))
	}))
	defer srv.Close()

	b := newTestIBKRBroker(srv.URL)
	if err := b.CheckAuth(t.Context()); err != nil {
		t.Errorf("CheckAuth error: %v", err)
	}
}

func TestIBKRCheckAuth_Unauthenticated(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"authenticated":false,"connected":true}`))
	}))
	defer srv.Close()

	b := newTestIBKRBroker(srv.URL)
	if err := b.CheckAuth(t.Context()); err == nil {
		t.Error("expected error for unauthenticated session, got nil")
	}
}

func TestIBKRCheckAuth_Disconnected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"authenticated":true,"connected":false}`))
	}))
	defer srv.Close()

	b := newTestIBKRBroker(srv.URL)
	if err := b.CheckAuth(t.Context()); err == nil {
		t.Error("expected error for disconnected session, got nil")
	}
}

func TestIBKRCheckAuth_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "gateway unavailable", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	b := newTestIBKRBroker(srv.URL)
	if err := b.CheckAuth(t.Context()); err == nil {
		t.Error("expected error for HTTP 503, got nil")
	}
}
