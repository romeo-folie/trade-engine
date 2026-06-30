package orb

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/execution"
	"github.com/romeo-folie/trade-engine/internal/store"
)

// flattenExec is a stub Executor (plus WorkingOrdersLister) for time-exit tests.
// It records the orders it is asked to place and cancel so assertions can verify
// the flatten side/quantity and that the dangling legs were cancelled.
type flattenExec struct {
	positions    []execution.Position
	working      []execution.WorkingOrder
	placeResult  execution.OrderResult
	placeErr     error
	statusResult execution.OrderResult

	placed    []execution.OrderRequest
	cancelled []string

	replaceErr error
	replaced   []stopReplace
}

type stopReplace struct {
	orderID string
	newStop float64
}

func (e *flattenExec) PlaceOrder(_ context.Context, req execution.OrderRequest) (execution.OrderResult, error) {
	e.placed = append(e.placed, req)
	return e.placeResult, e.placeErr
}
func (e *flattenExec) CancelOrder(_ context.Context, id string) error {
	e.cancelled = append(e.cancelled, id)
	return nil
}
func (e *flattenExec) GetOrderStatus(_ context.Context, _ string) (execution.OrderResult, error) {
	return e.statusResult, nil
}
func (e *flattenExec) GetPositions(_ context.Context) ([]execution.Position, error) {
	return e.positions, nil
}
func (e *flattenExec) GetAccount(_ context.Context) (execution.AccountInfo, error) {
	return execution.AccountInfo{}, nil
}
func (e *flattenExec) GetWorkingOrders(_ context.Context) ([]execution.WorkingOrder, error) {
	return e.working, nil
}
func (e *flattenExec) ReplaceStopOrder(_ context.Context, orderID string, newStop float64) (execution.OrderResult, error) {
	if e.replaceErr != nil {
		return execution.OrderResult{}, e.replaceErr
	}
	e.replaced = append(e.replaced, stopReplace{orderID: orderID, newStop: newStop})
	return execution.OrderResult{BrokerOrderID: orderID, Status: "pending"}, nil
}

// ─── shouldTimeExit ───────────────────────────────────────────────────────────

func TestShouldTimeExit(t *testing.T) {
	anchor := time.Date(2026, 6, 1, 8, 0, 0, 0, time.UTC)
	cases := []struct {
		name    string
		maxHold int
		now     time.Time
		want    bool
	}{
		{"disabled (zero)", 0, anchor.Add(10 * time.Hour), false},
		{"disabled (negative)", -5, anchor.Add(10 * time.Hour), false},
		{"under limit", 240, anchor.Add(239 * time.Minute), false},
		{"exactly at limit (inclusive)", 240, anchor.Add(240 * time.Minute), true},
		{"over limit", 240, anchor.Add(6 * time.Hour), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := shouldTimeExit(anchor, c.maxHold, c.now); got != c.want {
				t.Errorf("shouldTimeExit(maxHold=%d) = %v; want %v", c.maxHold, got, c.want)
			}
		})
	}
}

// ─── forexSecType ─────────────────────────────────────────────────────────────

func TestForexSecType(t *testing.T) {
	s := &ORBScheduler{forexCfg: &config.ForexORBConfig{Instruments: []config.Instrument{
		{Symbol: "EUR/USD", SecType: "CASH"},
		{Symbol: "XAU/USD", SecType: "CFD"},
	}}}
	if got := s.forexSecType("EUR/USD"); got != "CASH" {
		t.Errorf("forexSecType(EUR/USD) = %q; want CASH", got)
	}
	if got := s.forexSecType("XAU/USD"); got != "CFD" {
		t.Errorf("forexSecType(XAU/USD) = %q; want CFD", got)
	}
	if got := s.forexSecType("UNKNOWN"); got != "" {
		t.Errorf("forexSecType(UNKNOWN) = %q; want empty", got)
	}
}

// ─── cancelBracketLegs ────────────────────────────────────────────────────────

// tradeWithLegs is a filled trade whose TP=1.12, SL=1.09 are used to price-match legs.
func tradeWithLegs() store.ORBTrade {
	return store.ORBTrade{ID: "t1", Ticker: "EUR/USD", Side: "buy", Qty: 5000,
		FillPrice: 1.10, TakeProfit: 1.12, StopLoss: 1.09}
}

