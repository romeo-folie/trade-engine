package trailing

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/data"
	"github.com/romeo-folie/trade-engine/internal/events"
	"github.com/romeo-folie/trade-engine/internal/execution"
	"github.com/romeo-folie/trade-engine/internal/indicators"
	"github.com/romeo-folie/trade-engine/internal/store"
)

// ── ComputeNewStop ───────────────────────────────────────────────────────────

// TestComputeNewStop_Formula verifies the documented trailing-stop formula:
//
//	new_stop = max(currentStop, hwm − atr_multiple × atr)
//
// The PRD-review feedback flagged that the wrong formula
// `hwm × (1 − atr_multiple)` would invert when atr_multiple > 1; this test
// pins the correct subtraction-based formula in absolute price units.
func TestComputeNewStop_Formula(t *testing.T) {
	// hwm=100, atr=2.0, mult=1.5 ⇒ 100 - 3 = 97. currentStop=95 → new=97.
	got := ComputeNewStop(95, 100, 2.0, 1.5)
	if got != 97 {
		t.Fatalf("expected 97, got %v", got)
	}

	// Same formula at default 1.5x with a $50 stock and ATR=$1: trail at $48.50.
	got = ComputeNewStop(48.0, 50, 1.0, DefaultATRMultiple)
	if got != 48.5 {
		t.Fatalf("expected 48.5, got %v", got)
	}
}

// TestComputeNewStop_NeverWidens makes sure the max clamp keeps the stop
// from sinking below currentStop even when the candidate would.
func TestComputeNewStop_NeverWidens(t *testing.T) {
	// Candidate: 100 - 1.5*5 = 92.5. currentStop already 98 → must stay 98.
	got := ComputeNewStop(98, 100, 5.0, 1.5)
	if got != 98 {
		t.Fatalf("stop widened: expected 98, got %v", got)
	}
}

// TestComputeNewStop_Monotonicity walks a price up and confirms the stop
// only ever ratchets up — never sideways or down — across many iterations.
func TestComputeNewStop_Monotonicity(t *testing.T) {
	stop := 95.0
	atr := 2.0
	mult := 1.5
	for hwm := 100.0; hwm <= 130.0; hwm += 0.5 {
		next := ComputeNewStop(stop, hwm, atr, mult)
		if next < stop {
			t.Fatalf("stop receded: %v → %v at hwm=%v", stop, next, hwm)
		}
		stop = next
	}
	// At hwm=130 with 1.5*2=3 band, the stop should be 127.
	if stop != 127 {
		t.Fatalf("final stop expected 127, got %v", stop)
	}
}

// TestComputeNewStop_BadInputs returns currentStop unchanged when ATR or
// multiplier are non-positive — a transient zero must never wipe the stop.
func TestComputeNewStop_BadInputs(t *testing.T) {
	if got := ComputeNewStop(95, 100, 0, 1.5); got != 95 {
		t.Fatalf("zero ATR: expected 95, got %v", got)
	}
	if got := ComputeNewStop(95, 100, 2, 0); got != 95 {
		t.Fatalf("zero mult: expected 95, got %v", got)
	}
	if got := ComputeNewStop(95, 0, 2, 1.5); got != 95 {
		t.Fatalf("zero hwm: expected 95, got %v", got)
	}
}

// TestComputeNewStop_WrongFormulaWouldInvert is the regression test for the
// PRD-review bug: the (1 − atr_multiple) formulation would compute
// 100 * (1 - 1.5) = -50, which is obviously wrong. The correct formula
// returns 97 for the same inputs and that is what we assert.
func TestComputeNewStop_WrongFormulaWouldInvert(t *testing.T) {
	got := ComputeNewStop(0, 100, 2.0, 1.5)
	if got <= 0 {
		t.Fatalf("formula sanity check failed: got non-positive stop %v", got)
	}
	if got != 97 {
		t.Fatalf("expected 97 (= 100 - 1.5*2), got %v — has the formula regressed?", got)
	}
}

// ── Manager integration: HWM ratchet, formula, broker submit ─────────────────

// fakeStore implements TradeStore in-memory.
type fakeStore struct {
	mu                   sync.Mutex
	trades               map[string]store.Trade
	orders               map[string]store.Order // keyed by order id
	applied              map[string]float64
	nextID               int
	updateReplacementErr error
	insertErr            error
	stockIntentIDs       map[string]string
	stockIntentErr       error
	stockIntentRetryKeys map[string]string
	replacementIntentIDs map[string]string
}

func newFakeStore() *fakeStore {
	return &fakeStore{trades: map[string]store.Trade{}, orders: map[string]store.Order{}, applied: map[string]float64{}, stockIntentIDs: map[string]string{}, stockIntentRetryKeys: map[string]string{}, replacementIntentIDs: map[string]string{}}
}

func (f *fakeStore) StockIntentIDForKey(_ context.Context, key string) (string, error) {
	return f.stockIntentIDs[key], f.stockIntentErr
}

func (f *fakeStore) ResolveStockOrderIntentRetryGeneration(_ context.Context, key string) (string, error) {
	if f.stockIntentErr != nil {
		return "", f.stockIntentErr
	}
	return f.stockIntentRetryKeys[key], nil
}

func (f *fakeStore) LinkReplacementOrderIntent(_ context.Context, orderID, brokerOrderID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	o := f.orders[orderID]
	o.StockIntentID = f.replacementIntentIDs[brokerOrderID]
	f.orders[orderID] = o
	return nil
}

func (f *fakeStore) GetOpenTrades(_ context.Context) ([]store.Trade, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]store.Trade, 0, len(f.trades))
	for _, t := range f.trades {
		out = append(out, t)
	}
	return out, nil
}

func (f *fakeStore) UpdateTradeTrailing(_ context.Context, id string, stopLoss, hwm float64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.trades[id]
	if !ok {
		return errors.New("trade not found")
	}
	if stopLoss != 0 {
		t.StopLoss = stopLoss
	}
	if hwm != 0 {
		t.HighWaterMark = hwm
	}
	f.trades[id] = t
	return nil
}

func (f *fakeStore) LatestStopLossOrderForTrade(_ context.Context, tradeID string) (*store.Order, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, o := range f.orders {
		if o.TradeID == tradeID && o.IsStopLoss && (o.Status == "pending" || o.Status == "partial") {
			return &o, nil
		}
	}
	return nil, nil
}

func (f *fakeStore) ExitLegsForMarketExit(_ context.Context, tradeID string) ([]store.Order, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []store.Order
	for _, o := range f.orders {
		if o.TradeID != tradeID || (!o.IsStopLoss && !o.IsTakeProfit) || o.BrokerOrderID == "" {
			continue
		}
		working := o.Status == "pending" || o.Status == "partial"
		terminalFill := (o.Status == "filled" || o.Status == "cancelled" || o.Status == "rejected") &&
			(o.Status == "filled" || o.FilledQty > 0) &&
			(o.OrderType == "stop" || o.OrderType == "stop_limit" || o.OrderType == "limit")
		if working || terminalFill {
			out = append(out, o)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out, nil
}

func (f *fakeStore) ClaimTradeExit(_ context.Context, id string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	tr, ok := f.trades[id]
	if !ok || tr.Status != "open" {
		return false, nil
	}
	tr.Status = "exiting"
	f.trades[id] = tr
	return true, nil
}

func (f *fakeStore) ClaimTradeExitWithIntent(ctx context.Context, id string, intent store.ExitIntent) (bool, error) {
	claimed, err := f.ClaimTradeExit(ctx, id)
	if claimed {
		f.mu.Lock()
		tr := f.trades[id]
		tr.ExitReason, tr.ExitEvidence = intent.Reason, intent.Evidence
		f.trades[id] = tr
		f.mu.Unlock()
	}
	return claimed, err
}

func (f *fakeStore) ReleaseTradeExitClaim(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	tr, ok := f.trades[id]
	if ok && tr.Status == "exiting" {
		tr.Status = "open"
		f.trades[id] = tr
	}
	return nil
}

func (f *fakeStore) InsertOrder(_ context.Context, o store.Order) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.insertErr != nil {
		return "", f.insertErr
	}
	f.nextID++
	id := "order-" + itoa(f.nextID)
	o.ID = id
	f.orders[id] = o
	return id, nil
}

func (f *fakeStore) UpdateOrderFill(_ context.Context, orderID string, filledPrice, filledQty float64, status string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	o := f.orders[orderID]
	o.FilledPrice, o.FilledQty, o.Status = filledPrice, filledQty, status
	f.orders[orderID] = o
	return nil
}

func (f *fakeStore) ApplyPartialExit(
	_ context.Context,
	tradeID, orderID string,
	filledPrice, cumulativeFilledQty float64,
	status string,
) (float64, float64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	o := f.orders[orderID]
	applied := math.Max(0, cumulativeFilledQty-f.applied[orderID])
	f.applied[orderID] = math.Max(f.applied[orderID], cumulativeFilledQty)
	o.FilledPrice, o.FilledQty, o.Status =
		filledPrice, math.Max(o.FilledQty, cumulativeFilledQty), status
	f.orders[orderID] = o
	tr := f.trades[tradeID]
	tr.Quantity = math.Max(0, tr.Quantity-applied)
	f.trades[tradeID] = tr
	return tr.Quantity, applied, nil
}

func (f *fakeStore) ApplyPartialExitAt(
	ctx context.Context,
	tradeID, orderID string,
	filledPrice, cumulativeFilledQty float64,
	status string,
	filledAt time.Time,
) (float64, float64, error) {
	remaining, applied, err := f.ApplyPartialExit(
		ctx, tradeID, orderID, filledPrice, cumulativeFilledQty, status,
	)
	if err == nil && !filledAt.IsZero() {
		f.mu.Lock()
		o := f.orders[orderID]
		o.FilledAt = &filledAt
		f.orders[orderID] = o
		f.mu.Unlock()
	}
	return remaining, applied, err
}

func (f *fakeStore) UpdateExitingTradeQuantity(_ context.Context, id string, quantity float64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	tr := f.trades[id]
	tr.Quantity = quantity
	f.trades[id] = tr
	return nil
}

func (f *fakeStore) CloseTradeRealized(
	_ context.Context,
	id string,
	exitPrice float64,
) (store.TradeRealization, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.trades[id]
	if !ok {
		return store.TradeRealization{}, errors.New("trade not found")
	}
	pnl := (exitPrice - t.EntryPrice) * t.Quantity
	if t.Direction == "short" {
		pnl = -pnl
	}
	pnlPct := 0.0
	if t.EntryPrice > 0 && t.Quantity > 0 {
		pnlPct = pnl / (t.EntryPrice * t.Quantity) * 100
	}
	t.Status = "closed"
	t.ExitPrice = exitPrice
	t.PnL = pnl
	t.PnLPct = pnlPct
	t.ClosedAt = time.Now()
	f.trades[id] = t
	return store.TradeRealization{
		ExitPrice: exitPrice,
		PnL:       pnl,
		PnLPct:    pnlPct,
		ExitQty:   t.Quantity,
		Status:    "closed",
	}, nil
}

func (f *fakeStore) CloseTradeRealizedWithIntent(
	ctx context.Context, id string, exitPrice float64, filledAt time.Time, intent store.ExitIntent,
) (store.TradeRealization, error) {
	realized, err := f.CloseTradeRealized(ctx, id, exitPrice)
	if err != nil {
		return realized, err
	}
	f.mu.Lock()
	tr := f.trades[id]
	tr.ExitReason, tr.ExitEvidence = intent.Reason, intent.Evidence
	if !filledAt.IsZero() {
		tr.ExitFilledAt = &filledAt
	}
	f.trades[id] = tr
	f.mu.Unlock()
	realized.TradeID, realized.Ticker = tr.ID, tr.Ticker
	realized.Direction, realized.EntryPrice = tr.Direction, tr.EntryPrice
	realized.ExitReason, realized.ExitEvidence = tr.ExitReason, tr.ExitEvidence
	realized.ExitFilledAt, realized.JustClosed = tr.ExitFilledAt, true
	return realized, nil
}

func (f *fakeStore) UpdateOrderStopReplacement(
	_ context.Context,
	orderID, _, newBrokerOrderID string,
	_, _, newStopPrice, newQuantity float64,
	status string,
	filledPrice, filledQty float64,
	filledAt time.Time,
	intent store.ExitIntent,
) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.updateReplacementErr != nil {
		return f.updateReplacementErr
	}
	o, ok := f.orders[orderID]
	if !ok {
		return errors.New("order not found")
	}
	o.StopPrice = newStopPrice
	o.Quantity = newQuantity
	o.Status = status
	o.FilledPrice = filledPrice
	o.FilledQty = filledQty
	o.FilledAt = timePtr(filledAt)
	o.ExitReason, o.ExitEvidence = intent.Reason, intent.Evidence
	if newBrokerOrderID != "" {
		o.BrokerOrderID = newBrokerOrderID
	}
	f.orders[orderID] = o
	return nil
}

// fakePrices feeds canned candles to the manager.
type fakePrices struct {
	mu      sync.Mutex
	candles []data.Candle
	calls   int
	lastTF  data.Timeframe
}

func (f *fakePrices) FetchHistory(_ context.Context, _ string, tf data.Timeframe, _ int) ([]data.Candle, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.lastTF = tf
	out := make([]data.Candle, len(f.candles))
	copy(out, f.candles)
	return out, nil
}

// fakeBroker records replace/place calls and reports what the trailing-stop
// manager submitted.
type fakeBroker struct {
	mu             sync.Mutex
	placed         []execution.OrderRequest
	replaced       []replaceCall
	cancelled      []string
	supportReplace bool
	failReplace    bool
	replaceErr     error
	nextID         int
	// failCancel makes CancelOrder return an error — used to exercise the
	// duplicate-stop safety path (P1: cancel-error must NOT submit a new
	// stop, otherwise two live stops can over-exit/flip the position).
	failCancel bool
	// cancelledStatus drives what GetOrderStatus reports for cancelled
	// order IDs. Defaults to "cancelled" so happy-path cancel+resubmit
	// works; tests can set it to "pending" or "" to simulate broker
	// timeouts where the cancel didn't actually take.
	statusOverride map[string]string
	statusResults  map[string]execution.OrderResult
	replaceResult  *execution.OrderResult
	// notFoundIDs contains broker order ids GetOrderStatus must report as
	// ErrOrderNotFound — exercises the cancelAndConfirm sentinel path.
	notFoundIDs map[string]bool
	// nextFillPrice / nextFillQty are returned on the next PlaceOrder call
	// to simulate the broker echoing back the immediate fill (e.g. Alpaca
	// IOC market sells). Reset to zero after each call.
	nextFillPrice float64
	nextFillQty   float64
	nextStatus    string // optional override of "pending" on PlaceOrder
	placeResults  []execution.OrderResult
	placeErr      error
}

type replaceCall struct {
	BrokerOrderID string
	NewStopPrice  float64
}

func (b *fakeBroker) PlaceOrder(_ context.Context, req execution.OrderRequest) (execution.OrderResult, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.placed = append(b.placed, req)
	if b.placeErr != nil {
		return execution.OrderResult{}, b.placeErr
	}
	if len(b.placeResults) > 0 {
		res := b.placeResults[0]
		b.placeResults = b.placeResults[1:]
		if res.BrokerOrderID == "" {
			b.nextID++
			res.BrokerOrderID = "broker-" + itoa(b.nextID)
		}
		return res, nil
	}
	b.nextID++
	status := "pending"
	if b.nextStatus != "" {
		status = b.nextStatus
	}
	res := execution.OrderResult{
		BrokerOrderID: "broker-" + itoa(b.nextID),
		Status:        status,
		FilledPrice:   b.nextFillPrice,
		FilledQty:     b.nextFillQty,
	}
	// One-shot — caller resets per test.
	b.nextFillPrice = 0
	b.nextFillQty = 0
	b.nextStatus = ""
	return res, nil
}

func (b *fakeBroker) CancelOrder(_ context.Context, brokerOrderID string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.cancelled = append(b.cancelled, brokerOrderID)
	if b.failCancel {
		return errors.New("simulated broker cancel failure")
	}
	if b.statusOverride == nil {
		b.statusOverride = map[string]string{}
	}
	// Default: a successful cancel flips the broker-side status so the
	// safety check in cancelAndConfirm sees a terminal state.
	if _, ok := b.statusOverride[brokerOrderID]; !ok {
		b.statusOverride[brokerOrderID] = "cancelled"
	}
	return nil
}

func (b *fakeBroker) GetOrderStatus(_ context.Context, brokerOrderID string) (execution.OrderResult, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.notFoundIDs[brokerOrderID] {
		return execution.OrderResult{}, execution.ErrOrderNotFound
	}
	if result, ok := b.statusResults[brokerOrderID]; ok {
		return result, nil
	}
	if s, ok := b.statusOverride[brokerOrderID]; ok {
		return execution.OrderResult{Status: s}, nil
	}
	return execution.OrderResult{Status: "pending"}, nil
}

func (b *fakeBroker) GetPositions(_ context.Context) ([]execution.Position, error) {
	return nil, nil
}

func (b *fakeBroker) GetAccount(_ context.Context) (execution.AccountInfo, error) {
	return execution.AccountInfo{}, nil
}

// fakeBrokerWithReplace embeds fakeBroker and adds StopOrderReplacer support.
type fakeBrokerWithReplace struct{ *fakeBroker }

