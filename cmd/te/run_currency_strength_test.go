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
	started     []store.CurrencyStrengthRun
	evaluating  []time.Time
	noSignals   []string
	entries     []store.CurrencyStrengthRun
	exits       []store.CurrencyStrengthRun
	failures    []string
	exitErrors  int
	entryErrors int
	startErrors int
	runs        map[string]*store.CurrencyStrengthRun
	activeRun   *store.CurrencyStrengthRun
}

func (j *captureCurrencyStrengthJournal) Start(_ context.Context, run store.CurrencyStrengthRun) error {
	j.started = append(j.started, run)
	if j.startErrors > 0 {
		j.startErrors--
		return errors.New("temporary journal startup failure")
	}
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
	if j.entryErrors > 0 {
		j.entryErrors--
		return errors.New("temporary entry journal failure")
	}
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

func (j *captureCurrencyStrengthJournal) Get(_ context.Context, date time.Time) (*store.CurrencyStrengthRun, error) {
	if j.runs == nil {
		return nil, nil
	}
	return j.runs[date.Format("2006-01-02")], nil
}

func (j *captureCurrencyStrengthJournal) GetActive(_ context.Context, date time.Time) (*store.CurrencyStrengthRun, error) {
	if j.activeRun != nil && !j.activeRun.SessionDate.After(date) {
		return j.activeRun, nil
	}
	return nil, nil
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
	result             execution.OrderResult
	statusResult       execution.OrderResult
	positions          []execution.Position
	requests           []execution.OrderRequest
	calls              int
	keepPositionOnFill bool
}

type workingExitExecutor struct {
	*successfulCurrencyStrengthExecutor
	orders []execution.WorkingOrder
}

type sequencedCurrencyStrengthExecutor struct {
	statuses  []execution.OrderResult
	positions [][]execution.Position
	requests  []execution.OrderRequest
	cancels   int
}

func (f *sequencedCurrencyStrengthExecutor) PlaceOrder(_ context.Context, req execution.OrderRequest) (execution.OrderResult, error) {
	f.requests = append(f.requests, req)
	return execution.OrderResult{BrokerOrderID: "EXIT-SEQUENCED", Status: "pending"}, nil
}

func (f *sequencedCurrencyStrengthExecutor) CancelOrder(context.Context, string) error {
	f.cancels++
	return nil
}

func (f *sequencedCurrencyStrengthExecutor) GetOrderStatus(context.Context, string) (execution.OrderResult, error) {
	if len(f.statuses) == 0 {
		return execution.OrderResult{}, execution.ErrOrderNotFound
	}
	result := f.statuses[0]
	f.statuses = f.statuses[1:]
	return result, nil
}

func (f *sequencedCurrencyStrengthExecutor) GetPositions(context.Context) ([]execution.Position, error) {
	if len(f.positions) == 0 {
		return nil, nil
	}
	result := f.positions[0]
	if len(f.positions) > 1 {
		f.positions = f.positions[1:]
	}
	return result, nil
}

func (*sequencedCurrencyStrengthExecutor) GetAccount(context.Context) (execution.AccountInfo, error) {
	return execution.AccountInfo{}, nil
}

func (f *workingExitExecutor) GetWorkingOrders(context.Context) ([]execution.WorkingOrder, error) {
	return f.orders, nil
}

func (f *successfulCurrencyStrengthExecutor) PlaceOrder(_ context.Context, req execution.OrderRequest) (execution.OrderResult, error) {
	f.calls++
	f.requests = append(f.requests, req)
	if normalizeCurrencyStrengthOrderStatus(f.result.Status) == "filled" &&
		!f.keepPositionOnFill {
		f.positions = nil
	}
	return f.result, nil
}

func (*successfulCurrencyStrengthExecutor) CancelOrder(context.Context, string) error {
	return nil
}

func (f *successfulCurrencyStrengthExecutor) GetOrderStatus(context.Context, string) (execution.OrderResult, error) {
	if normalizeCurrencyStrengthOrderStatus(f.statusResult.Status) == "filled" &&
		!f.keepPositionOnFill {
		f.positions = nil
	}
	return f.statusResult, nil
}

func (f *successfulCurrencyStrengthExecutor) GetPositions(context.Context) ([]execution.Position, error) {
	return f.positions, nil
}

func (*successfulCurrencyStrengthExecutor) GetAccount(context.Context) (execution.AccountInfo, error) {
	return execution.AccountInfo{}, nil
}

func TestCurrencyStrengthCancelledEntryWithFillRetainsExposure(t *testing.T) {
	broker := &sequencedCurrencyStrengthExecutor{
		statuses: []execution.OrderResult{{
			BrokerOrderID: "ENTRY-PARTIAL-CANCELLED",
			Status:        "cancelled",
			FilledPrice:   1.101,
			FilledQty:     40000,
		}},
		positions: [][]execution.Position{{
			{Ticker: "GBP/USD", Quantity: 40000, AvgCostBasis: 1.101},
		}},
	}
	runner := &currencyStrengthPaperRunner{
		broker:    broker,
		statePath: t.TempDir() + "/currency-strength.json",
		state: &currencyStrengthRunState{
			Date:         "2026-07-27",
			Symbol:       "GBP/USD",
			Side:         "buy",
			Quantity:     100000,
			EntryOrderID: "ENTRY-PARTIAL-CANCELLED",
			EntryStatus:  "pending",
		},
	}
	if err := runner.reconcileActiveEntry(context.Background()); err != nil {
		t.Fatal(err)
	}
	if runner.state == nil || runner.state.EntryStatus != "filled" ||
		runner.state.EntryFilledQty != 40000 || runner.state.Quantity != 40000 {
		t.Fatalf("cancelled partial fill lost broker exposure: %+v", runner.state)
	}
}

func TestCurrencyStrengthTerminalFillWaitsForDelayedPosition(t *testing.T) {
	broker := &sequencedCurrencyStrengthExecutor{
		statuses: []execution.OrderResult{
			{BrokerOrderID: "ENTRY-DELAYED", Status: "cancelled", FilledQty: 40000},
			{BrokerOrderID: "ENTRY-DELAYED", Status: "cancelled", FilledQty: 40000},
		},
		positions: [][]execution.Position{
			nil,
			{{Ticker: "GBP/USD", Quantity: 40000, AvgCostBasis: 1.101}},
		},
	}
	runner := &currencyStrengthPaperRunner{
		broker:    broker,
		statePath: t.TempDir() + "/currency-strength.json",
		state: &currencyStrengthRunState{
			Date:         "2026-07-27",
			Symbol:       "GBP/USD",
			Side:         "buy",
			Quantity:     100000,
			EntryOrderID: "ENTRY-DELAYED",
			EntryStatus:  "pending",
		},
	}
	if err := runner.reconcileActiveEntry(context.Background()); err != nil {
		t.Fatal(err)
	}
	if runner.state == nil || runner.state.EntryFilledQty != 40000 {
		t.Fatalf("terminal fill evidence was cleared before position appeared: %+v", runner.state)
	}
	if err := runner.reconcileActiveEntry(context.Background()); err != nil {
		t.Fatal(err)
	}
	if runner.state == nil || runner.state.EntryStatus != "filled" ||
		runner.state.Quantity != 40000 {
		t.Fatalf("delayed broker position was not adopted: %+v", runner.state)
	}
}

func TestCurrencyStrengthWaitsForTerminalCancellationAndRefreshesExitSize(t *testing.T) {
	broker := &sequencedCurrencyStrengthExecutor{
		statuses: []execution.OrderResult{
			{BrokerOrderID: "ENTRY-CANCEL", Status: "pending", FilledQty: 25000},
			{BrokerOrderID: "ENTRY-CANCEL", Status: "cancelled", FilledQty: 40000},
		},
		positions: [][]execution.Position{
			{{Ticker: "GBP/USD", Quantity: 25000, AvgCostBasis: 1.10}},
			{{Ticker: "GBP/USD", Quantity: 25000, AvgCostBasis: 1.10}},
			{{Ticker: "GBP/USD", Quantity: 40000, AvgCostBasis: 1.10}},
		},
	}
	runner := &currencyStrengthPaperRunner{
		broker:        broker,
		statePath:     t.TempDir() + "/currency-strength.json",
		entryRecorded: true,
		state: &currencyStrengthRunState{
			Date:         "2026-07-27",
			Symbol:       "GBP/USD",
			Side:         "buy",
			Quantity:     100000,
			EntryOrderID: "ENTRY-CANCEL",
			EntryStatus:  "partial",
		},
		now: func() time.Time { return time.Date(2026, 7, 27, 18, 0, 0, 0, time.UTC) },
	}
	if err := runner.exitActive(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(broker.requests) != 0 {
		t.Fatalf("exit submitted before cancellation became terminal: %+v", broker.requests)
	}
	if err := runner.exitActive(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(broker.requests) != 1 || broker.requests[0].Quantity != 40000 {
		t.Fatalf("exit did not use refreshed post-cancel position: %+v", broker.requests)
	}
}

func TestCurrencyStrengthJournalFailureDoesNotBlockPostCancelExit(t *testing.T) {
	journal := &captureCurrencyStrengthJournal{entryErrors: 1}
	broker := &sequencedCurrencyStrengthExecutor{
		statuses: []execution.OrderResult{{
			BrokerOrderID: "ENTRY-CANCEL", Status: "cancelled", FilledQty: 40000,
		}},
		positions: [][]execution.Position{
			{{Ticker: "GBP/USD", Quantity: 40000, AvgCostBasis: 1.10}},
			{{Ticker: "GBP/USD", Quantity: 40000, AvgCostBasis: 1.10}},
		},
	}
	runner := &currencyStrengthPaperRunner{
		cfg:    config.CurrencyStrengthConfig{Units: 100000},
		broker: broker, journal: journal,
		statePath:     t.TempDir() + "/currency-strength.json",
		entryRecorded: true,
		state: &currencyStrengthRunState{
			Date:         "2026-07-27",
			Symbol:       "GBP/USD",
			Side:         "buy",
			Quantity:     100000,
			EntryOrderID: "ENTRY-CANCEL",
			EntryStatus:  "partial",
		},
		now: func() time.Time { return time.Date(2026, 7, 27, 18, 0, 0, 0, time.UTC) },
	}
	if err := runner.exitActive(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(broker.requests) != 1 {
		t.Fatalf("journal outage prevented broker flatten: requests=%+v", broker.requests)
	}
}

func TestCurrencyStrengthAcceptedEntryFallsBackToJournalWhenSidecarFails(t *testing.T) {
	journal := &captureCurrencyStrengthJournal{}
	runner := &currencyStrengthPaperRunner{
		cfg:       config.CurrencyStrengthConfig{Units: 100000},
		statePath: t.TempDir(), // Writing a file over this directory must fail.
		journal:   journal,
		state: &currencyStrengthRunState{
			Date:           "2026-07-27",
			Symbol:         "GBP/USD",
			Side:           "buy",
			Quantity:       100000,
			EntryOrderID:   "ENTRY-DURABLE",
			EntryStatus:    "pending",
			EntryFilledQty: 0,
			EnteredAt:      "2026-07-27T09:00:00Z",
		},
	}
	if err := runner.persistActiveEntryRecovery(context.Background()); err != nil {
		t.Fatalf("journal fallback should preserve accepted entry: %v", err)
	}
	if len(journal.entries) != 1 ||
		journal.entries[0].EntryOrderID != "ENTRY-DURABLE" {
		t.Fatalf("accepted entry was not journaled after sidecar failure: %+v", journal.entries)
	}
}

func TestCurrencyStrengthExitPersistsRealizedPnL(t *testing.T) {
	journal := &captureCurrencyStrengthJournal{}
	broker := &successfulCurrencyStrengthExecutor{result: execution.OrderResult{
		BrokerOrderID: "EXIT-1",
		Status:        "filled",
		FilledPrice:   1.105,
		FilledQty:     100000,
	}, positions: []execution.Position{{Ticker: "GBP/USD", Quantity: 100000}}}
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
			EntryStatus:    "filled",
			EntryFillPrice: 1.100,
			EntryFilledQty: 100000,
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
	}, statusResult: execution.OrderResult{
		BrokerOrderID: "EXIT-RETRY",
		Status:        "filled",
	}, positions: []execution.Position{{Ticker: "GBP/USD", Quantity: 100000}}}
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

func TestCurrencyStrengthActivePositionSurvivesJournalStartupFailure(t *testing.T) {
	journal := &captureCurrencyStrengthJournal{startErrors: 10}
	broker := &successfulCurrencyStrengthExecutor{
		result: execution.OrderResult{
			BrokerOrderID: "EXIT-SAFE",
			Status:        "filled",
			FilledQty:     100000,
		},
		positions: []execution.Position{{Ticker: "GBP/USD", Quantity: 100000}},
	}
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
		now:           func() time.Time { return time.Date(2026, 7, 27, 18, 1, 0, 0, time.UTC) },
		entryRecorded: true,
		state: &currencyStrengthRunState{
			Date:           "2026-07-27",
			Symbol:         "GBP/USD",
			Side:           "buy",
			Quantity:       100000,
			EntryOrderID:   "ENTRY-SAFE",
			EntryStatus:    "filled",
			EntryFilledQty: 100000,
		},
		journal: journal,
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := runner.Run(ctx)
	if err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("Run: %v", err)
	}
	if broker.calls != 1 {
		t.Fatalf("journal startup failure disabled broker exit management: calls=%d", broker.calls)
	}
}

func TestCurrencyStrengthRestoresCompletedDecision(t *testing.T) {
	date := time.Date(2026, 7, 27, 0, 0, 0, 0, time.UTC)
	journal := &captureCurrencyStrengthJournal{runs: map[string]*store.CurrencyStrengthRun{
		"2026-07-27": {SessionDate: date, Status: "no_signal"},
	}}
	runner := &currencyStrengthPaperRunner{journal: journal}
	if err := runner.restoreJournalState(context.Background(), date); err != nil {
		t.Fatal(err)
	}
	if runner.decisionDoneFor != "2026-07-27" {
		t.Fatalf("completed decision was not restored: %q", runner.decisionDoneFor)
	}
	if runner.state != nil {
		t.Fatalf("no-signal decision unexpectedly restored active state: %+v", runner.state)
	}
}

func TestCurrencyStrengthRestoresPriorDayActiveRunWithoutSidecar(t *testing.T) {
	today := time.Date(2026, 7, 28, 0, 0, 0, 0, time.UTC)
	yesterday := today.AddDate(0, 0, -1)
	journal := &captureCurrencyStrengthJournal{
		runs: map[string]*store.CurrencyStrengthRun{
			"2026-07-28": {SessionDate: today, Status: "scheduled"},
		},
		activeRun: &store.CurrencyStrengthRun{
			SessionDate:    yesterday,
			Status:         "entered",
			SignalSymbol:   "GBP/USD",
			SignalSide:     "buy",
			Units:          100000,
			EntryOrderID:   "ENTRY-OVERNIGHT",
			EntryStatus:    "filled",
			EntryFilledQty: 100000,
		},
	}
	runner := &currencyStrengthPaperRunner{journal: journal}
	if err := runner.restoreJournalState(context.Background(), today); err != nil {
		t.Fatal(err)
	}
	if runner.state == nil || runner.state.Date != "2026-07-27" ||
		runner.state.EntryOrderID != "ENTRY-OVERNIGHT" {
		t.Fatalf("prior-day obligation was not restored: %+v", runner.state)
	}
}

func TestCurrencyStrengthRestoresExitAckWithoutResubmission(t *testing.T) {
	date := time.Date(2026, 7, 27, 0, 0, 0, 0, time.UTC)
	journal := &captureCurrencyStrengthJournal{runs: map[string]*store.CurrencyStrengthRun{
		"2026-07-27": {
			SessionDate:    date,
			Status:         "exit_submitted",
			SignalSymbol:   "GBP/USD",
			SignalSide:     "buy",
			Units:          100000,
			EntryOrderID:   "ENTRY-1",
			EntryStatus:    "filled",
			ExitOrderID:    "EXIT-RECOVERED",
			ExitStatus:     "pending",
			EntryFilledQty: 100000,
		},
	}}
	broker := &successfulCurrencyStrengthExecutor{statusResult: execution.OrderResult{
		BrokerOrderID: "EXIT-RECOVERED",
		Status:        "filled",
		FilledQty:     100000,
	}}
	runner := &currencyStrengthPaperRunner{
		broker:    broker,
		statePath: t.TempDir() + "/currency-strength.json",
		state: &currencyStrengthRunState{
			Date:         "2026-07-27",
			Symbol:       "GBP/USD",
			Side:         "buy",
			Quantity:     100000,
			EntryOrderID: "ENTRY-1",
			EntryStatus:  "filled",
		},
		journal: journal,
	}
	if err := runner.mergeActiveStateFromJournal(context.Background()); err != nil {
		t.Fatal(err)
	}
	if runner.state.ExitOrderID != "EXIT-RECOVERED" {
		t.Fatalf("journal exit acknowledgement not restored: %+v", runner.state)
	}
	if err := runner.exitActive(context.Background()); err != nil {
		t.Fatal(err)
	}
	if broker.calls != 0 {
		t.Fatalf("restart resubmitted recovered exit: calls=%d", broker.calls)
	}
}

func TestCurrencyStrengthAdoptsBrokerWorkingExitWhenDurabilityFailed(t *testing.T) {
	base := &successfulCurrencyStrengthExecutor{
		statusResult: execution.OrderResult{
			BrokerOrderID: "EXIT-WORKING",
			Status:        "filled",
			FilledQty:     100000,
		},
		positions: []execution.Position{{Ticker: "GBP/USD", Quantity: 100000}},
	}
	broker := &workingExitExecutor{
		successfulCurrencyStrengthExecutor: base,
		orders: []execution.WorkingOrder{{
			OrderID:      "EXIT-WORKING",
			TradeID:      "currency-strength-exit-2026-07-27-GBPUSD",
			Ticker:       "GBP.USD",
			Side:         "sell",
			Status:       "pending",
			RemainingQty: 100000,
		}},
	}
	runner := &currencyStrengthPaperRunner{
		broker:        broker,
		statePath:     t.TempDir() + "/currency-strength.json",
		entryRecorded: true,
		state: &currencyStrengthRunState{
			Date:           "2026-07-27",
			Symbol:         "GBP/USD",
			Side:           "buy",
			Quantity:       100000,
			EntryOrderID:   "ENTRY-1",
			EntryStatus:    "filled",
			EntryFilledQty: 100000,
		},
		now: func() time.Time { return time.Date(2026, 7, 27, 18, 0, 0, 0, time.UTC) },
	}
	if err := runner.exitActive(context.Background()); err != nil {
		t.Fatal(err)
	}
	if base.calls != 0 {
		t.Fatalf("working broker exit was duplicated: calls=%d", base.calls)
	}
	if runner.state != nil {
		t.Fatalf("filled adopted exit did not clear state: %+v", runner.state)
	}
}

func TestCurrencyStrengthExitReferenceMatchesReservedProductionID(t *testing.T) {
	base := "currency-strength-exit-2026-07-27-GBPUSD"
	reserved := base + "-1785146400000000000-1"
	if !currencyStrengthExitReferenceMatches(reserved, base, base, false) {
		t.Fatal("journal-restored state did not recognize strategy-owned suffixed reference")
	}
	if currencyStrengthExitReferenceMatches("orb-GBP/USD-1", base, base, false) {
		t.Fatal("foreign strategy reference matched currency-strength ownership")
	}
	if currencyStrengthExitReferenceMatches(base+"-other", reserved, base, true) {
		t.Fatal("sidecar-owned reference did not require an exact match")
	}
}

func TestCurrencyStrengthDoesNotAdoptAnotherStrategysWorkingOrder(t *testing.T) {
	base := &successfulCurrencyStrengthExecutor{
		result: execution.OrderResult{
			BrokerOrderID: "EXIT-OWNED",
			Status:        "pending",
		},
		positions: []execution.Position{{Ticker: "GBP/USD", Quantity: 100000}},
	}
	broker := &workingExitExecutor{
		successfulCurrencyStrengthExecutor: base,
		orders: []execution.WorkingOrder{{
			OrderID:      "ORB-SELL",
			TradeID:      "orb-GBP/USD-123",
			Ticker:       "GBP/USD",
			Side:         "sell",
			Status:       "pending",
			RemainingQty: 100000,
		}},
	}
	runner := &currencyStrengthPaperRunner{
		broker:        broker,
		statePath:     t.TempDir() + "/currency-strength.json",
		entryRecorded: true,
		state: &currencyStrengthRunState{
			Date:           "2026-07-27",
			Symbol:         "GBP/USD",
			Side:           "buy",
			Quantity:       100000,
			EntryStatus:    "filled",
			EntryFilledQty: 100000,
		},
		now: func() time.Time { return time.Date(2026, 7, 27, 18, 0, 0, 0, time.UTC) },
	}
	if err := runner.exitActive(context.Background()); err != nil {
		t.Fatal(err)
	}
	if base.calls != 1 || runner.state.ExitOrderID != "EXIT-OWNED" {
		t.Fatalf("foreign working order was adopted: calls=%d state=%+v", base.calls, runner.state)
	}
}

func TestCurrencyStrengthRetainsPendingExitUntilTerminal(t *testing.T) {
	journal := &captureCurrencyStrengthJournal{}
	broker := &successfulCurrencyStrengthExecutor{
		result: execution.OrderResult{
			BrokerOrderID: "EXIT-PENDING",
			Status:        "pending",
		},
		statusResult: execution.OrderResult{
			BrokerOrderID: "EXIT-PENDING",
			Status:        "pending",
		},
		positions: []execution.Position{{Ticker: "GBP/USD", Quantity: 100000}},
	}
	runner := &currencyStrengthPaperRunner{
		broker:        broker,
		statePath:     t.TempDir() + "/currency-strength.json",
		entryRecorded: true,
		state: &currencyStrengthRunState{
			Date:           "2026-07-27",
			Symbol:         "GBP/USD",
			Side:           "buy",
			Quantity:       100000,
			EntryStatus:    "filled",
			EntryFilledQty: 100000,
		},
		journal: journal,
		now:     func() time.Time { return time.Date(2026, 7, 27, 18, 0, 0, 0, time.UTC) },
	}
	if err := runner.exitActive(context.Background()); err != nil {
		t.Fatal(err)
	}
	if runner.state == nil || runner.state.ExitOrderID != "EXIT-PENDING" {
		t.Fatalf("pending exit state was cleared: %+v", runner.state)
	}
	broker.statusResult = execution.OrderResult{
		BrokerOrderID: "EXIT-PENDING",
		Status:        "filled",
		FilledQty:     100000,
	}
	if err := runner.exitActive(context.Background()); err != nil {
		t.Fatal(err)
	}
	if runner.state != nil || broker.calls != 1 {
		t.Fatalf("terminal exit reconciliation failed: state=%+v calls=%d", runner.state, broker.calls)
	}
}

func TestCurrencyStrengthResubmitsConfirmedResidualWithoutPnLOverstatement(t *testing.T) {
	journal := &captureCurrencyStrengthJournal{}
	broker := &successfulCurrencyStrengthExecutor{
		result: execution.OrderResult{
			BrokerOrderID: "EXIT-PART-1",
			Status:        "filled",
			FilledPrice:   1.102,
			FilledQty:     50000,
		},
		positions:          []execution.Position{{Ticker: "GBP/USD", Quantity: 50000}},
		keepPositionOnFill: true,
	}
	runner := &currencyStrengthPaperRunner{
		broker:        broker,
		statePath:     t.TempDir() + "/currency-strength.json",
		entryRecorded: true,
		state: &currencyStrengthRunState{
			Date:           "2026-07-27",
			Symbol:         "GBP/USD",
			Side:           "buy",
			Quantity:       100000,
			EntryStatus:    "filled",
			EntryFillPrice: 1.100,
			EntryFilledQty: 100000,
		},
		journal: journal,
		now:     func() time.Time { return time.Date(2026, 7, 27, 18, 0, 0, 0, time.UTC) },
	}
	if err := runner.exitActive(context.Background()); err != nil {
		t.Fatal(err)
	}
	if runner.state == nil || runner.state.ExitOrderID != "" || !runner.state.ExitRetry {
		t.Fatalf("residual position was not prepared for a safe retry: %+v", runner.state)
	}

	broker.result = execution.OrderResult{
		BrokerOrderID: "EXIT-PART-2",
		Status:        "filled",
		FilledPrice:   1.103,
		FilledQty:     50000,
	}
	broker.keepPositionOnFill = false
	if err := runner.exitActive(context.Background()); err != nil {
		t.Fatal(err)
	}
	if broker.calls != 2 || runner.state != nil {
		t.Fatalf("residual flatten was not completed: calls=%d state=%+v", broker.calls, runner.state)
	}
	if broker.requests[0].TradeID == broker.requests[1].TradeID {
		t.Fatalf("residual exits reused broker client order id %q", broker.requests[0].TradeID)
	}
	last := journal.exits[len(journal.exits)-1]
	if last.RealizedPnL != nil {
		t.Fatalf("multi-order exit published incomplete realized P&L: %+v", last)
	}
}

func TestCurrencyStrengthResidualRetryProceedsWhileJournalIsDown(t *testing.T) {
	journal := &captureCurrencyStrengthJournal{exitErrors: 1}
	broker := &successfulCurrencyStrengthExecutor{
		result: execution.OrderResult{
			BrokerOrderID: "EXIT-RESIDUAL",
			Status:        "filled",
			FilledQty:     50000,
		},
		positions:          []execution.Position{{Ticker: "GBP/USD", Quantity: 50000}},
		keepPositionOnFill: true,
	}
	runner := &currencyStrengthPaperRunner{
		broker:        broker,
		statePath:     t.TempDir() + "/currency-strength.json",
		entryRecorded: true,
		state: &currencyStrengthRunState{
			Date:           "2026-07-27",
			Symbol:         "GBP/USD",
			Side:           "buy",
			Quantity:       100000,
			EntryStatus:    "filled",
			EntryFilledQty: 100000,
		},
		journal: journal,
		now:     func() time.Time { return time.Date(2026, 7, 27, 18, 0, 0, 0, time.UTC) },
	}
	if err := runner.exitActive(context.Background()); err != nil {
		t.Fatal(err)
	}
	if runner.state == nil || runner.state.ExitOrderID != "" || !runner.state.ExitRetry ||
		len(runner.state.PendingExitAudits) != 1 {
		t.Fatalf("journal outage pinned terminal residual order: %+v", runner.state)
	}
	if err := runner.syncPendingExitAudit(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(runner.state.PendingExitAudits) != 0 {
		t.Fatalf("durable audit snapshot was not retried: %+v", runner.state.PendingExitAudits)
	}
	restarted := &currencyStrengthPaperRunner{statePath: runner.statePath}
	if err := restarted.loadState(); err != nil {
		t.Fatal(err)
	}
	if restarted.state == nil || len(restarted.state.PendingExitAudits) != 0 {
		t.Fatalf("successful audit dequeue was not persisted: %+v", restarted.state)
	}
}

func TestCurrencyStrengthRestoresResidualAsExitRetry(t *testing.T) {
	run := store.CurrencyStrengthRun{
		SessionDate:    time.Date(2026, 7, 27, 0, 0, 0, 0, time.UTC),
		Status:         "exit_failed",
		SignalSymbol:   "GBP/USD",
		SignalSide:     "buy",
		EntryStatus:    "filled",
		EntryFilledQty: 100000,
		ExitOrderID:    "EXIT-TERMINAL",
		ExitStatus:     "residual",
		ExitFilledQty:  50000,
	}
	state := currencyStrengthStateFromRun(run)
	if state.ExitOrderID != "" || !state.ExitRetry {
		t.Fatalf("terminal residual restored as working: %+v", state)
	}
}

func TestCurrencyStrengthMergePreservesLocalBrokerTruth(t *testing.T) {
	state := &currencyStrengthRunState{
		EntryStatus:    "filled",
		EntryFillPrice: 1.101,
		EntryFilledQty: 100000,
		Quantity:       100000,
		ExitRetry:      true,
	}
	stale := store.CurrencyStrengthRun{
		Status:         "exit_submitted",
		EntryStatus:    "partial",
		EntryFillPrice: 1.099,
		EntryFilledQty: 25000,
		ExitOrderID:    "STALE-EXIT",
		ExitStatus:     "pending",
	}
	mergeCurrencyStrengthRunIntoState(state, stale)
	if state.EntryStatus != "filled" {
		t.Fatalf("stale journal downgraded broker-confirmed entry: %+v", state)
	}
	if state.EntryFilledQty != 100000 || state.EntryFillPrice != 1.101 {
		t.Fatalf("stale journal overwrote newer fill economics: %+v", state)
	}
	if state.ExitOrderID != "" {
		t.Fatalf("stale journal replaced residual retry state: %+v", state)
	}
	if currencyStrengthJournalCoversEntry(state, stale) {
		t.Fatal("stale journal was incorrectly considered caught up")
	}
	date := time.Date(2026, 7, 27, 0, 0, 0, 0, time.UTC)
	stale.SessionDate = date
	state.Date = "2026-07-27"
	runner := &currencyStrengthPaperRunner{
		state:         state,
		entryRecorded: false,
		journal: &captureCurrencyStrengthJournal{runs: map[string]*store.CurrencyStrengthRun{
			"2026-07-27": &stale,
		}},
	}
	if err := runner.mergeActiveStateFromJournal(context.Background()); err != nil {
		t.Fatal(err)
	}
	if runner.entryRecorded {
		t.Fatal("stale journal incorrectly cleared the dirty entry marker")
	}
}

func TestCurrencyStrengthPnLUsesMatchedFilledQuantity(t *testing.T) {
	journal := &captureCurrencyStrengthJournal{}
	runner := &currencyStrengthPaperRunner{
		entryRecorded: true,
		journal:       journal,
		state: &currencyStrengthRunState{
			Date:           "2026-07-27",
			Side:           "buy",
			EntryStatus:    "filled",
			EntryFillPrice: 1.100,
			EntryFilledQty: 100000,
			ExitOrderID:    "EXIT-PARTIAL-QTY",
			ExitStatus:     "filled",
			ExitFillPrice:  1.105,
			ExitFilledQty:  40000,
			ExitedAt:       "2026-07-27T18:00:00Z",
		},
	}
	if err := runner.recordExitJournal(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(journal.exits) != 1 || journal.exits[0].RealizedPnL == nil {
		t.Fatalf("expected matched-fill P&L, got %+v", journal.exits)
	}
	if got := *journal.exits[0].RealizedPnL; got < 199.99 || got > 200.01 {
		t.Fatalf("matched-fill P&L = %.2f, want 200.00", got)
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
