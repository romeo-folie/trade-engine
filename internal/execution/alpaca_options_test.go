package execution

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestOCCSymbol_RoundTrip(t *testing.T) {
	oc := OptionContract{
		Underlying: "SPY",
		Expiry:     time.Date(2026, 7, 17, 0, 0, 0, 0, time.UTC),
		Strike:     551.0,
		Right:      "C",
		Multiplier: 100,
	}
	sym := OCCSymbol(oc)
	if sym != "SPY260717C00551000" {
		t.Fatalf("OCCSymbol = %q; want SPY260717C00551000", sym)
	}
	// Alpaca's documented example.
	if got := OCCSymbol(OptionContract{Underlying: "AAPL", Expiry: time.Date(2024, 2, 23, 0, 0, 0, 0, time.UTC), Strike: 95.0, Right: "C"}); got != "AAPL240223C00095000" {
		t.Fatalf("OCCSymbol(AAPL) = %q; want AAPL240223C00095000", got)
	}

	back, err := ParseOCCSymbol(sym)
	if err != nil {
		t.Fatalf("ParseOCCSymbol: %v", err)
	}
	if back.Underlying != "SPY" || back.Right != "C" || back.Strike != 551.0 || back.Multiplier != 100 {
		t.Errorf("parsed = %+v", back)
	}
	if !back.Expiry.Equal(oc.Expiry) {
		t.Errorf("parsed expiry = %v; want %v", back.Expiry, oc.Expiry)
	}
}

func TestParseOCCSymbol_Invalid(t *testing.T) {
	for _, bad := range []string{"", "SPY", "SPY260717X00551000", "260717C00551000"} {
		if _, err := ParseOCCSymbol(bad); err == nil {
			t.Errorf("ParseOCCSymbol(%q) expected error, got nil", bad)
		}
	}
}

func TestIsOCCSymbol(t *testing.T) {
	for _, s := range []string{"SPY260717C00551000", "AAPL240223C00095000", "QQQ260717P00500000"} {
		if !IsOCCSymbol(s) {
			t.Errorf("IsOCCSymbol(%q) = false; want true", s)
		}
	}
	for _, s := range []string{"SPY", "AAPL", "EUR/USD", "", "XAU/USD", "SPY260717X00551000"} {
		if IsOCCSymbol(s) {
			t.Errorf("IsOCCSymbol(%q) = true; want false", s)
		}
	}
}

func TestGetOptionChain_GreeksAndNulls(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1beta1/options/snapshots/SPY" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if got := r.URL.Query().Get("feed"); got != "opra" {
			t.Errorf("feed = %q; want opra", got)
		}
		if got := r.URL.Query().Get("type"); got != "call" {
			t.Errorf("type = %q; want call", got)
		}
		io.WriteString(w, `{"snapshots":{
			"SPY260717C00551000":{"latestQuote":{"bp":1.20,"ap":1.30},"greeks":{"delta":0.55},"impliedVolatility":0.18},
			"SPY260717C00560000":{"latestQuote":{"bp":0.50,"ap":0.60}}
		}}`)
	}))
	defer ts.Close()
	b := &AlpacaBroker{baseURL: ts.URL, dataURL: ts.URL, optionFeed: "opra", httpClient: ts.Client()}

	snaps, err := b.GetOptionChain(context.Background(), "SPY", OptionChainFilter{Right: "call"})
	if err != nil {
		t.Fatalf("GetOptionChain: %v", err)
	}
	byID := map[string]OptionSnapshot{}
	for _, s := range snaps {
		byID[s.OCCSymbol] = s
	}
	withGreeks := byID["SPY260717C00551000"]
	if withGreeks.Delta == nil || *withGreeks.Delta != 0.55 {
		t.Errorf("delta = %v; want 0.55", withGreeks.Delta)
	}
	if withGreeks.IV == nil || *withGreeks.IV != 0.18 {
		t.Errorf("iv = %v; want 0.18", withGreeks.IV)
	}
	if withGreeks.Mid() != 1.25 {
		t.Errorf("mid = %v; want 1.25", withGreeks.Mid())
	}
	// Null greeks/IV must stay nil (never a spurious zero) so gates fail closed.
	noGreeks := byID["SPY260717C00560000"]
	if noGreeks.Delta != nil {
		t.Errorf("expected nil delta, got %v", *noGreeks.Delta)
	}
	if noGreeks.IV != nil {
		t.Errorf("expected nil iv, got %v", *noGreeks.IV)
	}
}