// durableFakeBrokerWithReplace stands in for the coordinator wrapper in
// manager tests. It makes post-acceptance projections require their intent.
type durableFakeBrokerWithReplace struct{ fakeBrokerWithReplace }

func (durableFakeBrokerWithReplace) DurableStockOrderIntents() bool { return true }

type durableMarketExitBroker struct{ *fakeBroker }

func (*durableMarketExitBroker) DurableStockOrderIntents() bool { return true }

func (b fakeBrokerWithReplace) ReplaceStopOrder(_ context.Context, brokerOrderID string, newStopPrice float64) (execution.OrderResult, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.replaceErr != nil {
		return execution.OrderResult{}, b.replaceErr
	}
	if b.failReplace {
		return execution.OrderResult{}, errors.New("simulated PATCH failure")
	}
	b.replaced = append(b.replaced, replaceCall{BrokerOrderID: brokerOrderID, NewStopPrice: newStopPrice})
	if b.replaceResult != nil {
		return *b.replaceResult, nil
	}
	b.nextID++
	return execution.OrderResult{
		BrokerOrderID: "broker-" + itoa(b.nextID),
		Status:        "pending",
	}, nil
}

func TestManager_AmbiguousReplacementDoesNotFallBack(t *testing.T) {
	fs := newFakeStore()
	fs.trades["t1"] = store.Trade{
		ID: "t1", Ticker: "AAPL", Direction: "long", Status: "open",
		EntryPrice: 100, StopLoss: 95, Quantity: 10, HighWaterMark: 100,
	}
	_, _ = fs.InsertOrder(context.Background(), store.Order{
		TradeID: "t1", BrokerOrderID: "ambiguous-old-stop", Ticker: "AAPL",
		Side: "sell", OrderType: "stop", Quantity: 10, StopPrice: 95,
		Status: "pending", IsStopLoss: true,
	})
	fp := &fakePrices{candles: makeFlatPriceCandles(100, candle(108, 110, 107, 110))}
	base := &fakeBroker{replaceErr: execution.ErrAmbiguousStopReplacement}
	m := NewManager(Config{Enabled: true, ATRMultiple: 1.5}, fs, fp, fakeBrokerWithReplace{fakeBroker: base})
	m.UpdateAll(context.Background())

	if len(base.cancelled) != 0 || len(base.placed) != 0 {
		t.Fatalf("ambiguous replacement must not fall back: cancelled=%v placed=%v", base.cancelled, base.placed)
	}
	stop, err := fs.LatestStopLossOrderForTrade(context.Background(), "t1")
	if err != nil || stop == nil || stop.BrokerOrderID != "ambiguous-old-stop" {
		t.Fatalf("ambiguous replacement mutated live stop: %#v, %v", stop, err)
	}
}