func TestCancelBracketLegs_OnlyMatchingTicker(t *testing.T) {
	exec := &flattenExec{working: []execution.WorkingOrder{
		{OrderID: "tp-eur", Ticker: "EUR/USD", Price: 1.12},     // TP leg — matches
		{OrderID: "sl-eur", Ticker: "EUR/USD", StopPrice: 1.09}, // SL leg — matches
		{OrderID: "tp-gbp", Ticker: "GBP/USD", Price: 1.12},     // different ticker — must NOT be cancelled
	}}
	s := &ORBScheduler{executor: exec}
	s.cancelBracketLegs(context.Background(), tradeWithLegs())

	if len(exec.cancelled) != 2 {
		t.Fatalf("cancelled %d orders; want 2 (only EUR/USD legs)", len(exec.cancelled))
	}
	for _, id := range exec.cancelled {
		if id == "tp-gbp" {
			t.Error("cancelled a GBP/USD leg; must only touch the target ticker")
		}
	}
}

func TestCancelBracketLegs_OnlyCancelsProvenLegs(t *testing.T) {
	exec := &flattenExec{working: []execution.WorkingOrder{
		{OrderID: "sl-leg", Ticker: "EUR/USD", StopPrice: 1.09}, // matches SL — cancel
		{OrderID: "manual", Ticker: "EUR/USD", Price: 1.30},     // unrelated price — must survive
		{OrderID: "no-price", Ticker: "EUR/USD"},                // no price exposed — cannot prove → must survive
	}}
	s := &ORBScheduler{executor: exec}
	s.cancelBracketLegs(context.Background(), tradeWithLegs())

	if len(exec.cancelled) != 1 || exec.cancelled[0] != "sl-leg" {
		t.Errorf("cancelled = %v; want [sl-leg] only (unrelated and no-price orders must not be cancelled)", exec.cancelled)
	}
}

func TestCancelBracketLegs_NoListerIsNoop(t *testing.T) {
	// stubExecutor (from reconcile_test.go) does NOT implement WorkingOrdersLister,
	// so cancelBracketLegs must be a safe no-op rather than panic.
	s := &ORBScheduler{executor: &stubExecutor{}}
	s.cancelBracketLegs(context.Background(), tradeWithLegs()) // must not panic
}

func TestCancelBracketLegs_ProtectsCurrentSessionEntries(t *testing.T) {
	// A prior-session trade time-exiting must cancel its own dangling leg but NOT
	// the current session's freshly-placed entry stop on the same ticker (which the
	// account-wide working-orders lookup also returns).
	exec := &flattenExec{working: []execution.WorkingOrder{
		{OrderID: "entry-sell", Ticker: "EUR/USD", StopPrice: 1.09},  // current session entry — protected even though price matches
		{OrderID: "dangling-sl", Ticker: "EUR/USD", StopPrice: 1.09}, // prior trade's stale leg — must be cancelled
	}}
	s := &ORBScheduler{
		executor: exec,
		orderIDs: map[string][2]string{"EUR/USD": {"entry-buy", "entry-sell"}},
	}
	s.cancelBracketLegs(context.Background(), tradeWithLegs())

	if len(exec.cancelled) != 1 || exec.cancelled[0] != "dangling-sl" {
		t.Errorf("cancelled = %v; want [dangling-sl] (current-session entry must be protected)", exec.cancelled)
	}
}

// ─── classifyOrphanLegs ───────────────────────────────────────────────────────

func TestClassifyOrphanLegs(t *testing.T) {
	s := &ORBScheduler{forexCfg: &config.ForexORBConfig{Instruments: []config.Instrument{
		{Symbol: "EUR/USD"}, {Symbol: "GBP/USD"},
	}}}
	orders := []execution.WorkingOrder{
		{OrderID: "orphan-tp", Ticker: "EUR/USD", Price: 1.12},     // proven stale leg → cancel
		{OrderID: "orphan-sl", Ticker: "EUR/USD", StopPrice: 1.09}, // proven stale leg → cancel
		{OrderID: "live-leg", Ticker: "GBP/USD", StopPrice: 1.30},  // ticker has an open filled trade → skip
		{OrderID: "protected", Ticker: "EUR/USD", StopPrice: 1.09}, // current-session entry → skip
		{OrderID: "noprice", Ticker: "EUR/USD"},                    // idle ticker, no price → ambiguous (alert)
		{OrderID: "unrelated", Ticker: "EUR/USD", Price: 1.30},     // idle ticker, non-matching price → leave alone
		{OrderID: "equity", Ticker: "AAPL", Price: 1.12},           // not a forex instrument → skip
	}
	openTickers := map[string]struct{}{"GBP/USD": {}}
	timeExitPrices := map[string][]float64{"EUR/USD": {1.12, 1.09}}
	protected := map[string]struct{}{"protected": {}}

	cancel, ambiguous := s.classifyOrphanLegs(orders, openTickers, timeExitPrices, protected)

	wantCancel := map[string]bool{"orphan-tp": true, "orphan-sl": true}
	if len(cancel) != len(wantCancel) {
		t.Fatalf("cancel = %v; want %v", cancel, wantCancel)
	}
	for _, id := range cancel {
		if !wantCancel[id] {
			t.Errorf("cancel includes %q; want only proven stale legs %v", id, wantCancel)
		}
	}
	if len(ambiguous) != 1 || ambiguous[0].OrderID != "noprice" {
		t.Errorf("ambiguous = %v; want only [noprice]", ambiguous)
	}
}

