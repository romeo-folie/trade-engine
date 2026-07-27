package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
		"exit submitted",
		"SELL USD/JPY · 100000 units",
		"Order OID-X · Filled",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("exit message missing %q\ngot:\n%s", want, msg)
		}
	}
}

func TestBuildCurrencyStrengthFlattenedMessage(t *testing.T) {
	msg := buildCurrencyStrengthFlattenedMessage("USD/JPY", "OID-X", 100000, 151.25)
	for _, want := range []string{"position flattened", "confirmed closed", "fill 151.25000"} {
		if !strings.Contains(msg, want) {
			t.Errorf("flattened message missing %q\ngot:\n%s", want, msg)
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
	recentFills        []execution.BrokerOrder
	requests           []execution.OrderRequest
	calls              int
	keepPositionOnFill bool
}

type workingExitExecutor struct {
	*successfulCurrencyStrengthExecutor
	orders  []execution.WorkingOrder
	cancels int
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

func (f *workingExitExecutor) CancelOrder(context.Context, string) error {
	f.cancels++
	return nil
}

func (f *successfulCurrencyStrengthExecutor) PlaceOrder(_ context.Context, req execution.OrderRequest) (execution.OrderResult, error) {
	f.calls++
	f.requests = append(f.requests, req)
	if normalizeCurrencyStrengthOrderStatus(f.result.Status) == "filled" &&
		f.result.FilledQty > 0 {
		found := false
		for _, fill := range f.recentFills {
			if fill.BrokerOrderID == f.result.BrokerOrderID {
				found = true
				break
			}
		}
		if !found {
			f.recentFills = append(f.recentFills, execution.BrokerOrder{
				BrokerOrderID: f.result.BrokerOrderID,
				TradeID:       req.TradeID,
				Symbol:        req.Ticker,
				Side:          string(req.Side),
				FilledQty:     f.result.FilledQty,
				FilledAvgPx:   f.result.FilledPrice,
				FilledAt:      time.Now().UTC(),
			})
		}
	}
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

func (f *successfulCurrencyStrengthExecutor) RecentFills(context.Context) ([]execution.BrokerOrder, error) {
	return f.recentFills, nil
}

func TestCurrencyStrengthRecoversImmediatelyFilledEntryByTradeID(t *testing.T) {
	broker := &successfulCurrencyStrengthExecutor{
		statusResult: execution.OrderResult{
			BrokerOrderID: "ENTRY-FILLED", Status: "filled",
			FilledPrice: 1.1, FilledQty: 100000,
		},
		positions: []execution.Position{{
			Ticker: "GBP/USD", Quantity: 75000, AvgCostBasis: 1.10125,
		}},
		recentFills: []execution.BrokerOrder{
			{
				BrokerOrderID: "ENTRY-FILLED",
				TradeID:       "currency-strength-2026-07-27-GBPUSD",
				Symbol:        "GBP",
				Side:          "buy",
				FilledQty:     100000,
				FilledAvgPx:   1.1,
				FilledAt:      time.Date(2026, 7, 27, 9, 0, 0, 0, time.UTC),
			},
			{
				BrokerOrderID: "MANUAL-REDUCTION",
				Symbol:        "GBP/USD",
				Side:          "sell",
				FilledQty:     25000,
				FilledAvgPx:   1.101,
				FilledAt:      time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC),
			},
		},
	}
	runner := &currencyStrengthPaperRunner{
		accountID: "DU123",
		cfg: config.CurrencyStrengthConfig{
			Units:            100000,
			TradeInstruments: []string{"GBP/USD"},
		},
		broker:    broker,
		statePath: t.TempDir() + "/currency-strength.json",
	}
	recovered, err := runner.recoverEntryObligation(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !recovered || runner.state == nil {
		t.Fatalf("filled strategy entry was not recovered: %+v", runner.state)
	}
	if runner.state.AccountID != "DU123" || runner.state.Symbol != "GBP/USD" ||
		runner.state.Quantity != 75000 || runner.state.EntryFilledQty != 100000 ||
		runner.state.EntryFillPrice != 1.1 {
		t.Fatalf("recovery did not reconstruct broker execution truth: %+v", runner.state)
	}
}

func TestCurrencyStrengthRecoveredPartialEntryRemainsCancellable(t *testing.T) {
	broker := &successfulCurrencyStrengthExecutor{
		statusResult: execution.OrderResult{
			BrokerOrderID: "ENTRY-PARTIAL", Status: "partial",
			FilledPrice: 1.1, FilledQty: 40000,
		},
		positions: []execution.Position{{
			Ticker: "GBP/USD", Quantity: 40000, AvgCostBasis: 1.1,
		}},
		recentFills: []execution.BrokerOrder{{
			BrokerOrderID: "ENTRY-PARTIAL",
			TradeID:       "currency-strength-2026-07-27-GBPUSD",
			Symbol:        "GBP/USD", Side: "buy", FilledQty: 40000,
			FilledAvgPx: 1.1, FilledAt: time.Date(2026, 7, 27, 9, 0, 0, 0, time.UTC),
		}},
	}
	runner := &currencyStrengthPaperRunner{
		cfg: config.CurrencyStrengthConfig{
			Units: 100000, TradeInstruments: []string{"GBP/USD"},
		},
		broker: broker, statePath: t.TempDir() + "/currency-strength.json",
	}
	recovered, err := runner.recoverEntryObligation(context.Background())
	if err != nil || !recovered {
		t.Fatalf("partial entry recovery = %v, %v", recovered, err)
	}
	if runner.state == nil || runner.state.EntryStatus != "partial" ||
		runner.state.EntryFilledQty != 40000 {
		t.Fatalf("working entry remainder was incorrectly marked terminal: %+v", runner.state)
	}
}

func TestCurrencyStrengthDoesNotClaimReopenedManualPosition(t *testing.T) {
	broker := &successfulCurrencyStrengthExecutor{
		positions: []execution.Position{{
			Ticker: "GBP/USD", Quantity: 100000, AvgCostBasis: 1.2,
		}},
		recentFills: []execution.BrokerOrder{
			{
				BrokerOrderID: "ENTRY-OLD",
				TradeID:       "currency-strength-2026-07-24-GBPUSD",
				Symbol:        "GBP/USD", Side: "buy", FilledQty: 100000,
				FilledAvgPx: 1.1, FilledAt: time.Date(2026, 7, 24, 9, 0, 0, 0, time.UTC),
			},
			{
				BrokerOrderID: "EXIT-OLD",
				TradeID:       "currency-strength-exit-2026-07-24-GBPUSD-1",
				Symbol:        "GBP/USD", Side: "sell", FilledQty: 100000,
				FilledAvgPx: 1.11, FilledAt: time.Date(2026, 7, 24, 18, 0, 0, 0, time.UTC),
			},
			{
				BrokerOrderID: "MANUAL-NEW",
				Symbol:        "GBP/USD", Side: "buy", FilledQty: 100000,
				FilledAvgPx: 1.2, FilledAt: time.Date(2026, 7, 27, 8, 0, 0, 0, time.UTC),
			},
		},
	}
	runner := &currencyStrengthPaperRunner{
		cfg: config.CurrencyStrengthConfig{
			Units: 100000, TradeInstruments: []string{"GBP/USD"},
		},
		broker: broker, statePath: t.TempDir() + "/currency-strength.json",
		now: func() time.Time { return time.Date(2026, 7, 27, 19, 0, 0, 0, time.UTC) },
	}
	recovered, err := runner.recoverEntryObligation(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if recovered || runner.state != nil {
		t.Fatalf("historical strategy lifecycle claimed a manual position: %+v", runner.state)
	}
}

func TestCurrencyStrengthRecoversWorkingEntryOutsideDecisionWindow(t *testing.T) {
	broker := &workingExitExecutor{
		successfulCurrencyStrengthExecutor: &successfulCurrencyStrengthExecutor{},
		orders: []execution.WorkingOrder{{
			OrderID: "ENTRY-WORKING",
			TradeID: "currency-strength-2026-07-27-GBPUSD",
			Ticker:  "GBP.USD", Side: "buy", Status: "pending", RemainingQty: 100000,
		}},
	}
	runner := &currencyStrengthPaperRunner{
		cfg: config.CurrencyStrengthConfig{
			Units: 100000, TradeInstruments: []string{"GBP/USD"},
		},
		broker: broker, statePath: t.TempDir() + "/currency-strength.json",
		now: func() time.Time { return time.Date(2026, 7, 27, 19, 0, 0, 0, time.UTC) },
	}
	recovered, err := runner.recoverEntryObligation(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !recovered || runner.state == nil || runner.state.EntryOrderID != "ENTRY-WORKING" {
		t.Fatalf("working entry was not recovered independently of decision time: %+v", runner.state)
	}
}

func TestCurrencyStrengthRetriesFilledEntryRecoveryUntilVisible(t *testing.T) {
	now := time.Date(2026, 7, 27, 18, 30, 0, 0, time.UTC)
	broker := &successfulCurrencyStrengthExecutor{
		positions: []execution.Position{{
			Ticker: "GBP/USD", Quantity: 100000, AvgCostBasis: 1.1,
		}},
	}
	runner := &currencyStrengthPaperRunner{
		cfg: config.CurrencyStrengthConfig{
			Units: 100000, TradeInstruments: []string{"GBP/USD"},
		},
		broker: broker, statePath: t.TempDir() + "/currency-strength.json",
		now: func() time.Time { return now },
	}
	recovered, err := runner.recoverEntryObligation(context.Background())
	if err != nil || recovered {
		t.Fatalf("initial invisible fill recovery = %v, %v", recovered, err)
	}
	broker.recentFills = []execution.BrokerOrder{{
		BrokerOrderID: "ENTRY-DELAYED-VISIBILITY",
		TradeID:       "currency-strength-2026-07-27-GBPUSD",
		Symbol:        "GBP/USD", Side: "buy", FilledQty: 100000,
		FilledAvgPx: 1.1, FilledAt: now.Add(-time.Minute),
	}}
	now = now.Add(currencyStrengthTradeRecoveryInterval + time.Second)
	recovered, err = runner.recoverEntryObligation(context.Background())
	if err != nil || !recovered || runner.state == nil {
		t.Fatalf("later-visible fill was not recovered: recovered=%v err=%v state=%+v",
			recovered, err, runner.state)
	}
}

func TestCurrencyStrengthRecoversReservedFilledExitBeforeBrokerFlat(t *testing.T) {
	journal := &captureCurrencyStrengthJournal{}
	broker := &successfulCurrencyStrengthExecutor{
		statusResult: execution.OrderResult{
			BrokerOrderID: "EXIT-FILLED", Status: "filled",
			FilledPrice: 1.1056, FilledQty: 100000,
		},
		recentFills: []execution.BrokerOrder{
			{
				BrokerOrderID: "EXIT-FILLED",
				TradeID:       "currency-strength-exit-2026-07-27-GBPUSD-123-1",
				Symbol:        "GBP/USD", Side: "sell", FilledQty: 40000,
				FilledAvgPx: 1.105, FilledAt: time.Date(2026, 7, 27, 18, 0, 0, 0, time.UTC),
			},
			{
				BrokerOrderID: "EXIT-FILLED",
				TradeID:       "currency-strength-exit-2026-07-27-GBPUSD-123-1",
				Symbol:        "GBP/USD", Side: "sell", FilledQty: 60000,
				FilledAvgPx: 1.106, FilledAt: time.Date(2026, 7, 27, 18, 0, 1, 0, time.UTC),
			},
		},
	}
	runner := &currencyStrengthPaperRunner{
		broker: broker, journal: journal, entryRecorded: true,
		statePath: t.TempDir() + "/currency-strength.json",
		state: &currencyStrengthRunState{
			Date: "2026-07-27", Symbol: "GBP/USD", Side: "buy", Quantity: 100000,
			EntryStatus: "filled", EntryFillPrice: 1.1, EntryFilledQty: 100000,
			ExitTradeID: "currency-strength-exit-2026-07-27-GBPUSD-123-1",
		},
	}
	if err := runner.exitActive(context.Background()); err != nil {
		t.Fatal(err)
	}
	if runner.state != nil || len(journal.exits) != 1 {
		t.Fatalf("filled exit was not recovered and finalized: state=%+v exits=%+v",
			runner.state, journal.exits)
	}
	got := journal.exits[0]
	if got.ExitOrderID != "EXIT-FILLED" || got.ExitFilledQty != 100000 ||
		got.ExitFillPrice < 1.10559 || got.ExitFillPrice > 1.10561 ||
		got.RealizedPnL == nil {
		t.Fatalf("recovered exit economics incomplete: %+v", got)
	}
}

func TestCurrencyStrengthRecoversFilledExitFromJournalOnlyPrefix(t *testing.T) {
	journal := &captureCurrencyStrengthJournal{}
	broker := &successfulCurrencyStrengthExecutor{
		statusResult: execution.OrderResult{
			BrokerOrderID: "EXIT-JOURNAL-ONLY", Status: "filled",
			FilledPrice: 1.105, FilledQty: 100000,
		},
		recentFills: []execution.BrokerOrder{{
			BrokerOrderID: "EXIT-JOURNAL-ONLY",
			TradeID:       "currency-strength-exit-2026-07-27-GBPUSD-999-1",
			Symbol:        "GBP/USD", Side: "sell", FilledQty: 100000,
			FilledAvgPx: 1.105, FilledAt: time.Date(2026, 7, 27, 18, 0, 0, 0, time.UTC),
		}},
	}
	runner := &currencyStrengthPaperRunner{
		broker: broker, journal: journal, entryRecorded: true,
		statePath: t.TempDir() + "/currency-strength.json",
		state: &currencyStrengthRunState{
			Date: "2026-07-27", Symbol: "GBP/USD", Side: "buy", Quantity: 100000,
			EntryStatus: "filled", EntryFillPrice: 1.1, EntryFilledQty: 100000,
		},
	}
	if err := runner.exitActive(context.Background()); err != nil {
		t.Fatal(err)
	}
	if runner.state != nil || len(journal.exits) != 1 ||
		journal.exits[0].ExitOrderID != "EXIT-JOURNAL-ONLY" {
		t.Fatalf("journal-only exit fill was not recovered: state=%+v exits=%+v",
			runner.state, journal.exits)
	}
}

func TestCurrencyStrengthRecoveredPartialExitRemainsWorking(t *testing.T) {
	broker := &successfulCurrencyStrengthExecutor{
		statusResult: execution.OrderResult{
			BrokerOrderID: "EXIT-PARTIAL", Status: "partial",
			FilledPrice: 1.105, FilledQty: 40000,
		},
		positions: []execution.Position{{Ticker: "GBP/USD", Quantity: 60000}},
		recentFills: []execution.BrokerOrder{{
			BrokerOrderID: "EXIT-PARTIAL",
			TradeID:       "currency-strength-exit-2026-07-27-GBPUSD-999-1",
			Symbol:        "GBP/USD", Side: "sell", FilledQty: 40000,
			FilledAvgPx: 1.105, FilledAt: time.Date(2026, 7, 27, 18, 0, 0, 0, time.UTC),
		}},
	}
	runner := &currencyStrengthPaperRunner{
		broker: broker, entryRecorded: true,
		statePath: t.TempDir() + "/currency-strength.json",
		state: &currencyStrengthRunState{
			Date: "2026-07-27", Symbol: "GBP/USD", Side: "buy", Quantity: 100000,
			EntryStatus: "filled", EntryFillPrice: 1.1, EntryFilledQty: 100000,
			ExitTradeID: "currency-strength-exit-2026-07-27-GBPUSD-999-1",
		},
	}
	if err := runner.exitActive(context.Background()); err != nil {
		t.Fatal(err)
	}
	if runner.state == nil || runner.state.ExitOrderID != "EXIT-PARTIAL" ||
		runner.state.ExitStatus != "partial" || runner.state.ExitFilledQty != 40000 {
		t.Fatalf("working exit remainder was incorrectly marked terminal: %+v", runner.state)
	}
	if broker.calls != 0 {
		t.Fatalf("partial exit recovery submitted a duplicate order: calls=%d", broker.calls)
	}
}

func TestCurrencyStrengthFilledExitWaitsForPortfolioPropagation(t *testing.T) {
	now := time.Date(2026, 7, 27, 18, 0, 0, 0, time.UTC)
	broker := &successfulCurrencyStrengthExecutor{
		statusResult: execution.OrderResult{
			BrokerOrderID: "EXIT-PROPAGATING", Status: "filled",
			FilledPrice: 1.105, FilledQty: 100000,
		},
		positions: []execution.Position{{Ticker: "GBP/USD", Quantity: 100000}},
	}
	runner := &currencyStrengthPaperRunner{
		broker: broker, entryRecorded: true,
		statePath: t.TempDir() + "/currency-strength.json",
		state: &currencyStrengthRunState{
			Date: "2026-07-27", Symbol: "GBP/USD", Side: "buy", Quantity: 100000,
			EntryStatus: "filled", EntryFillPrice: 1.1, EntryFilledQty: 100000,
			ExitOrderID: "EXIT-PROPAGATING", ExitStatus: "filled",
			ExitFillPrice: 1.105, ExitFilledQty: 100000, ExitRequestedQty: 100000,
			ExitFillTimeQty: 100000, ExitedAt: now.Format(time.RFC3339),
		},
		now: func() time.Time { return now },
	}
	if err := runner.finalizeExitedState(context.Background()); err != nil {
		t.Fatal(err)
	}
	if runner.state == nil || runner.state.ExitOrderID != "EXIT-PROPAGATING" ||
		runner.state.ExitFillObserved == "" {
		t.Fatalf("filled exit was released before portfolio propagation: %+v", runner.state)
	}
	broker.positions = nil
	now = now.Add(30 * time.Second)
	if err := runner.finalizeExitedState(context.Background()); err != nil {
		t.Fatal(err)
	}
	if runner.state != nil {
		t.Fatalf("broker-flat propagated exit was not finalized: %+v", runner.state)
	}
}

func TestCurrencyStrengthFullExitNeverRetriesStalePortfolio(t *testing.T) {
	now := time.Date(2026, 7, 27, 18, 0, 0, 0, time.UTC)
	broker := &successfulCurrencyStrengthExecutor{
		positions: []execution.Position{{Ticker: "GBP/USD", Quantity: 100000}},
	}
	runner := &currencyStrengthPaperRunner{
		broker: broker, entryRecorded: true,
		statePath: t.TempDir() + "/currency-strength.json",
		state: &currencyStrengthRunState{
			Date: "2026-07-27", Symbol: "GBP/USD", Side: "buy", Quantity: 100000,
			EntryStatus: "filled", EntryFillPrice: 1.1, EntryFilledQty: 100000,
			ExitOrderID: "EXIT-FULL-STALE", ExitStatus: "filled",
			ExitFillPrice: 1.105, ExitFilledQty: 100000, ExitRequestedQty: 100000,
			ExitFillTimeQty: 100000, ExitedAt: now.Format(time.RFC3339),
		},
		now: func() time.Time { return now },
	}
	if err := runner.finalizeExitedState(context.Background()); err != nil {
		t.Fatal(err)
	}
	now = now.Add(currencyStrengthPortfolioPropagationGrace + time.Minute)
	if err := runner.finalizeExitedState(context.Background()); err != nil {
		t.Fatal(err)
	}
	if broker.calls != 0 || runner.state == nil ||
		runner.state.ExitOrderID != "EXIT-FULL-STALE" || runner.state.ExitRetry {
		t.Fatalf("full fill was released for retry against stale portfolio: calls=%d state=%+v",
			broker.calls, runner.state)
	}
}

func TestCurrencyStrengthPartialExitWaitsForPortfolioPropagation(t *testing.T) {
	now := time.Date(2026, 7, 27, 18, 0, 0, 0, time.UTC)
	broker := &successfulCurrencyStrengthExecutor{
		positions: []execution.Position{{Ticker: "GBP/USD", Quantity: 100000}},
	}
	runner := &currencyStrengthPaperRunner{
		broker: broker, journal: &captureCurrencyStrengthJournal{}, entryRecorded: true,
		statePath: t.TempDir() + "/currency-strength.json",
		state: &currencyStrengthRunState{
			Date: "2026-07-27", Symbol: "GBP/USD", Side: "buy", Quantity: 100000,
			EntryStatus: "filled", EntryFillPrice: 1.1, EntryFilledQty: 100000,
			ExitOrderID: "EXIT-PARTIAL-PROPAGATING", ExitStatus: "filled",
			ExitFillPrice: 1.105, ExitFilledQty: 40000, ExitRequestedQty: 100000,
			ExitFillTimeQty: 40000, ExitedAt: now.Format(time.RFC3339),
		},
		now: func() time.Time { return now },
	}
	if err := runner.finalizeExitedState(context.Background()); err != nil {
		t.Fatal(err)
	}
	if runner.state == nil ||
		runner.state.ExitOrderID != "EXIT-PARTIAL-PROPAGATING" ||
		runner.state.ExitFillObserved == "" {
		t.Fatalf("partial fill bypassed portfolio propagation guard: %+v", runner.state)
	}
	broker.positions = []execution.Position{{Ticker: "GBP/USD", Quantity: 60000}}
	now = now.Add(currencyStrengthPortfolioPropagationGrace + time.Second)
	if err := runner.finalizeExitedState(context.Background()); err != nil {
		t.Fatal(err)
	}
	if runner.state == nil || runner.state.ExitOrderID != "" || !runner.state.ExitRetry ||
		runner.state.ExitRetryMaxQty != 60000 {
		t.Fatalf("propagated partial residual was not released for retry: %+v", runner.state)
	}
	broker.positions = []execution.Position{{Ticker: "GBP/USD", Quantity: 100000}}
	broker.result = execution.OrderResult{
		BrokerOrderID: "EXIT-CAPPED-RESIDUAL", Status: "pending",
	}
	if err := runner.exitActive(context.Background()); err != nil {
		t.Fatal(err)
	}
	if broker.calls != 1 || len(broker.requests) != 1 ||
		broker.requests[0].Quantity != 60000 {
		t.Fatalf("stale portfolio oversized residual retry: calls=%d requests=%+v",
			broker.calls, broker.requests)
	}
}

func TestCurrencyStrengthJournalRestoredPartialCapsStalePortfolioRetry(t *testing.T) {
	now := time.Date(2026, 7, 27, 18, 0, 0, 0, time.UTC)
	filledAt := now.Add(-time.Minute)
	broker := &successfulCurrencyStrengthExecutor{
		result: execution.OrderResult{
			BrokerOrderID: "EXIT-JOURNAL-RETRY", Status: "pending",
		},
		statusResult: execution.OrderResult{
			BrokerOrderID: "EXIT-JOURNAL-PARTIAL", Status: "filled",
			FilledPrice: 1.104, FilledQty: 40000,
		},
		positions:          []execution.Position{{Ticker: "GBP/USD", Quantity: 100000}},
		keepPositionOnFill: true,
		recentFills: []execution.BrokerOrder{{
			BrokerOrderID: "EXIT-JOURNAL-PARTIAL",
			TradeID:       "currency-strength-exit-2026-07-27-GBPUSD-1-1",
			Symbol:        "GBP/USD", Side: "sell", FilledQty: 40000,
			FilledAvgPx: 1.104, FilledAt: filledAt,
		}},
	}
	runner := &currencyStrengthPaperRunner{
		broker: broker, journal: &captureCurrencyStrengthJournal{}, entryRecorded: true,
		statePath: t.TempDir() + "/currency-strength.json",
		state: currencyStrengthStateFromRun(store.CurrencyStrengthRun{
			SessionDate: time.Date(2026, 7, 27, 0, 0, 0, 0, time.UTC),
			Status:      "exit_submitted", SignalSymbol: "GBP/USD", SignalSide: "buy",
			EntryStatus: "filled", EntryFillPrice: 1.1, EntryFilledQty: 100000,
			ExitOrderID: "EXIT-JOURNAL-PARTIAL", ExitStatus: "pending",
			ExitFillPrice: 1.104, ExitFilledQty: 40000,
		}),
		now: func() time.Time { return now },
	}
	if err := runner.exitActive(context.Background()); err != nil {
		t.Fatal(err)
	}
	now = now.Add(currencyStrengthPortfolioPropagationGrace + time.Second)
	if err := runner.exitActive(context.Background()); err != nil {
		t.Fatal(err)
	}
	if runner.state == nil || !runner.state.ExitRetry ||
		runner.state.ExitRetryMaxQty != 60000 {
		t.Fatalf("journal-restored cumulative fill did not derive safe remainder: %+v", runner.state)
	}
	if err := runner.exitActive(context.Background()); err != nil {
		t.Fatal(err)
	}
	if broker.calls != 1 || len(broker.requests) != 1 ||
		broker.requests[0].Quantity != 60000 {
		t.Fatalf("journal-only retry used stale portfolio quantity: calls=%d requests=%+v",
			broker.calls, broker.requests)
	}
}

func TestCurrencyStrengthNotifiesSubmittedThenConfirmedFlat(t *testing.T) {
	broker := &successfulCurrencyStrengthExecutor{
		result: execution.OrderResult{
			BrokerOrderID: "EXIT-NOTIFY",
			Status:        "filled",
			FilledPrice:   1.105,
			FilledQty:     100000,
		},
		positions: []execution.Position{{
			Ticker: "GBP/USD", Quantity: 100000, AvgCostBasis: 1.1,
		}},
	}
	notifier := &captureNotifier{}
	runner := &currencyStrengthPaperRunner{
		broker: broker, notifier: notifier,
		statePath:     t.TempDir() + "/currency-strength.json",
		entryRecorded: true,
		state: &currencyStrengthRunState{
			Date: "2026-07-27", Symbol: "GBP/USD", Side: "buy", Quantity: 100000,
			EntryStatus: "filled", EntryFillPrice: 1.1, EntryFilledQty: 100000,
		},
		now: func() time.Time { return time.Date(2026, 7, 27, 18, 0, 0, 0, time.UTC) },
	}
	if err := runner.exitActive(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(notifier.msgs) != 2 ||
		!strings.Contains(notifier.msgs[0], "exit submitted") ||
		!strings.Contains(notifier.msgs[1], "position flattened") {
		t.Fatalf("exit lifecycle notifications were not truthful: %v", notifier.msgs)
	}
}

func TestCurrencyStrengthStateRejectsAnotherAccount(t *testing.T) {
	path := t.TempDir() + "/currency-strength.json"
	writer := &currencyStrengthPaperRunner{
		accountID: "DU111",
		statePath: path,
		state: &currencyStrengthRunState{
			AccountID: "DU111", Date: "2026-07-27", Symbol: "GBP/USD",
			Side: "buy", Quantity: 100000,
		},
	}
	if err := writer.saveState(); err != nil {
		t.Fatal(err)
	}
	reader := &currencyStrengthPaperRunner{accountID: "DU222", statePath: path}
	if err := reader.loadState(); err == nil {
		t.Fatal("state from a different IBKR account was accepted")
	}
	if reader.state != nil {
		t.Fatalf("cross-account state was loaded: %+v", reader.state)
	}
}

func TestCurrencyStrengthMigratesLegacyStateToConfiguredAccount(t *testing.T) {
	dir := t.TempDir()
	legacyPath := filepath.Join(dir, "currency_strength_state.json")
	statePath := filepath.Join(dir, "currency_strength_state_du123.json")
	legacy := currencyStrengthRunState{
		Date: "2026-07-27", Symbol: "GBP/USD", Side: "buy", Quantity: 100000,
		EntryOrderID: "ENTRY-LEGACY", EntryStatus: "filled",
	}
	b, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacyPath, b, 0600); err != nil {
		t.Fatal(err)
	}
	runner := &currencyStrengthPaperRunner{
		accountID: "DU123", statePath: statePath, legacyStatePath: legacyPath,
	}
	if err := runner.loadState(); err != nil {
		t.Fatal(err)
	}
	if runner.state == nil || runner.state.AccountID != "DU123" ||
		runner.state.EntryOrderID != "ENTRY-LEGACY" {
		t.Fatalf("legacy obligation was not adopted: %+v", runner.state)
	}
	if _, err := os.Stat(statePath); err != nil {
		t.Fatalf("account-scoped sidecar was not persisted: %v", err)
	}
	if _, err := os.Stat(legacyPath); !os.IsNotExist(err) {
		t.Fatalf("legacy sidecar was not retired: %v", err)
	}
	restarted := &currencyStrengthPaperRunner{accountID: "DU123", statePath: statePath}
	if err := restarted.loadState(); err != nil {
		t.Fatal(err)
	}
	if restarted.state == nil || restarted.state.AccountID != "DU123" {
		t.Fatalf("migrated state was not restart-safe: %+v", restarted.state)
	}
}

func TestCurrencyStrengthRejectsLegacyStateTaggedForAnotherAccount(t *testing.T) {
	dir := t.TempDir()
	legacyPath := filepath.Join(dir, "currency_strength_state.json")
	state := currencyStrengthRunState{
		AccountID: "DU111", Date: "2026-07-27", Symbol: "GBP/USD",
		Side: "buy", Quantity: 100000,
	}
	b, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacyPath, b, 0600); err != nil {
		t.Fatal(err)
	}
	runner := &currencyStrengthPaperRunner{
		accountID: "DU222", statePath: filepath.Join(dir, "currency_strength_state_du222.json"),
		legacyStatePath: legacyPath,
	}
	if err := runner.loadState(); err == nil {
		t.Fatal("legacy state tagged for another account was accepted")
	}
	if runner.state != nil {
		t.Fatalf("cross-account legacy state was loaded: %+v", runner.state)
	}
	if _, err := os.Stat(legacyPath); err != nil {
		t.Fatalf("mismatched legacy claim was not restored: %v", err)
	}
	rightful := &currencyStrengthPaperRunner{
		accountID: "DU111", statePath: filepath.Join(dir, "currency_strength_state_du111.json"),
		legacyStatePath: legacyPath,
	}
	if err := rightful.loadState(); err != nil || rightful.state == nil {
		t.Fatalf("rightful account could not recover restored legacy state: state=%+v err=%v",
			rightful.state, err)
	}
}

func TestCurrencyStrengthLegacyStateHasSingleConcurrentAccountClaim(t *testing.T) {
	dir := t.TempDir()
	legacyPath := filepath.Join(dir, "currency_strength_state.json")
	legacy := currencyStrengthRunState{
		Date: "2026-07-27", Symbol: "GBP/USD", Side: "buy", Quantity: 100000,
		EntryOrderID: "ENTRY-ONE-OWNER", EntryStatus: "filled",
	}
	b, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacyPath, b, 0600); err != nil {
		t.Fatal(err)
	}
	runners := []*currencyStrengthPaperRunner{
		{
			accountID: "DU111", statePath: filepath.Join(dir, "currency_strength_state_du111.json"),
			legacyStatePath: legacyPath,
		},
		{
			accountID: "DU222", statePath: filepath.Join(dir, "currency_strength_state_du222.json"),
			legacyStatePath: legacyPath,
		},
	}
	start := make(chan struct{})
	errs := make(chan error, len(runners))
	for _, runner := range runners {
		go func() {
			<-start
			errs <- runner.loadState()
		}()
	}
	close(start)
	for range runners {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	owners := 0
	for _, runner := range runners {
		if runner.state != nil {
			owners++
			if runner.state.AccountID != runner.accountID {
				t.Fatalf("legacy state assigned across accounts: runner=%s state=%+v",
					runner.accountID, runner.state)
			}
		}
	}
	if owners != 1 {
		t.Fatalf("legacy obligation claimed by %d accounts, want exactly one", owners)
	}
}

func TestCurrencyStrengthLegacyMigrationFailureRetainsActiveState(t *testing.T) {
	dir := t.TempDir()
	legacyPath := filepath.Join(dir, "currency_strength_state.json")
	statePath := filepath.Join(dir, "currency_strength_state_du123.json")
	claimPath := statePath + ".legacy-claim"
	legacy := currencyStrengthRunState{
		Date: "2026-07-27", Symbol: "GBP/USD", Side: "buy", Quantity: 100000,
		EntryOrderID: "ENTRY-STILL-MANAGED", EntryStatus: "filled",
	}
	b, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(claimPath, b, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0500); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(dir, 0750) }()
	runner := &currencyStrengthPaperRunner{
		accountID: "DU123", statePath: statePath, legacyStatePath: legacyPath,
	}
	if err := runner.loadState(); err != nil {
		t.Fatalf("migration failure blocked active recovery: %v", err)
	}
	if runner.state == nil || runner.state.EntryOrderID != "ENTRY-STILL-MANAGED" {
		t.Fatalf("migration failure discarded active obligation: %+v", runner.state)
	}
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

func TestCurrencyStrengthEntryUsesBrokerExecutionTimestamp(t *testing.T) {
	acknowledgedAt := time.Date(2026, 7, 27, 9, 0, 0, 0, time.UTC)
	filledAt := acknowledgedAt.Add(12 * time.Second)
	for _, tc := range []struct {
		name        string
		entryStatus string
		status      execution.OrderResult
	}{
		{
			name:        "immediate fill acknowledgement",
			entryStatus: "filled",
		},
		{
			name:        "fill after submission",
			entryStatus: "pending",
			status: execution.OrderResult{
				BrokerOrderID: "ENTRY-TIMESTAMP", Status: "filled",
				FilledPrice: 1.101, FilledQty: 100000,
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			broker := &successfulCurrencyStrengthExecutor{
				statusResult: tc.status,
				recentFills: []execution.BrokerOrder{
					{
						BrokerOrderID: "ENTRY-TIMESTAMP",
						TradeID:       "currency-strength-2026-07-27-GBPUSD",
						Symbol:        "GBP/USD", Side: "buy", FilledQty: 40000,
						FilledAvgPx: 1.1005, FilledAt: filledAt,
					},
					{
						BrokerOrderID: "ENTRY-TIMESTAMP",
						TradeID:       "currency-strength-2026-07-27-GBPUSD",
						Symbol:        "GBP/USD", Side: "buy", FilledQty: 60000,
						FilledAvgPx: 1.101333333333, FilledAt: filledAt.Add(5 * time.Second),
					},
				},
			}
			runner := &currencyStrengthPaperRunner{
				broker: broker, statePath: t.TempDir() + "/currency-strength.json",
				state: &currencyStrengthRunState{
					Date: "2026-07-27", Symbol: "GBP/USD", Side: "buy", Quantity: 100000,
					EntryOrderID: "ENTRY-TIMESTAMP", EntryStatus: tc.entryStatus,
					EntryFillPrice: 1.101, EntryFilledQty: 100000,
					EnteredAt: acknowledgedAt.Format(time.RFC3339),
				},
			}
			if err := runner.reconcileActiveEntry(context.Background()); err != nil {
				t.Fatal(err)
			}
			got, err := time.Parse(time.RFC3339, runner.state.EnteredAt)
			if err != nil {
				t.Fatal(err)
			}
			if !got.Equal(filledAt) || runner.state.EntryFillTimeQty != 100000 {
				t.Fatalf("entry retained acknowledgement time: state=%+v", runner.state)
			}
		})
	}
}

func TestCurrencyStrengthFilledEntryWaitsForExecutionTimestamp(t *testing.T) {
	journal := &captureCurrencyStrengthJournal{}
	broker := &successfulCurrencyStrengthExecutor{}
	runner := &currencyStrengthPaperRunner{
		broker: broker, journal: journal,
		statePath: t.TempDir() + "/currency-strength.json",
		state: &currencyStrengthRunState{
			Date: "2026-07-27", Symbol: "GBP/USD", Side: "buy", Quantity: 100000,
			EntryOrderID: "ENTRY-DELAYED-TRADE", EntryStatus: "filled",
			EntryFillPrice: 1.101, EntryFilledQty: 100000,
		},
	}
	if err := runner.reconcileActiveEntry(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := runner.syncActiveEntryJournal(context.Background()); err == nil {
		t.Fatal("filled entry was journaled without a broker execution timestamp")
	}
	if len(journal.entries) != 0 {
		t.Fatalf("timestamp-less entry reached durable history: %+v", journal.entries)
	}
	filledAt := time.Date(2026, 7, 27, 9, 0, 12, 0, time.UTC)
	broker.recentFills = []execution.BrokerOrder{{
		BrokerOrderID: "ENTRY-DELAYED-TRADE",
		Symbol:        "GBP/USD", Side: "buy", FilledQty: 100000,
		FilledAvgPx: 1.101, FilledAt: filledAt,
	}}
	if err := runner.reconcileActiveEntry(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := runner.syncActiveEntryJournal(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(journal.entries) != 1 || journal.entries[0].EnteredAt == nil ||
		!journal.entries[0].EnteredAt.Equal(filledAt) {
		t.Fatalf("broker-timestamped entry was not journaled: %+v", journal.entries)
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

func TestCurrencyStrengthCorruptSidecarFallsBackToJournal(t *testing.T) {
	statePath := t.TempDir() + "/currency-strength.json"
	if err := os.WriteFile(statePath, []byte("{not-json"), 0600); err != nil {
		t.Fatal(err)
	}
	runner := &currencyStrengthPaperRunner{
		statePath: statePath,
		journal:   &captureCurrencyStrengthJournal{},
	}
	if err := loadCurrencyStrengthRecovery(runner); err != nil {
		t.Fatalf("healthy journal should tolerate corrupt sidecar: %v", err)
	}
	if runner.state != nil {
		t.Fatalf("corrupt sidecar state was retained: %+v", runner.state)
	}

	withoutJournal := &currencyStrengthPaperRunner{statePath: statePath}
	if err := loadCurrencyStrengthRecovery(withoutJournal); err == nil {
		t.Fatal("corrupt sidecar without journal should remain fatal")
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
		broker:        broker,
		entryRecorded: true,
		statePath:     t.TempDir() + "/currency-strength.json",
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
		FilledPrice:   1.105,
		FilledQty:     100000,
	}, positions: []execution.Position{{Ticker: "GBP/USD", Quantity: 100000}},
		recentFills: []execution.BrokerOrder{{
			BrokerOrderID: "EXIT-RETRY",
			Symbol:        "GBP/USD", Side: "sell", FilledQty: 100000,
			FilledAvgPx: 1.105,
			FilledAt:    time.Date(2026, 7, 27, 18, 0, 10, 0, time.UTC),
		}},
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

func TestCurrencyStrengthReconciledFillUsesExecutionTimestamp(t *testing.T) {
	submittedAt := time.Date(2026, 7, 27, 18, 0, 0, 0, time.UTC)
	filledAt := submittedAt.Add(7 * time.Minute)
	journal := &captureCurrencyStrengthJournal{}
	broker := &successfulCurrencyStrengthExecutor{
		statusResult: execution.OrderResult{
			BrokerOrderID: "EXIT-DELAYED", Status: "filled",
			FilledPrice: 161, FilledQty: 100000,
		},
		recentFills: []execution.BrokerOrder{{
			BrokerOrderID: "EXIT-DELAYED",
			TradeID:       "currency-strength-exit-2026-07-27-EURJPY-1-1",
			Symbol:        "EUR/JPY", Side: "sell", FilledQty: 100000,
			FilledAvgPx: 161, FilledAt: filledAt,
		}},
	}
	runner := &currencyStrengthPaperRunner{
		broker: broker, journal: journal, entryRecorded: true,
		market: historicalConversionMarketData{candles: map[string][]data.Candle{
			"JPY/USD": {
				{Timestamp: submittedAt, Close: 0.0050},
				{Timestamp: filledAt, Close: 0.0067},
			},
		}},
		statePath: t.TempDir() + "/currency-strength.json",
		state: &currencyStrengthRunState{
			Date: "2026-07-27", Symbol: "EUR/JPY", Side: "buy", Quantity: 100000,
			EntryStatus: "filled", EntryFillPrice: 160, EntryFilledQty: 100000,
			ExitOrderID: "EXIT-DELAYED", ExitStatus: "pending",
			ExitedAt: submittedAt.Format(time.RFC3339),
		},
	}
	if err := runner.finalizeExitedState(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(journal.exits) != 1 || journal.exits[0].ExitedAt == nil ||
		!journal.exits[0].ExitedAt.Equal(filledAt) {
		t.Fatalf("reconciled exit retained submission time: %+v", journal.exits)
	}
	if journal.exits[0].RealizedPnL == nil ||
		*journal.exits[0].RealizedPnL < 669.99 ||
		*journal.exits[0].RealizedPnL > 670.01 {
		t.Fatalf("reconciled exit used submission-time FX conversion: %+v", journal.exits[0])
	}
}

func TestCurrencyStrengthImmediateFilledAckWaitsForExecutionEconomics(t *testing.T) {
	now := time.Date(2026, 7, 27, 18, 0, 0, 0, time.UTC)
	journal := &captureCurrencyStrengthJournal{}
	broker := &successfulCurrencyStrengthExecutor{
		result: execution.OrderResult{
			BrokerOrderID: "EXIT-ACK-ONLY", Status: "filled",
		},
		positions:          []execution.Position{{Ticker: "GBP/USD", Quantity: 100000}},
		keepPositionOnFill: true,
	}
	runner := &currencyStrengthPaperRunner{
		broker: broker, journal: journal, entryRecorded: true,
		statePath: t.TempDir() + "/currency-strength.json",
		state: &currencyStrengthRunState{
			Date: "2026-07-27", Symbol: "GBP/USD", Side: "buy", Quantity: 100000,
			EntryStatus: "filled", EntryFillPrice: 1.1, EntryFilledQty: 100000,
		},
		now: func() time.Time { return now },
	}
	if err := runner.exitActive(context.Background()); err != nil {
		t.Fatal(err)
	}
	if runner.state == nil || runner.state.ExitOrderID != "EXIT-ACK-ONLY" ||
		runner.state.ExitRetry || runner.state.ExitFilledQty != 0 ||
		len(journal.exits) != 0 {
		t.Fatalf("status-only filled acknowledgement was finalized or retried: state=%+v exits=%+v",
			runner.state, journal.exits)
	}

	filledAt := now.Add(20 * time.Second)
	broker.recentFills = []execution.BrokerOrder{{
		BrokerOrderID: "EXIT-ACK-ONLY",
		TradeID:       runner.state.ExitTradeID,
		Symbol:        "GBP/USD", Side: "sell", FilledQty: 100000,
		FilledAvgPx: 1.105, FilledAt: filledAt,
	}}
	broker.positions = nil
	if err := runner.exitActive(context.Background()); err != nil {
		t.Fatal(err)
	}
	if runner.state != nil || len(journal.exits) != 1 ||
		journal.exits[0].ExitFilledQty != 100000 ||
		journal.exits[0].ExitFillPrice != 1.105 ||
		journal.exits[0].ExitedAt == nil ||
		!journal.exits[0].ExitedAt.Equal(filledAt) {
		t.Fatalf("execution economics were not recovered before finalization: state=%+v exits=%+v",
			runner.state, journal.exits)
	}
}

func TestCurrencyStrengthImmediateFilledAckUsesTradeTimestamp(t *testing.T) {
	submittedAt := time.Date(2026, 7, 27, 18, 0, 0, 0, time.UTC)
	filledAt := submittedAt.Add(15 * time.Second)
	journal := &captureCurrencyStrengthJournal{}
	broker := &successfulCurrencyStrengthExecutor{
		result: execution.OrderResult{
			BrokerOrderID: "EXIT-POSITIVE-ACK", Status: "filled",
			FilledPrice: 1.105, FilledQty: 100000,
		},
		positions: []execution.Position{{Ticker: "GBP/USD", Quantity: 100000}},
		recentFills: []execution.BrokerOrder{{
			BrokerOrderID: "EXIT-POSITIVE-ACK",
			Symbol:        "GBP/USD", Side: "sell", FilledQty: 100000,
			FilledAvgPx: 1.105, FilledAt: filledAt,
		}},
	}
	runner := &currencyStrengthPaperRunner{
		broker: broker, journal: journal, entryRecorded: true,
		statePath: t.TempDir() + "/currency-strength.json",
		state: &currencyStrengthRunState{
			Date: "2026-07-27", Symbol: "GBP/USD", Side: "buy", Quantity: 100000,
			EntryStatus: "filled", EntryFillPrice: 1.1, EntryFilledQty: 100000,
		},
		now: func() time.Time { return submittedAt },
	}
	if err := runner.exitActive(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(journal.exits) != 1 || journal.exits[0].ExitedAt == nil ||
		!journal.exits[0].ExitedAt.Equal(filledAt) {
		t.Fatalf("positive filled acknowledgement used local submission time: %+v", journal.exits)
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

func TestCurrencyStrengthBrokerRecoverySurvivesJournalStartupFailure(t *testing.T) {
	now := time.Date(2026, 7, 27, 18, 1, 0, 0, time.UTC)
	journal := &captureCurrencyStrengthJournal{startErrors: 10}
	broker := &successfulCurrencyStrengthExecutor{
		statusResult: execution.OrderResult{
			BrokerOrderID: "ENTRY-BROKER-ONLY", Status: "filled",
			FilledPrice: 1.1, FilledQty: 100000,
		},
		result: execution.OrderResult{
			BrokerOrderID: "EXIT-BROKER-ONLY", Status: "filled",
			FilledPrice: 1.105, FilledQty: 100000,
		},
		positions: []execution.Position{{
			Ticker: "GBP/USD", Quantity: 100000, AvgCostBasis: 1.1,
		}},
		keepPositionOnFill: true,
		recentFills: []execution.BrokerOrder{{
			BrokerOrderID: "ENTRY-BROKER-ONLY",
			TradeID:       "currency-strength-2026-07-27-GBPUSD",
			Symbol:        "GBP/USD", Side: "buy", FilledQty: 100000,
			FilledAvgPx: 1.1, FilledAt: now.Add(-9 * time.Hour),
		}},
	}
	runner := &currencyStrengthPaperRunner{
		cfg: config.CurrencyStrengthConfig{
			Timezone: "UTC", StrengthStart: "06:00", StrengthEnd: "09:00",
			ExitTime: "18:00", Units: 100000, TradeInstruments: []string{"GBP/USD"},
		},
		broker: broker, journal: journal,
		statePath: t.TempDir() + "/currency-strength.json",
		now:       func() time.Time { return now },
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := runner.Run(ctx)
	if err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("Run: %v", err)
	}
	if broker.calls != 1 || len(broker.requests) != 1 ||
		broker.requests[0].TradeID == "" {
		t.Fatalf("journal outage blocked broker-only recovery/flatten: calls=%d requests=%+v state=%+v",
			broker.calls, broker.requests, runner.state)
	}
}

func TestCurrencyStrengthJournalOutageSuppressesNewEntry(t *testing.T) {
	journal := &captureCurrencyStrengthJournal{startErrors: 10}
	broker := &successfulCurrencyStrengthExecutor{}
	runner := &currencyStrengthPaperRunner{
		cfg: config.CurrencyStrengthConfig{
			Timezone: "UTC", StrengthStart: "06:00", StrengthEnd: "09:00",
			ExitTime: "18:00", Units: 100000, TradeInstruments: []string{"GBP/USD"},
		},
		broker: broker, journal: journal, market: fakeMarketDataProvider{},
		statePath: t.TempDir() + "/currency-strength.json",
		now:       func() time.Time { return time.Date(2026, 7, 27, 9, 1, 0, 0, time.UTC) },
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := runner.Run(ctx)
	if err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("Run: %v", err)
	}
	if broker.calls != 0 || len(journal.evaluating) != 0 {
		t.Fatalf("new entry evaluation ran without durable session journal: calls=%d evaluating=%d",
			broker.calls, len(journal.evaluating))
	}
}

func TestCurrencyStrengthNilJournalSuppressesNewEntry(t *testing.T) {
	broker := &successfulCurrencyStrengthExecutor{}
	runner := &currencyStrengthPaperRunner{
		cfg: config.CurrencyStrengthConfig{
			Timezone: "UTC", StrengthStart: "06:00", StrengthEnd: "09:00",
			ExitTime: "18:00", Units: 100000, TradeInstruments: []string{"GBP/USD"},
		},
		broker: broker, market: fakeMarketDataProvider{},
		statePath: t.TempDir() + "/currency-strength.json",
		now:       func() time.Time { return time.Date(2026, 7, 27, 9, 1, 0, 0, time.UTC) },
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := runner.Run(ctx)
	if err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("Run: %v", err)
	}
	if broker.calls != 0 || runner.decisionDoneFor != "" {
		t.Fatalf("new entry ran without any journal: calls=%d decision=%q",
			broker.calls, runner.decisionDoneFor)
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
	broker := &successfulCurrencyStrengthExecutor{
		statusResult: execution.OrderResult{
			BrokerOrderID: "EXIT-RECOVERED",
			Status:        "filled",
			FilledQty:     100000,
		},
		recentFills: []execution.BrokerOrder{{
			BrokerOrderID: "EXIT-RECOVERED",
			TradeID:       "currency-strength-exit-2026-07-27-GBPUSD-1-1",
			Symbol:        "GBP/USD", Side: "sell", FilledQty: 100000,
			FilledAvgPx: 1.105, FilledAt: time.Date(2026, 7, 27, 18, 0, 0, 0, time.UTC),
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
		recentFills: []execution.BrokerOrder{{
			BrokerOrderID: "EXIT-WORKING",
			TradeID:       "currency-strength-exit-2026-07-27-GBPUSD",
			Symbol:        "GBP/USD", Side: "sell", FilledQty: 100000,
			FilledAvgPx: 1.105, FilledAt: time.Date(2026, 7, 27, 18, 0, 0, 0, time.UTC),
		}},
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

func TestCurrencyStrengthResidualRecoveryAdoptsUniqueWorkingRetry(t *testing.T) {
	base := &successfulCurrencyStrengthExecutor{
		statusResult: execution.OrderResult{
			BrokerOrderID: "EXIT-RESIDUAL-WORKING", Status: "pending",
		},
		positions: []execution.Position{{Ticker: "GBP/USD", Quantity: 60000}},
	}
	broker := &workingExitExecutor{
		successfulCurrencyStrengthExecutor: base,
		orders: []execution.WorkingOrder{{
			OrderID: "EXIT-RESIDUAL-WORKING",
			TradeID: "currency-strength-exit-2026-07-27-GBPUSD-999-2",
			Ticker:  "GBP/USD", Side: "sell", Status: "pending", RemainingQty: 60000,
		}},
	}
	runner := &currencyStrengthPaperRunner{
		broker: broker, journal: &captureCurrencyStrengthJournal{}, entryRecorded: true,
		statePath: t.TempDir() + "/currency-strength.json",
		state: &currencyStrengthRunState{
			Date: "2026-07-27", Symbol: "GBP/USD", Side: "buy", Quantity: 100000,
			EntryStatus: "filled", EntryFilledQty: 100000, ExitRetry: true,
		},
	}
	if err := runner.exitActive(context.Background()); err != nil {
		t.Fatal(err)
	}
	if base.calls != 0 || runner.state.ExitOrderID != "EXIT-RESIDUAL-WORKING" ||
		runner.state.ExitTradeID != "currency-strength-exit-2026-07-27-GBPUSD-999-2" {
		t.Fatalf("working residual retry was not uniquely adopted: calls=%d state=%+v",
			base.calls, runner.state)
	}
}

func TestCurrencyStrengthRecoveryCancelsOversizedWorkingRetry(t *testing.T) {
	base := &successfulCurrencyStrengthExecutor{
		statusResult: execution.OrderResult{
			BrokerOrderID: "EXIT-OVERSIZED-WORKING", Status: "pending",
		},
		positions: []execution.Position{{Ticker: "GBP/USD", Quantity: 100000}},
	}
	broker := &workingExitExecutor{
		successfulCurrencyStrengthExecutor: base,
		orders: []execution.WorkingOrder{{
			OrderID: "EXIT-OVERSIZED-WORKING",
			TradeID: "currency-strength-exit-2026-07-27-GBPUSD-2-2",
			Ticker:  "GBP/USD", Side: "sell", Status: "pending", RemainingQty: 60000,
		}},
	}
	runner := &currencyStrengthPaperRunner{
		broker: broker, entryRecorded: true,
		statePath: t.TempDir() + "/currency-strength.json",
		state: &currencyStrengthRunState{
			Date: "2026-07-27", Symbol: "GBP/USD", Side: "buy", Quantity: 100000,
			EntryStatus: "filled", EntryFilledQty: 100000,
			ExitRetry: true, ExitPriorOrderID: "EXIT-PRIOR",
			ExitCumulativeQty: 70000, ExitRetryMaxQty: 30000,
		},
	}
	if err := runner.exitActive(context.Background()); err != nil {
		t.Fatal(err)
	}
	if broker.cancels != 1 || base.calls != 0 || runner.state == nil ||
		runner.state.ExitOrderID != "EXIT-OVERSIZED-WORKING" {
		t.Fatalf("oversized working retry remained live: cancels=%d calls=%d state=%+v",
			broker.cancels, base.calls, runner.state)
	}
}

func TestCurrencyStrengthFlatRecoveryCancelsWorkingRetryBeforeBrokerFlat(t *testing.T) {
	base := &successfulCurrencyStrengthExecutor{
		statusResult: execution.OrderResult{
			BrokerOrderID: "EXIT-FLAT-WORKING", Status: "pending",
		},
	}
	broker := &workingExitExecutor{
		successfulCurrencyStrengthExecutor: base,
		orders: []execution.WorkingOrder{{
			OrderID: "EXIT-FLAT-WORKING",
			TradeID: "currency-strength-exit-2026-07-27-GBPUSD-2-2",
			Ticker:  "GBP/USD", Side: "sell", Status: "pending", RemainingQty: 60000,
		}},
	}
	runner := &currencyStrengthPaperRunner{
		broker: broker, journal: &captureCurrencyStrengthJournal{}, entryRecorded: true,
		statePath: t.TempDir() + "/currency-strength.json",
		state: &currencyStrengthRunState{
			Date: "2026-07-27", Symbol: "GBP/USD", Side: "buy", Quantity: 100000,
			EntryStatus: "filled", EntryFilledQty: 100000,
			ExitRetry: true, ExitPriorOrderID: "EXIT-PRIOR",
			ExitCumulativeQty: 40000, ExitRetryMaxQty: 60000,
		},
	}
	if err := runner.exitActive(context.Background()); err != nil {
		t.Fatal(err)
	}
	if broker.cancels != 1 || base.calls != 0 || runner.state == nil ||
		runner.state.ExitOrderID != "EXIT-FLAT-WORKING" {
		t.Fatalf("flat recovery left working retry live: cancels=%d calls=%d state=%+v",
			broker.cancels, base.calls, runner.state)
	}
}

func TestCurrencyStrengthLatePriorFillsShrinkResidualRetry(t *testing.T) {
	firstFillAt := time.Date(2026, 7, 27, 18, 0, 0, 0, time.UTC)
	broker := &successfulCurrencyStrengthExecutor{
		result: execution.OrderResult{
			BrokerOrderID: "EXIT-LATE-CAPPED-RETRY", Status: "pending",
		},
		positions: []execution.Position{{Ticker: "GBP/USD", Quantity: 100000}},
		recentFills: []execution.BrokerOrder{
			{
				BrokerOrderID: "EXIT-PRIOR-LATE", TradeID: "currency-strength-exit-2026-07-27-GBPUSD-1-1",
				Symbol: "GBP/USD", Side: "sell", FilledQty: 40000,
				FilledAvgPx: 1.104, FilledAt: firstFillAt,
			},
			{
				BrokerOrderID: "EXIT-PRIOR-LATE", TradeID: "currency-strength-exit-2026-07-27-GBPUSD-1-1",
				Symbol: "GBP/USD", Side: "sell", FilledQty: 30000,
				FilledAvgPx: 1.105, FilledAt: firstFillAt.Add(time.Minute),
			},
		},
	}
	runner := &currencyStrengthPaperRunner{
		broker: broker, entryRecorded: true,
		statePath: t.TempDir() + "/currency-strength.json",
		state: &currencyStrengthRunState{
			Date: "2026-07-27", Symbol: "GBP/USD", Side: "buy", Quantity: 100000,
			EntryStatus: "filled", EntryFilledQty: 100000,
			ExitRetry: true, ExitPriorOrderID: "EXIT-PRIOR-LATE",
			ExitCumulativeQty: 40000, ExitRetryMaxQty: 60000,
		},
		now: func() time.Time { return firstFillAt.Add(2 * time.Minute) },
	}
	if err := runner.exitActive(context.Background()); err != nil {
		t.Fatal(err)
	}
	if broker.calls != 1 || len(broker.requests) != 1 ||
		broker.requests[0].Quantity != 30000 ||
		runner.state.ExitCumulativeQty != 70000 {
		t.Fatalf("late prior fills did not shrink retry: calls=%d requests=%+v state=%+v",
			broker.calls, broker.requests, runner.state)
	}
}

func TestCurrencyStrengthLatePriorFillsSuppressCoveredRetry(t *testing.T) {
	filledAt := time.Date(2026, 7, 27, 18, 0, 0, 0, time.UTC)
	broker := &successfulCurrencyStrengthExecutor{
		statusResult: execution.OrderResult{
			BrokerOrderID: "EXIT-PRIOR-FULL", Status: "filled",
			FilledPrice: 1.105, FilledQty: 100000,
		},
		positions:          []execution.Position{{Ticker: "GBP/USD", Quantity: 100000}},
		keepPositionOnFill: true,
		recentFills: []execution.BrokerOrder{{
			BrokerOrderID: "EXIT-PRIOR-FULL",
			TradeID:       "currency-strength-exit-2026-07-27-GBPUSD-1-1",
			Symbol:        "GBP/USD", Side: "sell", FilledQty: 100000,
			FilledAvgPx: 1.105, FilledAt: filledAt,
		}},
	}
	runner := &currencyStrengthPaperRunner{
		broker: broker, entryRecorded: true,
		statePath: t.TempDir() + "/currency-strength.json",
		state: &currencyStrengthRunState{
			Date: "2026-07-27", Symbol: "GBP/USD", Side: "buy", Quantity: 100000,
			EntryStatus: "filled", EntryFilledQty: 100000,
			ExitRetry: true, ExitPriorOrderID: "EXIT-PRIOR-FULL",
			ExitCumulativeQty: 40000, ExitRetryMaxQty: 60000,
		},
	}
	if err := runner.exitActive(context.Background()); err != nil {
		t.Fatal(err)
	}
	if broker.calls != 0 || runner.state == nil ||
		runner.state.ExitOrderID != "EXIT-PRIOR-FULL" ||
		runner.state.ExitCumulativeQty != 100000 {
		t.Fatalf("fully covered prior order released another retry: calls=%d state=%+v",
			broker.calls, runner.state)
	}
}

func TestCurrencyStrengthDurableCumulativeCoverageSuppressesRetryWithoutTradeDetail(t *testing.T) {
	broker := &successfulCurrencyStrengthExecutor{
		positions: []execution.Position{{Ticker: "GBP/USD", Quantity: 100000}},
	}
	runner := &currencyStrengthPaperRunner{
		broker: broker, entryRecorded: true,
		statePath: t.TempDir() + "/currency-strength.json",
		state: &currencyStrengthRunState{
			Date: "2026-07-27", Symbol: "GBP/USD", Side: "buy", Quantity: 100000,
			EntryStatus: "filled", EntryFilledQty: 100000,
			ExitRetry: true, ExitPriorOrderID: "EXIT-PRIOR-FULL",
			ExitCumulativeQty: 100000,
		},
	}
	if err := runner.exitActive(context.Background()); err != nil {
		t.Fatal(err)
	}
	if broker.calls != 0 || runner.state == nil || !runner.state.ExitRetry {
		t.Fatalf("durable cumulative coverage submitted a blind retry: calls=%d state=%+v",
			broker.calls, runner.state)
	}
}

func TestCurrencyStrengthResidualRecoveryAdoptsNewestFilledRetry(t *testing.T) {
	oldFilledAt := time.Date(2026, 7, 27, 18, 0, 0, 0, time.UTC)
	// The prior order can report a later execution after the retry filled.
	// Reservation sequence, not fill time, identifies the newer retry order.
	newFilledAt := oldFilledAt.Add(-time.Minute)
	journal := &captureCurrencyStrengthJournal{}
	broker := &successfulCurrencyStrengthExecutor{
		statusResult: execution.OrderResult{
			BrokerOrderID: "EXIT-RETRY-FILLED", Status: "filled",
			FilledPrice: 1.106, FilledQty: 60000,
		},
		recentFills: []execution.BrokerOrder{
			{
				BrokerOrderID: "EXIT-PRIOR", TradeID: "currency-strength-exit-2026-07-27-GBPUSD-1-1",
				Symbol: "GBP/USD", Side: "sell", FilledQty: 40000,
				FilledAvgPx: 1.104, FilledAt: oldFilledAt,
			},
			{
				BrokerOrderID: "EXIT-RETRY-FILLED", TradeID: "currency-strength-exit-2026-07-27-GBPUSD-2-2",
				Symbol: "GBP/USD", Side: "sell", FilledQty: 60000,
				FilledAvgPx: 1.106, FilledAt: newFilledAt,
			},
		},
	}
	runner := &currencyStrengthPaperRunner{
		broker: broker, journal: journal, entryRecorded: true,
		statePath: t.TempDir() + "/currency-strength.json",
		state: currencyStrengthStateFromRun(store.CurrencyStrengthRun{
			SessionDate: time.Date(2026, 7, 27, 0, 0, 0, 0, time.UTC),
			Status:      "exit_failed", SignalSymbol: "GBP/USD", SignalSide: "buy",
			EntryStatus: "filled", EntryFillPrice: 1.1, EntryFilledQty: 100000,
			ExitOrderID: "EXIT-PRIOR", ExitStatus: "residual", ExitFilledQty: 40000,
		}),
	}
	if err := runner.exitActive(context.Background()); err != nil {
		t.Fatal(err)
	}
	if broker.calls != 0 || runner.state != nil {
		t.Fatalf("filled residual retry was not recovered and finalized: calls=%d state=%+v",
			broker.calls, runner.state)
	}
	if len(journal.exits) != 1 || journal.exits[0].ExitOrderID != "EXIT-RETRY-FILLED" ||
		journal.exits[0].ExitFilledQty != 60000 || journal.exits[0].ExitedAt == nil ||
		!journal.exits[0].ExitedAt.Equal(newFilledAt) {
		t.Fatalf("newest residual fill was not journaled accurately: %+v", journal.exits)
	}
}

func TestCurrencyStrengthResidualRecoveryRejectsAmbiguousWorkingRetries(t *testing.T) {
	base := &successfulCurrencyStrengthExecutor{
		positions: []execution.Position{{Ticker: "GBP/USD", Quantity: 60000}},
	}
	broker := &workingExitExecutor{
		successfulCurrencyStrengthExecutor: base,
		orders: []execution.WorkingOrder{
			{
				OrderID: "EXIT-A", TradeID: "currency-strength-exit-2026-07-27-GBPUSD-1-2",
				Ticker: "GBP/USD", Side: "sell", Status: "pending", RemainingQty: 30000,
			},
			{
				OrderID: "EXIT-B", TradeID: "currency-strength-exit-2026-07-27-GBPUSD-2-3",
				Ticker: "GBP/USD", Side: "sell", Status: "pending", RemainingQty: 30000,
			},
		},
	}
	runner := &currencyStrengthPaperRunner{
		broker: broker, statePath: t.TempDir() + "/currency-strength.json",
		state: &currencyStrengthRunState{
			Date: "2026-07-27", Symbol: "GBP/USD", Side: "buy", Quantity: 100000,
			EntryStatus: "filled", EntryFilledQty: 100000, ExitRetry: true,
		},
	}
	if err := runner.exitActive(context.Background()); err == nil {
		t.Fatal("ambiguous working residual retries were accepted")
	}
	if base.calls != 0 {
		t.Fatalf("ambiguous recovery submitted another exit: calls=%d", base.calls)
	}
}

func TestCurrencyStrengthAdoptsDeterministicWorkingEntryAfterDurabilityFailure(t *testing.T) {
	base := &successfulCurrencyStrengthExecutor{}
	broker := &workingExitExecutor{
		successfulCurrencyStrengthExecutor: base,
		orders: []execution.WorkingOrder{{
			OrderID:      "ENTRY-WORKING",
			TradeID:      "currency-strength-2026-07-27-GBPUSD",
			Ticker:       "GBP/USD",
			Side:         "buy",
			Status:       "pending",
			RemainingQty: 100000,
		}},
	}
	runner := &currencyStrengthPaperRunner{
		cfg:       config.CurrencyStrengthConfig{Units: 100000},
		broker:    broker,
		statePath: t.TempDir(), // force the sidecar write to fail
		journal:   &captureCurrencyStrengthJournal{entryErrors: 1},
		now:       func() time.Time { return time.Date(2026, 7, 27, 9, 0, 0, 0, time.UTC) },
	}
	adopted, err := runner.adoptWorkingEntry(
		context.Background(),
		"2026-07-27",
		currencystrength.Signal{Symbol: "GBP/USD", Side: "buy"},
		"currency-strength-2026-07-27-GBPUSD",
	)
	if err != nil {
		t.Fatal(err)
	}
	if !adopted || runner.state == nil ||
		runner.state.EntryOrderID != "ENTRY-WORKING" || base.calls != 0 {
		t.Fatalf("strategy-owned working entry was not adopted: adopted=%v state=%+v calls=%d",
			adopted, runner.state, base.calls)
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

func TestCurrencyStrengthExitReferenceDoesNotRequireWritableSidecar(t *testing.T) {
	runner := &currencyStrengthPaperRunner{
		statePath: t.TempDir(), // force saveState to fail
		state: &currencyStrengthRunState{
			Date:   "2026-07-27",
			Symbol: "GBP/USD",
		},
		now: func() time.Time { return time.Date(2026, 7, 27, 18, 0, 0, 0, time.UTC) },
	}
	ref, err := runner.reserveExitTradeID()
	if err != nil {
		t.Fatalf("sidecar failure blocked exit reference: %v", err)
	}
	if !strings.HasPrefix(ref, "currency-strength-exit-2026-07-27-GBPUSD-") {
		t.Fatalf("unexpected broker-discoverable exit reference: %q", ref)
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
	broker.recentFills = []execution.BrokerOrder{{
		BrokerOrderID: "EXIT-PENDING",
		TradeID:       runner.state.ExitTradeID,
		Symbol:        "GBP/USD", Side: "sell", FilledQty: 100000,
		FilledAvgPx: 1.105, FilledAt: time.Date(2026, 7, 27, 18, 1, 0, 0, time.UTC),
	}}
	if err := runner.exitActive(context.Background()); err != nil {
		t.Fatal(err)
	}
	if runner.state != nil || broker.calls != 1 {
		t.Fatalf("terminal exit reconciliation failed: state=%+v calls=%d", runner.state, broker.calls)
	}
}

func TestCurrencyStrengthRetriesPurgedZeroFillExitWithOpenPosition(t *testing.T) {
	broker := &sequencedCurrencyStrengthExecutor{
		positions: [][]execution.Position{{
			{Ticker: "GBP/USD", Quantity: 100000},
		}},
	}
	runner := &currencyStrengthPaperRunner{
		broker: broker, journal: &captureCurrencyStrengthJournal{}, entryRecorded: true,
		statePath: t.TempDir() + "/currency-strength.json",
		state: &currencyStrengthRunState{
			Date: "2026-07-27", Symbol: "GBP/USD", Side: "buy", Quantity: 100000,
			EntryStatus: "filled", EntryFilledQty: 100000,
			ExitOrderID: "EXIT-PURGED", ExitTradeID: "currency-strength-exit-old",
			ExitStatus: "pending", ExitRequestedQty: 100000,
		},
		now: func() time.Time { return time.Date(2026, 7, 27, 18, 0, 0, 0, time.UTC) },
	}
	if err := runner.finalizeExitedState(context.Background()); err != nil {
		t.Fatal(err)
	}
	if runner.state == nil || runner.state.ExitOrderID != "" || !runner.state.ExitRetry {
		t.Fatalf("purged exit remained pinned: %+v", runner.state)
	}
	broker.statuses = []execution.OrderResult{{
		BrokerOrderID: "EXIT-SEQUENCED", Status: "pending",
	}}
	if err := runner.exitActive(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(broker.requests) != 1 || runner.state.ExitOrderID != "EXIT-SEQUENCED" {
		t.Fatalf("purged exit did not release a replacement flatten: requests=%+v state=%+v",
			broker.requests, runner.state)
	}
}

func TestCurrencyStrengthLaterFillRefreshesExecutionTimestamp(t *testing.T) {
	firstFillAt := time.Date(2026, 7, 27, 18, 1, 0, 0, time.UTC)
	finalFillAt := firstFillAt.Add(4 * time.Minute)
	journal := &captureCurrencyStrengthJournal{}
	broker := &successfulCurrencyStrengthExecutor{
		statusResult: execution.OrderResult{
			BrokerOrderID: "EXIT-MULTI-FILL", Status: "filled",
			FilledPrice: 1.1056, FilledQty: 100000,
		},
		recentFills: []execution.BrokerOrder{
			{
				BrokerOrderID: "EXIT-MULTI-FILL", Symbol: "GBP/USD",
				Side: "sell", FilledQty: 40000, FilledAvgPx: 1.105,
				FilledAt: firstFillAt,
			},
			{
				BrokerOrderID: "EXIT-MULTI-FILL", Symbol: "GBP/USD",
				Side: "sell", FilledQty: 60000, FilledAvgPx: 1.106,
				FilledAt: finalFillAt,
			},
		},
	}
	runner := &currencyStrengthPaperRunner{
		broker: broker, journal: journal, entryRecorded: true,
		statePath: t.TempDir() + "/currency-strength.json",
		state: &currencyStrengthRunState{
			Date: "2026-07-27", Symbol: "GBP/USD", Side: "buy", Quantity: 100000,
			EntryStatus: "filled", EntryFillPrice: 1.1, EntryFilledQty: 100000,
			ExitOrderID: "EXIT-MULTI-FILL", ExitStatus: "partial",
			ExitFillPrice: 1.105, ExitFilledQty: 40000,
			ExitFillTimeQty: 40000, ExitedAt: firstFillAt.Format(time.RFC3339),
		},
	}
	if err := runner.finalizeExitedState(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(journal.exits) != 1 || journal.exits[0].ExitedAt == nil ||
		!journal.exits[0].ExitedAt.Equal(finalFillAt) {
		t.Fatalf("later fill did not refresh execution timestamp: %+v", journal.exits)
	}
}

func TestCurrencyStrengthCancelsWorkingExitBeforeAcceptingFlatSnapshot(t *testing.T) {
	broker := &sequencedCurrencyStrengthExecutor{
		statuses: []execution.OrderResult{
			{BrokerOrderID: "EXIT-WORKING-FLAT", Status: "pending"},
			{BrokerOrderID: "EXIT-WORKING-FLAT", Status: "pending"},
			{BrokerOrderID: "EXIT-WORKING-FLAT", Status: "pending"},
			{BrokerOrderID: "EXIT-WORKING-FLAT", Status: "cancelled"},
		},
		positions: [][]execution.Position{nil, nil},
	}
	runner := &currencyStrengthPaperRunner{
		broker:        broker,
		statePath:     t.TempDir() + "/currency-strength.json",
		entryRecorded: true,
		journal:       &captureCurrencyStrengthJournal{},
		state: &currencyStrengthRunState{
			Date:           "2026-07-27",
			Symbol:         "GBP/USD",
			Side:           "buy",
			Quantity:       100000,
			EntryStatus:    "filled",
			EntryFilledQty: 100000,
			ExitOrderID:    "EXIT-WORKING-FLAT",
			ExitStatus:     "pending",
		},
		now: func() time.Time { return time.Date(2026, 7, 27, 18, 0, 0, 0, time.UTC) },
	}
	if err := runner.finalizeExitedState(context.Background()); err != nil {
		t.Fatal(err)
	}
	if runner.state == nil || runner.state.ExitOrderID == "" || broker.cancels != 1 {
		t.Fatalf("working exit was cleared before cancellation settled: state=%+v cancels=%d",
			runner.state, broker.cancels)
	}
	if err := runner.finalizeExitedState(context.Background()); err != nil {
		t.Fatal(err)
	}
	if runner.state != nil || broker.cancels != 2 {
		t.Fatalf("cancel-confirmed flat exit did not finalize: state=%+v cancels=%d",
			runner.state, broker.cancels)
	}
}

func TestCurrencyStrengthResubmitsConfirmedResidualWithoutPnLOverstatement(t *testing.T) {
	now := time.Date(2026, 7, 27, 18, 0, 0, 0, time.UTC)
	journal := &captureCurrencyStrengthJournal{}
	broker := &successfulCurrencyStrengthExecutor{
		result: execution.OrderResult{
			BrokerOrderID: "EXIT-PART-1",
			Status:        "filled",
			FilledPrice:   1.102,
			FilledQty:     50000,
		},
		positions:          []execution.Position{{Ticker: "GBP/USD", Quantity: 100000}},
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
		now:     func() time.Time { return now },
	}
	if err := runner.exitActive(context.Background()); err != nil {
		t.Fatal(err)
	}
	if runner.state == nil || runner.state.ExitOrderID != "EXIT-PART-1" ||
		runner.state.ExitFillObserved == "" {
		t.Fatalf("filled exit did not wait for portfolio propagation: %+v", runner.state)
	}
	broker.positions = []execution.Position{{Ticker: "GBP/USD", Quantity: 50000}}
	now = now.Add(currencyStrengthPortfolioPropagationGrace + time.Second)
	if err := runner.exitActive(context.Background()); err != nil {
		t.Fatal(err)
	}
	if runner.state == nil || runner.state.ExitOrderID != "" || !runner.state.ExitRetry {
		t.Fatalf("residual position was not prepared for a safe retry: %+v", runner.state)
	}
	broker.recentFills = []execution.BrokerOrder{{
		BrokerOrderID: "EXIT-PART-1",
		TradeID:       broker.requests[0].TradeID,
		Symbol:        "GBP/USD", Side: "sell", FilledQty: 50000,
		FilledAvgPx: 1.102, FilledAt: now.Add(-time.Minute),
	}}

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
	if last.RealizedPnL == nil || *last.RealizedPnL < 149.99 || *last.RealizedPnL > 150.01 {
		t.Fatalf("second exit attempt P&L was not recorded independently: %+v", last)
	}
}

func TestCurrencyStrengthResidualRetryProceedsWhileJournalIsDown(t *testing.T) {
	now := time.Date(2026, 7, 27, 18, 0, 0, 0, time.UTC)
	journal := &captureCurrencyStrengthJournal{exitErrors: 1}
	broker := &successfulCurrencyStrengthExecutor{
		result: execution.OrderResult{
			BrokerOrderID: "EXIT-RESIDUAL",
			Status:        "filled",
			FilledPrice:   1.102,
			FilledQty:     50000,
		},
		positions:          []execution.Position{{Ticker: "GBP/USD", Quantity: 100000}},
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
		now:     func() time.Time { return now },
	}
	if err := runner.exitActive(context.Background()); err != nil {
		t.Fatal(err)
	}
	if runner.state == nil || runner.state.ExitOrderID != "EXIT-RESIDUAL" ||
		runner.state.ExitFillObserved == "" {
		t.Fatalf("filled exit did not wait for portfolio propagation: %+v", runner.state)
	}
	broker.positions = []execution.Position{{Ticker: "GBP/USD", Quantity: 50000}}
	now = now.Add(currencyStrengthPortfolioPropagationGrace + time.Second)
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
	for _, exitStatus := range []string{"residual", "cancelled", "rejected"} {
		t.Run(exitStatus, func(t *testing.T) {
			run := store.CurrencyStrengthRun{
				SessionDate:    time.Date(2026, 7, 27, 0, 0, 0, 0, time.UTC),
				Status:         "exit_failed",
				SignalSymbol:   "GBP/USD",
				SignalSide:     "buy",
				EntryStatus:    "filled",
				EntryFilledQty: 100000,
				ExitOrderID:    "EXIT-TERMINAL",
				ExitStatus:     exitStatus,
				ExitFilledQty:  50000,
			}
			state := currencyStrengthStateFromRun(run)
			if state.ExitOrderID != "" || !state.ExitRetry ||
				state.ExitPriorOrderID != "EXIT-TERMINAL" ||
				state.ExitRetryMaxQty != 50000 ||
				state.ExitCumulativeQty != 50000 {
				t.Fatalf("terminal exit restored as working: %+v", state)
			}
		})
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
	brokerEnteredAt := time.Date(2026, 7, 27, 9, 0, 12, 0, time.UTC)
	staleEnteredAt := brokerEnteredAt.Add(-12 * time.Second)
	timestamped := &currencyStrengthRunState{
		EntryStatus: "filled", EntryFillPrice: 1.101, EntryFilledQty: 100000,
		EntryFillTimeQty: 100000, EnteredAt: brokerEnteredAt.Format(time.RFC3339),
	}
	staleTimestamp := store.CurrencyStrengthRun{
		Status: "entered", EntryStatus: "filled", EntryFillPrice: 1.101,
		EntryFilledQty: 100000, EnteredAt: &staleEnteredAt,
	}
	if currencyStrengthJournalCoversEntry(timestamped, staleTimestamp) {
		t.Fatal("acknowledgement-time journal was considered to cover broker entry timestamp")
	}
	mergeCurrencyStrengthRunIntoState(timestamped, staleTimestamp)
	if timestamped.EnteredAt != brokerEnteredAt.Format(time.RFC3339) {
		t.Fatalf("stale journal replaced broker execution timestamp: %+v", timestamped)
	}
	currentExit := &currencyStrengthRunState{
		ExitOrderID: "EXIT-CURRENT", ExitStatus: "partial",
		ExitFillPrice: 1.105, ExitFilledQty: 30000, ExitFillTimeQty: 30000,
	}
	cumulativeExit := store.CurrencyStrengthRun{
		Status: "exit_submitted", ExitOrderID: "EXIT-CURRENT",
		ExitStatus: "partial", ExitFillPrice: 1.104, ExitFilledQty: 70000,
	}
	mergeCurrencyStrengthRunIntoState(currentExit, cumulativeExit)
	if currentExit.ExitFilledQty != 30000 || currentExit.ExitFillPrice != 1.105 ||
		currentExit.ExitCumulativeQty != 70000 {
		t.Fatalf("cumulative journal overwrote current-order economics: %+v", currentExit)
	}
	journalOnlyExit := &currencyStrengthRunState{}
	mergeCurrencyStrengthRunIntoState(journalOnlyExit, cumulativeExit)
	if journalOnlyExit.ExitOrderID != "EXIT-CURRENT" ||
		journalOnlyExit.ExitFilledQty != 0 ||
		journalOnlyExit.ExitCumulativeQty != 70000 {
		t.Fatalf("journal aggregate was mistaken for current-order economics: %+v", journalOnlyExit)
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

	partial := &currencyStrengthRunState{
		EntryStatus:    "partial",
		EntryFillPrice: 1.101,
		EntryFilledQty: 50000,
		Quantity:       50000,
	}
	mergeCurrencyStrengthRunIntoState(partial, store.CurrencyStrengthRun{
		EntryStatus:    "cancelled",
		EntryFillPrice: 1.099,
		EntryFilledQty: 25000,
	})
	if partial.EntryFillPrice != 1.101 || partial.EntryFilledQty != 50000 {
		t.Fatalf("smaller terminal journal fill corrupted local economics: %+v", partial)
	}
}

func TestCurrencyStrengthPnLUsesMatchedFilledQuantity(t *testing.T) {
	journal := &captureCurrencyStrengthJournal{}
	runner := &currencyStrengthPaperRunner{
		entryRecorded: true,
		journal:       journal,
		state: &currencyStrengthRunState{
			Date:           "2026-07-27",
			Symbol:         "GBP/USD",
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

func TestCurrencyStrengthPnLConvertsUSDJPYQuoteCurrencyToUSD(t *testing.T) {
	runner := &currencyStrengthPaperRunner{
		state: &currencyStrengthRunState{
			Symbol:         "USD/JPY",
			Side:           "buy",
			EntryStatus:    "filled",
			EntryFillPrice: 150,
		},
	}
	pnl, err := runner.realizedPnLUSD(context.Background(), currencyStrengthExitAudit{
		FillPrice: 151,
		FilledQty: 100000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if pnl == nil || *pnl < 662.24 || *pnl > 662.26 {
		t.Fatalf("USD/JPY P&L = %v, want about 662.25 USD", pnl)
	}
}

type historicalConversionMarketData struct {
	candles map[string][]data.Candle
}

func (f historicalConversionMarketData) FetchCandles(
	_ context.Context,
	symbol string,
	_ data.Timeframe,
	_, _ time.Time,
) ([]data.Candle, error) {
	candles, ok := f.candles[symbol]
	if !ok {
		return nil, fmt.Errorf("unexpected historical conversion request for %s", symbol)
	}
	return candles, nil
}

func (historicalConversionMarketData) FetchQuote(context.Context, string) (*data.Quote, error) {
	return nil, errors.New("spot quote must not be used for deferred P&L")
}

func (historicalConversionMarketData) FetchHistory(
	context.Context,
	string,
	data.Timeframe,
	int,
) ([]data.Candle, error) {
	return nil, errors.New("unexpected history fetch")
}

func TestCurrencyStrengthCrossPnLUsesExitTimestampConversion(t *testing.T) {
	exitedAt := time.Date(2026, 7, 27, 18, 0, 30, 0, time.UTC)
	runner := &currencyStrengthPaperRunner{
		market: historicalConversionMarketData{candles: map[string][]data.Candle{
			"JPY/USD": {
				{Timestamp: exitedAt.Add(-time.Minute), Close: 0.0067},
				{Timestamp: exitedAt.Add(time.Minute), Close: 0.0099},
			},
		}},
		state: &currencyStrengthRunState{
			Symbol: "EUR/JPY", Side: "buy", EntryStatus: "filled", EntryFillPrice: 160,
		},
	}
	pnl, err := runner.realizedPnLUSD(context.Background(), currencyStrengthExitAudit{
		FillPrice: 161, FilledQty: 100000, ExitedAt: exitedAt.Format(time.RFC3339),
	})
	if err != nil {
		t.Fatal(err)
	}
	if pnl == nil || *pnl < 669.99 || *pnl > 670.01 {
		t.Fatalf("EUR/JPY historical-conversion P&L = %v, want 670.00 USD", pnl)
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
