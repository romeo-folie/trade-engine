package execution

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
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
	if len(res.ChildOrderIDs) != 0 {
		t.Errorf("ChildOrderIDs = %v, want empty", res.ChildOrderIDs)
	}
}
