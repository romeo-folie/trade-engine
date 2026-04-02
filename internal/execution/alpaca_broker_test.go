package execution

import (
	"context"
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
		fmt.Fprintln(w, `{"buying_power": "25000.50", "portfolio_value": "105000.75", "day_pnl": "1500.25"}`)
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
	if acct.DayPnL != 1500.25 {
		t.Errorf("DayPnL = %.2f, want 1500.25", acct.DayPnL)
	}
}
