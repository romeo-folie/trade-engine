package execution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestGetAccount_DayPnL(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/account" {
			t.Errorf("expected /v2/account, got %s", r.URL.Path)
		}
		// Realistic Alpaca /v2/account fields.
		fmt.Fprintln(w, `{
			"cash": "25000.50",
			"equity": "105000.75",
			"last_equity": "103500.50"
		}`)
	}))
	defer ts.Close()

	broker := &AlpacaBroker{
		apiKey:     "test-key",
		secret:     "test-secret",
		baseURL:    ts.URL,
		httpClient: ts.Client(),
	}

	acct, err := broker.GetAccount(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if acct.Cash != 25000.50 {
		t.Errorf("Cash = %.2f, want 25000.50", acct.Cash)
	}
	if acct.PortfolioEquity != 105000.75 {
		t.Errorf("PortfolioEquity = %.2f, want 105000.75", acct.PortfolioEquity)
	}
	// DayPnL should be 105000.75 - 103500.50 = 1500.25
	if acct.DayPnL != 1500.25 {
		t.Errorf("DayPnL = %.2f, want 1500.25", acct.DayPnL)
	}
}

func TestAlpacaPlaceOrderUsesClientIDAndCapturesRequestID(t *testing.T) {
	var clientID string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		clientID, _ = body["client_order_id"].(string)
		w.Header().Set("X-Request-ID", "req-123")
		fmt.Fprintln(w, `{"id":"broker-1","status":"accepted","filled_avg_price":"","filled_qty":""}`)
	}))
	defer ts.Close()
	b := &AlpacaBroker{baseURL: ts.URL, httpClient: ts.Client()}
	res, err := b.PlaceOrder(context.Background(), OrderRequest{Ticker: "SPY", Side: OrderSideBuy, Type: OrderTypeMarket, Quantity: 1, ClientOrderID: "te-stock-abc"})
	if err != nil {
		t.Fatal(err)
	}
	if clientID != "te-stock-abc" || res.RequestID != "req-123" {
		t.Fatalf("client/request ids = %q/%q", clientID, res.RequestID)
	}
}

func TestAlpacaGetOrderByClientID(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/orders:by_client_order_id" || r.URL.Query().Get("client_order_id") != "te-stock-abc" || r.URL.Query().Get("nested") != "true" {
			t.Fatalf("path/query = %s?%s", r.URL.Path, r.URL.RawQuery)
		}
		fmt.Fprintln(w, `{"id":"broker-1","status":"filled","filled_avg_price":"10","filled_qty":"2","legs":[{"id":"tp-1","type":"limit"},{"id":"sl-1","type":"stop"}]}`)
	}))
	defer ts.Close()
	b := &AlpacaBroker{baseURL: ts.URL, httpClient: ts.Client()}
	res, err := b.GetOrderByClientID(context.Background(), "te-stock-abc")
	if err != nil || res.BrokerOrderID != "broker-1" || res.FilledQty != 2 ||
		len(res.ChildOrderIDs) != 2 || res.ChildOrderIDs[0] != "tp-1" || res.ChildOrderIDs[1] != "sl-1" {
		t.Fatalf("lookup = %+v, %v", res, err)
	}
}

func TestAlpacaOrderLookupsMapStructured404ToNotFound(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) }))
	defer ts.Close()
	b := &AlpacaBroker{baseURL: ts.URL, httpClient: ts.Client()}
	if _, err := b.GetOrderByClientID(context.Background(), "missing"); !errors.Is(err, ErrOrderNotFound) {
		t.Fatalf("client lookup error = %v", err)
	}
	if _, err := b.GetOrderStatus(context.Background(), "missing"); !errors.Is(err, ErrOrderNotFound) {
		t.Fatalf("status lookup error = %v", err)
	}
}

func TestAlpacaReplaceStopOrderUsesClientIDAndCapturesRequestID(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch || r.URL.Path != "/v2/orders/old-stop" {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["client_order_id"] != "te-stock-replacement" {
			t.Fatalf("client_order_id = %#v", body["client_order_id"])
		}
		w.Header().Set("X-Request-ID", "replace-request")
		fmt.Fprintln(w, `{"id":"new-stop","status":"accepted","filled_avg_price":"","filled_qty":""}`)
	}))
	defer ts.Close()
	b := &AlpacaBroker{baseURL: ts.URL, httpClient: ts.Client()}
	res, err := b.ReplaceStopOrderWithClientID(context.Background(), "old-stop", 99.25, "te-stock-replacement")
	if err != nil {
		t.Fatal(err)
	}
	if res.BrokerOrderID != "new-stop" || res.RequestID != "replace-request" {
		t.Fatalf("replacement = %+v", res)
	}
}