func itoa(n int) string {
	// Minimal int→string to avoid pulling fmt into the test harness paths.
	if n == 0 {
		return "0"
	}
	neg := false
	if n < 0 {
		neg = true
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// candle helper
func candle(o, h, l, c float64) data.Candle {
	return data.Candle{Open: o, High: h, Low: l, Close: c, Volume: 1000}
}

// makeFlatPriceCandles produces 30 flat candles around `last` with a small
// range so ATR is non-zero and stable. Then it appends the supplied tail to
// drive the manager.
func makeFlatPriceCandles(last float64, tail ...data.Candle) []data.Candle {
	out := make([]data.Candle, 0, 30+len(tail))
	for i := 0; i < 30; i++ {
		out = append(out, candle(last-0.5, last+0.5, last-0.5, last))
	}
	out = append(out, tail...)
	return out
}

// alwaysOpenSession pins the market calendar for tests that assert a market
// exit actually prints. The ATR rule defers its market exit outside regular
// hours (a DAY order submitted off-hours fills at the next open on stale
// data), so without an injected calendar these tests would pass or fail
// depending on the wall clock at which the suite happens to run.
func alwaysOpenSession(time.Time) bool { return true }

func dailyCloses(values ...float64) []data.Candle {
	out := make([]data.Candle, 0, len(values))
	for _, closePrice := range values {
		out = append(out, data.Candle{
			Timeframe: data.Timeframe1Day,
			Open:      closePrice,
			High:      closePrice,
			Low:       closePrice,
			Close:     closePrice,
		})
	}
	return out
}

func TestEMA9UsesBacktestSeedingConvention(t *testing.T) {
	candles := dailyCloses(1, 2, 3, 4, 5, 6, 7, 8, 9)
	if got := emaClose(candles, EMAPeriod); got != 5 {
		t.Fatalf("seed ema = %v, want SMA seed 5", got)
	}
	candles = append(candles, dailyCloses(10)...)
	// k=2/(9+1)=0.2 => 10*0.2 + 5*0.8 = 6
	if got := emaClose(candles, EMAPeriod); got != 6 {
		t.Fatalf("next ema = %v, want 6", got)
	}
}

func TestCompletedDailyCandlesDropsCurrentPartialSession(t *testing.T) {
	now := time.Date(2026, time.July, 24, 15, 30, 0, 0, newYorkLocation)
	previous := data.Candle{
		Timestamp: time.Date(2026, time.July, 23, 0, 0, 0, 0, newYorkLocation),
		Close:     100,
	}
	current := data.Candle{
		Timestamp: time.Date(2026, time.July, 24, 0, 0, 0, 0, newYorkLocation),
		Close:     90,
	}
	got := completedDailyCandles([]data.Candle{previous, current}, now)
	if len(got) != 1 || got[0].Close != 100 {
		t.Fatalf("completed candles = %+v, want only previous session", got)
	}

	afterClose := time.Date(2026, time.July, 24, 16, 5, 0, 0, newYorkLocation)
	got = completedDailyCandles([]data.Candle{previous, current}, afterClose)
	if len(got) != 2 {
		t.Fatalf("after-close candles = %d, want current session included", len(got))
	}
}

func TestManager_EMA9CloseAboveKeepsTradeAndSeedsHardStop(t *testing.T) {
	fs := newFakeStore()
	fs.trades["t1"] = store.Trade{
		ID: "t1", Ticker: "AAPL", Direction: "long", Status: "open",
		EntryPrice: 100, StopLoss: 95, Quantity: 10,
	}
	fp := &fakePrices{candles: dailyCloses(100, 100, 100, 100, 100, 100, 100, 100, 100, 110)}
	fb := &fakeBroker{}

	m := NewManager(Config{Enabled: true, Rule: RuleEMA9}, fs, fp, fb)
	m.now = func() time.Time {
		return time.Date(2026, time.July, 24, 10, 0, 0, 0, newYorkLocation)
	}
	m.UpdateAll(context.Background())

	if fp.lastTF != data.Timeframe1Day {
		t.Fatalf("timeframe = %s, want daily to match study", fp.lastTF)
	}
	if fs.trades["t1"].Status != "open" {
		t.Fatal("close above ema9 must keep trade open")
	}
	if len(fb.placed) != 1 {
		t.Fatalf("placed = %d, want one protective stop", len(fb.placed))
	}
	stop := fb.placed[0]
	if stop.Type != execution.OrderTypeStop || stop.StopPrice != 95 || stop.TimeInForce != "gtc" {
		t.Fatalf("protective order = %+v, want GTC stop at 95", stop)
	}
}

func TestManager_EMA9PreCloseIgnoresCurrentPartialDailyBar(t *testing.T) {
	fs := newFakeStore()
	fs.trades["t1"] = store.Trade{
		ID: "t1", Ticker: "AAPL", Direction: "long", Status: "open",
		EntryPrice: 100, StopLoss: 95, Quantity: 10,
	}

	// Nine completed closes at 100 establish EMA9=100. The current session's
	// partial bar is 50, but it must not trigger a close before 16:00 ET.
	candles := make([]data.Candle, 0, 10)
	for i := 0; i < 9; i++ {
		candles = append(candles, data.Candle{
			Timestamp: time.Date(2026, time.July, 13+i, 0, 0, 0, 0, newYorkLocation),
			Close:     100,
		})
	}
	candles = append(candles, data.Candle{
		Timestamp: time.Date(2026, time.July, 24, 0, 0, 0, 0, newYorkLocation),
		Close:     50,
	})
	fp := &fakePrices{candles: candles}
	fb := &durableMarketExitBroker{fakeBroker: &fakeBroker{}}

	m := NewManager(Config{Enabled: true, Rule: RuleEMA9}, fs, fp, fb)
	m.now = func() time.Time {
		return time.Date(2026, time.July, 24, 10, 0, 0, 0, newYorkLocation)
	}
	m.UpdateAll(context.Background())

	if fs.trades["t1"].Status != "open" {
		t.Fatal("current partial daily bar must not trigger an EMA9 exit")
	}
	if len(fb.placed) != 1 || fb.placed[0].Type != execution.OrderTypeStop {
		t.Fatalf("orders = %+v, want one protective stop", fb.placed)
	}
}

func TestManager_EMA9CappedTradeDoesNotSeedStandaloneStop(t *testing.T) {
	fs := newFakeStore()
	fs.trades["t1"] = store.Trade{
		ID: "t1", Ticker: "AAPL", Direction: "long", Status: "open",
		EntryPrice: 100, StopLoss: 95, TakeProfit: 110, Quantity: 10,
	}
	fp := &fakePrices{candles: dailyCloses(100, 100, 100, 100, 100, 100, 100, 100, 100, 105)}
	fb := &fakeBroker{}

	m := NewManager(Config{Enabled: true, Rule: RuleEMA9}, fs, fp, fb)
	m.now = func() time.Time {
		return time.Date(2026, time.July, 24, 10, 0, 0, 0, newYorkLocation)
	}
	m.UpdateAll(context.Background())

	if fs.trades["t1"].Status != "open" {
		t.Fatal("close above ema9 must keep capped trade open")
	}
	if len(fb.placed) != 0 {
		t.Fatalf("orders = %+v, want no standalone stop beside software take-profit", fb.placed)
	}
}

func TestManager_ATRCappedTradeDoesNotSeedStandaloneStop(t *testing.T) {
	fs := newFakeStore()
	fs.trades["t1"] = store.Trade{
		ID: "t1", Ticker: "AAPL", Direction: "long", Status: "open",
		EntryPrice: 100, StopLoss: 95, TakeProfit: 110, Quantity: 10,
	}
	fp := &fakePrices{candles: makeFlatPriceCandles(100, candle(100, 101, 99, 100))}
	fb := &fakeBroker{}

	m := NewManager(Config{Enabled: true, Rule: RuleATR}, fs, fp, fb)
	m.UpdateAll(context.Background())

	if len(fb.placed) != 0 {
		t.Fatalf("orders = %+v, want capped ATR trade to remain app-managed", fb.placed)
	}
}

func TestManager_EMA9SeedsHardStopBeforeRequiringHistory(t *testing.T) {
	fs := newFakeStore()
	fs.trades["t1"] = store.Trade{
		ID: "t1", Ticker: "AAPL", Direction: "long", Status: "open",
		EntryPrice: 100, StopLoss: 95, Quantity: 10,
	}
	// Fewer than nine completed daily candles makes EMA calculation fail.
	fp := &fakePrices{candles: dailyCloses(100, 101, 102)}
	fb := &fakeBroker{}
	fs.stockIntentIDs[trailingStopIntentKey("t1", nil, 95, 10)] = "durable-stop-intent"

	m := NewManager(Config{Enabled: true, Rule: RuleEMA9}, fs, fp, durableFakeBrokerWithReplace{fakeBrokerWithReplace{fakeBroker: fb}})
	m.now = func() time.Time {
		return time.Date(2026, time.July, 24, 10, 0, 0, 0, newYorkLocation)
	}
	m.UpdateAll(context.Background())

	if fs.trades["t1"].Status != "open" {
		t.Fatal("missing EMA history must leave the trade open")
	}
	if len(fb.placed) != 1 || fb.placed[0].Type != execution.OrderTypeStop {
		t.Fatalf("orders = %+v, want a protective stop despite missing EMA history", fb.placed)
	}
	if stop := fb.placed[0]; stop.StopPrice != 95 || stop.TimeInForce != "gtc" {
		t.Fatalf("protective order = %+v, want GTC stop at 95", stop)
	} else if stop.IntentKey == "" {
		t.Fatal("protective stop must carry a durable intent key")
	}
	for _, order := range fs.orders {
		if order.StockIntentID != "durable-stop-intent" {
			t.Fatalf("standalone trailing stop intent link = %q", order.StockIntentID)
		}
	}
}

func TestManager_EMA9ResizesHardStopWhenTradeQuantityGrows(t *testing.T) {
	fs := newFakeStore()
	fs.trades["t1"] = store.Trade{
		ID: "t1", Ticker: "AAPL", Direction: "long", Status: "open",
		EntryPrice: 100, StopLoss: 95, Quantity: 5,
	}
	_, _ = fs.InsertOrder(context.Background(), store.Order{
		TradeID: "t1", BrokerOrderID: "partial-stop", Ticker: "AAPL",
		Side: "sell", OrderType: "stop", Quantity: 2, StopPrice: 95,
		Status: "pending", IsStopLoss: true,
	})
	fp := &fakePrices{candles: dailyCloses(100, 100, 100, 100, 100, 100, 100, 100, 100)}
	fb := &durableMarketExitBroker{fakeBroker: &fakeBroker{}}
	// The fallback's newly submitted durable intent is resolved by the live
	// adapter after the broker returns its generated id. Pin that the mutable
	// stop row advances to this intent rather than remaining attached to its
	// cancelled predecessor.
	fs.replacementIntentIDs["broker-1"] = "resubmitted-durable-intent"
	m := NewManager(Config{Enabled: true, Rule: RuleEMA9}, fs, fp, fb)

	m.UpdateAll(context.Background())

	if len(fb.cancelled) != 1 || fb.cancelled[0] != "partial-stop" {
		t.Fatalf("cancelled = %v, want partial-stop", fb.cancelled)
	}
	if len(fb.placed) != 1 || fb.placed[0].Quantity != 5 {
		t.Fatalf("replacement stop = %+v, want quantity 5", fb.placed)
	}
	if fb.placed[0].IntentKey == "" {
		t.Fatal("cancel+resubmit stop must carry a durable intent key")
	}
	stop, err := fs.LatestStopLossOrderForTrade(context.Background(), "t1")
	if err != nil || stop == nil {
		t.Fatalf("LatestStopLossOrderForTrade: %v (nil=%v)", err, stop == nil)
	}
	if stop.Quantity != 5 {
		t.Fatalf("stored stop quantity = %v, want 5", stop.Quantity)
	}
	if stop.StockIntentID != "resubmitted-durable-intent" {
		t.Fatalf("cancel-resubmit stop intent = %q, want repointed durable intent", stop.StockIntentID)
	}
}

func TestManager_EMA9CloseBelowExitsWithoutIntradayStopRewrite(t *testing.T) {
	fs := newFakeStore()
	fs.trades["t1"] = store.Trade{
		ID: "t1", Ticker: "AAPL", Direction: "long", Status: "open",
		EntryPrice: 100, StopLoss: 95, Quantity: 10,
	}
	_, _ = fs.InsertOrder(context.Background(), store.Order{
		TradeID: "t1", BrokerOrderID: "hard-stop", Ticker: "AAPL",
		Side: "sell", OrderType: "stop", Quantity: 10, StopPrice: 95,
		Status: "pending", IsStopLoss: true,
	})
	// Nine closes at 100 seed EMA=100; the final 90 close produces EMA=98.
	fp := &fakePrices{candles: dailyCloses(100, 100, 100, 100, 100, 100, 100, 100, 100, 90)}
	fb := &fakeBroker{
		nextFillPrice: 90,
		nextFillQty:   10,
		nextStatus:    "filled",
	}
	losses := 0
	var closed events.TradeClosedPayload

	m := NewManager(Config{
		Enabled:       true,
		Rule:          RuleEMA9,
		RecordLoss:    func() { losses++ },
		OnTradeClosed: func(p events.TradeClosedPayload) { closed = p },
	}, fs, fp, fb)
	m.now = func() time.Time {
		return time.Date(2026, time.July, 24, 10, 0, 0, 0, newYorkLocation)
	}
	m.UpdateAll(context.Background())

	if fs.trades["t1"].Status != "closed" {
		t.Fatal("daily close below ema9 must close the trade")
	}
	if len(fb.placed) != 1 || fb.placed[0].Type != execution.OrderTypeMarket {
		t.Fatalf("placed = %+v, want one market exit after cancelling hard stop", fb.placed)
	}
	if fb.placed[0].ReferencePrice != 90 {
		t.Fatalf("EMA market reference price = %v, want close 90", fb.placed[0].ReferencePrice)
	}
	if got := fs.trades["t1"].ExitPrice; got != 90 {
		t.Fatalf("fallback exit price = %v, want triggering daily close 90", got)
	}
	if losses != 1 {
		t.Fatalf("loss cooldown callbacks = %d, want 1", losses)
	}
	if closed.Reason != store.ExitReasonEMA9Close {
		t.Fatalf("close notification reason = %q, want %q", closed.Reason, store.ExitReasonEMA9Close)
	}
	for _, order := range fs.orders {
		if order.OrderType == string(execution.OrderTypeMarket) && order.IsStopLoss {
			t.Fatalf("EMA market exit incorrectly marked as stop-loss: %+v", order)
		}
	}
}

func TestManager_EMA9AfterCloseKeepsHardStopUntilNextSession(t *testing.T) {
	fs := newFakeStore()
	fs.trades["t1"] = store.Trade{
		ID: "t1", Ticker: "AAPL", Direction: "long", Status: "open",
		EntryPrice: 100, StopLoss: 95, Quantity: 10,
	}
	fp := &fakePrices{candles: dailyCloses(100, 100, 100, 100, 100, 100, 100, 100, 100, 90)}
	fb := &fakeBroker{}

	m := NewManager(Config{Enabled: true, Rule: RuleEMA9}, fs, fp, fb)
	m.now = func() time.Time {
		return time.Date(2026, time.July, 24, 16, 5, 0, 0, newYorkLocation)
	}
	m.UpdateAll(context.Background())

	if fs.trades["t1"].Status != "open" {
		t.Fatal("after-close EMA signal must leave trade tracked overnight")
	}
	if len(fb.placed) != 1 || fb.placed[0].Type != execution.OrderTypeStop {
		t.Fatalf("after-close orders = %+v, want only the protective stop", fb.placed)
	}
	if len(fb.cancelled) != 0 {
		t.Fatalf("after-close stop was cancelled: %v", fb.cancelled)
	}

	// The same completed-close signal is retried during the next regular
	// session. Only then may the manager cancel the stop and market-exit.
	m.now = func() time.Time {
		return time.Date(2026, time.July, 27, 10, 0, 0, 0, newYorkLocation)
	}
	fb.nextFillPrice = 90
	fb.nextFillQty = 10
	fb.nextStatus = "filled"
	m.UpdateAll(context.Background())
	if fs.trades["t1"].Status != "closed" {
		t.Fatal("EMA signal must exit during the next regular session")
	}
	if len(fb.cancelled) != 1 {
		t.Fatalf("cancelled = %v, want protective stop cancelled at eligible exit", fb.cancelled)
	}
	if len(fb.placed) != 2 || fb.placed[1].Type != execution.OrderTypeMarket {
		t.Fatalf("orders = %+v, want stop followed by market exit", fb.placed)
	}
}

func TestManager_EMA9WaitsForConfirmedStopCancellation(t *testing.T) {
	fs := newFakeStore()
	fs.trades["t1"] = store.Trade{
		ID: "t1", Ticker: "AAPL", Direction: "long", Status: "open",
		EntryPrice: 100, StopLoss: 95, Quantity: 10,
	}
	_, _ = fs.InsertOrder(context.Background(), store.Order{
		TradeID: "t1", BrokerOrderID: "hard-stop", Ticker: "AAPL",
		Side: "sell", OrderType: "stop", Quantity: 10, StopPrice: 95,
		Status: "pending", IsStopLoss: true,
	})
	fp := &fakePrices{candles: dailyCloses(100, 100, 100, 100, 100, 100, 100, 100, 100, 90)}
	fb := &fakeBroker{statusOverride: map[string]string{"hard-stop": "pending"}}

	m := NewManager(Config{Enabled: true, Rule: RuleEMA9}, fs, fp, fb)
	m.now = func() time.Time {
		return time.Date(2026, time.July, 24, 10, 0, 0, 0, newYorkLocation)
	}
	m.UpdateAll(context.Background())

	if fs.trades["t1"].Status != "open" {
		t.Fatal("trade closed before hard-stop cancellation was confirmed")
	}
	if len(fb.placed) != 0 {
		t.Fatalf("orders = %+v, want no market exit while hard stop remains live", fb.placed)
	}
	if len(fb.cancelled) != 1 || fb.cancelled[0] != "hard-stop" {
		t.Fatalf("cancelled = %v, want attempted hard-stop cancellation", fb.cancelled)
	}
}

func TestManager_EMA9PendingExitRemainsDurablyExiting(t *testing.T) {
	fs := newFakeStore()
	fs.trades["t1"] = store.Trade{
		ID: "t1", Ticker: "AAPL", Direction: "long", Status: "open",
		EntryPrice: 100, StopLoss: 95, Quantity: 10,
	}
	_, _ = fs.InsertOrder(context.Background(), store.Order{
		TradeID: "t1", BrokerOrderID: "hard-stop", Ticker: "AAPL",
		Side: "sell", OrderType: "stop", Quantity: 10, StopPrice: 95,
		Status: "pending", IsStopLoss: true,
	})
	fp := &fakePrices{candles: dailyCloses(100, 100, 100, 100, 100, 100, 100, 100, 100, 90)}
	fb := &fakeBroker{nextStatus: "partially_filled", nextFillPrice: 99, nextFillQty: 4}
	losses := 0

	m := NewManager(Config{
		Enabled: true, Rule: RuleEMA9, RecordLoss: func() { losses++ },
	}, fs, fp, fb)
	m.now = func() time.Time {
		return time.Date(2026, time.July, 24, 10, 0, 0, 0, newYorkLocation)
	}
	m.UpdateAll(context.Background())

	if got := fs.trades["t1"].Status; got != "exiting" {
		t.Fatalf("trade status = %q, want exiting until full broker fill", got)
	}
	if losses != 0 {
		t.Fatalf("loss cooldown recorded from partial fill: %d", losses)
	}
	placed := len(fb.placed)
	m.UpdateAll(context.Background())
	if len(fb.placed) != placed {
		t.Fatal("pending EMA exit was submitted again")
	}
}

func TestExitAndCloseRetainsClaimAfterAmbiguousPlacementError(t *testing.T) {
	fs := newFakeStore()
	trade := store.Trade{
		ID: "t1", Ticker: "AAPL", Direction: "long", Status: "open",
		EntryPrice: 100, Quantity: 10,
	}
	fs.trades[trade.ID] = trade
	fb := &fakeBroker{placeErr: errors.New("response decode timed out")}
	m := NewManager(Config{Enabled: true}, fs, &fakePrices{}, fb)

	err := m.exitAndClose(context.Background(), trade, 90, store.ExitIntent{Reason: store.ExitReasonTrailingATR}, true)
	if err == nil {
		t.Fatal("ambiguous placement unexpectedly succeeded")
	}
	if got := fs.trades[trade.ID].Status; got != "exiting" {
		t.Fatalf("trade status = %q, want exiting for broker recovery", got)
	}
}

func TestExitAndClosePaperExecutorUsesReferencePrice(t *testing.T) {
	fs := newFakeStore()
	trade := store.Trade{
		ID: "paper-exit", Ticker: "AAPL", Direction: "long", Status: "open",
		EntryPrice: 100, Quantity: 10,
	}
	fs.trades[trade.ID] = trade
	m := NewManager(Config{Enabled: true}, fs, &fakePrices{}, execution.NewPaperExecutor())

	intent := store.ExitIntent{Reason: store.ExitReasonTrailingATR}
	if err := m.exitAndClose(context.Background(), trade, 90, intent, true); err != nil {
		t.Fatalf("paper market exit: %v", err)
	}
	got := fs.trades[trade.ID]
	if got.Status != "closed" || got.ExitPrice != 90 {
		t.Fatalf("trade = status %q exit %.2f, want closed at 90", got.Status, got.ExitPrice)
	}
}

func TestExitAndCloseDurableMarketExitLinksImmediateAndPendingProjections(t *testing.T) {
	for _, tc := range []struct {
		name             string
		status           string
		filledPrice, qty float64
		wantOrderStatus  string
		wantTradeStatus  string
	}{
		{"immediate fill", "filled", 91, 2, "filled", "closed"},
		{"pending", "pending", 0, 0, "pending", "exiting"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := newFakeStore()
			trade := store.Trade{ID: "t1", Ticker: "AAPL", Direction: "long", Status: "open", EntryPrice: 100, Quantity: 2}
			fs.trades[trade.ID] = trade
			base := &fakeBroker{nextStatus: tc.status, nextFillPrice: tc.filledPrice, nextFillQty: tc.qty}
			exec := durableFakeBrokerWithReplace{fakeBrokerWithReplace{fakeBroker: base}}
			key := "trailing-market-exit:t1:2.00000000"
			fs.stockIntentIDs[key] = "durable-market-exit"
			m := NewManager(Config{Enabled: true}, fs, &fakePrices{}, exec)
			if err := m.exitAndClose(context.Background(), trade, 91, store.ExitIntent{Reason: store.ExitReasonTrailingATR}, false); err != nil {
				t.Fatal(err)
			}
			var order store.Order
			for _, o := range fs.orders {
				order = o
			}
			if order.StockIntentID != "durable-market-exit" || order.Status != tc.wantOrderStatus {
				t.Fatalf("durable trailing order = %#v", order)
			}
			if got := fs.trades[trade.ID].Status; got != tc.wantTradeStatus {
				t.Fatalf("trade status=%q, want %q", got, tc.wantTradeStatus)
			}
		})
	}
}

func TestExitAndCloseDurableIntentLookupFailureWritesNoUnlinkedOrder(t *testing.T) {
	fs := newFakeStore()
	trade := store.Trade{ID: "t1", Ticker: "AAPL", Direction: "long", Status: "open", EntryPrice: 100, Quantity: 2}
	fs.trades[trade.ID] = trade
	fs.stockIntentErr = errors.New("intent store unavailable")
	base := &fakeBroker{nextStatus: "filled", nextFillPrice: 91, nextFillQty: 2}
	m := NewManager(Config{Enabled: true}, fs, &fakePrices{}, durableFakeBrokerWithReplace{fakeBrokerWithReplace{fakeBroker: base}})
	if err := m.exitAndClose(context.Background(), trade, 91, store.ExitIntent{Reason: store.ExitReasonTrailingATR}, false); err == nil {
		t.Fatal("expected durable intent lookup failure")
	}
	if len(fs.orders) != 0 {
		t.Fatalf("lookup failure wrote unlinked market order: %#v", fs.orders)
	}
	if got := fs.trades[trade.ID].Status; got != "exiting" {
		t.Fatalf("accepted exit must retain claim, got status %q", got)
	}
}

func TestManager_RollbackCancelledStopRotatesDurableGeneration(t *testing.T) {
	fs := newFakeStore()
	trade := store.Trade{ID: "t1", Ticker: "AAPL", Direction: "long", Status: "open", EntryPrice: 100, Quantity: 2, StopLoss: 90}
	fs.trades[trade.ID] = trade
	originalKey := trailingStopIntentKey(trade.ID, nil, 95, trade.Quantity)
	retryKey := trailingStopRetryIntentKey(originalKey, "rollback-cancelled")
	fs.stockIntentIDs[originalKey] = "cancelled-generation"
	fs.stockIntentIDs[retryKey] = "active-generation"
	fs.stockIntentRetryKeys[originalKey] = retryKey
	fs.insertErr = errors.New("simulated projection write failure")
	base := &fakeBroker{placeResults: []execution.OrderResult{
		{BrokerOrderID: "rollback-cancelled", Status: "accepted"},
		// Next identical tick: coordinator finds the old client id's
		// confirmed cancellation, then manager must submit retryKey.
		{BrokerOrderID: "rollback-cancelled", Status: "cancelled"},
		{BrokerOrderID: "fresh-live-stop", Status: "pending"},
	}}
	m := NewManager(Config{Enabled: true}, fs, &fakePrices{}, durableFakeBrokerWithReplace{fakeBrokerWithReplace{fakeBroker: base}})
	if err := m.submitOrReplaceStop(context.Background(), trade, 95, stopExitIntent(95)); err == nil {
		t.Fatal("first accepted stop should report projection failure after rollback")
	}
	if len(fs.orders) != 0 {
		t.Fatalf("failed first projection persisted an order: %#v", fs.orders)
	}
	fs.insertErr = nil
	if err := m.submitOrReplaceStop(context.Background(), trade, 95, stopExitIntent(95)); err != nil {
		t.Fatalf("fresh generation after rollback: %v", err)
	}
	if len(base.placed) != 3 {
		t.Fatalf("placements=%d, want accepted + cancelled lookup + fresh generation", len(base.placed))
	}
	if base.placed[1].IntentKey != originalKey || base.placed[2].IntentKey != retryKey {
		t.Fatalf("retry intent keys = %q then %q; want %q then %q", base.placed[1].IntentKey, base.placed[2].IntentKey, originalKey, retryKey)
	}
	if len(fs.orders) != 1 {
		t.Fatalf("active projections=%d, want one", len(fs.orders))
	}
	for _, order := range fs.orders {
		if order.BrokerOrderID != "fresh-live-stop" || order.StockIntentID != "active-generation" || order.Status != "pending" {
			t.Fatalf("active stop projection=%#v", order)
		}
	}
}

func TestExitAndCloseRetainsClaimWhenCancelledLegFlattensTrade(t *testing.T) {
	fs := newFakeStore()
	trade := store.Trade{
		ID: "t1", Ticker: "AAPL", Direction: "long", Status: "open",
		EntryPrice: 100, Quantity: 5,
	}
	fs.trades[trade.ID] = trade
	_, _ = fs.InsertOrder(context.Background(), store.Order{
		TradeID: trade.ID, BrokerOrderID: "hard-stop", Ticker: trade.Ticker,
		Side: "sell", OrderType: "stop", Quantity: 5,
		Status: "pending", IsStopLoss: true,
	})
	fb := &fakeBroker{statusResults: map[string]execution.OrderResult{
		"hard-stop": {
			Status: "cancelled", FilledPrice: 95, FilledQty: 5,
		},
	}}
	m := NewManager(Config{Enabled: true}, fs, &fakePrices{}, fb)

	err := m.exitAndClose(context.Background(), trade, 90, store.ExitIntent{Reason: store.ExitReasonTrailingATR}, true)
	if !errors.Is(err, errExitClaimMustRemain) {
		t.Fatalf("error = %v, want claim-retention sentinel", err)
	}
	got := fs.trades[trade.ID]
	if got.Status != "exiting" || got.Quantity != 0 {
		t.Fatalf("trade = status %q qty %v, want exiting/0 for reconciliation",
			got.Status, got.Quantity)
	}
	if len(fb.placed) != 0 {
		t.Fatalf("market sell placed after leg already flattened trade: %+v", fb.placed)
	}
}

func TestExitAndCloseRetainsClaimWhenLegReportsFilledDuringCancel(t *testing.T) {
	fs := newFakeStore()
	trade := store.Trade{
		ID: "t1", Ticker: "AAPL", Direction: "long", Status: "open",
		EntryPrice: 100, Quantity: 5,
	}
	fs.trades[trade.ID] = trade
	_, _ = fs.InsertOrder(context.Background(), store.Order{
		TradeID: trade.ID, BrokerOrderID: "hard-stop", Ticker: trade.Ticker,
		Side: "sell", OrderType: "stop", Quantity: 5,
		Status: "pending", IsStopLoss: true,
	})
	fb := &fakeBroker{statusResults: map[string]execution.OrderResult{
		"hard-stop": {
			Status: "filled", FilledPrice: 95, FilledQty: 5,
		},
	}}
	m := NewManager(Config{Enabled: true}, fs, &fakePrices{}, fb)

	err := m.exitAndClose(context.Background(), trade, 90, store.ExitIntent{Reason: store.ExitReasonTrailingATR}, true)
	if !errors.Is(err, errExitClaimMustRemain) {
		t.Fatalf("error = %v, want claim-retention sentinel", err)
	}
	if got := fs.trades[trade.ID].Status; got != "exiting" {
		t.Fatalf("trade status = %q, want exiting for fill reconciliation", got)
	}
	if len(fb.placed) != 0 {
		t.Fatalf("market sell placed after leg filled: %+v", fb.placed)
	}
}

func TestCancelAndConfirmRejectsStopFillRace(t *testing.T) {
	fb := &fakeBroker{statusOverride: map[string]string{"hard-stop": "filled"}}
	m := NewManager(Config{Enabled: true}, newFakeStore(), &fakePrices{}, fb)

	if err := m.cancelAndConfirm(context.Background(), "hard-stop", "gtc"); err == nil {
		t.Fatal("filled stop must abort cancel-and-resubmit")
	}
}

func TestCancelAndConfirmRejectsGTCDoneForDay(t *testing.T) {
	fb := &fakeBroker{statusOverride: map[string]string{"hard-stop": "done_for_day"}}
	m := NewManager(Config{Enabled: true}, newFakeStore(), &fakePrices{}, fb)

	if err := m.cancelAndConfirm(context.Background(), "hard-stop", "gtc"); err == nil {
		t.Fatal("GTC done-for-day stop must not be considered cancelled")
	}
}

func TestManager_MarketExitFailsClosedWhenGTCBracketLegReportsDoneForDayAfterCancel(t *testing.T) {
	fs := newFakeStore()
	trade := store.Trade{ID: "t1", Ticker: "AAPL", Direction: "long", Status: "open", EntryPrice: 100, StopLoss: 95, Quantity: 5}
	fs.trades[trade.ID] = trade
	if _, err := fs.InsertOrder(context.Background(), store.Order{
		TradeID: trade.ID, BrokerOrderID: "gtc-bracket-stop", Ticker: trade.Ticker,
		Side: "sell", OrderType: "stop", Quantity: 5, StopPrice: 95, Status: "pending",
		IsStopLoss: true, ParentOrderID: "bracket-parent",
	}); err != nil {
		t.Fatal(err)
	}
	fb := &fakeBroker{statusOverride: map[string]string{"gtc-bracket-stop": "done_for_day"}}
	m := NewManager(Config{Enabled: true}, fs, &fakePrices{}, fb)
	if err := m.exitAndClose(context.Background(), trade, 90, store.ExitIntent{Reason: store.ExitReasonTrailingATR}, true); err == nil {
		t.Fatal("market exit proceeded while GTC bracket stop remained live")
	}
	if len(fb.placed) != 0 {
		t.Fatalf("market exit submitted despite live GTC bracket stop: %+v", fb.placed)
	}
	if got := fs.trades[trade.ID].Status; got != "open" {
		t.Fatalf("trade status=%q, want open after an unconfirmed cancellation", got)
	}
}

func TestManager_EMA9HonorsInjectedMarketCalendar(t *testing.T) {
	fs := newFakeStore()
	fs.trades["t1"] = store.Trade{
		ID: "t1", Ticker: "AAPL", Direction: "long", Status: "open",
		EntryPrice: 100, StopLoss: 95, Quantity: 10,
	}
	_, _ = fs.InsertOrder(context.Background(), store.Order{
		TradeID: "t1", BrokerOrderID: "hard-stop", Ticker: "AAPL",
		Side: "sell", OrderType: "stop", Quantity: 10, StopPrice: 95,
		Status: "pending", IsStopLoss: true,
	})
	fp := &fakePrices{candles: dailyCloses(100, 100, 100, 100, 100, 100, 100, 100, 100, 90)}
	fb := &fakeBroker{
		nextFillPrice: 110,
		nextFillQty:   10,
		nextStatus:    "filled",
	}
	m := NewManager(Config{
		Enabled: true,
		Rule:    RuleEMA9,
		MarketOpen: func(time.Time) bool {
			return false // e.g. an exchange holiday during normal clock hours
		},
	}, fs, fp, fb)
	m.now = func() time.Time {
		return time.Date(2026, time.July, 3, 10, 0, 0, 0, newYorkLocation)
	}

	m.UpdateAll(context.Background())
	if fs.trades["t1"].Status != "open" {
		t.Fatal("closed-calendar EMA signal must remain tracked")
	}
	if len(fb.placed) != 0 || len(fb.cancelled) != 0 {
		t.Fatalf("closed-calendar manager touched broker orders: placed=%+v cancelled=%v", fb.placed, fb.cancelled)
	}
}

func TestManager_EMA9IgnoresClosePredatingTrade(t *testing.T) {
	fs := newFakeStore()
	fs.trades["t1"] = store.Trade{
		ID: "t1", Ticker: "AAPL", Direction: "long", Status: "open",
		EntryPrice: 100, StopLoss: 95, Quantity: 10,
		OpenedAt: time.Date(2026, time.July, 24, 9, 35, 0, 0, newYorkLocation),
	}
	candles := dailyCloses(100, 100, 100, 100, 100, 100, 100, 100, 100, 90)
	for i := range candles {
		candles[i].Timestamp = time.Date(2026, time.July, 14+i, 0, 0, 0, 0, newYorkLocation)
	}
	fp := &fakePrices{candles: candles}
	fb := &fakeBroker{}
	m := NewManager(Config{Enabled: true, Rule: RuleEMA9}, fs, fp, fb)
	m.now = func() time.Time {
		return time.Date(2026, time.July, 24, 10, 0, 0, 0, newYorkLocation)
	}

	for attempt := 0; attempt < 3 && len(fb.placed) < 3; attempt++ {
		m.UpdateAll(context.Background())
	}
	if fs.trades["t1"].Status != "open" {
		t.Fatal("pre-entry EMA close must not churn the new trade")
	}
	if len(fb.placed) != 1 || fb.placed[0].Type != execution.OrderTypeStop {
		t.Fatalf("orders = %+v, want only the protective stop", fb.placed)
	}
	if len(fb.cancelled) != 0 {
		t.Fatalf("pre-entry EMA close cancelled protection: %v", fb.cancelled)
	}
}

func TestProtectiveStopTIF(t *testing.T) {
	if got := protectiveStopTIF(10); got != "gtc" {
		t.Fatalf("whole-share TIF = %q, want gtc", got)
	}
	if got := protectiveStopTIF(10.5); got != "day" {
		t.Fatalf("fractional TIF = %q, want day", got)
	}
}

// TestManager_FirstTickSeedsHWM confirms that on the first observation the
// manager seeds high_water_mark from MAX(entry_price, current_price).
func TestManager_FirstTickSeedsHWM(t *testing.T) {
	fs := newFakeStore()
	fs.trades["t1"] = store.Trade{
		ID:         "t1",
		Ticker:     "AAPL",
		Direction:  "long",
		Status:     "open",
		EntryPrice: 100,
		StopLoss:   95,
		Quantity:   10,
	}

	fp := &fakePrices{candles: makeFlatPriceCandles(100, candle(100, 100.2, 99.8, 100.1))}
	fb := &fakeBroker{}

	m := NewManager(Config{Enabled: true, ATRMultiple: 1.5}, fs, fp, fb)
	m.UpdateAll(context.Background())

	got := fs.trades["t1"].HighWaterMark
	if got <= 0 {
		t.Fatalf("expected HWM seeded, got %v", got)
	}
	if got < 100 {
		t.Fatalf("HWM should be >= entry_price (100), got %v", got)
	}
}

func TestManager_ATRDoesNotSubmitZeroPriceStop(t *testing.T) {
	fs := newFakeStore()
	fs.trades["t1"] = store.Trade{
		ID: "t1", Ticker: "AAPL", Direction: "long", Status: "open",
		EntryPrice: 100, StopLoss: 0, Quantity: 10,
	}
	candles := make([]data.Candle, 30)
	for i := range candles {
		candles[i] = candle(100, 100, 100, 100)
	}
	fp := &fakePrices{candles: candles}
	fb := &fakeBroker{}
	m := NewManager(Config{Enabled: true, ATRMultiple: 1.5}, fs, fp, fb)

	m.UpdateAll(context.Background())

	if len(fb.placed) != 0 {
		t.Fatalf("zero-price ATR input placed broker orders: %+v", fb.placed)
	}
	if got := fs.trades["t1"].HighWaterMark; got != 100 {
		t.Fatalf("high-water mark = %v, want 100 preserved for retry", got)
	}
}

// TestManager_RatchetsStopOnNewHigh simulates price moving up and confirms
// the trailing stop ratchets up exactly once with the new band.
func TestManager_RatchetsStopOnNewHigh(t *testing.T) {
	fs := newFakeStore()
	fs.trades["t1"] = store.Trade{
		ID:            "t1",
		Ticker:        "AAPL",
		Direction:     "long",
		Status:        "open",
		EntryPrice:    100,
		StopLoss:      95,
		Quantity:      10,
		HighWaterMark: 100, // already seeded by a prior tick
	}

	// Tail prints a fresh high at 110, ATR≈1.0 from the flat lookback.
	tail := candle(108, 110, 107, 110)
	fp := &fakePrices{candles: makeFlatPriceCandles(100, tail)}
	fb := &fakeBroker{}

	m := NewManager(Config{Enabled: true, ATRMultiple: 1.5}, fs, fp, fb)
	m.UpdateAll(context.Background())

	updated := fs.trades["t1"]
	if updated.HighWaterMark < 110 {
		t.Fatalf("HWM did not ratchet: got %v", updated.HighWaterMark)
	}
	// Exact ATR value is path-dependent; assert the stop moved up vs initial 95.
	if updated.StopLoss <= 95 {
		t.Fatalf("stop did not ratchet: %v", updated.StopLoss)
	}
	if updated.StopLoss > 110 {
		t.Fatalf("stop above current price (110): %v", updated.StopLoss)
	}
	// Broker should have received exactly one place call (no prior order).
	if len(fb.placed) != 1 {
		t.Fatalf("expected 1 place call, got %d", len(fb.placed))
	}
	if fb.placed[0].Side != execution.OrderSideSell || fb.placed[0].Type != execution.OrderTypeStop {
		t.Fatalf("wrong side/type: %+v", fb.placed[0])
	}
}

// TestManager_DoesNotWidenOnDrop is the integration-test specified by the
// task DoD: simulate price moving up, then a 1× ATR drop; the broker MUST
// receive a stop submission on the way up and the stop must NOT recede on
// the drop.
func TestManager_DoesNotWidenOnDrop(t *testing.T) {
	fs := newFakeStore()
	fs.trades["t1"] = store.Trade{
		ID:            "t1",
		Ticker:        "AAPL",
		Direction:     "long",
		Status:        "open",
		EntryPrice:    100,
		StopLoss:      95,
		Quantity:      10,
		HighWaterMark: 100,
	}

	fb := &fakeBroker{}
	m := NewManager(Config{Enabled: true, ATRMultiple: 1.5}, fs, nil, fb)

	// Tick 1 — price prints fresh high at 110.
	m.prices = &fakePrices{candles: makeFlatPriceCandles(100, candle(108, 110, 107, 110))}
	m.UpdateAll(context.Background())
	stopAfterUp := fs.trades["t1"].StopLoss
	if stopAfterUp <= 95 {
		t.Fatalf("stop didn't ratchet up: got %v", stopAfterUp)
	}
	if len(fb.placed) != 1 {
		t.Fatalf("expected stop submission on the way up, got %d", len(fb.placed))
	}

	// Tick 2 — price falls back ~1×ATR (~1.0). Stop must not retreat.
	m.prices = &fakePrices{candles: makeFlatPriceCandles(100, candle(110, 110, 108.9, 109))}
	m.UpdateAll(context.Background())
	stopAfterDip := fs.trades["t1"].StopLoss
	if stopAfterDip < stopAfterUp {
		t.Fatalf("stop receded after dip: %v → %v", stopAfterUp, stopAfterDip)
	}
}

// TestManager_ReplacesViaPATCH verifies that when the broker implements
// StopOrderReplacer (Alpaca PATCH / IBKR modify), the manager prefers the
// replace path and updates the orders row with the new broker id.
func TestManager_ReplacesViaPATCH(t *testing.T) {
	fs := newFakeStore()
	fs.trades["t1"] = store.Trade{
		ID:            "t1",
		Ticker:        "AAPL",
		Direction:     "long",
		Status:        "open",
		EntryPrice:    100,
		StopLoss:      95,
		Quantity:      10,
		HighWaterMark: 100,
	}
	// Pre-existing stop order — the manager should REPLACE, not place.
	_, _ = fs.InsertOrder(context.Background(), store.Order{
		TradeID:       "t1",
		BrokerOrderID: "broker-pre",
		Ticker:        "AAPL",
		Side:          string(execution.OrderSideSell),
		OrderType:     string(execution.OrderTypeStop),
		Quantity:      10,
		StopPrice:     95,
		Status:        "pending",
		IsStopLoss:    true,
		StockIntentID: "predecessor-intent",
	})

	fp := &fakePrices{candles: makeFlatPriceCandles(100, candle(108, 110, 107, 110))}
	base := &fakeBroker{}
	fs.replacementIntentIDs["broker-1"] = "replacement-intent"
	fb := durableFakeBrokerWithReplace{fakeBrokerWithReplace{fakeBroker: base}}

	m := NewManager(Config{Enabled: true, ATRMultiple: 1.5}, fs, fp, fb)
	m.UpdateAll(context.Background())

	if len(base.replaced) != 1 {
		t.Fatalf("expected one PATCH/replace, got %d", len(base.replaced))
	}
	if len(base.placed) != 0 {
		t.Fatalf("expected no fresh PlaceOrder calls, got %d", len(base.placed))
	}
	if base.replaced[0].BrokerOrderID != "broker-pre" {
		t.Fatalf("replaced wrong order id: %v", base.replaced[0].BrokerOrderID)
	}
	// The stored order's broker id should be repointed to the replace's new id.
	for _, o := range fs.orders {
		if o.TradeID == "t1" && o.BrokerOrderID == "broker-pre" {
			t.Fatalf("orders row not repointed to new broker id: %+v", o)
		}
		if o.TradeID == "t1" {
			if o.StockIntentID != "replacement-intent" {
				t.Fatalf("replacement intent link = %q", o.StockIntentID)
			}
			if o.ExitReason != store.ExitReasonTrailingATR {
				t.Fatalf("replacement exit reason = %q, want trailing_atr", o.ExitReason)
			}
			var evidence map[string]any
			if err := json.Unmarshal(o.ExitEvidence, &evidence); err != nil || evidence["trailing_stop"] == nil {
				t.Fatalf("replacement evidence = %s, err=%v", o.ExitEvidence, err)
			}
		}
	}
}

func TestManager_PersistStopReplacementSkipsIntentLinkForLegacyExecutor(t *testing.T) {
	fs := newFakeStore()
	orderID, err := fs.InsertOrder(context.Background(), store.Order{
		TradeID: "t1", BrokerOrderID: "broker-pre", Ticker: "AAPL", Side: "sell",
		OrderType: "stop", Quantity: 10, StopPrice: 95, Status: "pending", IsStopLoss: true,
		StockIntentID: "legacy-predecessor",
	})
	if err != nil {
		t.Fatal(err)
	}
	fs.replacementIntentIDs["broker-new"] = "must-not-be-linked"
	m := NewManager(Config{}, fs, &fakePrices{}, &fakeBroker{})
	if err := m.persistStopReplacement(context.Background(), &store.Order{
		ID: orderID, BrokerOrderID: "broker-pre", Quantity: 10,
	}, "broker-new", 10, 98, 10, "pending", 0, 0, time.Time{}, store.ExitIntent{}); err != nil {
		t.Fatal(err)
	}
	for _, order := range fs.orders {
		if order.ID != orderID {
			continue
		}
		if order.BrokerOrderID != "broker-new" {
			t.Fatalf("legacy replacement broker id=%q, want broker-new", order.BrokerOrderID)
		}
		if order.StockIntentID != "legacy-predecessor" {
			t.Fatalf("legacy replacement linked durable intent %q", order.StockIntentID)
		}
	}
}

// TestManager_FallsBackToCancelResubmit covers the Alpaca/IBKR-broker-but-
// PATCH-failed path: the manager logs the warn and falls back to cancel +
// resubmit. The fake replace is wired to fail so we exercise the branch.
func TestManager_FallsBackToCancelResubmit(t *testing.T) {
	fs := newFakeStore()
	fs.trades["t1"] = store.Trade{
		ID:            "t1",
		Ticker:        "AAPL",
		Direction:     "long",
		Status:        "open",
		EntryPrice:    100,
		StopLoss:      95,
		Quantity:      10,
		HighWaterMark: 100,
	}
	_, _ = fs.InsertOrder(context.Background(), store.Order{
		TradeID:       "t1",
		BrokerOrderID: "broker-pre",
		Ticker:        "AAPL",
		Side:          string(execution.OrderSideSell),
		OrderType:     string(execution.OrderTypeStop),
		Quantity:      10,
		StopPrice:     95,
		Status:        "pending",
		IsStopLoss:    true,
	})

	fp := &fakePrices{candles: makeFlatPriceCandles(100, candle(108, 110, 107, 110))}
	base := &fakeBroker{failReplace: true}
	fb := fakeBrokerWithReplace{fakeBroker: base}

	m := NewManager(Config{Enabled: true, ATRMultiple: 1.5}, fs, fp, fb)
	m.UpdateAll(context.Background())

	if len(base.cancelled) != 1 || base.cancelled[0] != "broker-pre" {
		t.Fatalf("expected cancel of stale stop, got %v", base.cancelled)
	}
	if len(base.placed) != 1 {
		t.Fatalf("expected fresh place after cancel, got %d", len(base.placed))
	}
}

// TestManager_SkipsForexORB confirms scope guard: forex and ORB strategies
// must not be touched by the trailing-stop manager.
func TestManager_SkipsForexORB(t *testing.T) {
	fs := newFakeStore()
	fs.trades["t1"] = store.Trade{
		ID: "t1", Ticker: "AAPL", Direction: "long", Status: "open",
		Strategy: "ORB", EntryPrice: 100, StopLoss: 95, Quantity: 10,
	}
	fs.trades["t2"] = store.Trade{
		ID: "t2", Ticker: "EURUSD", Direction: "long", Status: "open",
		Strategy: "forex_orb", EntryPrice: 1.10, StopLoss: 1.09, Quantity: 1000,
	}
	fp := &fakePrices{candles: makeFlatPriceCandles(100, candle(108, 110, 107, 110))}
	fb := &fakeBroker{}
	m := NewManager(Config{Enabled: true, ATRMultiple: 1.5}, fs, fp, fb)
	m.UpdateAll(context.Background())
	if len(fb.placed) != 0 || len(fb.cancelled) != 0 {
		t.Fatalf("manager touched ORB/forex trade: placed=%d cancelled=%d", len(fb.placed), len(fb.cancelled))
	}
}

// TestManager_CancelErrorDoesNotSubmitDuplicateStop is the P1 regression
// test for the duplicate-stop bug: when the cancel+resubmit fallback path
// runs and CancelOrder returns an error, the manager MUST NOT place a new
// stop. Two live stops on the same long can over-exit on a price drop and
// flip the account short. The fix only places after cancellation is
// confirmed via GetOrderStatus.
func TestManager_CancelErrorDoesNotSubmitDuplicateStop(t *testing.T) {
	fs := newFakeStore()
	fs.trades["t1"] = store.Trade{
		ID:            "t1",
		Ticker:        "AAPL",
		Direction:     "long",
		Status:        "open",
		EntryPrice:    100,
		StopLoss:      95,
		Quantity:      10,
		HighWaterMark: 100,
	}
	_, _ = fs.InsertOrder(context.Background(), store.Order{
		TradeID:       "t1",
		BrokerOrderID: "broker-pre",
		Ticker:        "AAPL",
		Side:          string(execution.OrderSideSell),
		OrderType:     string(execution.OrderTypeStop),
		Quantity:      10,
		StopPrice:     95,
		Status:        "pending",
		IsStopLoss:    true,
	})

	fp := &fakePrices{candles: makeFlatPriceCandles(100, candle(108, 110, 107, 110))}
	// failReplace forces the cancel+resubmit fallback; failCancel makes
	// the broker reject the cancel; status override leaves the order
	// "pending" so cancellation is unconfirmed.
	base := &fakeBroker{
		failReplace: true,
		failCancel:  true,
		statusOverride: map[string]string{
			"broker-pre": "pending",
		},
	}
	fb := fakeBrokerWithReplace{fakeBroker: base}

	m := NewManager(Config{Enabled: true, ATRMultiple: 1.5}, fs, fp, fb)
	m.UpdateAll(context.Background())

	if len(base.placed) != 0 {
		t.Fatalf("P1 BUG REGRESSED: cancel failed but new stop was submitted (placed=%d)", len(base.placed))
	}
	// The original stop row must remain intact at the old broker id and
	// price — we never touched it.
	for _, o := range fs.orders {
		if o.BrokerOrderID != "broker-pre" {
			t.Fatalf("stale stop replaced after cancel error: %+v", o)
		}
		if o.StopPrice != 95 {
			t.Fatalf("stale stop's price mutated despite cancel error: %v", o.StopPrice)
		}
	}
}

// TestManager_StopAtOrAbovePriceForcesMarketExit covers the edge case where
// the trailing band has crossed the current price. Leaving a stale broker
// stop sitting at the old level would mean the trail never actually fires;
// instead the manager submits a market sell so the exit prints immediately.
func TestManager_StopAtOrAbovePriceForcesMarketExit(t *testing.T) {
	fs := newFakeStore()
	fs.trades["t1"] = store.Trade{
		ID:            "t1",
		Ticker:        "AAPL",
		Direction:     "long",
		Status:        "open",
		EntryPrice:    100,
		StopLoss:      109, // already very tight
		Quantity:      10,
		HighWaterMark: 110,
	}
	// Pre-existing stop so we exercise the cancel-before-market-exit path.
	_, _ = fs.InsertOrder(context.Background(), store.Order{
		TradeID:       "t1",
		BrokerOrderID: "broker-pre",
		Ticker:        "AAPL",
		Side:          string(execution.OrderSideSell),
		OrderType:     string(execution.OrderTypeStop),
		Quantity:      10,
		StopPrice:     109,
		Status:        "pending",
		IsStopLoss:    true,
	})

	// HWM=200 (well above price), small ATR, modest mult → candidate stop
	// of 200 - 0.1*1 = 199.9, well above current price 110. The trail's
	// implication is "we should already be out", so the manager must
	// market-exit instead of leaving the stale 109 stop sitting there.
	fs.trades["t1"] = func() store.Trade {
		t := fs.trades["t1"]
		t.HighWaterMark = 200
		return t
	}()
	fp := &fakePrices{candles: makeFlatPriceCandles(110, candle(110, 110.5, 109.5, 110))}
	fb := &fakeBroker{
		nextFillPrice: 110,
		nextFillQty:   10,
		nextStatus:    "filled",
	}

	m := NewManager(Config{Enabled: true, ATRMultiple: 0.1, MarketOpen: alwaysOpenSession}, fs, fp, fb)
	m.UpdateAll(context.Background())

	if len(fb.placed) != 1 {
		t.Fatalf("expected 1 market sell, got %d", len(fb.placed))
	}
	if fb.placed[0].Type != execution.OrderTypeMarket {
		t.Fatalf("expected market order, got %v", fb.placed[0].Type)
	}
	if fb.placed[0].Side != execution.OrderSideSell {
		t.Fatalf("expected SELL side for long exit, got %v", fb.placed[0].Side)
	}
	if fb.placed[0].ReferencePrice != 110 {
		t.Fatalf("ATR market reference price = %v, want current price 110", fb.placed[0].ReferencePrice)
	}
	if len(fb.cancelled) == 0 {
		t.Fatalf("expected stale stop to be cancelled before market exit")
	}

	// After a successful market exit the trade row MUST be flipped to
	// status='closed' with exit_price + pnl set. Without this the trade
	// stays open, gets re-selected by GetOpenTrades on the next tick, and
	// the manager places a duplicate market sell.
	closed := fs.trades["t1"]
	if closed.Status != "closed" {
		t.Fatalf("trade not closed after market exit: status=%q", closed.Status)
	}
	if closed.ExitPrice == 0 {
		t.Fatalf("exit_price not persisted after market exit")
	}

	// Smoke test: a second UpdateAll tick must NOT emit another market sell
	// because the closed trade is no longer "open". Without the close in
	// the production path this would loop forever.
	placedBefore := len(fb.placed)
	m.UpdateAll(context.Background())
	if len(fb.placed) != placedBefore {
		t.Fatalf("duplicate market exit on second tick: placed %d → %d",
			placedBefore, len(fb.placed))
	}
}

// A restart must not turn the immediately fetched, potentially stale candle
// into a market sell. Run's private startup sweep leaves the existing HWM and
// broker stop untouched, then the first ordinary tick re-fetches the inputs
// and applies the crossed-band exit normally.
func TestManager_RunDefersCrossedATRExitUntilNextNormalTick(t *testing.T) {
	fs := newFakeStore()
	fs.trades["t1"] = store.Trade{
		ID: "t1", Ticker: "AAPL", Direction: "long", Status: "open",
		EntryPrice: 100, StopLoss: 109, Quantity: 10, HighWaterMark: 200,
	}
	_, _ = fs.InsertOrder(context.Background(), store.Order{
		TradeID: "t1", BrokerOrderID: "broker-pre", Ticker: "AAPL",
		Side:      string(execution.OrderSideSell),
		OrderType: string(execution.OrderTypeStop),
		Quantity:  10, StopPrice: 109, Status: "pending", IsStopLoss: true,
	})
	fp := &fakePrices{candles: makeFlatPriceCandles(110, candle(110, 110.5, 109.5, 110))}
	fb := &fakeBroker{nextFillPrice: 110, nextFillQty: 10, nextStatus: "filled"}
	m := NewManager(Config{
		Enabled: true, ATRMultiple: 0.1, Interval: time.Hour,
		MarketOpen: alwaysOpenSession,
	}, fs, fp, fb)

	// A pre-cancelled context makes Run return after its synchronous startup
	// sweep, without waiting for an interval tick.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	m.Run(ctx)

	if got := fs.trades["t1"]; got.Status != "open" || got.HighWaterMark != 200 || got.StopLoss != 109 {
		t.Fatalf("startup sweep mutated trade = %+v, want open with HWM 200 and stop 109", got)
	}
	if len(fb.placed) != 0 {
		t.Fatalf("startup sweep placed orders: %+v", fb.placed)
	}
	if len(fb.cancelled) != 0 {
		t.Fatalf("startup sweep cancelled protective stop: %v", fb.cancelled)
	}
	leg, err := fs.LatestStopLossOrderForTrade(context.Background(), "t1")
	if err != nil || leg == nil || leg.Status != "pending" || leg.StopPrice != 109 {
		t.Fatalf("startup protection changed: leg=%+v err=%v", leg, err)
	}

	// An ordinary timer tick with the exact same provider observation is still
	// stale and must not release the startup deferral.
	m.UpdateAll(context.Background())
	if got := fs.trades["t1"].Status; got != "open" {
		t.Fatalf("identical observation status = %q, want startup deferral to remain open", got)
	}
	if len(fb.placed) != 0 {
		t.Fatalf("identical observation placed orders: %+v", fb.placed)
	}

	// Once price/ATR evidence advances, the crossed band is revalidated and may
	// flatten normally even though it is still the same hourly bar.
	fp.candles = makeFlatPriceCandles(109, candle(109, 109.5, 108.5, 109))
	m.UpdateAll(context.Background())
	if got := fs.trades["t1"].Status; got != "closed" {
		t.Fatalf("fresh normal tick status = %q, want crossed trail to close trade", got)
	}
	if len(fb.placed) != 1 || fb.placed[0].Type != execution.OrderTypeMarket {
		t.Fatalf("normal tick orders = %+v, want exactly one market exit", fb.placed)
	}
}

// Startup deferral shares the off-hours safety invariant: if an uncapped
// position has lost its broker leg, restore only the original hard stop. The
// crossed ATR candidate must never be installed as a stop at/above market.
func TestManager_StartupDeferralSeedsMissingOriginalStop(t *testing.T) {
	fs := newFakeStore()
	fs.trades["t1"] = store.Trade{
		ID: "t1", Ticker: "AAPL", Direction: "long", Status: "open",
		EntryPrice: 100, StopLoss: 95, Quantity: 10, HighWaterMark: 200,
	}
	fp := &fakePrices{candles: makeFlatPriceCandles(110, candle(110, 110.5, 109.5, 110))}
	fb := &fakeBroker{}
	m := NewManager(Config{
		Enabled: true, ATRMultiple: 0.1, Interval: time.Hour,
		MarketOpen: alwaysOpenSession,
	}, fs, fp, fb)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	m.Run(ctx)

	if got := fs.trades["t1"].Status; got != "open" {
		t.Fatalf("startup sweep status = %q, want open", got)
	}
	if len(fb.placed) != 1 || fb.placed[0].Type != execution.OrderTypeStop || fb.placed[0].StopPrice != 95 {
		t.Fatalf("startup orders = %+v, want original protective stop at 95", fb.placed)
	}
	if got := fs.trades["t1"].StopLoss; got != 95 {
		t.Fatalf("persisted stop = %v, want original hard stop 95", got)
	}
}

// A transient broker failure during the startup sweep must not erase the
// RequireFresh gate. The next ordinary tick may retry protection, but it must
// not market-exit from the exact same observation that startup rejected.
func TestManager_StartupDeferralSurvivesProtectiveStopFailure(t *testing.T) {
	fs := newFakeStore()
	fs.trades["t1"] = store.Trade{
		ID: "t1", Ticker: "AAPL", Direction: "long", Status: "open",
		EntryPrice: 100, StopLoss: 95, Quantity: 10, HighWaterMark: 200,
	}
	fp := &fakePrices{candles: makeFlatPriceCandles(110, candle(110, 110.5, 109.5, 110))}
	fb := &fakeBroker{placeErr: errors.New("temporary broker failure")}
	m := NewManager(Config{
		Enabled: true, ATRMultiple: 0.1, Interval: time.Hour,
		MarketOpen: alwaysOpenSession,
	}, fs, fp, fb)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	m.Run(ctx)

	if got := fs.trades["t1"].Status; got != "open" {
		t.Fatalf("startup sweep status = %q, want open", got)
	}
	if _, ok := m.deferredATRExit["t1"]; !ok {
		t.Fatal("startup broker failure lost ATR exit deferral")
	}

	// Let the protection retry succeed, but leave the provider observation
	// unchanged. This tick must place only the original stop, never a market
	// sell derived from the stale startup candle.
	fb.placeErr = nil
	m.UpdateAll(context.Background())
	if got := fs.trades["t1"].Status; got != "open" {
		t.Fatalf("identical observation status = %q, want startup deferral to remain open", got)
	}
	if len(fb.placed) != 2 {
		t.Fatalf("placement attempts = %+v, want failed startup seed plus successful retry", fb.placed)
	}
	for _, order := range fb.placed {
		if order.Type != execution.OrderTypeStop || order.StopPrice != 95 {
			t.Fatalf("placement attempts = %+v, want only original protective stops", fb.placed)
		}
	}
}

// The restart safety gate applies only to a crossed-band market exit. A normal
// startup sweep must still ratchet broker protection immediately; otherwise
// every restart creates an avoidable full-interval window at the stale stop.
func TestManager_StartupSweepStillRatchetsNonCrossedStop(t *testing.T) {
	fs := newFakeStore()
	fs.trades["t1"] = store.Trade{
		ID: "t1", Ticker: "AAPL", Direction: "long", Status: "open",
		EntryPrice: 100, StopLoss: 95, Quantity: 10, HighWaterMark: 110,
	}
	_, _ = fs.InsertOrder(context.Background(), store.Order{
		TradeID: "t1", BrokerOrderID: "broker-old", Ticker: "AAPL",
		Side:      string(execution.OrderSideSell),
		OrderType: string(execution.OrderTypeStop),
		Quantity:  10, StopPrice: 95, Status: "pending", IsStopLoss: true,
	})
	fp := &fakePrices{candles: makeFlatPriceCandles(110, candle(110, 110.5, 109.5, 110))}
	fb := &fakeBroker{}
	m := NewManager(Config{
		Enabled: true, ATRMultiple: 2, Interval: time.Hour,
		MarketOpen: alwaysOpenSession,
	}, fs, fp, fb)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	m.Run(ctx)

	got := fs.trades["t1"]
	if got.Status != "open" || got.StopLoss <= 95 || got.StopLoss >= 110 {
		t.Fatalf("startup sweep trade = %+v, want open with ratcheted stop between 95 and market", got)
	}
	if len(fb.cancelled) != 1 || fb.cancelled[0] != "broker-old" {
		t.Fatalf("cancelled = %v, want old broker stop replaced", fb.cancelled)
	}
	if len(fb.placed) != 1 || fb.placed[0].Type != execution.OrderTypeStop || fb.placed[0].StopPrice != got.StopLoss {
		t.Fatalf("startup orders = %+v, want one replacement stop at %v", fb.placed, got.StopLoss)
	}
}

// TestManager_ATRDefersMarketExitOutsideRegularSession is the ATR-rule
// counterpart to TestManager_EMA9HonorsInjectedMarketCalendar.
//
// The gate only became load-bearing once the Alpaca 1h history fix landed:
// before it, an off-hours FetchHistory returned zero bars (Alpaca's bars
// endpoint defaults to the current session), so priceAndATR errored and the
// ATR rule silently no-op'd overnight. With a real 30-day window the rule now
// succeeds at 2am on the prior session's close, and an ungated
// `newStop >= currentPrice` would submit a DAY market sell that queues until
// the next open and fills on a decision never re-validated against the new
// session. The resting broker stop must be left to do that job instead.
func TestManager_ATRDefersMarketExitOutsideRegularSession(t *testing.T) {
	fs := newFakeStore()
	fs.trades["t1"] = store.Trade{
		ID: "t1", Ticker: "AAPL", Direction: "long", Status: "open",
		EntryPrice: 100, StopLoss: 109, Quantity: 10, HighWaterMark: 200,
	}
	_, _ = fs.InsertOrder(context.Background(), store.Order{
		TradeID: "t1", BrokerOrderID: "broker-pre", Ticker: "AAPL",
		Side:      string(execution.OrderSideSell),
		OrderType: string(execution.OrderTypeStop),
		Quantity:  10, StopPrice: 109, Status: "pending", IsStopLoss: true,
	})
	// HWM 200 against price 110 with a 0.1 multiple puts the trail far above
	// the market — the exact condition that forces a market exit in session.
	fp := &fakePrices{candles: makeFlatPriceCandles(110, candle(110, 110.5, 109.5, 110))}
	fb := &fakeBroker{nextFillPrice: 110, nextFillQty: 10, nextStatus: "filled"}

	m := NewManager(Config{
		Enabled:     true,
		ATRMultiple: 0.1,
		MarketOpen:  func(time.Time) bool { return false },
	}, fs, fp, fb)
	m.now = func() time.Time {
		return time.Date(2026, time.July, 24, 22, 15, 0, 0, newYorkLocation)
	}

	m.UpdateAll(context.Background())

	if got := fs.trades["t1"].Status; got != "open" {
		t.Fatalf("off-hours trail exit closed the trade: status=%q, want open", got)
	}
	for _, p := range fb.placed {
		if p.Type == execution.OrderTypeMarket {
			t.Fatalf("off-hours manager submitted a market order: %+v", p)
		}
	}
	if len(fb.cancelled) != 0 {
		t.Fatalf("off-hours manager cancelled the protective stop: %v", fb.cancelled)
	}
	// The overnight protection must survive untouched.
	leg, err := fs.LatestStopLossOrderForTrade(context.Background(), "t1")
	if err != nil || leg == nil {
		t.Fatalf("protective stop missing after off-hours tick: leg=%+v err=%v", leg, err)
	}
	if leg.Status != "pending" || leg.StopPrice != 109 {
		t.Fatalf("protective stop mutated off-hours: %+v", leg)
	}

	// Reopening the session must let the same condition fire immediately —
	// the deferral is a delay, not a cancellation.
	m.marketOpen = func(time.Time) bool { return true }
	m.UpdateAll(context.Background())

	if got := fs.trades["t1"].Status; got != "closed" {
		t.Fatalf("deferred trail exit did not fire at the open: status=%q", got)
	}
	var markets int
	for _, p := range fb.placed {
		if p.Type == execution.OrderTypeMarket {
			markets++
		}
	}
	if markets != 1 {
		t.Fatalf("market exits after reopen = %d, want exactly 1", markets)
	}
}

// TestManager_ATRDeferralStillSeedsMissingProtectiveStop closes the hole the
// off-hours gate would otherwise open. Uncapped entries are deliberately not
// brackets, so the manager owns their only protection. If the trail has crossed
// price overnight AND the leg is missing, returning early would leave the
// position naked until the next session — the deferral must seed the original
// hard stop first. The trail candidate itself is unusable here: a stop at or
// above the market fills or is rejected on sight.
func TestManager_ATRDeferralStillSeedsMissingProtectiveStop(t *testing.T) {
	newManager := func(takeProfit float64) (*fakeStore, *fakeBroker, *Manager) {
		fs := newFakeStore()
		fs.trades["t1"] = store.Trade{
			ID: "t1", Ticker: "AAPL", Direction: "long", Status: "open",
			EntryPrice: 100, StopLoss: 95, Quantity: 10,
			TakeProfit: takeProfit, HighWaterMark: 200,
		}
		// No protective leg exists at the broker.
		fp := &fakePrices{candles: makeFlatPriceCandles(110, candle(110, 110.5, 109.5, 110))}
		fb := &fakeBroker{}
		m := NewManager(Config{
			Enabled:     true,
			ATRMultiple: 0.1,
			MarketOpen:  func(time.Time) bool { return false },
		}, fs, fp, fb)
		m.now = func() time.Time {
			return time.Date(2026, time.July, 24, 22, 15, 0, 0, newYorkLocation)
		}
		return fs, fb, m
	}

	t.Run("uncapped trade is protected before deferring", func(t *testing.T) {
		fs, fb, m := newManager(0)
		m.UpdateAll(context.Background())

		if len(fb.placed) != 1 {
			t.Fatalf("orders placed = %+v, want exactly the protective stop", fb.placed)
		}
		if fb.placed[0].Type != execution.OrderTypeStop {
			t.Fatalf("placed %v off-hours, want a STOP not a market exit", fb.placed[0].Type)
		}
		// Seeded at the original hard stop, never at the crossed trail
		// candidate (which sits at 199.9, far above the 110 market).
		if fb.placed[0].StopPrice != 95 {
			t.Fatalf("seeded stop = %v, want the original hard stop 95", fb.placed[0].StopPrice)
		}
		if got := fs.trades["t1"].Status; got != "open" {
			t.Fatalf("trade status = %q, want open (exit deferred)", got)
		}
	})

	t.Run("capped trade keeps a single exit owner", func(t *testing.T) {
		_, fb, m := newManager(130)
		m.UpdateAll(context.Background())

		if len(fb.placed) != 0 {
			t.Fatalf("capped trade got a standalone stop off-hours: %+v", fb.placed)
		}
	})
}

func TestManager_NewProtectiveStopImmediateFillTransfersOwnership(t *testing.T) {
	fs := newFakeStore()
	trade := store.Trade{
		ID: "t1", Ticker: "AAPL", Direction: "long", Status: "open",
		EntryPrice: 100, StopLoss: 95, Quantity: 10, TakeProfit: 0,
		HighWaterMark: 100,
	}
	fs.trades[trade.ID] = trade
	fp := &fakePrices{candles: makeFlatPriceCandles(
		110, candle(109, 110, 108, 110),
	)}
	fb := &fakeBroker{
		nextStatus: "filled", nextFillPrice: 108.5, nextFillQty: 10,
	}
	m := NewManager(Config{Enabled: true, ATRMultiple: 1.5}, fs, fp, fb)

	err := m.updateOne(context.Background(), trade, false)
	if !errors.Is(err, errStopFilledDuringRollback) {
		t.Fatalf("error = %v, want immediate-fill sentinel", err)
	}
	if got := fs.trades[trade.ID].Status; got != "exiting" {
		t.Fatalf("trade status = %q, want exiting for reconciliation", got)
	}
	var found bool
	for _, order := range fs.orders {
		if order.BrokerOrderID == "broker-1" {
			found = true
			if order.Status != "filled" || order.FilledQty != 10 {
				t.Fatalf("immediate fill order = %+v", order)
			}
		}
	}
	if !found {
		t.Fatal("immediate stop fill was not persisted")
	}
}

// TestManager_RoundingAvoidsReplaceChurn pins the float-equality fix: when
// the recomputed stop rounds to the same broker tick as the persisted stop
// (US equities = $0.01), the manager must NOT issue a REPLACE call.
// Without rounding, sub-cent ATR jitter would churn the API every tick.
//
// We pre-stage the trade with the exact rounded stop the trail will compute
// from these inputs, prove no broker churn, then perturb ATR with a tiny
// candle wiggle and prove the rounded result is still byte-equal so the
// no-op short-circuit triggers.
func TestManager_RoundingAvoidsReplaceChurn(t *testing.T) {
	fp := &fakePrices{candles: makeFlatPriceCandles(50, candle(50, 50.01, 49.99, 50))}

	// Probe the actual rounded stop the formula will produce on this fake.
	atr := indicators.ComputeATR(fp.candles, DefaultATRPeriod)
	candidate := 50.0 - 1.5*atr
	rounded := roundToTick(candidate, 2)

	fs := newFakeStore()
	fs.trades["t1"] = store.Trade{
		ID:            "t1",
		Ticker:        "AAPL",
		Direction:     "long",
		Status:        "open",
		EntryPrice:    50,
		StopLoss:      rounded,
		Quantity:      10,
		HighWaterMark: 50,
	}
	_, _ = fs.InsertOrder(context.Background(), store.Order{
		TradeID:       "t1",
		BrokerOrderID: "broker-pre",
		Ticker:        "AAPL",
		Side:          string(execution.OrderSideSell),
		OrderType:     string(execution.OrderTypeStop),
		Quantity:      10,
		StopPrice:     rounded,
		Status:        "pending",
		IsStopLoss:    true,
	})

	base := &fakeBroker{}
	fb := fakeBrokerWithReplace{fakeBroker: base}

	m := NewManager(Config{Enabled: true, ATRMultiple: 1.5}, fs, fp, fb)
	m.UpdateAll(context.Background())

	if len(base.replaced)+len(base.placed) != 0 {
		t.Fatalf("rounded stop unchanged but broker was called: replaced=%d placed=%d",
			len(base.replaced), len(base.placed))
	}

	// Sanity: the helper rounds in both directions identically.
	if got := roundToTick(48.50000001, 2); got != 48.50 {
		t.Fatalf("roundToTick: 48.50000001 → %v (want 48.50)", got)
	}
	if got := roundToTick(48.504, 2); got != 48.50 {
		t.Fatalf("roundToTick: 48.504 → %v (want 48.50)", got)
	}
	if got := roundToTick(48.506, 2); got != 48.51 {
		t.Fatalf("roundToTick: 48.506 → %v (want 48.51)", got)
	}
}

// TestManager_ATRPeriodHonoured confirms that the configured atr_period
// flows through to the indicator computation. We compare a 5-period vs
// 14-period ATR over the same noisy series; for noisy candles the two
// values must differ.
func TestManager_ATRPeriodHonoured(t *testing.T) {
	// Ascending price series with widening true ranges.
	candles := make([]data.Candle, 0, 40)
	for i := 0; i < 40; i++ {
		base := 100.0 + float64(i)
		spread := 0.5 + float64(i%5)*0.3
		candles = append(candles, candle(base, base+spread, base-spread, base+0.1))
	}
	short := indicators.ComputeATR(candles, 5)
	long := indicators.ComputeATR(candles, 14)
	if short <= 0 || long <= 0 {
		t.Fatalf("ATR returned zero for sufficient lookback: short=%v long=%v", short, long)
	}
	if short == long {
		t.Fatalf("ATR(5) == ATR(14) on noisy series — period not applied")
	}
}

// TestLiveStore_NoDBPanicSafe documents the no-DB safety contract: when the
// store package has no live pool, the LiveStore adapter must NOT panic.
// Reads return empty + nil; writes return a clean sentinel error. Without
// this guard, GetDB() panics and crashes the trailing-stop goroutine.
func TestLiveStore_NoDBPanicSafe(t *testing.T) {
	ls := LiveStore()
	ctx := context.Background()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("LiveStore panicked with no DB: %v", r)
		}
	}()
	if trs, err := ls.GetOpenTrades(ctx); err != nil || trs != nil {
		t.Fatalf("GetOpenTrades(no DB): want nil/nil, got %v/%v", trs, err)
	}
	if err := ls.UpdateTradeTrailing(ctx, "t1", 1, 1); err == nil {
		t.Fatalf("UpdateTradeTrailing(no DB): want error, got nil")
	}
	if _, err := ls.LatestStopLossOrderForTrade(ctx, "t1"); err == nil {
		t.Fatalf("LatestStopLossOrderForTrade(no DB): want error, got nil")
	}
	if _, err := ls.InsertOrder(ctx, store.Order{}); err == nil {
		t.Fatalf("InsertOrder(no DB): want error, got nil")
	}
	if err := ls.UpdateOrderStopReplacement(ctx, "o1", "broker-old", "broker-x", 0, 1.0, 1.0, 1.0, "pending", 0, 0, time.Time{}, store.ExitIntent{}); err == nil {
		t.Fatalf("UpdateOrderStopReplacement(no DB): want error, got nil")
	}
	if _, err := ls.CloseTradeRealizedWithIntent(ctx, "t1", 100, time.Time{}, store.ExitIntent{}); err == nil {
		t.Fatalf("CloseTradeRealizedWithIntent(no DB): want error, got nil")
	}
}

