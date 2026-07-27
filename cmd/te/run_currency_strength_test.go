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
	"github.com/romeo-folie/trade-engine/internal/store"
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

type captureCurrencyStrengthJournal struct {
	started    []store.CurrencyStrengthRun
	evaluating []time.Time
	noSignals  []string
	entries    []store.CurrencyStrengthRun
	exits      []store.CurrencyStrengthRun
	failures   []string
	exitErrors int
}

func (j *captureCurrencyStrengthJournal) Start(_ context.Context, run store.CurrencyStrengthRun) error {
	j.started = append(j.started, run)
	return nil
}

func (j *captureCurrencyStrengthJournal) MarkEvaluating(_ context.Context, date time.Time) error {
	j.evaluating = append(j.evaluating, date)
	return nil
}

func (j *captureCurrencyStrengthJournal) RecordNoSignal(_ context.Context, _ time.Time, reason string) error {
	j.noSignals = append(j.noSignals, reason)
	return nil
}

func (j *captureCurrencyStrengthJournal) RecordEntry(_ context.Context, run store.CurrencyStrengthRun) error {
	j.entries = append(j.entries, run)
	return nil
}

func (j *captureCurrencyStrengthJournal) RecordExit(_ context.Context, run store.CurrencyStrengthRun) error {
	j.exits = append(j.exits, run)
	if j.exitErrors > 0 {
		j.exitErrors--
		return errors.New("temporary journal failure")
	}
	return nil
}

func (j *captureCurrencyStrengthJournal) RecordFailure(_ context.Context, _ time.Time, message string) error {
	j.failures = append(j.failures, message)
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
	journal := &captureCurrencyStrengthJournal{}
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
	runner.SetJournal(journal)

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
	if len(journal.evaluating) != 1 || len(journal.noSignals) != 1 ||
		!strings.Contains(journal.noSignals[0], "existing GBP/USD position") {
		t.Fatalf("expected durable evaluating/no-signal history, got %+v", journal)
	}
}

type successfulCurrencyStrengthExecutor struct {
	result execution.OrderResult
	calls  int
}

func (f *successfulCurrencyStrengthExecutor) PlaceOrder(context.Context, execution.OrderRequest) (execution.OrderResult, error) {
	f.calls++
	return f.result, nil
}

func (*successfulCurrencyStrengthExecutor) CancelOrder(context.Context, string) error {
	return nil
}

func (*successfulCurrencyStrengthExecutor) GetOrderStatus(context.Context, string) (execution.OrderResult, error) {
	return execution.OrderResult{}, nil
}

func (*successfulCurrencyStrengthExecutor) GetPositions(context.Context) ([]execution.Position, error) {
	return nil, nil
}

func (*successfulCurrencyStrengthExecutor) GetAccount(context.Context) (execution.AccountInfo, error) {
	return execution.AccountInfo{}, nil
}

func TestCurrencyStrengthExitPersistsRealizedPnL(t *testing.T) {
	journal := &captureCurrencyStrengthJournal{}
	broker := &successfulCurrencyStrengthExecutor{result: execution.OrderResult{
		BrokerOrderID: "EXIT-1",
		Status:        "filled",
		FilledPrice:   1.105,
		FilledQty:     100000,
	}}
	runner := &currencyStrengthPaperRunner{
		broker:    broker,
		statePath: t.TempDir() + "/currency-strength.json",
		now: func() time.Time {
			return time.Date(2026, 7, 27, 18, 0, 0, 0, time.UTC)
		},
		state: &currencyStrengthRunState{
			Date:           "2026-07-27",
			Symbol:         "GBP/USD",
			Side:           "buy",
			Quantity:       100000,
			EntryFillPrice: 1.100,
		},
		journal: journal,
	}

	if err := runner.exitActive(context.Background()); err != nil {
		t.Fatalf("exitActive: %v", err)
	}
	if len(journal.exits) != 1 || journal.exits[0].RealizedPnL == nil {
		t.Fatalf("expected one journaled exit with P&L, got %+v", journal.exits)
	}
	if got := *journal.exits[0].RealizedPnL; got < 499.99 || got > 500.01 {
		t.Fatalf("realized P&L = %.2f, want 500.00", got)
	}
	if runner.state != nil {
		t.Fatal("runner state was not cleared after journaled exit")
	}
}

func TestCurrencyStrengthExitJournalRetryDoesNotResubmitOrder(t *testing.T) {
	journal := &captureCurrencyStrengthJournal{exitErrors: 1}
	broker := &successfulCurrencyStrengthExecutor{result: execution.OrderResult{
		BrokerOrderID: "EXIT-RETRY",
		Status:        "pending",
	}}
	runner := &currencyStrengthPaperRunner{
		cfg: config.CurrencyStrengthConfig{
			Timezone:      "UTC",
			StrengthStart: "06:00",
			StrengthEnd:   "09:00",
			ExitTime:      "18:00",
			Units:         100000,
		},
		broker:        broker,
		statePath:     t.TempDir() + "/currency-strength.json",
		now:           func() time.Time { return time.Date(2026, 7, 27, 18, 0, 0, 0, time.UTC) },
		entryRecorded: true,
		state: &currencyStrengthRunState{
			Date:     "2026-07-27",
			Symbol:   "GBP/USD",
			Side:     "buy",
			Quantity: 100000,
		},
		journal: journal,
	}

	if err := runner.exitActive(context.Background()); err != nil {
		t.Fatalf("first exitActive: %v", err)
	}
	if broker.calls != 1 || runner.state == nil || runner.state.ExitOrderID == "" {
		t.Fatalf("failed journal write did not retain submitted exit safely: calls=%d state=%+v", broker.calls, runner.state)
	}
	if err := runner.exitActive(context.Background()); err != nil {
		t.Fatalf("retry exitActive: %v", err)
	}
	if broker.calls != 1 {
		t.Fatalf("journal retry submitted a duplicate exit: calls=%d", broker.calls)
	}
	if runner.state != nil {
		t.Fatal("state was not cleared after successful journal retry")
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