// TestGetOptionChain_FailsClosedOffOPRA verifies the chain read errors (fails
// closed) on the delayed indicative feed, so the gate aborts rather than trading
// on stale quotes.
func TestGetOptionChain_FailsClosedOffOPRA(t *testing.T) {
	called := false
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		io.WriteString(w, `{"snapshots":{}}`)
	}))
	defer ts.Close()
	b := &AlpacaBroker{baseURL: ts.URL, dataURL: ts.URL, optionFeed: "indicative", httpClient: ts.Client()}

	_, err := b.GetOptionChain(context.Background(), "SPY", OptionChainFilter{})
	if err == nil {
		t.Fatal("expected an error on the non-OPRA feed (fail closed)")
	}
	if called {
		t.Error("must not hit the data API on a non-OPRA feed")
	}
}

func TestResolveOptionContracts(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/options/contracts" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if got := r.URL.Query().Get("underlying_symbols"); got != "SPY" {
			t.Errorf("underlying_symbols = %q", got)
		}
		io.WriteString(w, `{"option_contracts":[
			{"symbol":"SPY260717C00551000","tradable":true,"expiration_date":"2026-07-17","strike_price":"551","type":"call","underlying_symbol":"SPY"}
		]}`)
	}))
	defer ts.Close()
	b := &AlpacaBroker{baseURL: ts.URL, dataURL: ts.URL, optionFeed: "opra", httpClient: ts.Client()}

	got, err := b.ResolveOptionContracts(context.Background(), OptionContractQuery{Underlying: "SPY", Right: "call"})
	if err != nil {
		t.Fatalf("ResolveOptionContracts: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d; want 1", len(got))
	}
	if !got[0].Tradable || got[0].Contract.OCCSymbol != "SPY260717C00551000" || got[0].Contract.Right != "C" {
		t.Errorf("contract = %+v", got[0])
	}
}

func TestAlpacaGetWorkingOrders(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/orders" || r.URL.Query().Get("status") != "open" {
			t.Errorf("unexpected request %s?%s", r.URL.Path, r.URL.RawQuery)
		}
		io.WriteString(w, `[
			{"id":"o1","symbol":"SPY260717C00551000","asset_class":"us_option","side":"sell","status":"accepted","qty":"2","filled_qty":"0","limit_price":"0"},
			{"id":"o2","symbol":"AAPL","asset_class":"us_equity","side":"buy","status":"new","qty":"10","filled_qty":"3","limit_price":"200"}
		]`)
	}))
	defer ts.Close()
	b := &AlpacaBroker{baseURL: ts.URL, dataURL: ts.URL, optionFeed: "opra", httpClient: ts.Client()}

	orders, err := b.GetWorkingOrders(context.Background())
	if err != nil {
		t.Fatalf("GetWorkingOrders: %v", err)
	}
	if len(orders) != 2 {
		t.Fatalf("len = %d; want 2", len(orders))
	}
	// The option sell order maps to its OCC symbol with remaining qty.
	var opt *WorkingOrder
	for i := range orders {
		if orders[i].OrderID == "o1" {
			opt = &orders[i]
		}
	}
	if opt == nil || opt.Ticker != "SPY260717C00551000" || opt.AssetClass != "us_option" || opt.Side != "sell" || opt.RemainingQty != 2 || opt.Status != "pending" {
		t.Errorf("option working order = %+v", opt)
	}
}

func TestAlpacaGetWorkingOrdersFailsClosedAtPageLimit(t *testing.T) {
	page := make([]map[string]string, 500)
	for i := range page {
		page[i] = map[string]string{
			"id": fmt.Sprintf("o-%d", i), "symbol": "AAPL", "side": "sell",
			"status": "accepted", "qty": "1", "filled_qty": "0",
		}
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(page)
	}))
	defer ts.Close()
	b := &AlpacaBroker{baseURL: ts.URL, httpClient: ts.Client()}
	if _, err := b.GetWorkingOrders(context.Background()); err == nil {
		t.Fatal("full open-order page was accepted as a complete inventory")
	}
}