func TestAlpacaPlaceOrderClassifiesDefinitiveRejection(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		fmt.Fprintln(w, `{"code":40310000,"message":"insufficient buying power"}`)
	}))
	defer ts.Close()
	b := &AlpacaBroker{baseURL: ts.URL, httpClient: ts.Client()}
	_, err := b.PlaceOrder(context.Background(), OrderRequest{Ticker: "SPY", Side: OrderSideBuy, Type: OrderTypeMarket, Quantity: 1})
	if !IsDefinitiveOrderRejection(err) {
		t.Fatalf("422 error not classified definitive: %v", err)
	}
}

func TestAccountSnapshotCapturesAccountIdentityAndSafetyFields(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v2/account" {
			t.Fatalf("request = %s %s, want GET /v2/account", r.Method, r.URL.Path)
		}
		fmt.Fprintln(w, `{
            "id":"internal-id", "account_number":"PA123", "status":"ACTIVE",
            "cash":"0", "equity":"100.50", "last_equity":"99.25",
            "buying_power":"200", "regt_buying_power":"190",
            "daytrading_buying_power":"400", "non_marginable_buying_power":"100",
            "multiplier":"2", "currency":"USD", "trading_blocked":false,
            "transfers_blocked":true, "account_blocked":false,
            "trade_suspended_by_user":false, "shorting_enabled":true
        }`)
	}))
	defer ts.Close()

	broker := &AlpacaBroker{apiKey: "k", secret: "s", baseURL: ts.URL, httpClient: ts.Client()}
	account, err := broker.AccountSnapshot(context.Background())
	if err != nil {
		t.Fatalf("AccountSnapshot: %v", err)
	}
	if account.AccountNumber != "PA123" || account.Status != "ACTIVE" || account.Cash != 0 ||
		account.RegTBuyingPower != 190 || account.DayTradingBuyingPower != 400 ||
		account.NonMarginableBuyingPower != 100 || account.Multiplier != 2 || !account.TransfersBlocked || !account.ShortingEnabled {
		t.Fatalf("unexpected snapshot: %+v", account)
	}
	if !account.LastEquityAvailable || !account.CashAvailable || !account.BuyingPowerAvailable ||
		!account.MultiplierAvailable || !account.RegTBuyingPowerAvailable ||
		!account.DayTradingBuyingPowerAvailable || !account.NonMarginableBuyingPowerAvailable {
		t.Fatalf("snapshot lost field presence: %+v", account)
	}
	if account.FetchedAt.IsZero() {
		t.Fatal("FetchedAt is zero")
	}
	encoded, err := json.Marshal(account)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) == "" || containsJSONField(encoded, "account_id") || containsJSONField(encoded, "account_number") {
		t.Fatalf("account identity leaked from normal JSON: %s", encoded)
	}
}

func TestStockAssetSnapshotUsesExactSymbolEndpoint(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v2/assets/AAPL" {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		fmt.Fprintln(w, `{"symbol":"AAPL","status":"active","class":"us_equity","tradable":true,"marginable":true,"fractionable":true}`)
	}))
	defer ts.Close()
	broker := &AlpacaBroker{baseURL: ts.URL, httpClient: ts.Client()}
	asset, err := broker.StockAssetSnapshot(context.Background(), " aapl ")
	if err != nil || asset.Symbol != "AAPL" || !asset.Tradable || !asset.Marginable || !asset.Fractionable || asset.FetchedAt.IsZero() {
		t.Fatalf("StockAssetSnapshot = %+v, %v", asset, err)
	}
}

func TestStockAssetSnapshotRejectsMissingCapabilityBooleans(t *testing.T) {
	for _, omitted := range []string{"tradable", "marginable", "fractionable"} {
		t.Run(omitted, func(t *testing.T) {
			payload := map[string]any{
				"symbol": "AAPL", "status": "active", "class": "us_equity",
				"tradable": true, "marginable": true, "fractionable": true,
			}
			delete(payload, omitted)
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(payload)
			}))
			defer ts.Close()
			broker := &AlpacaBroker{baseURL: ts.URL, httpClient: ts.Client()}
			if _, err := broker.StockAssetSnapshot(context.Background(), "AAPL"); err == nil {
				t.Fatalf("missing %s passed", omitted)
			}
		})
	}
}