// TestManager_MarketExitUsesBrokerFilledPrice pins the round-3 review fix:
// when exitAtMarket fires and the broker echoes back FilledPrice/FilledQty
// (typical for Alpaca IOC market sells), the trade close + persisted order
// MUST reflect the broker's actual fill, not the snapshot price that
// triggered the trail. Otherwise pnl misreports and the order_history view
// looks wrong.
func TestManager_MarketExitUsesBrokerFilledPrice(t *testing.T) {
	fs := newFakeStore()
	fs.trades["t1"] = store.Trade{
		ID:            "t1",
		Ticker:        "AAPL",
		Direction:     "long",
		Status:        "open",
		EntryPrice:    100,
		StopLoss:      109,
		Quantity:      10,
		HighWaterMark: 200, // forces trail-crossed exit
	}

	fp := &fakePrices{candles: makeFlatPriceCandles(100, candle(100, 100.5, 99.5, 100))}
	fb := &fakeBroker{
		nextFillPrice: 99.50, // broker fills below the snapshot 100
		nextFillQty:   10,
		nextStatus:    "filled",
	}

	m := NewManager(Config{Enabled: true, ATRMultiple: 0.1, MarketOpen: alwaysOpenSession}, fs, fp, fb)
	m.UpdateAll(context.Background())
	if len(fb.placed) != 1 || fb.placed[0].IntentKey == "" {
		t.Fatalf("market exit must carry a durable intent key: %+v", fb.placed)
	}

	closed := fs.trades["t1"]
	if closed.Status != "closed" {
		t.Fatalf("trade not closed: status=%q", closed.Status)
	}
	if closed.ExitPrice != 99.50 {
		t.Fatalf("exit_price not from broker fill: got %v want 99.50", closed.ExitPrice)
	}
	// pnl computed from broker fill price, not the snapshot 100.
	wantPnL := (99.50 - 100.0) * 10
	if closed.PnL != wantPnL {
		t.Fatalf("pnl misreported: got %v want %v", closed.PnL, wantPnL)
	}

	// Verify the persisted order row carries FilledPrice/FilledQty/Quantity
	// the same way engine.monitorPositions records its non-trailing exits.
	var marketOrder *store.Order
	for _, o := range fs.orders {
		if o.OrderType == string(execution.OrderTypeMarket) {
			oCopy := o
			marketOrder = &oCopy
			break
		}
	}
	if marketOrder == nil {
		t.Fatalf("no market order persisted")
	}
	if marketOrder.FilledPrice != 99.50 {
		t.Fatalf("order.filled_price: got %v want 99.50", marketOrder.FilledPrice)
	}
	if marketOrder.FilledQty != 10 {
		t.Fatalf("order.filled_qty: got %v want 10", marketOrder.FilledQty)
	}
	if marketOrder.Quantity != 10 {
		t.Fatalf("order.quantity: got %v want 10", marketOrder.Quantity)
	}
}

