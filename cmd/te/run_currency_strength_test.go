package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/data"
	"github.com/romeo-folie/trade-engine/internal/execution"
	"github.com/romeo-folie/trade-engine/internal/strategy/currencystrength"
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

func TestBuildCurrencyStrengthSessionStartMessage(t *testing.T) {
	cfg := config.CurrencyStrengthConfig{
		RankInstruments:  []string{"EUR/USD", "GBP/USD", "USD/JPY", "EUR/JPY", "GBP/JPY"},
		TradeInstruments: []string{"GBP/USD", "USD/JPY"},
		Timezone:         "UTC",
		StrengthStart:    "06:00",
		StrengthEnd:      "09:00",
		ExitTime:         "18:00",
	}

	msg := buildCurrencyStrengthSessionStartMessage(cfg)

	for _, want := range []string{
		"FX Currency-Strength",
		"EUR/USD, GBP/USD, USD/JPY, EUR/JPY, GBP/JPY", // full ranking basket
		"Tradeable: GBP/USD, USD/JPY",                 // order-eligible subset only
		"06:00–09:00 UTC",
		"decision ~09:00",
		"exit 18:00",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("session-start message missing %q\ngot:\n%s", want, msg)
		}
	}
}

func TestBuildCurrencyStrengthSessionStartMessageDefaultsTimezone(t *testing.T) {
	msg := buildCurrencyStrengthSessionStartMessage(config.CurrencyStrengthConfig{
		RankInstruments:  []string{"EUR/USD", "GBP/USD", "USD/JPY"},
		TradeInstruments: []string{"GBP/USD"},
		StrengthStart:    "06:00",
		StrengthEnd:      "09:00",
		ExitTime:         "18:00",
	})
	if !strings.Contains(msg, "06:00–09:00 UTC") {
		t.Errorf("empty timezone should render as UTC; got:\n%s", msg)
	}
}

func TestCurrencyStrengthRunEmitsSessionStartNotification(t *testing.T) {
	notifier := &captureNotifier{}
	runner := &currencyStrengthPaperRunner{
		cfg: config.CurrencyStrengthConfig{
			RankInstruments:  []string{"EUR/USD", "GBP/USD", "USD/JPY"},
			TradeInstruments: []string{"GBP/USD"},
			Timezone:         "UTC",
			StrengthStart:    "06:00",
			StrengthEnd:      "09:00",
			ExitTime:         "18:00",
			Units:            100000,
		},
		market:    fakeMarketDataProvider{},
		broker:    fakeExecutor{},
		statePath: t.TempDir() + "/currency-strength.json",
		now: func() time.Time {
			// Before the strength window closes, so Run announces the basket and
			// then blocks on the ticker without evaluating/entering — cancel to exit.
			return time.Date(2026, 7, 21, 6, 30, 0, 0, time.UTC)
		},
	}
	runner.SetNotifier(notifier)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Run announces before the first ticker wait, then returns on ctx.Done.

	if err := runner.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("Run returned unexpected error: %v", err)
	}
	if len(notifier.msgs) == 0 {
		t.Fatal("expected a session-start notification, got none")
	}
	if !strings.Contains(notifier.msgs[0], "FX Currency-Strength") {
		t.Errorf("first notification is not the session-start listing: %q", notifier.msgs[0])
	}
}

type captureNotifier struct {
	msgs []string
}

func (c *captureNotifier) Notify(_ context.Context, msg string) error {
	c.msgs = append(c.msgs, msg)
	return nil
}

func TestBuildCurrencyStrengthEntryMessage(t *testing.T) {
	sig := currencystrength.Signal{
		Symbol: "GBP/USD",
		Side:   "buy",
		Strong: "GBP",
		Weak:   "USD",
		Gap:    0.0123,
	}
	msg := buildCurrencyStrengthEntryMessage(sig, 100000, "OID-9", "Submitted")
	for _, want := range []string{
		"order placed",
		"BUY GBP/USD · 100000 units",
		"Strongest GBP · Weakest USD · gap 0.0123",
		"Order OID-9 · Submitted",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("entry message missing %q\ngot:\n%s", want, msg)
		}
	}
}

func TestBuildCurrencyStrengthNoOrderMessage(t *testing.T) {
	if msg := buildCurrencyStrengthNoOrderMessage("min_gap"); !strings.Contains(msg, "Reason: min_gap") {
		t.Errorf("no-order message missing reason; got:\n%s", msg)
	}
	// Empty reason falls back to a human-readable default rather than a blank line.
	if msg := buildCurrencyStrengthNoOrderMessage(""); !strings.Contains(msg, "no qualifying signal") {
		t.Errorf("empty reason should default; got:\n%s", msg)
	}
}

func TestBuildCurrencyStrengthExitMessage(t *testing.T) {
	msg := buildCurrencyStrengthExitMessage("USD/JPY", execution.OrderSideSell, 100000, "OID-X", "Filled")
	for _, want := range []string{
		"position flattened",
		"SELL USD/JPY · 100000 units",
		"Order OID-X · Filled",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("exit message missing %q\ngot:\n%s", want, msg)
		}
	}
}

func TestCurrencyStrengthSkipsConflictingPosition(t *testing.T) {
	notifier := &captureNotifier{}
	runner := &currencyStrengthPaperRunner{
		cfg: config.CurrencyStrengthConfig{
			TradeInstruments: []string{"GBP/USD"},
		},
		notifier:  notifier,
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
	if len(notifier.msgs) != 1 || !strings.Contains(notifier.msgs[0], "no order today") {
		t.Fatalf("expected one no-order notification, got %v", notifier.msgs)
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