func TestAccountSnapshotRejectsMissingRestrictionBooleans(t *testing.T) {
	for _, omitted := range []string{"trading_blocked", "account_blocked", "trade_suspended_by_user"} {
		t.Run(omitted, func(t *testing.T) {
			payload := map[string]any{
				"id": "id", "account_number": "PA123", "status": "ACTIVE", "currency": "USD",
				"cash": "0", "equity": "100", "last_equity": "99", "buying_power": "100",
				"regt_buying_power": "100", "non_marginable_buying_power": "100", "multiplier": "2",
				"trading_blocked": false, "account_blocked": false, "trade_suspended_by_user": false,
			}
			delete(payload, omitted)
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(payload)
			}))
			defer ts.Close()
			broker := &AlpacaBroker{baseURL: ts.URL, httpClient: ts.Client()}
			if _, err := broker.AccountSnapshot(context.Background()); err == nil {
				t.Fatalf("missing %s passed", omitted)
			}
		})
	}
}

func TestAccountSnapshotRejectsMalformedOrNonFiniteRequiredValues(t *testing.T) {
	for _, value := range []string{"", "not-a-number", "NaN", "+Inf", "-Inf"} {
		t.Run(fmt.Sprintf("cash=%q", value), func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				fmt.Fprintf(w, `{"cash":%q,"equity":"100","buying_power":"100"}`, value)
			}))
			defer ts.Close()
			broker := &AlpacaBroker{baseURL: ts.URL, httpClient: ts.Client()}
			if _, err := broker.AccountSnapshot(context.Background()); err == nil {
				t.Fatal("AccountSnapshot accepted invalid cash")
			}
		})
	}
}

func containsJSONField(encoded []byte, field string) bool {
	var decoded map[string]any
	_ = json.Unmarshal(encoded, &decoded)
	_, ok := decoded[field]
	return ok
}

func TestGetOrderStatusReturnsBrokerFillTime(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, `{"id":"o-1","status":"filled","filled_avg_price":"95.25","filled_qty":"10","filled_at":"2026-08-01T14:05:02Z"}`)
	}))
	defer ts.Close()

	broker := &AlpacaBroker{apiKey: "k", secret: "s", baseURL: ts.URL, httpClient: ts.Client()}
	result, err := broker.GetOrderStatus(context.Background(), "o-1")
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 8, 1, 14, 5, 2, 0, time.UTC)
	if !result.FilledAt.Equal(want) {
		t.Fatalf("filled_at = %v, want %v", result.FilledAt, want)
	}
}

func TestGetOrderStatusReturnsNestedBracketCoverage(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/orders/parent-1" || r.URL.Query().Get("nested") != "true" {
			t.Fatalf("path/query = %s?%s", r.URL.Path, r.URL.RawQuery)
		}
		fmt.Fprintln(w, `{
			"id":"parent-1","status":"filled","qty":"5","filled_qty":"5",
			"legs":[
				{"id":"tp-1","type":"limit","status":"new","qty":"3","filled_qty":"2","filled_avg_price":"109.75","filled_at":"2026-08-13T10:00:00Z","limit_price":"110"},
				{"id":"sl-1","type":"stop","status":"new","qty":"3","filled_qty":"0","stop_price":"95"}
			]
		}`)
	}))
	defer ts.Close()

	broker := &AlpacaBroker{baseURL: ts.URL, httpClient: ts.Client()}
	result, err := broker.GetOrderStatus(context.Background(), "parent-1")
	if err != nil {
		t.Fatal(err)
	}
	if result.Quantity != 5 || result.RemainingQty != 0 || len(result.ChildOrders) != 2 {
		t.Fatalf("nested result = %+v", result)
	}
	stop := result.ChildOrders[1]
	if stop.BrokerOrderID != "sl-1" || stop.Quantity != 3 || stop.RemainingQty != 3 || stop.StopPrice != 95 {
		t.Fatalf("stop leg = %+v", stop)
	}
	tp := result.ChildOrders[0]
	if tp.FilledQty != 2 || tp.FilledPrice != 109.75 || tp.FilledAt.IsZero() {
		t.Fatalf("take-profit fill = %+v", tp)
	}
}