func TestManager_MarketExitDefersAfterFreshTerminalGeneration(t *testing.T) {
	fs := newFakeStore()
	fs.trades["t1"] = store.Trade{
		ID: "t1", Ticker: "AAPL", Direction: "long", Status: "open",
		EntryPrice: 100, StopLoss: 109, Quantity: 10, HighWaterMark: 200,
	}
	baseKey := "trailing-market-exit:t1:10.00000000"
	fb := &durableMarketExitBroker{fakeBroker: &fakeBroker{placeResults: []execution.OrderResult{
		{BrokerOrderID: "terminal-1", Status: "cancelled"},
	}}}
	fp := &fakePrices{candles: makeFlatPriceCandles(100, candle(100, 100.5, 99.5, 100))}
	m := NewManager(Config{Enabled: true, ATRMultiple: 0.1, MarketOpen: alwaysOpenSession}, fs, fp, fb)
	m.UpdateAll(context.Background())

	if got := len(fb.placed); got != 1 {
		t.Fatalf("market-exit posts=%d, want one fresh terminal generation", got)
	}
	if got := fb.placed[0].IntentKey; got != baseKey {
		t.Fatalf("market-exit key=%q, want %q", got, baseKey)
	}
	if fs.trades["t1"].Status != "open" {
		t.Fatalf("fresh terminal market exit must release claim, got %q", fs.trades["t1"].Status)
	}
	if got := len(fs.orders); got != 0 {
		t.Fatalf("fresh terminal market exit stored %d orders, want none", got)
	}
}

