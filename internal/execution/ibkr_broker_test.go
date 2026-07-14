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
		case "/v1/api/iserver/account/U123456/orders":
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
		switch r.URL.Path {
		case "/v1/api/iserver/account/U123456/orders":
			if err := json.NewDecoder(r.Body).Decode(&capturedBody); err != nil {
				t.Errorf("decode request body: %v", err)
			}
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`[{"order_id":"1002","order_status":"PreSubmitted"}]`))
		case "/v1/api/iserver/account/orders":
			// Live order list used by verifyBracketLegIDs to identify legs structurally.
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"orders":[
				{"orderId":1002,"order_ref":"trade-bracket","orderType":"Stop"},
				{"orderId":1003,"parentId":1002,"orderType":"Limit"},
				{"orderId":1004,"parentId":1002,"orderType":"Stop"}
			]}`))
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
	if result.BrokerOrderID != "1002" {
		t.Errorf("BrokerOrderID = %q; want 1002", result.BrokerOrderID)
	}
	if len(result.ChildOrderIDs) != 2 || result.ChildOrderIDs[0] != "1003" || result.ChildOrderIDs[1] != "1004" {
		t.Errorf("ChildOrderIDs = %v; want [1003 1004]", result.ChildOrderIDs)
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

// TestIBKRBracketLegIDsCorrectedFromLiveOrders reproduces the 2026-07-14 XAG incident:
// the placement response (via the confirmation-prompt chain) returned the bracket's
// order IDs rotated — [TP, SL, parent] instead of [parent, TP, SL] — so the scheduler
// polled the ENTRY order as an exit leg and booked phantom exits. The broker must
// re-derive leg identity from the live order list (order_ref → parent, parentId +
// orderType → children) rather than trust array position.
func TestIBKRBracketLegIDsCorrectedFromLiveOrders(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/api/iserver/account/U123456/orders":
			w.Header().Set("Content-Type", "application/json")
			// Rotated: TP first, then SL, then the parent entry order.
			w.Write([]byte(`[
				{"order_id":"918","order_status":"PreSubmitted"},
				{"order_id":"919","order_status":"PreSubmitted"},
				{"order_id":"917","order_status":"Submitted"}
			]`))
		case "/v1/api/iserver/account/orders":
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"orders":[
				{"orderId":917,"order_ref":"orb-XAG/USD-1","orderType":"Stop"},
				{"orderId":918,"parentId":917,"orderType":"Limit"},
				{"orderId":919,"parentId":917,"orderType":"Stop"}
			]}`))
		}
	}))
	defer srv.Close()

	b := newTestIBKRBroker(srv.URL)
	result, err := b.PlaceOrder(t.Context(), OrderRequest{
		Ticker:          "GBP/USD",
		Side:            OrderSideBuy,
		Type:            OrderTypeStop,
		Quantity:        1600,
		StopPrice:       58.09,
		TradeID:         "orb-XAG/USD-1",
		OrderClass:      "bracket",
		TakeProfitPrice: 58.61,
		StopLossPrice:   57.83,
	})
	if err != nil {
		t.Fatalf("PlaceOrder error: %v", err)
	}
	if result.BrokerOrderID != "917" {
		t.Errorf("BrokerOrderID = %q; want 917 (the true parent, not the first array element)", result.BrokerOrderID)
	}
	if len(result.ChildOrderIDs) != 2 || result.ChildOrderIDs[0] != "918" || result.ChildOrderIDs[1] != "919" {
		t.Errorf("ChildOrderIDs = %v; want [918 919] (TP, SL identified by orderType)", result.ChildOrderIDs)
	}
}