// TestPlaceOrder_BracketLegsAndTIF exercises the #435 bracket path: the
// request must carry order_class/take_profit/stop_loss and the overridden
// time_in_force, and the response's child legs must come back as
// ChildOrderIDs ordered [take-profit, stop-loss] by leg TYPE — Alpaca does
// not document leg array ordering, so the stop leg is listed first here on
// purpose.
func TestPlaceOrder_BracketLegsAndTIF(t *testing.T) {
	var gotBody map[string]any
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/orders" {
			t.Errorf("expected /v2/orders, got %s", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		fmt.Fprintln(w, `{
			"id": "parent-1",
			"status": "accepted",
			"filled_avg_price": "",
			"filled_qty": "",
			"legs": [
				{"id": "leg-stop", "type": "stop"},
				{"id": "leg-tp", "type": "limit"}
			]
		}`)
	}))
	defer ts.Close()

	broker := &AlpacaBroker{
		apiKey:     "k",
		secret:     "s",
		baseURL:    ts.URL,
		httpClient: ts.Client(),
	}

	res, err := broker.PlaceOrder(context.Background(), OrderRequest{
		Ticker:          "SPY",
		Side:            OrderSideBuy,
		Type:            OrderTypeMarket,
		Quantity:        3,
		OrderClass:      "bracket",
		TakeProfitPrice: 110,
		StopLossPrice:   95,
		TimeInForce:     "gtc",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotBody["time_in_force"] != "gtc" {
		t.Errorf("time_in_force = %v, want gtc", gotBody["time_in_force"])
	}
	if gotBody["order_class"] != "bracket" {
		t.Errorf("order_class = %v, want bracket", gotBody["order_class"])
	}
	if len(res.ChildOrderIDs) != 2 || res.ChildOrderIDs[0] != "leg-tp" || res.ChildOrderIDs[1] != "leg-stop" {
		t.Fatalf("ChildOrderIDs = %v, want [leg-tp leg-stop] regardless of response order", res.ChildOrderIDs)
	}
}

func TestPlaceOrder_FormatsStockPricesAtValidAlpacaPrecision(t *testing.T) {
	var gotBody map[string]any
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		fmt.Fprintln(w, `{"id": "parent-1", "status": "accepted", "filled_avg_price": "", "filled_qty": ""}`)
	}))
	defer ts.Close()

	broker := &AlpacaBroker{apiKey: "k", secret: "s", baseURL: ts.URL, httpClient: ts.Client()}
	_, err := broker.PlaceOrder(context.Background(), OrderRequest{
		Ticker:          "ATAI",
		Side:            OrderSideBuy,
		Type:            OrderTypeStop,
		Quantity:        10,
		StopPrice:       7.2377,
		OrderClass:      "bracket",
		TakeProfitPrice: 7.5599,
		StopLossPrice:   7.1622,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if gotBody["stop_price"] != "7.24" {
		t.Fatalf("parent stop_price = %v, want 7.24", gotBody["stop_price"])
	}
	tp := gotBody["take_profit"].(map[string]any)
	if tp["limit_price"] != "7.56" {
		t.Fatalf("take_profit.limit_price = %v, want 7.56", tp["limit_price"])
	}
	sl := gotBody["stop_loss"].(map[string]any)
	if sl["stop_price"] != "7.16" {
		t.Fatalf("stop_loss.stop_price = %v, want 7.16", sl["stop_price"])
	}
}

func TestFormatAlpacaStockPrice_AllowsSubDollarFourDecimals(t *testing.T) {
	if got := formatAlpacaStockPrice(0.123456); got != "0.1235" {
		t.Fatalf("formatAlpacaStockPrice(0.123456) = %q, want 0.1235", got)
	}
	if got := formatAlpacaStockPrice(39.6196); got != "39.62" {
		t.Fatalf("formatAlpacaStockPrice(39.6196) = %q, want 39.62", got)
	}
}

// TestPlaceOrder_DefaultTIFAndNoLegs pins the pre-#435 behavior: simple
// orders keep day TIF and produce no ChildOrderIDs.
func TestPlaceOrder_DefaultTIFAndNoLegs(t *testing.T) {
	var gotBody map[string]any
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		fmt.Fprintln(w, `{"id": "o-1", "status": "accepted", "filled_avg_price": "", "filled_qty": ""}`)
	}))
	defer ts.Close()

	broker := &AlpacaBroker{apiKey: "k", secret: "s", baseURL: ts.URL, httpClient: ts.Client()}
	res, err := broker.PlaceOrder(context.Background(), OrderRequest{
		Ticker: "SPY", Side: OrderSideBuy, Type: OrderTypeMarket, Quantity: 1,
		ReferencePrice: 500,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotBody["time_in_force"] != "day" {
		t.Errorf("time_in_force = %v, want day", gotBody["time_in_force"])
	}
	if _, ok := gotBody["order_class"]; ok {
		t.Error("order_class must be absent on simple orders")
	}
	if _, ok := gotBody["limit_price"]; ok {
		t.Error("paper-only ReferencePrice must not become an Alpaca limit_price")
	}
	if len(res.ChildOrderIDs) != 0 {
		t.Errorf("ChildOrderIDs = %v, want empty", res.ChildOrderIDs)
	}
}