func TestPlaceOptionOrder(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/orders" || r.Method != http.MethodPost {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if body["symbol"] != "SPY260717C00551000" {
			t.Errorf("symbol = %v", body["symbol"])
		}
		if body["qty"] != "2" {
			t.Errorf("qty = %v; want \"2\" (whole contracts)", body["qty"])
		}
		if body["limit_price"] != "1.25" {
			t.Errorf("limit_price = %v; want 1.25", body["limit_price"])
		}
		if _, hasClass := body["order_class"]; hasClass {
			t.Errorf("options order must not set order_class (Alpaca rejects bracket/OCO)")
		}
		io.WriteString(w, `{"id":"opt-1","status":"accepted","filled_avg_price":"","filled_qty":"0"}`)
	}))
	defer ts.Close()
	b := &AlpacaBroker{baseURL: ts.URL, dataURL: ts.URL, optionFeed: "opra", httpClient: ts.Client()}

	res, err := b.PlaceOrder(context.Background(), OrderRequest{
		SecType:    "OPT",
		Side:       OrderSideBuy,
		Type:       OrderTypeLimit,
		Quantity:   2,
		LimitPrice: 1.25,
		Option:     &OptionContract{OCCSymbol: "SPY260717C00551000"},
	})
	if err != nil {
		t.Fatalf("PlaceOrder(OPT): %v", err)
	}
	if res.BrokerOrderID != "opt-1" || res.Status != "accepted" {
		t.Errorf("result = %+v", res)
	}
}

func TestPlaceOptionOrder_MissingContract(t *testing.T) {
	b := &AlpacaBroker{baseURL: "http://unused", optionFeed: "opra", httpClient: http.DefaultClient}
	if _, err := b.PlaceOrder(context.Background(), OrderRequest{SecType: "OPT", Type: OrderTypeLimit, Quantity: 1}); err == nil {
		t.Fatal("expected error when Option is nil for SecType OPT")
	}
}

func TestGetPositions_OptionAware(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `[
			{"symbol":"AAPL","asset_class":"us_equity","qty":"10","avg_entry_price":"200","current_price":"205","market_value":"2050","unrealized_pl":"50","side":"long"},
			{"symbol":"SPY260717C00551000","asset_class":"us_option","qty":"2","avg_entry_price":"1.25","current_price":"1.40","market_value":"280","unrealized_pl":"30","side":"long"}
		]`)
	}))
	defer ts.Close()
	b := &AlpacaBroker{baseURL: ts.URL, dataURL: ts.URL, optionFeed: "opra", httpClient: ts.Client()}

	pos, err := b.GetPositions(context.Background())
	if err != nil {
		t.Fatalf("GetPositions: %v", err)
	}
	if len(pos) != 2 {
		t.Fatalf("len = %d; want 2", len(pos))
	}
	var equity, option *Position
	for i := range pos {
		if pos[i].Ticker == "AAPL" {
			equity = &pos[i]
		} else {
			option = &pos[i]
		}
	}
	if equity == nil || equity.Option != nil {
		t.Errorf("equity position should have nil Option: %+v", equity)
	}
	if option == nil || option.Option == nil {
		t.Fatalf("option position missing Option identity: %+v", option)
	}
	if option.Option.Underlying != "SPY" || option.Option.Right != "C" || option.Option.Strike != 551.0 {
		t.Errorf("option identity = %+v", option.Option)
	}
	if option.Option.Multiplier != 100 {
		t.Errorf("option multiplier = %d; want 100", option.Option.Multiplier)
	}
}

func TestGetOptionSnapshots_TargetedSymbols(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// By-symbols endpoint (no underlying in the path), symbols= set.
		if r.URL.Path != "/v1beta1/options/snapshots" {
			t.Errorf("unexpected path %s; want the by-symbols endpoint", r.URL.Path)
		}
		if got := r.URL.Query().Get("symbols"); got != "SPY260717C00551000" {
			t.Errorf("symbols = %q; want the single held contract", got)
		}
		if got := r.URL.Query().Get("feed"); got != "opra" {
			t.Errorf("feed = %q; want opra", got)
		}
		io.WriteString(w, `{"snapshots":{"SPY260717C00551000":{"latestQuote":{"bp":1.20,"ap":1.30}}}}`)
	}))
	defer ts.Close()
	b := &AlpacaBroker{baseURL: ts.URL, dataURL: ts.URL, optionFeed: "opra", httpClient: ts.Client()}

	snaps, err := b.GetOptionSnapshots(context.Background(), []string{"SPY260717C00551000"})
	if err != nil {
		t.Fatalf("GetOptionSnapshots: %v", err)
	}
	if len(snaps) != 1 || snaps[0].Mid() != 1.25 {
		t.Fatalf("snaps = %+v; want one contract mid 1.25", snaps)
	}
	// Empty input makes no call.
	if s, err := b.GetOptionSnapshots(context.Background(), nil); err != nil || s != nil {
		t.Errorf("empty symbols: got (%v,%v); want (nil,nil)", s, err)
	}
}
