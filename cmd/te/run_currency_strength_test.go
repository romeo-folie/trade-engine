package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/data"
	"github.com/romeo-folie/trade-engine/internal/execution"
)

func TestIsIBKRPaperAccountID(t *testing.T) {
	cases := []struct {
		account string
		want    bool
	}{
		{"DU1234567", true},
		{" du1234567 ", true},
		{"U1234567", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := isIBKRPaperAccountID(tc.account); got != tc.want {
			t.Errorf("isIBKRPaperAccountID(%q) = %v, want %v", tc.account, got, tc.want)
		}
	}
}

func TestCloseAtOrBefore(t *testing.T) {
	base := time.Date(2026, 7, 21, 9, 0, 0, 0, time.UTC)
	candles := []data.Candle{
		{Timestamp: base.Add(-2 * time.Minute), Close: 1.10},
		{Timestamp: base.Add(-1 * time.Minute), Close: 1.11},
		{Timestamp: base.Add(time.Minute), Close: 1.12},
	}

	got, ok := closeAtOrBefore(candles, base)
	if !ok || got != 1.11 {
		t.Fatalf("closeAtOrBefore = %.2f/%v, want 1.11/true", got, ok)
	}

	if _, ok := closeAtOrBefore(candles, base.Add(-3*time.Minute)); ok {
		t.Fatal("closeAtOrBefore before first candle returned ok=true")
	}
}

func TestCurrencyStrengthShouldExitStaleStateImmediately(t *testing.T) {
	now := time.Date(2026, 7, 21, 10, 0, 0, 0, time.UTC)
	exitTime := time.Date(2026, 7, 21, 18, 0, 0, 0, time.UTC)

	stale := &currencyStrengthRunState{Date: "2026-07-20", Symbol: "GBP/USD", Side: "buy", Quantity: 100000}
	if !currencyStrengthShouldExitState(stale, "2026-07-21", now, exitTime) {
		t.Fatal("stale state should flatten immediately before today's exit time")
	}

	current := &currencyStrengthRunState{Date: "2026-07-21", Symbol: "GBP/USD", Side: "buy", Quantity: 100000}
	if currencyStrengthShouldExitState(current, "2026-07-21", now, exitTime) {
		t.Fatal("current-date state should wait until configured exit time")
	}

	afterExit := time.Date(2026, 7, 21, 18, 0, 0, 0, time.UTC)
	if !currencyStrengthShouldExitState(current, "2026-07-21", afterExit, exitTime) {
		t.Fatal("current-date state should flatten at configured exit time")
	}
}

func TestShouldRunCurrencyStrengthForForexSession(t *testing.T) {
	cases := []struct {
		session string
		want    bool
	}{
		{"", true},
		{"london", true},
		{" both ", true},
		{"newyork", false},
		{"futures", false},
	}
	for _, tc := range cases {
		if got := shouldRunCurrencyStrengthForForexSession(tc.session); got != tc.want {
			t.Errorf("shouldRunCurrencyStrengthForForexSession(%q) = %v, want %v", tc.session, got, tc.want)
		}
	}
}

func TestShouldRunCurrencyStrengthPaperOrders(t *testing.T) {
	cases := []struct {
		mode string
		want bool
	}{
		{"paper", true},
		{" PAPER ", true},
		{"shadow", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := shouldRunCurrencyStrengthPaperOrders(tc.mode); got != tc.want {
			t.Errorf("shouldRunCurrencyStrengthPaperOrders(%q) = %v, want %v", tc.mode, got, tc.want)
		}
	}
}

func TestCurrencyStrengthSkipsConflictingPosition(t *testing.T) {
	runner := &currencyStrengthPaperRunner{
		cfg: config.CurrencyStrengthConfig{
			TradeInstruments: []string{"GBP/USD"},
		},
		market:    fakeMarketDataProvider{},
		broker:    fakeExecutor{positions: []execution.Position{{Ticker: "GBP/USD", Quantity: 100000}}},
		statePath: t.TempDir() + "/currency-strength.json",
		now: func() time.Time {
			return time.Date(2026, 7, 21, 10, 0, 0, 0, time.UTC)
		},
	}

	err := runner.evaluateAndEnter(context.Background(), "2026-07-21",
		time.Date(2026, 7, 21, 6, 0, 0, 0, time.UTC),
		time.Date(2026, 7, 21, 9, 0, 0, 0, time.UTC),
	)
	if err != nil {
		t.Fatalf("evaluateAndEnter returned error: %v", err)
	}
	if runner.decisionDoneFor != "2026-07-21" {
		t.Fatalf("decisionDoneFor = %q, want 2026-07-21", runner.decisionDoneFor)
	}
}

type fakeMarketDataProvider struct{}

func (fakeMarketDataProvider) FetchCandles(context.Context, string, data.Timeframe, time.Time, time.Time) ([]data.Candle, error) {
	return nil, errors.New("unexpected market fetch")
}

func (fakeMarketDataProvider) FetchQuote(context.Context, string) (*data.Quote, error) {
	return nil, errors.New("unexpected market fetch")
}

func (fakeMarketDataProvider) FetchHistory(context.Context, string, data.Timeframe, int) ([]data.Candle, error) {
	return nil, errors.New("unexpected market fetch")
}

type fakeExecutor struct {
	positions []execution.Position
}

func (f fakeExecutor) PlaceOrder(context.Context, execution.OrderRequest) (execution.OrderResult, error) {
	return execution.OrderResult{}, errors.New("unexpected order")
}

func (f fakeExecutor) CancelOrder(context.Context, string) error {
	return errors.New("unexpected cancel")
}

func (f fakeExecutor) GetOrderStatus(context.Context, string) (execution.OrderResult, error) {
	return execution.OrderResult{}, errors.New("unexpected status")
}

func (f fakeExecutor) GetPositions(context.Context) ([]execution.Position, error) {
	return f.positions, nil
}

func (f fakeExecutor) GetAccount(context.Context) (execution.AccountInfo, error) {
	return execution.AccountInfo{}, nil
}