func TestManager_MarketExitDefersAfterFreshDefinitiveRejection(t *testing.T) {
	fs := newFakeStore()
	fs.trades["t1"] = store.Trade{
		ID: "t1", Ticker: "AAPL", Direction: "long", Status: "open",
		EntryPrice: 100, StopLoss: 109, Quantity: 10, HighWaterMark: 200,
	}
	freshRejection := errors.Join(
		execution.ErrFreshTerminalZeroFillPlacement,
		execution.ErrTerminalZeroFillPlacement,
	)
	fb := &durableMarketExitBroker{fakeBroker: &fakeBroker{placeErr: freshRejection}}
	fp := &fakePrices{candles: makeFlatPriceCandles(100, candle(100, 100.5, 99.5, 100))}
	m := NewManager(Config{Enabled: true, ATRMultiple: 0.1, MarketOpen: alwaysOpenSession}, fs, fp, fb)
	m.UpdateAll(context.Background())

	if got := len(fb.placed); got != 1 {
		t.Fatalf("fresh definitive market-exit rejection posted %d orders in one tick, want one", got)
	}
	if got := fs.trades["t1"].Status; got != "open" {
		t.Fatalf("fresh definitive market-exit rejection must release claim, got %q", got)
	}
	if got := len(fs.orders); got != 0 {
		t.Fatalf("fresh definitive market-exit rejection stored %d orders, want none", got)
	}
}

func TestManager_MarketExitRefusesRecordedFilledExitLeg(t *testing.T) {
	fs := newFakeStore()
	fs.trades["t1"] = store.Trade{
		ID: "t1", Ticker: "AAPL", Direction: "long", Status: "open",
		EntryPrice: 100, StopLoss: 109, Quantity: 10,
		HighWaterMark: 200, // forces the trail-crossed market-exit path
	}
	if _, err := fs.InsertOrder(context.Background(), store.Order{
		TradeID: "t1", BrokerOrderID: "broker-filled-stop", Ticker: "AAPL",
		Side: string(execution.OrderSideSell), OrderType: string(execution.OrderTypeStop),
		Quantity: 10, StopPrice: 109, FilledPrice: 108.5, FilledQty: 10,
		Status: "filled", IsStopLoss: true,
	}); err != nil {
		t.Fatalf("InsertOrder: %v", err)
	}

	fp := &fakePrices{candles: makeFlatPriceCandles(110, candle(110, 110.5, 109.5, 110))}
	fb := &fakeBroker{
		statusResults: map[string]execution.OrderResult{
			"broker-filled-stop": {Status: "filled", FilledPrice: 108.5, FilledQty: 10},
		},
		nextStatus: "filled", nextFillPrice: 110, nextFillQty: 10,
	}
	m := NewManager(Config{Enabled: true, ATRMultiple: 0.1, MarketOpen: alwaysOpenSession}, fs, fp, fb)
	m.UpdateAll(context.Background())

	if len(fb.placed) != 0 {
		t.Fatalf("market exit submitted despite a recorded filled stop: %+v", fb.placed)
	}
	if got := fs.trades["t1"].Status; got != "exiting" {
		t.Fatalf("trade status = %q, want exiting so the reconciler owns the filled leg", got)
	}
}