// ─── isForexInstrument ────────────────────────────────────────────────────────

func TestIsForexInstrument(t *testing.T) {
	s := &ORBScheduler{forexCfg: &config.ForexORBConfig{Instruments: []config.Instrument{
		{Symbol: "EUR/USD"}, {Symbol: "XAU/USD"},
	}}}
	if !s.isForexInstrument("EUR/USD") {
		t.Error("EUR/USD should be a configured forex instrument")
	}
	if s.isForexInstrument("AAPL") {
		t.Error("AAPL must NOT be treated as a forex instrument (shared-table equity trade)")
	}
	// nil forexCfg → never a forex instrument.
	if (&ORBScheduler{}).isForexInstrument("EUR/USD") {
		t.Error("nil forexCfg should report no forex instruments")
	}
}

func TestTickersWithOpenFilledTrades_NilDBIsEmpty(t *testing.T) {
	// A nil/unavailable DB must yield an empty set so a transient DB error can never
	// block bracket placement for the whole session.
	s := &ORBScheduler{}
	if got := s.tickersWithOpenFilledTrades(context.Background()); len(got) != 0 {
		t.Errorf("tickersWithOpenFilledTrades with nil db = %v; want empty", got)
	}
}

// ─── awaitFlattenFill ─────────────────────────────────────────────────────────

func TestAwaitFlattenFill(t *testing.T) {
	t.Run("immediate fill from place result", func(t *testing.T) {
		s := &ORBScheduler{executor: &flattenExec{}}
		price, ok := s.awaitFlattenFill(context.Background(),
			execution.OrderResult{Status: "filled", FilledPrice: 1.1750})
		if !ok || price != 1.1750 {
			t.Errorf("got (%v, %v); want (1.1750, true)", price, ok)
		}
	})

	t.Run("poll then fill", func(t *testing.T) {
		exec := &flattenExec{statusResult: execution.OrderResult{Status: "filled", FilledPrice: 1.20}}
		s := &ORBScheduler{executor: exec}
		price, ok := s.awaitFlattenFill(context.Background(),
			execution.OrderResult{BrokerOrderID: "ord-1", Status: "pending"})
		if !ok || price != 1.20 {
			t.Errorf("got (%v, %v); want (1.20, true)", price, ok)
		}
	})

	t.Run("never fills — not confirmed", func(t *testing.T) {
		// statusResult stays pending; bounded poll must give up and report not-ok.
		exec := &flattenExec{statusResult: execution.OrderResult{Status: "pending"}}
		s := &ORBScheduler{executor: exec}
		ctx, cancel := context.WithCancel(context.Background())
		cancel() // cancel immediately so the poll loop exits without real sleeps
		_, ok := s.awaitFlattenFill(ctx, execution.OrderResult{BrokerOrderID: "ord-1", Status: "pending"})
		if ok {
			t.Error("got ok=true for an order that never filled; want false")
		}
	})
}

// ─── flattenAndRecordTimeExit ─────────────────────────────────────────────────

func newForexScheduler(exec execution.Executor) *ORBScheduler {
	return &ORBScheduler{
		executor: exec,
		forexCfg: &config.ForexORBConfig{
			Enabled:        true,
			MaxHoldMinutes: 240,
			Instruments:    []config.Instrument{{Symbol: "EUR/USD", SecType: "CASH"}},
		},
	}
}