// TestIBKRBracketLegVerifyUnavailableKeepsPositional confirms the fallback: when the
// live order list cannot identify the legs (e.g. gateway snapshot lag outlasting the
// retries), the positional assignment is kept rather than failing the placement.
func TestIBKRBracketLegVerifyUnavailableKeepsPositional(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/api/iserver/account/U123456/orders":
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`[
				{"order_id":"1","order_status":"Submitted"},
				{"order_id":"2","order_status":"PreSubmitted"},
				{"order_id":"3","order_status":"PreSubmitted"}
			]`))
		case "/v1/api/iserver/account/orders":
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"orders":[]}`))
		}
	}))
	defer srv.Close()

	b := newTestIBKRBroker(srv.URL)
	result, err := b.PlaceOrder(t.Context(), OrderRequest{
		Ticker:          "GBP/USD",
		Side:            OrderSideBuy,
		Type:            OrderTypeStop,
		Quantity:        1600,
		StopPrice:       1.25,
		TradeID:         "orb-GBP/USD-1",
		OrderClass:      "bracket",
		TakeProfitPrice: 1.26,
		StopLossPrice:   1.24,
	})
	if err != nil {
		t.Fatalf("PlaceOrder error: %v", err)
	}
	if result.BrokerOrderID != "1" {
		t.Errorf("BrokerOrderID = %q; want 1", result.BrokerOrderID)
	}
	if len(result.ChildOrderIDs) != 2 || result.ChildOrderIDs[0] != "2" || result.ChildOrderIDs[1] != "3" {
		t.Errorf("ChildOrderIDs = %v; want positional [2 3] fallback", result.ChildOrderIDs)
	}
}

// ─── Order confirmation flow ──────────────────────────────────────────────────

func TestIBKROrderConfirmation(t *testing.T) {
	confirmCalled := false

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/api/iserver/account/U123456/orders":
			// First response: confirmation prompt (no order_id, has id).
			w.Write([]byte(`[{"id":"REPLY-99","message":["Order requires confirmation"]}]`))

		case "/v1/api/iserver/reply/REPLY-99":
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

// TestIBKROrderConfirmation_TwoRound verifies that bracket placement survives
// two sequential confirmation prompts before receiving order IDs, which is the
// common real-world IBKR flow (price-check prompt + outside-RTH prompt).
func TestIBKROrderConfirmation_TwoRound(t *testing.T) {
	round := 0

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/api/iserver/account/U123456/orders":
			// Initial placement: first confirmation prompt.
			w.Write([]byte(`[{"id":"REPLY-A1","message":["Price check required"]}]`))
		case "/v1/api/iserver/reply/REPLY-A1":
			round++
			// First confirm resolves to a second prompt (outside-RTH warning).
			w.Write([]byte(`[{"id":"REPLY-A2","message":["Order outside regular trading hours"]}]`))
		case "/v1/api/iserver/reply/REPLY-A2":
			round++
			// Second confirm resolves to all three bracket order IDs — in the ROTATED
			// order the confirmation chain really produces (2026-07-14 XAG incident):
			// [TP, SL, parent], NOT [parent, TP, SL].
			w.Write([]byte(`[` +
				`{"order_id":"701","order_status":"PreSubmitted"},` +
				`{"order_id":"702","order_status":"PreSubmitted"},` +
				`{"order_id":"700","order_status":"Submitted"}` +
				`]`))
		case "/v1/api/iserver/account/orders":
			// Structural truth: parent by order_ref, children by parentId + orderType.
			w.Write([]byte(`{"orders":[
				{"orderId":700,"order_ref":"trade-tworound","orderType":"Stop"},
				{"orderId":701,"parentId":700,"orderType":"Limit"},
				{"orderId":702,"parentId":700,"orderType":"Stop"}
			]}`))
		default:
			t.Errorf("unexpected request to %s", r.URL.Path)
			http.NotFound(w, r)
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
		TradeID:         "trade-tworound",
		OrderClass:      "bracket",
		TakeProfitPrice: 1.275,
		StopLossPrice:   1.255,
	})
	if err != nil {
		t.Fatalf("PlaceOrder (two-round) error: %v", err)
	}
	if round != 2 {
		t.Errorf("expected 2 confirmation rounds, got %d", round)
	}
	if result.BrokerOrderID != "700" {
		t.Errorf("BrokerOrderID = %q; want 700 (the true parent despite the rotated response)", result.BrokerOrderID)
	}
	if len(result.ChildOrderIDs) != 2 {
		t.Errorf("ChildOrderIDs = %v; want [701 702]", result.ChildOrderIDs)
	} else {
		if result.ChildOrderIDs[0] != "701" {
			t.Errorf("ChildOrderIDs[0] = %q; want 701 (TP identified by orderType)", result.ChildOrderIDs[0])
		}
		if result.ChildOrderIDs[1] != "702" {
			t.Errorf("ChildOrderIDs[1] = %q; want 702 (SL identified by orderType)", result.ChildOrderIDs[1])
		}
	}
}

// TestIBKROrderConfirmation_FiveRounds verifies that when IBKR issues exactly
// maxConfirmRounds (5) sequential prompts, the final confirmation response that
// carries the order IDs is still parsed correctly (off-by-one guard).
func TestIBKROrderConfirmation_FiveRounds(t *testing.T) {
	round := 0

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v1/api/iserver/account/U123456/orders":
			w.Write([]byte(`[{"id":"R1","message":["prompt 1"]}]`))
		case r.URL.Path == "/v1/api/iserver/reply/R1":
			round++
			w.Write([]byte(`[{"id":"R2","message":["prompt 2"]}]`))
		case r.URL.Path == "/v1/api/iserver/reply/R2":
			round++
			w.Write([]byte(`[{"id":"R3","message":["prompt 3"]}]`))
		case r.URL.Path == "/v1/api/iserver/reply/R3":
			round++
			w.Write([]byte(`[{"id":"R4","message":["prompt 4"]}]`))
		case r.URL.Path == "/v1/api/iserver/reply/R4":
			round++
			w.Write([]byte(`[{"id":"R5","message":["prompt 5"]}]`))
		case r.URL.Path == "/v1/api/iserver/reply/R5":
			round++
			// 5th confirm finally returns order IDs — this is the off-by-one case.
			w.Write([]byte(`[{"order_id":"ORD-FINAL","order_status":"Submitted"}]`))
		default:
			t.Errorf("unexpected request to %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	b := newTestIBKRBroker(srv.URL)
	result, err := b.PlaceOrder(t.Context(), OrderRequest{
		Ticker:   "EUR/USD",
		Side:     OrderSideBuy,
		Type:     OrderTypeMarket,
		Quantity: 100000,
		TradeID:  "trade-fiverounds",
	})
	if err != nil {
		t.Fatalf("PlaceOrder (5-round) error: %v", err)
	}
	if round != 5 {
		t.Errorf("expected 5 confirmation rounds, got %d", round)
	}
	if result.BrokerOrderID != "ORD-FINAL" {
		t.Errorf("BrokerOrderID = %q; want ORD-FINAL", result.BrokerOrderID)
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
		"XAG/USD": "77124483",
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
		if r.URL.Path == "/v1/api/iserver/secdef/search" && r.URL.Query().Get("symbol") == "XPT" {
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

func TestResolveConidFutures_TrsrvDynamic(t *testing.T) {
	// "XYZ" is not in the static FuturesConids map, so FUT resolution falls through
	// to the dynamic /trsrv/futures path. The chain has an expired contract and two
	// future ones (out of order) — the resolver must pick the NEAREST future expiry.
	var trsrvHits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/api/trsrv/futures" && r.URL.Query().Get("symbols") == "XYZ" {
			trsrvHits++
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"XYZ":[
				{"symbol":"XYZ","conid":222,"expirationDate":20991231,"ltd":20991231},
				{"symbol":"XYZ","conid":111,"expirationDate":20990601,"ltd":20990601},
				{"symbol":"XYZ","conid":999,"expirationDate":20000101,"ltd":20000101}
			]}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	b := newTestIBKRBroker(srv.URL)

	conid, err := b.resolveConid(t.Context(), "XYZ", "FUT")
	if err != nil {
		t.Fatalf("resolveConid FUT error: %v", err)
	}
	if conid != "111" { // 20990601 is the nearest expiry >= today
		t.Errorf("conid = %q; want 111 (nearest front-month)", conid)
	}

	// Second call must be served from cache (no second trsrv hit).
	if _, err := b.resolveConid(t.Context(), "XYZ", "FUT"); err != nil {
		t.Fatalf("resolveConid FUT (cached) error: %v", err)
	}
	if trsrvHits != 1 {
		t.Errorf("trsrv hit %d times; want 1 (second call should be cached)", trsrvHits)
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
		if r.URL.Path != "/v1/api/iserver/auth/status" || r.Method != http.MethodGet {
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

// ─── GetPositions ─────────────────────────────────────────────────────────────

// GetPositions must use the Client Portal /portfolio/{acct}/positions/{page}
// route with a /portfolio/accounts priming call first. The previous
// /iserver/account/... path does not exist in the gateway (HTML 404 on every
// call since April): ORB time exits and the reconciler were position-blind,
// which left filled metals brackets as live unprotected shorts recorded as
// "cancelled" in the DB.
func TestIBKRGetPositions_PortfolioRoute(t *testing.T) {
	var primed bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/api/portfolio/accounts":
			primed = true
			w.Write([]byte(`[{"id":"U123456"}]`))
		case "/v1/api/portfolio/U123456/positions/0":
			if !primed {
				t.Error("positions requested before /portfolio/accounts priming call")
			}
			w.Write([]byte(`[{"conid":77124483,"position":-700,"avgCost":58.2525,"mktValue":-41391.0,"unrealizedPnl":-616.69}]`))
		case "/v1/api/iserver/account/U123456/positions/0":
			t.Error("legacy /iserver/account positions route must not be called (it 404s at the real gateway)")
			http.NotFound(w, r)
		default:
			t.Errorf("unexpected request to %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	b := newTestIBKRBroker(srv.URL)
	positions, err := b.GetPositions(t.Context())
	if err != nil {
		t.Fatalf("GetPositions: %v", err)
	}
	if len(positions) != 1 || positions[0].Quantity != -700 {
		t.Fatalf("positions = %+v, want the -700 XAG short", positions)
	}
}

// PositionsSnapshot shares the corrected route and priming.
func TestIBKRPositionsSnapshot_PortfolioRoute(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/api/portfolio/accounts":
			w.Write([]byte(`[]`))
		case "/v1/api/portfolio/U123456/positions/0":
			w.Write([]byte(`[{"conid":69067925,"position":-57,"avgCost":4090.18,"mktPrice":4108.06,"mktValue":-234159.4,"unrealizedPnl":-1019.31}]`))
		default:
			t.Errorf("unexpected request to %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	b := newTestIBKRBroker(srv.URL)
	snap, err := b.PositionsSnapshot(t.Context())
	if err != nil {
		t.Fatalf("PositionsSnapshot: %v", err)
	}
	if len(snap) != 1 || snap[0].Quantity != -57 {
		t.Fatalf("snapshot = %+v, want the -57 XAU short", snap)
	}
}