func TestManager_MarketExitFlattensResidualAfterPartialCancelledExitLeg(t *testing.T) {
	fs := newFakeStore()
	fs.trades["t1"] = store.Trade{
		ID: "t1", Ticker: "AAPL", Direction: "long", Status: "open",
		EntryPrice: 100, StopLoss: 109, Quantity: 10, HighWaterMark: 200,
	}
	if _, err := fs.InsertOrder(context.Background(), store.Order{
		TradeID: "t1", BrokerOrderID: "broker-partial-stop", Ticker: "AAPL",
		Side: string(execution.OrderSideSell), OrderType: string(execution.OrderTypeStop),
		Quantity: 10, StopPrice: 109, FilledPrice: 108.5, FilledQty: 4,
		Status: "cancelled", IsStopLoss: true,
	}); err != nil {
		t.Fatalf("InsertOrder: %v", err)
	}

	fp := &fakePrices{candles: makeFlatPriceCandles(110, candle(110, 110.5, 109.5, 110))}
	fb := &fakeBroker{statusResults: map[string]execution.OrderResult{
		"broker-partial-stop": {Status: "cancelled", FilledPrice: 108.5, FilledQty: 4},
	}, nextStatus: "filled", nextFillPrice: 110, nextFillQty: 6}
	m := NewManager(Config{Enabled: true, ATRMultiple: 0.1, MarketOpen: alwaysOpenSession}, fs, fp, fb)
	m.UpdateAll(context.Background())

	if len(fb.placed) != 1 || fb.placed[0].Quantity != 6 {
		t.Fatalf("market exit = %+v, want one residual sell of 6", fb.placed)
	}
	if got := fs.trades["t1"].Status; got != "closed" {
		t.Fatalf("trade status = %q, want closed after residual sell", got)
	}
}

func TestManager_MarketExitUsesDurableFillWhenTerminalLegPurged(t *testing.T) {
	fs := newFakeStore()
	fs.trades["t1"] = store.Trade{
		ID: "t1", Ticker: "AAPL", Direction: "long", Status: "open",
		EntryPrice: 100, StopLoss: 109, Quantity: 10, HighWaterMark: 200,
	}
	if _, err := fs.InsertOrder(context.Background(), store.Order{
		TradeID: "t1", BrokerOrderID: "broker-purged-partial-stop", Ticker: "AAPL",
		Side: string(execution.OrderSideSell), OrderType: string(execution.OrderTypeStop),
		Quantity: 10, StopPrice: 109, FilledPrice: 108.5, FilledQty: 4,
		Status: "cancelled", IsStopLoss: true,
	}); err != nil {
		t.Fatalf("InsertOrder: %v", err)
	}

	fp := &fakePrices{candles: makeFlatPriceCandles(110, candle(110, 110.5, 109.5, 110))}
	fb := &fakeBroker{
		notFoundIDs: map[string]bool{"broker-purged-partial-stop": true},
		nextStatus:  "filled", nextFillPrice: 110, nextFillQty: 6,
	}
	m := NewManager(Config{Enabled: true, ATRMultiple: 0.1, MarketOpen: alwaysOpenSession}, fs, fp, fb)
	m.UpdateAll(context.Background())

	if len(fb.placed) != 1 || fb.placed[0].Quantity != 6 {
		t.Fatalf("market exit = %+v, want one residual sell of 6 after purged partial leg", fb.placed)
	}
	if got := fs.trades["t1"].Status; got != "closed" {
		t.Fatalf("trade status = %q, want closed after residual sell", got)
	}
}

func TestManager_MarketExitUsesDurableFilledLegWhenPurged(t *testing.T) {
	fs := newFakeStore()
	fs.trades["t1"] = store.Trade{
		ID: "t1", Ticker: "AAPL", Direction: "long", Status: "open",
		EntryPrice: 100, StopLoss: 109, Quantity: 10, HighWaterMark: 200,
	}
	if _, err := fs.InsertOrder(context.Background(), store.Order{
		TradeID: "t1", BrokerOrderID: "broker-purged-filled-stop", Ticker: "AAPL",
		Side: string(execution.OrderSideSell), OrderType: string(execution.OrderTypeStop),
		Quantity: 10, StopPrice: 109, FilledPrice: 108.5, FilledQty: 4,
		Status: "filled", IsStopLoss: true,
	}); err != nil {
		t.Fatalf("InsertOrder: %v", err)
	}

	fp := &fakePrices{candles: makeFlatPriceCandles(110, candle(110, 110.5, 109.5, 110))}
	fb := &fakeBroker{
		notFoundIDs: map[string]bool{"broker-purged-filled-stop": true},
		nextStatus:  "filled", nextFillPrice: 110, nextFillQty: 6,
	}
	m := NewManager(Config{Enabled: true, ATRMultiple: 0.1, MarketOpen: alwaysOpenSession}, fs, fp, fb)
	m.UpdateAll(context.Background())

	if len(fb.placed) != 1 || fb.placed[0].Quantity != 6 {
		t.Fatalf("market exit = %+v, want one residual sell of 6 after purged filled leg", fb.placed)
	}
	if got := fs.trades["t1"].Status; got != "closed" {
		t.Fatalf("trade status = %q, want closed after residual sell", got)
	}
}

func TestManager_MarketExitFailsClosedForPurgedFilledLegWithoutQuantity(t *testing.T) {
	fs := newFakeStore()
	fs.trades["t1"] = store.Trade{
		ID: "t1", Ticker: "AAPL", Direction: "long", Status: "open",
		EntryPrice: 100, StopLoss: 109, Quantity: 10, HighWaterMark: 200,
	}
	if _, err := fs.InsertOrder(context.Background(), store.Order{
		TradeID: "t1", BrokerOrderID: "broker-purged-filled-unknown", Ticker: "AAPL",
		Side: string(execution.OrderSideSell), OrderType: string(execution.OrderTypeStop),
		Quantity: 10, StopPrice: 109, Status: "filled", IsStopLoss: true,
	}); err != nil {
		t.Fatalf("InsertOrder: %v", err)
	}

	fp := &fakePrices{candles: makeFlatPriceCandles(110, candle(110, 110.5, 109.5, 110))}
	fb := &fakeBroker{
		notFoundIDs: map[string]bool{"broker-purged-filled-unknown": true},
		nextStatus:  "filled", nextFillPrice: 110, nextFillQty: 10,
	}
	m := NewManager(Config{Enabled: true, ATRMultiple: 0.1, MarketOpen: alwaysOpenSession}, fs, fp, fb)
	m.UpdateAll(context.Background())

	if len(fb.placed) != 0 {
		t.Fatalf("market exit submitted despite ambiguous purged filled leg: %+v", fb.placed)
	}
	if got := fs.trades["t1"].Status; got != "exiting" {
		t.Fatalf("trade status = %q, want exiting for fail-closed retry", got)
	}
}

func TestManager_MarketExitFailsClosedWhenTerminalLegIsWorkingAtBroker(t *testing.T) {
	fs := newFakeStore()
	fs.trades["t1"] = store.Trade{
		ID: "t1", Ticker: "AAPL", Direction: "long", Status: "open",
		EntryPrice: 100, StopLoss: 109, Quantity: 10, HighWaterMark: 200,
	}
	if _, err := fs.InsertOrder(context.Background(), store.Order{
		TradeID: "t1", BrokerOrderID: "broker-stale-stop", Ticker: "AAPL",
		Side: string(execution.OrderSideSell), OrderType: string(execution.OrderTypeStop),
		Quantity: 10, StopPrice: 109, FilledPrice: 108.5, FilledQty: 4,
		Status: "cancelled", IsStopLoss: true,
	}); err != nil {
		t.Fatalf("InsertOrder: %v", err)
	}
	fp := &fakePrices{candles: makeFlatPriceCandles(110, candle(110, 110.5, 109.5, 110))}
	fb := &fakeBroker{statusResults: map[string]execution.OrderResult{
		"broker-stale-stop": {Status: "pending", FilledQty: 4},
	}, nextStatus: "filled", nextFillPrice: 110, nextFillQty: 10}
	m := NewManager(Config{Enabled: true, ATRMultiple: 0.1, MarketOpen: alwaysOpenSession}, fs, fp, fb)
	m.UpdateAll(context.Background())

	if len(fb.placed) != 0 {
		t.Fatalf("market exit submitted while protective leg remained working: %+v", fb.placed)
	}
	if got := fs.trades["t1"].Status; got != "exiting" {
		t.Fatalf("trade status = %q, want exiting for fail-closed retry", got)
	}
}

func TestManager_MarketExitFailsClosedWhenWorkingLegIsPurged(t *testing.T) {
	fs := newFakeStore()
	fs.trades["t1"] = store.Trade{
		ID: "t1", Ticker: "AAPL", Direction: "long", Status: "open",
		EntryPrice: 100, StopLoss: 109, Quantity: 10, HighWaterMark: 200,
	}
	if _, err := fs.InsertOrder(context.Background(), store.Order{
		TradeID: "t1", BrokerOrderID: "broker-purged-working-stop", Ticker: "AAPL",
		Side: string(execution.OrderSideSell), OrderType: string(execution.OrderTypeStop),
		Quantity: 10, StopPrice: 109, Status: "pending", IsStopLoss: true,
	}); err != nil {
		t.Fatalf("InsertOrder: %v", err)
	}
	fp := &fakePrices{candles: makeFlatPriceCandles(110, candle(110, 110.5, 109.5, 110))}
	fb := &fakeBroker{notFoundIDs: map[string]bool{"broker-purged-working-stop": true}}
	m := NewManager(Config{Enabled: true, ATRMultiple: 0.1, MarketOpen: alwaysOpenSession}, fs, fp, fb)
	m.UpdateAll(context.Background())

	if len(fb.placed) != 0 {
		t.Fatalf("market exit submitted after working leg disappeared: %+v", fb.placed)
	}
	if got := fs.trades["t1"].Status; got != "exiting" {
		t.Fatalf("trade status = %q, want exiting for fail-closed retry", got)
	}
}

// TestManager_StopRoundedAgainstNumericRoundtrip pins the round-3 fix for
// the float-equality bug: after persistence, t.StopLoss is scanned from a
// NUMERIC column and binary roundtrip can leave it as 48.509999999999998
// for what the broker stored as 48.51. The earlier code rounded only the
// candidate (newStop) before comparing — the inequality then fired a
// pointless REPLACE on every tick. Round BOTH sides.
func TestManager_StopRoundedAgainstNumericRoundtrip(t *testing.T) {
	// Pre-stage: trade.StopLoss carries the canonical float that NUMERIC
	// roundtrip can produce for 48.51.
	noisyStop := 48.509999999999998
	fs := newFakeStore()
	fs.trades["t1"] = store.Trade{
		ID:            "t1",
		Ticker:        "AAPL",
		Direction:     "long",
		Status:        "open",
		EntryPrice:    50,
		StopLoss:      noisyStop,
		Quantity:      10,
		HighWaterMark: 50,
	}
	_, _ = fs.InsertOrder(context.Background(), store.Order{
		TradeID:       "t1",
		BrokerOrderID: "broker-pre",
		Ticker:        "AAPL",
		Side:          string(execution.OrderSideSell),
		OrderType:     string(execution.OrderTypeStop),
		Quantity:      10,
		StopPrice:     noisyStop,
		Status:        "pending",
		IsStopLoss:    true,
	})

	// Construct candles whose computed candidate rounds to exactly 48.51,
	// matching the persisted noisyStop after symmetric rounding. The flat
	// 50-price baseline produces a small ATR; with multiple chosen so
	// hwm − mult*atr → ~48.51 the comparator must say "no change".
	fp := &fakePrices{candles: makeFlatPriceCandles(50, candle(50, 50.01, 49.99, 50))}
	atr := indicators.ComputeATR(fp.candles, DefaultATRPeriod)
	if atr <= 0 {
		t.Fatalf("ATR was zero in test setup; can't compute deterministic stop")
	}
	// Solve mult so 50 - mult*atr == 48.51 exactly post-rounding.
	mult := (50.0 - 48.51) / atr

	base := &fakeBroker{}
	fb := fakeBrokerWithReplace{fakeBroker: base}

	m := NewManager(Config{Enabled: true, ATRMultiple: mult}, fs, fp, fb)
	m.UpdateAll(context.Background())

	// Sanity check: roundToTick should map both representations to the
	// same broker tick. If this ever drifts the test below would mask the
	// bug it's trying to pin.
	if got := roundToTick(noisyStop, 2); got != 48.51 {
		t.Fatalf("setup precondition failed: roundToTick(%v) = %v, want 48.51", noisyStop, got)
	}

	if len(base.replaced)+len(base.placed) != 0 {
		t.Fatalf("BUG REGRESSED: NUMERIC-roundtrip stop triggered broker churn: replaced=%d placed=%d (mult=%v atr=%v)",
			len(base.replaced), len(base.placed), mult, atr)
	}
}

// TestCancelAndConfirm_NotFoundTreatedAsTerminal pins the round-3 fix for
// the cancelAndConfirm comment/behavior mismatch: the comment says a 404 /
// "order not found" status response is fine (terminal), but the original
// code errored out and blocked resubmission. With the executor.ErrOrderNotFound
// sentinel, cancelAndConfirm now returns nil so the cancel+resubmit
// fallback can complete — exactly what the comment promised.
func TestCancelAndConfirm_NotFoundTreatedAsTerminal(t *testing.T) {
	fs := newFakeStore()
	fs.trades["t1"] = store.Trade{
		ID:            "t1",
		Ticker:        "AAPL",
		Direction:     "long",
		Status:        "open",
		EntryPrice:    100,
		StopLoss:      95,
		Quantity:      10,
		HighWaterMark: 100,
	}
	_, _ = fs.InsertOrder(context.Background(), store.Order{
		TradeID:       "t1",
		BrokerOrderID: "broker-pre",
		Ticker:        "AAPL",
		Side:          string(execution.OrderSideSell),
		OrderType:     string(execution.OrderTypeStop),
		Quantity:      10,
		StopPrice:     95,
		Status:        "pending",
		IsStopLoss:    true,
	})

	fp := &fakePrices{candles: makeFlatPriceCandles(100, candle(108, 110, 107, 110))}
	// failReplace forces the cancel+resubmit fallback. CancelOrder
	// "succeeds" (no error) but GetOrderStatus reports the broker has
	// already purged the id → ErrOrderNotFound. Per the comment this is
	// terminal-cancelled state and resubmission must proceed.
	base := &fakeBroker{
		failReplace: true,
		notFoundIDs: map[string]bool{"broker-pre": true},
	}
	fb := fakeBrokerWithReplace{fakeBroker: base}

	m := NewManager(Config{Enabled: true, ATRMultiple: 1.5}, fs, fp, fb)
	m.UpdateAll(context.Background())

	if len(base.placed) != 1 {
		t.Fatalf("expected resubmit after broker reported not-found: placed=%d", len(base.placed))
	}
	if base.placed[0].Type != execution.OrderTypeStop {
		t.Fatalf("resubmitted order wrong type: %v", base.placed[0].Type)
	}
}

func TestRollbackUnrecordedStopPersistsBrokerFillTruth(t *testing.T) {
	fs := newFakeStore()
	fs.trades["t1"] = store.Trade{
		ID: "t1", Ticker: "AAPL", Direction: "long", Status: "open",
		EntryPrice: 100, Quantity: 10,
	}
	filledAt := time.Date(2026, 8, 1, 14, 5, 2, 0, time.UTC)
	fb := &fakeBroker{statusResults: map[string]execution.OrderResult{
		"broker-rollback": {
			Status: "filled", FilledPrice: 94.5, FilledQty: 10, FilledAt: filledAt,
		},
	}}
	m := NewManager(Config{Enabled: true}, fs, &fakePrices{}, fb)

	err := m.rollbackUnrecordedStop(context.Background(), fs.trades["t1"], 95, "broker-rollback", "", stopExitIntent(95))
	if !errors.Is(err, errStopFilledDuringRollback) {
		t.Fatalf("rollback error = %v, want fill sentinel", err)
	}
	orders, err := fs.ExitLegsForMarketExit(context.Background(), "t1")
	if err != nil || len(orders) != 1 {
		t.Fatalf("rollback orders = %+v err=%v", orders, err)
	}
	got := orders[0]
	if got.Status != "filled" || got.FilledPrice != 94.5 || got.FilledQty != 10 ||
		got.FilledAt == nil || !got.FilledAt.Equal(filledAt) {
		t.Fatalf("rollback fill truth = %+v", got)
	}
}

func TestReplaceStopImmediateFillPersistsBrokerTruth(t *testing.T) {
	fs := newFakeStore()
	trade := store.Trade{
		ID: "t1", Ticker: "AAPL", Direction: "long", Status: "open",
		EntryPrice: 100, Quantity: 10,
	}
	fs.trades[trade.ID] = trade
	_, _ = fs.InsertOrder(context.Background(), store.Order{
		TradeID: trade.ID, BrokerOrderID: "old-stop", Ticker: trade.Ticker,
		Side: "sell", OrderType: "stop", Quantity: 10, StopPrice: 95,
		Status: "pending", IsStopLoss: true,
	})
	filledAt := time.Date(2026, 8, 1, 14, 5, 2, 0, time.UTC)
	base := &fakeBroker{replaceResult: &execution.OrderResult{
		BrokerOrderID: "filled-replacement", Status: "filled",
		FilledAt: filledAt,
	}}
	m := NewManager(Config{Enabled: true}, fs, &fakePrices{}, fakeBrokerWithReplace{fakeBroker: base})

	err := m.submitOrReplaceStop(context.Background(), trade, 97, stopExitIntent(97))
	if !errors.Is(err, errStopFilledDuringRollback) {
		t.Fatalf("replacement error = %v, want fill sentinel", err)
	}
	var got store.Order
	for _, order := range fs.orders {
		got = order
	}
	if got.BrokerOrderID != "filled-replacement" || got.Status != "filled" ||
		got.FilledPrice != 97 || got.FilledQty != 10 || got.FilledAt == nil ||
		!got.FilledAt.Equal(filledAt) {
		t.Fatalf("replacement fill truth = %+v", got)
	}
	if fs.trades[trade.ID].Status != "exiting" || fs.trades[trade.ID].ExitReason != store.ExitReasonStopLoss {
		t.Fatalf("replacement fill claim = %+v", fs.trades[trade.ID])
	}
}