// TestFlattenDueTimeExits_SplitGroupCancelsOnlyWhenAllFlat is the P1 safety invariant: a
// split-bracket group (runner + partial sharing one stop price) must cancel its legs ONLY
// once every leg is confirmed flat — never while a sibling may still hold a position.
func TestFlattenDueTimeExits_SplitGroupCancelsOnlyWhenAllFlat(t *testing.T) {
	past := time.Date(2026, 6, 1, 8, 0, 0, 0, time.UTC)
	now := past.Add(6 * time.Hour) // well past the 240-minute max hold
	parent := "runner"
	mkLegs := func() []store.ORBTrade {
		return []store.ORBTrade{
			{ID: "runner", Ticker: "EUR/USD", Side: "buy", Qty: 6000, FillPrice: 1.10,
				EntryStop: 1.10, StopLoss: 1.0950, TakeProfit: 1.11,
				ScaleOutRole: "runner", FilledAt: &past},
			{ID: "partial", Ticker: "EUR/USD", Side: "buy", Qty: 4000, FillPrice: 1.10,
				EntryStop: 1.10, StopLoss: 1.0950, TakeProfit: 1.105,
				ScaleOutRole: "partial", ScaleOutParentID: &parent, FilledAt: &past},
		}
	}
	legOrders := []execution.WorkingOrder{
		{OrderID: "r-tp", Ticker: "EUR/USD", Price: 1.11},     // runner TP
		{OrderID: "sl", Ticker: "EUR/USD", StopPrice: 1.0950}, // shared stop
		{OrderID: "p-tp", Ticker: "EUR/USD", Price: 1.105},    // partial TP
	}

	// allFlat=true: broker holds no position → both legs already flat → the group's legs are
	// cancelled (cleanup is safe because nothing is open).
	flat := &flattenExec{positions: nil, working: legOrders}
	newForexScheduler(flat).flattenDueTimeExits(context.Background(), mkLegs(), now)
	if len(flat.placed) != 0 {
		t.Errorf("placed %d flatten orders on an already-flat group; want 0", len(flat.placed))
	}
	for _, want := range []string{"r-tp", "sl", "p-tp"} {
		if !slices.Contains(flat.cancelled, want) {
			t.Errorf("all-flat group: leg %q not cancelled; cancelled=%v", want, flat.cancelled)
		}
	}

	// allFlat=false: broker still holds the position and the flatten never confirms → NO leg
	// may be cancelled. Cancelling the shared stop here would leave a still-open leg naked.
	open := &flattenExec{
		positions:    []execution.Position{{Ticker: "EUR/USD", Quantity: 10000}},
		working:      legOrders,
		statusResult: execution.OrderResult{Status: "pending"}, // flatten never confirms
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // keep the unconfirmed-fill poll from sleeping
	newForexScheduler(open).flattenDueTimeExits(ctx, mkLegs(), now)
	if len(open.cancelled) != 0 {
		t.Errorf("unconfirmed split group cancelled legs %v; want none (stops must stay until all legs flat)", open.cancelled)
	}
}

// TestFlattenDueTimeExits_PullsInNotYetDueSibling guards the timing-skew edge: split legs
// fill seconds apart, so the runner can pass max-hold one sweep before the partial. The
// whole group must still be handled together — the runner must NOT take the single-leg path
// and cancel the shared stop while the partial is still open.
func TestFlattenDueTimeExits_PullsInNotYetDueSibling(t *testing.T) {
	now := time.Date(2026, 6, 1, 14, 0, 0, 0, time.UTC)
	runnerFill := now.Add(-5 * time.Hour)     // past the 240-min max hold → due
	partialFill := now.Add(-10 * time.Minute) // not yet due
	parent := "runner"
	legs := []store.ORBTrade{
		{ID: "runner", Ticker: "EUR/USD", Side: "buy", Qty: 6000, FillPrice: 1.10,
			EntryStop: 1.10, StopLoss: 1.0950, TakeProfit: 1.11,
			ScaleOutRole: "runner", FilledAt: &runnerFill},
		{ID: "partial", Ticker: "EUR/USD", Side: "buy", Qty: 4000, FillPrice: 1.10,
			EntryStop: 1.10, StopLoss: 1.0950, TakeProfit: 1.105,
			ScaleOutRole: "partial", ScaleOutParentID: &parent, FilledAt: &partialFill},
	}
	// Broker still holds the position; the flatten never confirms (pending).
	exec := &flattenExec{
		positions:    []execution.Position{{Ticker: "EUR/USD", Quantity: 10000}},
		working:      []execution.WorkingOrder{{OrderID: "sl", Ticker: "EUR/USD", StopPrice: 1.0950}},
		statusResult: execution.OrderResult{Status: "pending"},
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	newForexScheduler(exec).flattenDueTimeExits(ctx, legs, now)

	// BOTH legs must be flattened as one group (placed=2), proving the not-yet-due partial was
	// pulled in rather than the runner taking the single-leg path.
	if len(exec.placed) != 2 {
		t.Errorf("placed %d flatten orders; want 2 (the not-yet-due sibling must be pulled into the group)", len(exec.placed))
	}
	// And because neither flatten confirmed, the shared stop must NOT be cancelled.
	if len(exec.cancelled) != 0 {
		t.Errorf("cancelled %v; want none (shared stop must survive while a leg is still open)", exec.cancelled)
	}
}

func TestFlattenAndRecordTimeExit_SkipsWhenAlreadyFlat(t *testing.T) {
	// Broker reports no open position → the bracket already closed; the time-exit
	// must NOT place a flatten order or fabricate an exit.
	exec := &flattenExec{positions: nil}
	s := newForexScheduler(exec)
	trade := store.ORBTrade{ID: "t1", Ticker: "EUR/USD", Side: "buy", Qty: 5000, FillPrice: 1.17}

	s.flattenAndRecordTimeExit(context.Background(), trade, 240, false)

	if len(exec.placed) != 0 {
		t.Errorf("placed %d flatten orders; want 0 when position already flat", len(exec.placed))
	}
}

func TestFlattenAndRecordTimeExit_BoundsQtyToNetPosition(t *testing.T) {
	// Trade qty 5000 but the broker only holds 3000 long (part already closed):
	// flatten must sell only 3000, never more than the net position.
	exec := &flattenExec{
		positions:    []execution.Position{{Ticker: "EUR/USD", Quantity: 3000}},
		working:      []execution.WorkingOrder{{OrderID: "sl-eur", Ticker: "EUR/USD"}},
		statusResult: execution.OrderResult{Status: "pending"}, // never confirms → no DB close
	}
	s := newForexScheduler(exec)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // keep the unconfirmed-fill poll from sleeping
	trade := store.ORBTrade{ID: "t1", Ticker: "EUR/USD", Side: "buy", Qty: 5000, FillPrice: 1.17}

	s.flattenAndRecordTimeExit(ctx, trade, 240, false)

	if len(exec.placed) != 1 {
		t.Fatalf("placed %d orders; want 1", len(exec.placed))
	}
	got := exec.placed[0]
	if got.Side != execution.OrderSideSell {
		t.Errorf("flatten side = %q; want sell (closing a long)", got.Side)
	}
	if got.Quantity != 3000 {
		t.Errorf("flatten qty = %v; want 3000 (bounded to net position, not trade qty 5000)", got.Quantity)
	}
	if got.Type != execution.OrderTypeMarket {
		t.Errorf("flatten type = %q; want market", got.Type)
	}
	if got.SecType != "CASH" {
		t.Errorf("flatten secType = %q; want CASH", got.SecType)
	}

	// P1 invariant: the flatten was NOT confirmed, so the protective bracket legs
	// must be left intact — cancelling them before a confirmed flatten would leave
	// the position naked.
	if len(exec.cancelled) != 0 {
		t.Errorf("cancelled %v legs on an unconfirmed flatten; want none (protection must be retained)", exec.cancelled)
	}
}

func TestFlattenAndRecordTimeExit_ShortFlattenSideIsBuy(t *testing.T) {
	// A sell (short) trade flattens by BUYING back; net position is negative.
	exec := &flattenExec{
		positions:    []execution.Position{{Ticker: "EUR/USD", Quantity: -5000}},
		statusResult: execution.OrderResult{Status: "pending"},
	}
	s := newForexScheduler(exec)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	trade := store.ORBTrade{ID: "t1", Ticker: "EUR/USD", Side: "sell", Qty: 5000, FillPrice: 1.17}

	s.flattenAndRecordTimeExit(ctx, trade, 240, false)

	if len(exec.placed) != 1 {
		t.Fatalf("placed %d orders; want 1", len(exec.placed))
	}
	if exec.placed[0].Side != execution.OrderSideBuy {
		t.Errorf("flatten side = %q; want buy (closing a short)", exec.placed[0].Side)
	}
	if exec.placed[0].Quantity != 5000 {
		t.Errorf("flatten qty = %v; want 5000", exec.placed[0].Quantity)
	}
}

// ─── enforceTimeExits guards ──────────────────────────────────────────────────

func TestEnforceTimeExits_NoopWhenForexDisabled(t *testing.T) {
	exec := &flattenExec{}
	// forexCfg present but disabled → must not even reach the executor.
	s := &ORBScheduler{executor: exec, forexCfg: &config.ForexORBConfig{Enabled: false}}
	s.enforceTimeExits(context.Background()) // db nil too; must be a clean no-op
	if len(exec.placed) != 0 {
		t.Error("enforceTimeExits acted while forex disabled")
	}
}