func TestSubmitStopFreshDefinitiveRejectionDoesNotFanOut(t *testing.T) {
	fs := newFakeStore()
	trade := store.Trade{
		ID: "t1", Ticker: "AAPL", Direction: "long", Status: "open",
		EntryPrice: 100, Quantity: 10,
	}
	freshRejection := errors.Join(
		execution.ErrFreshTerminalZeroFillPlacement,
		execution.ErrTerminalZeroFillPlacement,
	)
	fb := &durableMarketExitBroker{fakeBroker: &fakeBroker{placeErr: freshRejection}}
	m := NewManager(Config{Enabled: true}, fs, &fakePrices{}, fb)

	err := m.submitOrReplaceStop(context.Background(), trade, 97, stopExitIntent(97))
	if !errors.Is(err, execution.ErrFreshTerminalZeroFillPlacement) {
		t.Fatalf("stop placement error = %v, want fresh-terminal sentinel", err)
	}
	if got := len(fb.placed); got != 1 {
		t.Fatalf("fresh definitive stop rejection posted %d orders in one tick, want one", got)
	}
}

func TestReplaceStopFillClaimsBeforeReplacementPersistence(t *testing.T) {
	fs := newFakeStore()
	trade := store.Trade{
		ID: "t1", Ticker: "AAPL", Direction: "long", Status: "open",
		EntryPrice: 100, Quantity: 10,
	}
	fs.trades[trade.ID] = trade
	_, _ = fs.InsertOrder(context.Background(), store.Order{
		TradeID: trade.ID, BrokerOrderID: "old-stop", Ticker: trade.Ticker,
		Side: "sell", OrderType: "stop", Quantity: 10, StopPrice: 95,
		Status: "pending", IsStopLoss: true,
	})
	fs.updateReplacementErr = errors.New("simulated replacement persistence failure")
	base := &fakeBroker{replaceResult: &execution.OrderResult{
		BrokerOrderID: "filled-replacement", Status: "filled",
	}}
	m := NewManager(Config{Enabled: true}, fs, &fakePrices{}, fakeBrokerWithReplace{fakeBroker: base})

	if err := m.submitOrReplaceStop(context.Background(), trade, 97, stopExitIntent(97)); err == nil {
		t.Fatal("replacement persistence failure returned nil")
	}
	if fs.trades[trade.ID].Status != "exiting" {
		t.Fatalf("trade status = %q, want exiting recovery owner", fs.trades[trade.ID].Status)
	}
}

// TestManager_DisabledIsFastReturn ensures Run() exits cleanly when
// Config.Enabled is false — no goroutine leaks, no broker calls.
func TestManager_DisabledIsFastReturn(t *testing.T) {
	fs := newFakeStore()
	m := NewManager(Config{Enabled: false}, fs, &fakePrices{}, &fakeBroker{})
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	go func() {
		m.Run(ctx)
		close(done)
	}()
	select {
	case <-done:
		// expected fast-return
	case <-time.After(300 * time.Millisecond):
		t.Fatalf("Run did not return when Enabled=false")
	}
}

// TestBracketLeg_NoCancelResubmitFallback pins the #435 rule: when the
// working stop is an OCO-linked bracket child (ParentOrderID set) and the
// atomic PATCH replace fails, the manager must NOT fall back to
// cancel+resubmit — cancelling one OCO leg kills or orphans the sibling
// take-profit, and a standalone replacement stop would leave the position
// half-managed. It surfaces an error and retries the replace next sweep.
func TestBracketLeg_NoCancelResubmitFallback(t *testing.T) {
	fs := newFakeStore()
	fs.trades["t1"] = store.Trade{
		ID:            "t1",
		Ticker:        "AAPL",
		Direction:     "long",
		Status:        "open",
		EntryPrice:    100,
		StopLoss:      95,
		Quantity:      10,
		HighWaterMark: 100,
	}
	_, _ = fs.InsertOrder(context.Background(), store.Order{
		TradeID:       "t1",
		BrokerOrderID: "leg-sl",
		Ticker:        "AAPL",
		Side:          string(execution.OrderSideSell),
		OrderType:     string(execution.OrderTypeStop),
		Quantity:      10,
		StopPrice:     95,
		Status:        "pending",
		IsStopLoss:    true,
		ParentOrderID: "parent-1", // bracket child
	})

	fp := &fakePrices{candles: makeFlatPriceCandles(100, candle(108, 110, 107, 110))}
	base := &fakeBroker{failReplace: true}
	fb := fakeBrokerWithReplace{fakeBroker: base}

	m := NewManager(Config{Enabled: true, ATRMultiple: 1.5}, fs, fp, fb)
	m.UpdateAll(context.Background())

	if len(base.cancelled) != 0 {
		t.Fatalf("bracket leg was cancelled after failed replace: %v", base.cancelled)
	}
	if len(base.placed) != 0 {
		t.Fatalf("standalone stop was placed for a bracket-managed trade: %v", base.placed)
	}
}

// TestManager_CorruptBarCannotRatchetStopAboveMarket is the live-path
// regression for issue #519.
//
// priceAndATR reads the provider directly and never touches the DB, so the
// UpsertCandles guard does not cover it. Without sanitisation the corrupt
// 2026-08-04 ANET bar (high 224.98 against a real market near 190) would
// ratchet the high-water mark to 224.98, compute a stop far above the current
// price, and trip updateOne's "trail crossed price → exit at market" branch —
// liquidating a healthy position on fabricated data.
func TestManager_CorruptBarCannotRatchetStopAboveMarket(t *testing.T) {
	fs := newFakeStore()
	fs.trades["t1"] = store.Trade{
		ID: "t1", Ticker: "ANET", Direction: "long", Status: "open",
		EntryPrice: 168.86, StopLoss: 156.87, Quantity: 10, HighWaterMark: 190,
	}
	fb := &fakeBroker{}
	m := NewManager(Config{Enabled: true, ATRMultiple: 1.5, ATRPeriod: 14}, fs, nil, fb)

	// 25 ordinary ~190 bars, then the corrupt spike. Ticker/Timeframe must be
	// set: the guard groups per series and only gates intraday timeframes,
	// exactly as real provider output is shaped.
	base := time.Date(2026, 8, 4, 0, 0, 0, 0, time.UTC)
	bars := make([]data.Candle, 0, 26)
	for i := 0; i < 25; i++ {
		bars = append(bars, data.Candle{
			Ticker: "ANET", Timeframe: data.Timeframe1Hour,
			Timestamp: base.Add(time.Duration(i) * time.Hour),
			Open:      190.34, High: 191.34, Low: 189.34, Close: 190.34, Volume: 1000,
		})
	}
	bars = append(bars, data.Candle{
		Ticker: "ANET", Timeframe: data.Timeframe1Hour,
		Timestamp: base.Add(25 * time.Hour),
		Open:      190.40, High: 224.98, Low: 190.20, Close: 211.47, Volume: 5000,
	})
	m.prices = &fakePrices{candles: bars}
	m.UpdateAll(context.Background())

	got := fs.trades["t1"]
	if got.HighWaterMark > 200 {
		t.Errorf("corrupt bar ratcheted the high-water mark to %.2f", got.HighWaterMark)
	}
	if got.StopLoss > 190.34 {
		t.Errorf("stop placed above the real market: %.2f (market ~190.34)", got.StopLoss)
	}
	if got.Status == "closed" {
		t.Error("position liquidated on a corrupt bar")
	}
	for _, r := range fb.placed {
		if r.StopPrice > 190.34 {
			t.Errorf("submitted a stop above market: %.2f", r.StopPrice)
		}
	}
}

// ── Activation gate (min gain before the trail takes over) ───────────────────

func TestTrailArmedGatesOnPeakGain(t *testing.T) {
	const entry, gate = 100.0, 5.0
	for _, tc := range []struct {
		name string
		hwm  float64
		want bool
	}{
		{"never moved", 100, false},
		{"up 1.5% — the case that was exiting early", 101.5, false},
		{"just under the gate", 104.99, false},
		{"exactly at the gate arms", 105, true},
		{"well past the gate", 130, true},
		{"underwater never arms", 95, false},
	} {
		if got := TrailArmed(entry, tc.hwm, gate); got != tc.want {
			t.Errorf("%s: TrailArmed(%v, %v, %v) = %v, want %v", tc.name, entry, tc.hwm, gate, got, tc.want)
		}
	}
}

// Arming must LATCH. The gate reads the high-water mark, which only ratchets up,
// so a trade that earned the trail keeps it through a pullback. Keying on the
// current price instead would arm and disarm on every oscillation across the
// threshold — the trail would keep forgetting the peak it exists to protect.
func TestTrailArmedLatchesThroughPullback(t *testing.T) {
	// Peaked at +8%, price now back to +1%. HWM is what is passed, so it stays armed.
	if !TrailArmed(100, 108, 5) {
		t.Error("TrailArmed = false after peaking above the gate; arming must latch on the high-water mark")
	}
	// Sanity: had the caller passed current price (101) it would disarm — the
	// bug this test exists to prevent.
	if TrailArmed(100, 101, 5) {
		t.Error("TrailArmed(entry, currentPrice=101, 5) = true; the 1%% case must not arm")
	}
}

func TestTrailArmedZeroGateKeepsLegacyBehaviour(t *testing.T) {
	for _, hwm := range []float64{0, 95, 100, 100.01} {
		if !TrailArmed(100, hwm, 0) {
			t.Errorf("TrailArmed(100, %v, 0) = false; a zero gate must trail from the first tick", hwm)
		}
	}
}

func TestTrailArmedRejectsUnusableInputs(t *testing.T) {
	if TrailArmed(0, 110, 5) {
		t.Error("armed with a zero entry price; percent gain is undefined without an entry")
	}
	if TrailArmed(100, 0, 5) {
		t.Error("armed with no high-water mark yet")
	}
}

func TestSubmitOrReplaceStopSerializesRecoveryLifecycle(t *testing.T) {
	fs := newFakeStore()
	trade := store.Trade{ID: "recovery-lock-trade", Ticker: "AAPL", Direction: "long", Status: "open", EntryPrice: 100, StopLoss: 95, Quantity: 2}
	fs.trades[trade.ID] = trade
	fs.orders["old-stop"] = store.Order{ID: "old-stop", TradeID: trade.ID, BrokerOrderID: "old-stop", Ticker: trade.Ticker, Side: "sell", OrderType: "stop", Quantity: 2, StopPrice: 95, Status: "pending", IsStopLoss: true}
	broker := &fakeBroker{}
	manager := NewManager(Config{Enabled: true}, fs, &fakePrices{}, broker)

	enteredRecovery := make(chan struct{})
	releaseRecovery := make(chan struct{})
	recoveryDone := make(chan error, 1)
	go func() {
		recoveryDone <- store.WithTradeOrderLock(trade.ID, func() error {
			close(enteredRecovery)
			<-releaseRecovery
			return nil
		})
	}()
	<-enteredRecovery

	trailingDone := make(chan error, 1)
	go func() {
		trailingDone <- manager.submitOrReplaceStop(context.Background(), trade, 97, stopExitIntent(97))
	}()
	// The recovery lifecycle owns the shared lock, so trailing cannot even
	// read/cancel the old stop before recovery has finished its own
	// lookup/cancel/submit/persist window.
	select {
	case err := <-trailingDone:
		t.Fatalf("trailing lifecycle entered recovery window: %v", err)
	case <-time.After(25 * time.Millisecond):
	}
	broker.mu.Lock()
	cancels, submissions := len(broker.cancelled), len(broker.placed)
	broker.mu.Unlock()
	if cancels != 0 || submissions != 0 {
		t.Fatalf("trailing mutated broker during recovery window: cancels=%d submissions=%d", cancels, submissions)
	}

	close(releaseRecovery)
	if err := <-recoveryDone; err != nil {
		t.Fatal(err)
	}
	if err := <-trailingDone; err != nil {
		t.Fatal(err)
	}
	broker.mu.Lock()
	cancels, submissions = len(broker.cancelled), len(broker.placed)
	broker.mu.Unlock()
	if cancels != 1 || submissions != 1 {
		t.Fatalf("serialized trailing lifecycle broker calls: cancels=%d submissions=%d, want 1/1", cancels, submissions)
	}
	active, err := fs.LatestStopLossOrderForTrade(context.Background(), trade.ID)
	if err != nil || active == nil || active.BrokerOrderID != "broker-1" || active.StopPrice != 97 {
		t.Fatalf("active protective stop=%#v err=%v, want one broker-1 @ 97", active, err)
	}
	fs.mu.Lock()
	var rows int
	for _, order := range fs.orders {
		if order.TradeID == trade.ID && order.IsStopLoss && (order.Status == "pending" || order.Status == "partial") {
			rows++
		}
	}
	fs.mu.Unlock()
	if rows != 1 {
		t.Fatalf("active protective rows=%d, want one", rows)
	}
}

// The live regression this feature exists for: a position that ticks up ~1.5%
// and pulls back must NOT be closed by the trail. Before the gate, the ratchet
// ran from the first tick and a pullback of atr_multiple × ATR from a trivial
// high crossed the price and triggered a market exit.
func TestManager_ATRDisarmedBelowGainGateDoesNotExit(t *testing.T) {
	fs := newFakeStore()
	fs.trades["t1"] = store.Trade{
		ID: "t1", Ticker: "AAPL", Direction: "long", Status: "open",
		EntryPrice: 100, StopLoss: 95, Quantity: 10, HighWaterMark: 101.5,
	}
	// Price has fallen back to 99 after peaking at 101.5 (+1.5%).
	fp := &fakePrices{candles: makeFlatPriceCandles(100, candle(99, 99.5, 98.5, 99))}
	fb := &fakeBroker{}

	// The calendar MUST be pinned open. With the market closed, deleting the
	// gate entirely still leaves the trade open — the ungated code would reach
	// the exit branch, find the session shut and take the deferral path — so
	// this assertion would pass against the very bug it exists to catch.
	m := NewManager(Config{
		Enabled: true, Rule: RuleATR, ATRMultiple: 1.5, ActivateAtGainPct: 5,
		MarketOpen: alwaysOpenSession,
	}, fs, fp, fb)
	m.UpdateAll(context.Background())

	if got := fs.trades["t1"].Status; got != "open" {
		t.Fatalf("trade status = %q, want open — a +1.5%% peak has not earned the trail", got)
	}
	// The original protective stop must be untouched: the trail owns nothing yet.
	if got := fs.trades["t1"].StopLoss; got != 95 {
		t.Errorf("stop_loss = %v, want the original 95 left in place while disarmed", got)
	}
}

// Once the peak clears the gate the trail behaves exactly as before, including
// closing the position when the band crosses price.
func TestManager_ATRArmedAboveGainGateStillExits(t *testing.T) {
	fs := newFakeStore()
	fs.trades["t1"] = store.Trade{
		ID: "t1", Ticker: "AAPL", Direction: "long", Status: "open",
		EntryPrice: 100, StopLoss: 95, Quantity: 10, HighWaterMark: 120,
	}
	// Peaked +20%, now back to 100 — far below hwm - 1.5×ATR, so the trail fires.
	fp := &fakePrices{candles: makeFlatPriceCandles(100, candle(100, 100.5, 99.5, 100))}
	fb := &fakeBroker{}

	m := NewManager(Config{
		Enabled: true, Rule: RuleATR, ATRMultiple: 1.5, ActivateAtGainPct: 5,
	}, fs, fp, fb)
	m.marketOpen = func(time.Time) bool { return true }
	m.UpdateAll(context.Background())

	if got := fs.trades["t1"].Status; got == "open" {
		t.Fatal("trade still open; a +20% peak has earned the trail and the band is through price")
	}
}

// The whole feature in one assertion: identical inputs, opposite outcomes.
//
// A trade that peaked at +1.5% and pulled back to -1% is exactly the shape that
// was closing early. Ungated (activate_at_gain_pct: 0, the legacy behaviour)
// the band lands at 99.95 against a price of 99 and the position is sold at
// market. Gated at 5% the same trade is left alone for its original stop to
// manage. This is also the opt-in guarantee: a zero gate changes nothing.
func TestManager_ATRGateDecidesTheSameTrade(t *testing.T) {
	newTrade := func() store.Trade {
		return store.Trade{
			ID: "t1", Ticker: "AAPL", Direction: "long", Status: "open",
			EntryPrice: 100, StopLoss: 95, Quantity: 10, HighWaterMark: 101.5,
		}
	}
	run := func(gate float64) string {
		fs := newFakeStore()
		fs.trades["t1"] = newTrade()
		fp := &fakePrices{candles: makeFlatPriceCandles(100, candle(99, 99.5, 98.5, 99))}
		m := NewManager(Config{
			Enabled: true, Rule: RuleATR, ATRMultiple: 1.5, ActivateAtGainPct: gate,
		}, fs, fp, &fakeBroker{})
		m.marketOpen = func(time.Time) bool { return true }
		m.UpdateAll(context.Background())
		return fs.trades["t1"].Status
	}

	if got := run(0); got == "open" {
		t.Errorf("ungated status = %q, want the legacy premature exit — otherwise this test is not "+
			"exercising the behaviour the gate changes", got)
	}
	if got := run(5); got != "open" {
		t.Errorf("gated status = %q, want open — a +1.5%% peak has not earned the trail", got)
	}
}
