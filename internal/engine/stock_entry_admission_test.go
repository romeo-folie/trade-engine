package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/data"
	"github.com/romeo-folie/trade-engine/internal/execution"
	"github.com/romeo-folie/trade-engine/internal/portfolio"
	"github.com/romeo-folie/trade-engine/internal/risk"
	"github.com/romeo-folie/trade-engine/internal/stockorders"
	"github.com/romeo-folie/trade-engine/internal/store"
)

type portfolioAlpacaExecutor struct {
	account execution.BrokerAccount
}

type concurrentAdmissionMarket struct{}

func (*concurrentAdmissionMarket) FetchQuote(_ context.Context, ticker string) (*data.Quote, error) {
	return &data.Quote{Ticker: ticker, Bid: 29.99, Ask: 30, Timestamp: time.Now().UTC()}, nil
}
func (*concurrentAdmissionMarket) FetchCandles(context.Context, string, data.Timeframe, time.Time, time.Time) ([]data.Candle, error) {
	return nil, nil
}
func (*concurrentAdmissionMarket) FetchHistory(context.Context, string, data.Timeframe, int) ([]data.Candle, error) {
	return nil, nil
}

type concurrentAdmissionBroker struct {
	mu          sync.Mutex
	buyingPower float64
	posts       int
	working     []execution.WorkingOrder
	byClient    map[string]execution.OrderResult
	readOrder   []string
}

func (*concurrentAdmissionBroker) BrokerName() string { return "alpaca" }
func (b *concurrentAdmissionBroker) AccountSnapshot(context.Context) (execution.BrokerAccount, error) {
	// This delay makes an omitted production admission lock deterministic:
	// all five workers would read the same pre-submit buying power.
	time.Sleep(20 * time.Millisecond)
	b.mu.Lock()
	defer b.mu.Unlock()
	b.readOrder = append(b.readOrder, "account")
	account := validAdmissionAccount(time.Now().UTC())
	account.Equity, account.LastEquity, account.Cash = 100, 100, 100
	account.BuyingPower, account.RegTBuyingPower = b.buyingPower, b.buyingPower
	account.Multiplier = 1
	return account, nil
}
func (*concurrentAdmissionBroker) PositionsSnapshot(context.Context) ([]execution.BrokerPosition, error) {
	return nil, nil
}
func (*concurrentAdmissionBroker) EquityCurve(context.Context, string, string) (execution.EquityHistory, error) {
	return execution.EquityHistory{}, nil
}
func (*concurrentAdmissionBroker) RecentFills(context.Context) ([]execution.BrokerOrder, error) {
	return nil, nil
}
func (*concurrentAdmissionBroker) StockAssetSnapshot(_ context.Context, symbol string) (execution.StockAsset, error) {
	return execution.StockAsset{
		Symbol: symbol, Status: "active", AssetClass: "us_equity", Tradable: true,
		TradableAvailable: true, Marginable: true, MarginableAvailable: true,
		Fractionable: false, FractionableAvailable: true, FetchedAt: time.Now().UTC(),
	}, nil
}
func (b *concurrentAdmissionBroker) GetWorkingOrders(context.Context) ([]execution.WorkingOrder, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.readOrder = append(b.readOrder, "working")
	return append([]execution.WorkingOrder(nil), b.working...), nil
}
func (b *concurrentAdmissionBroker) GetOrderByClientID(_ context.Context, clientID string) (execution.OrderResult, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if result, ok := b.byClient[clientID]; ok {
		return result, nil
	}
	return execution.OrderResult{}, execution.ErrOrderNotFound
}
func (b *concurrentAdmissionBroker) PlaceOrder(_ context.Context, req execution.OrderRequest) (execution.OrderResult, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	notional := req.Quantity * req.ReferencePrice
	if notional > b.buyingPower+1e-9 {
		return execution.OrderResult{}, fmt.Errorf("oversubscribed %.2f > %.2f", notional, b.buyingPower)
	}
	b.posts++
	b.buyingPower -= notional
	result := execution.OrderResult{BrokerOrderID: fmt.Sprintf("broker-%d", b.posts), Status: "accepted"}
	b.byClient[req.ClientOrderID] = result
	b.working = append(b.working, execution.WorkingOrder{
		OrderID: result.BrokerOrderID, TradeID: req.ClientOrderID, Ticker: req.Ticker,
		Side: "buy", Status: "pending", RemainingQty: req.Quantity,
	})
	return result, nil
}
func (*concurrentAdmissionBroker) CancelOrder(context.Context, string) error { return nil }
func (*concurrentAdmissionBroker) GetOrderStatus(context.Context, string) (execution.OrderResult, error) {
	return execution.OrderResult{}, nil
}
func (*concurrentAdmissionBroker) GetPositions(context.Context) ([]execution.Position, error) {
	return nil, nil
}
func (*concurrentAdmissionBroker) GetAccount(context.Context) (execution.AccountInfo, error) {
	return execution.AccountInfo{}, nil
}

func (*portfolioAlpacaExecutor) BrokerName() string { return "alpaca" }
func (*portfolioAlpacaExecutor) PlaceOrder(context.Context, execution.OrderRequest) (execution.OrderResult, error) {
	return execution.OrderResult{}, nil
}
func (*portfolioAlpacaExecutor) CancelOrder(context.Context, string) error { return nil }
func (*portfolioAlpacaExecutor) GetOrderStatus(context.Context, string) (execution.OrderResult, error) {
	return execution.OrderResult{}, nil
}
func (*portfolioAlpacaExecutor) GetPositions(context.Context) ([]execution.Position, error) {
	return nil, nil
}
func (*portfolioAlpacaExecutor) GetAccount(context.Context) (execution.AccountInfo, error) {
	return execution.AccountInfo{}, nil
}
func (e *portfolioAlpacaExecutor) AccountSnapshot(context.Context) (execution.BrokerAccount, error) {
	return e.account, nil
}

func TestSizeStockEntryRepresentativeBalances(t *testing.T) {
	tests := []struct {
		name     string
		equity   float64
		capacity float64
		ask      float64
		fraction bool
		want     float64
	}{
		{"437.25", 437.25, 1_000, 10, true, 2.18625},
		{"1000", 1_000, 2_000, 10, true, 5},
		{"3700", 3_700, 7_400, 10, true, 18.5},
		{"large", 1_000_000, 2_000_000, 10, true, 5_000},
		{"buying power caps cash", 100_000, 37, 10, true, 3.7},
		{"whole share floors", 1_000, 2_000, 13, false, 3},
		{"fractional floors six decimals", 1_000, 2_000, 11, true, 4.545454},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := sizeStockEntry(tt.equity, 5, tt.capacity, tt.ask, tt.fraction)
			if err != nil || math.Abs(got-tt.want) > 1e-9 {
				t.Fatalf("sizeStockEntry = %.9f, %v; want %.9f", got, err, tt.want)
			}
			if got*tt.ask > math.Min(tt.equity*.05, tt.capacity)+1e-9 {
				t.Fatalf("quantity rounded up: %.9f shares at %.2f", got, tt.ask)
			}
		})
	}
}

func TestMinimumWholeShareAllowedOnlyForValidSmallAccountRoundDown(t *testing.T) {
	if !minimumWholeShareAllowed(0, false, 1, 15, 1_000, 20, 20) {
		t.Fatal("valid $15 whole-share minimum did not pass $200 cap/capacity")
	}
	for _, tc := range []struct {
		name                               string
		qty, ask, equity, capacity, maxPct float64
		fractionable                       bool
	}{
		{"already sized", 1, 15, 1_000, 20, 20, false},
		{"fractionable", 0, 15, 1_000, 20, 20, true},
		{"over position cap", 0, 201, 1_000, 300, 20, false},
		{"over capacity", 0, 15, 1_000, 14, 20, false},
	} {
		if minimumWholeShareAllowed(tc.qty, tc.fractionable, 1, tc.ask, tc.equity, tc.capacity, tc.maxPct) {
			t.Fatalf("%s: invalid one-share minimum passed", tc.name)
		}
	}
}

func TestFiveConcurrentStockEntryWorkersCannotReuseFunds(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("integration database unavailable")
	}
	ctx := context.Background()
	_, _ = store.GetDB().Exec(ctx, `DELETE FROM stock_order_intents WHERE idempotency_key LIKE 'entry:concurrency-%'`)
	t.Cleanup(func() {
		_, _ = store.GetDB().Exec(context.Background(), `DELETE FROM stock_order_intents WHERE idempotency_key LIKE 'entry:concurrency-%'`)
	})
	broker := &concurrentAdmissionBroker{buyingPower: 100, byClient: make(map[string]execution.OrderResult)}
	coordinator := stockorders.New(broker)
	engine := &Engine{
		cfg:      &config.Config{Markets: config.MarketsConfig{Stocks: config.StocksConfig{ExpectedAccountNumber: "PA123"}}},
		executor: coordinator, marketData: &concurrentAdmissionMarket{},
	}
	var admitted int
	errorsCh := make(chan error, 5)
	var wg sync.WaitGroup
	for worker := range 5 {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			key := fmt.Sprintf("entry:concurrency-%d", worker)
			admission, _, effectiveKey, release, err := engine.beginStockEntryAdmission(ctx, key, "AAPL", 100)
			if err != nil {
				errorsCh <- err
				return
			}
			defer release()
			_, err = coordinator.PlaceOrder(ctx, execution.OrderRequest{
				Ticker: "AAPL", Side: execution.OrderSideBuy, Type: execution.OrderTypeMarket,
				Quantity: admission.Quantity, ReferencePrice: admission.Ask, IntentKey: effectiveKey,
			})
			if err == nil {
				admitted++
			}
		}(worker)
	}
	wg.Wait()
	close(errorsCh)
	var workerErrors []error
	for err := range errorsCh {
		workerErrors = append(workerErrors, err)
	}
	broker.mu.Lock()
	posts, remaining := broker.posts, broker.buyingPower
	broker.mu.Unlock()
	if admitted != 1 || posts != 1 || remaining != 10 {
		t.Fatalf("concurrent production admission reused funds: admitted=%d posts=%d remaining=%.2f errors=%v", admitted, posts, remaining, workerErrors)
	}
}

func TestStockAdmissionReadsWorkingOrdersBeforeBuyingPower(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("integration database unavailable")
	}
	broker := &concurrentAdmissionBroker{buyingPower: 100, byClient: make(map[string]execution.OrderResult)}
	engine := &Engine{
		cfg:      &config.Config{Markets: config.MarketsConfig{Stocks: config.StocksConfig{ExpectedAccountNumber: "PA123"}}},
		executor: stockorders.New(broker), marketData: &concurrentAdmissionMarket{},
	}
	_, _, _, release, _ := engine.beginStockEntryAdmission(context.Background(), "entry:read-order", "AAPL", 10)
	if release != nil {
		release()
	}
	broker.mu.Lock()
	defer broker.mu.Unlock()
	if len(broker.readOrder) < 2 || broker.readOrder[0] != "working" || broker.readOrder[1] != "account" {
		t.Fatalf("broker admission read order = %v, want working then account", broker.readOrder)
	}
}

func validAdmissionAccount(now time.Time) execution.BrokerAccount {
	return execution.BrokerAccount{
		AccountID: "account-id", AccountNumber: "PA123", Status: "ACTIVE", Currency: "USD",
		Equity: 1_000, LastEquity: 990, Cash: 0, BuyingPower: 1_500,
		RegTBuyingPower: 1_200, NonMarginableBuyingPower: 300, Multiplier: 2,
		LastEquityAvailable: true, RegTBuyingPowerAvailable: true,
		CashAvailable: true, BuyingPowerAvailable: true, MultiplierAvailable: true,
		NonMarginableBuyingPowerAvailable: true, FetchedAt: now,
	}
}

func TestValidateStockAccountApplicableBuyingPower(t *testing.T) {
	now := time.Now().UTC()
	t.Run("marginable Reg-T cap allows zero cash", func(t *testing.T) {
		got, err := validateStockAccount(validAdmissionAccount(now), true, "PA123", now)
		if err != nil || got != 1_200 {
			t.Fatalf("capacity = %.2f, %v; want 1200", got, err)
		}
	})
	t.Run("negative cash allowed on margin", func(t *testing.T) {
		acct := validAdmissionAccount(now)
		acct.Cash = -250
		if got, err := validateStockAccount(acct, true, "PA123", now); err != nil || got != 1_200 {
			t.Fatalf("capacity = %.2f, %v", got, err)
		}
	})
	t.Run("nonmarginable field", func(t *testing.T) {
		if got, err := validateStockAccount(validAdmissionAccount(now), false, "PA123", now); err != nil || got != 300 {
			t.Fatalf("capacity = %.2f, %v; want 300", got, err)
		}
	})
	t.Run("cash account is cash constrained", func(t *testing.T) {
		acct := validAdmissionAccount(now)
		acct.Multiplier, acct.Cash = 1, 175
		if got, err := validateStockAccount(acct, true, "PA123", now); err != nil || got != 175 {
			t.Fatalf("capacity = %.2f, %v; want 175", got, err)
		}
	})
}

func TestValidateStockAccountRejectsUnavailableOrMalformedState(t *testing.T) {
	now := time.Now().UTC()
	tests := []struct {
		name string
		edit func(*execution.BrokerAccount)
	}{
		{"missing last equity", func(a *execution.BrokerAccount) { a.LastEquityAvailable = false }},
		{"missing cash", func(a *execution.BrokerAccount) { a.CashAvailable = false }},
		{"missing buying power", func(a *execution.BrokerAccount) { a.BuyingPowerAvailable = false }},
		{"missing multiplier", func(a *execution.BrokerAccount) { a.MultiplierAvailable = false }},
		{"missing Reg-T", func(a *execution.BrokerAccount) { a.RegTBuyingPowerAvailable = false }},
		{"stale", func(a *execution.BrokerAccount) { a.FetchedAt = now.Add(-time.Minute) }},
		{"NaN equity", func(a *execution.BrokerAccount) { a.Equity = math.NaN() }},
		{"infinite BP", func(a *execution.BrokerAccount) { a.BuyingPower = math.Inf(1) }},
		{"negative BP", func(a *execution.BrokerAccount) { a.RegTBuyingPower = -1 }},
		{"identity mismatch", func(a *execution.BrokerAccount) { a.AccountNumber = "PA999" }},
		{"blocked", func(a *execution.BrokerAccount) { a.TradingBlocked = true }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			acct := validAdmissionAccount(now)
			tt.edit(&acct)
			if _, err := validateStockAccount(acct, true, "PA123", now); err == nil {
				t.Fatal("invalid account passed admission")
			}
		})
	}
}

func TestValidateStockAskRequiresFreshPositiveAsk(t *testing.T) {
	now := time.Now().UTC()
	if got, err := validateStockAsk(&data.Quote{Ticker: "AAPL", Ask: 101.25, Timestamp: now}, "AAPL", now); err != nil || got != 101.25 {
		t.Fatalf("fresh ask = %.2f, %v", got, err)
	}
	for _, quote := range []*data.Quote{
		nil,
		{Ticker: "AAPL", Ask: 0, Timestamp: now},
		{Ticker: "AAPL", Ask: math.NaN(), Timestamp: now},
		{Ticker: "AAPL", Ask: 100, Timestamp: now.Add(-time.Minute)},
	} {
		if _, err := validateStockAsk(quote, "AAPL", now); err == nil {
			t.Fatalf("invalid quote passed: %#v", quote)
		}
	}
}

func TestRecalculateStockEntryQuantityUsesFinalAsk(t *testing.T) {
	admission := stockEntryAdmission{
		Account:           execution.BrokerAccount{Equity: 1_000},
		Asset:             execution.StockAsset{Fractionable: true},
		RemainingCapacity: 500,
	}
	got, err := recalculateStockEntryQuantity(admission, 10, 125, 50)
	if err != nil {
		t.Fatalf("recalculate quantity: %v", err)
	}
	if got != 0.8 {
		t.Fatalf("final-ask quantity = %.6f, want 0.8", got)
	}
}

func TestRepricePreparedStockEntryRequestUsesFinalAdmissionValues(t *testing.T) {
	original := execution.OrderRequest{
		Ticker: "AAPL", Side: execution.OrderSideBuy, Type: execution.OrderTypeMarket,
		Quantity: 5, ReferencePrice: 100, IntentKey: "entry:prepared", ClientOrderID: "te-stock-prepared",
		OrderClass: "bracket", StopLossPrice: 95, TakeProfitPrice: 110,
	}
	got := repricePreparedStockEntryRequest(original, 4, 125)
	if got.Quantity != 4 || got.ReferencePrice != 125 {
		t.Fatalf("repriced request = %+v, want quantity 4 at 125", got)
	}
	if got.ClientOrderID != original.ClientOrderID || got.OrderClass != original.OrderClass || got.StopLossPrice != original.StopLossPrice {
		t.Fatalf("repricing changed durable order identity or policy: %+v", got)
	}
}

func TestApplyStockEntryReservations(t *testing.T) {
	now := time.Now().UTC()
	prepared := store.ActiveStockEntryReservation{
		IntentID: "prepared", IdempotencyKey: "entry:other", ClientOrderID: "te-stock-prepared",
		Ticker: "CHEAP", RemainingQty: 2, ReferencePrice: 10, CreatedAt: now,
	}
	got, err := applyStockEntryReservations(100, "entry:new", []store.ActiveStockEntryReservation{prepared}, nil)
	if err != nil || got != 80 {
		t.Fatalf("prepared reservation capacity = %.2f, %v; want 80", got, err)
	}

	working := []execution.WorkingOrder{{
		OrderID: "broker-1", TradeID: prepared.ClientOrderID, Ticker: prepared.Ticker,
		Side: "buy", Status: "pending", RemainingQty: prepared.RemainingQty,
	}}
	prepared.BrokerOrderID = "broker-1"
	if got, err := applyStockEntryReservations(100, "entry:new", []store.ActiveStockEntryReservation{prepared}, working); err != nil || got != 100 {
		t.Fatalf("broker-held reservation double deducted: %.2f, %v", got, err)
	}

	manual := []execution.WorkingOrder{{OrderID: "manual", TradeID: "manual-client", Ticker: "AAPL", Side: "buy", Status: "pending", RemainingQty: 1}}
	if _, err := applyStockEntryReservations(100, "entry:new", nil, manual); err != nil {
		t.Fatalf("manual working BUY blocked admission: %v", err)
	}
	engineUnknown := []execution.WorkingOrder{{OrderID: "unknown", TradeID: "te-stock-unknown", Ticker: "AAPL", Side: "buy", Status: "pending", RemainingQty: 1}}
	if _, err := applyStockEntryReservations(100, "entry:new", nil, engineUnknown); err == nil {
		t.Fatal("unattributed engine working BUY did not fail closed")
	}

	ambiguous := prepared
	ambiguous.BrokerOrderID = ""
	ambiguous.SubmittedAt = &now
	ambiguous.BrokerError = "response lost"
	if _, err := applyStockEntryReservations(100, "entry:new", []store.ActiveStockEntryReservation{ambiguous}, nil); err == nil || !strings.Contains(err.Error(), "unresolved broker truth") {
		t.Fatalf("ambiguous reservation = %v", err)
	}
	if got, err := applyStockEntryReservations(100, ambiguous.IdempotencyKey, []store.ActiveStockEntryReservation{ambiguous}, nil); err != nil || got != 100 {
		t.Fatalf("exact ambiguous replay charged itself: %.2f, %v", got, err)
	}
}

func TestApplyStockEntryReservationsRejectsBrokerMismatch(t *testing.T) {
	reservation := store.ActiveStockEntryReservation{
		IntentID: "intent", IdempotencyKey: "entry:one", ClientOrderID: "te-stock-one",
		BrokerOrderID: "broker-one", Ticker: "AAPL", RemainingQty: 2, ReferencePrice: 100,
	}
	base := execution.WorkingOrder{
		OrderID: "broker-one", TradeID: "te-stock-one", Ticker: "AAPL",
		Side: "buy", Status: "pending", RemainingQty: 2,
	}
	for _, test := range []struct {
		name string
		edit func(*execution.WorkingOrder)
	}{
		{"ticker", func(o *execution.WorkingOrder) { o.Ticker = "MSFT" }},
		{"status", func(o *execution.WorkingOrder) { o.Status = "filled" }},
		{"remaining", func(o *execution.WorkingOrder) { o.RemainingQty = 1 }},
		{"NaN remaining", func(o *execution.WorkingOrder) { o.RemainingQty = math.NaN() }},
		{"client identity", func(o *execution.WorkingOrder) { o.TradeID = "te-stock-other" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			order := base
			test.edit(&order)
			if _, err := applyStockEntryReservations(1_000, "entry:new", []store.ActiveStockEntryReservation{reservation}, []execution.WorkingOrder{order}); err == nil {
				t.Fatal("broker/local mismatch passed admission")
			}
		})
	}
}

func TestExistingBrokerHeldEntryDoesNotRequireUnheldBuyingPower(t *testing.T) {
	reservation := store.ActiveStockEntryReservation{
		IntentID: "intent", IdempotencyKey: "entry:existing", ClientOrderID: "te-stock-existing",
		BrokerOrderID: "broker-existing", Ticker: "AAPL", RemainingQty: 5, ReferencePrice: 100,
	}
	working := execution.WorkingOrder{
		OrderID: reservation.BrokerOrderID, TradeID: reservation.ClientOrderID,
		Ticker: reservation.Ticker, Side: "buy", Status: "pending", RemainingQty: 5,
	}
	if !existingEntryBrokerHeld(reservation.IdempotencyKey, []store.ActiveStockEntryReservation{reservation}, []execution.WorkingOrder{working}) {
		t.Fatal("exact broker-held replay was not recognized")
	}
	if got, err := applyStockEntryReservations(0, reservation.IdempotencyKey, []store.ActiveStockEntryReservation{reservation}, []execution.WorkingOrder{working}); err != nil || got != 0 {
		t.Fatalf("broker-held replay required fresh unheld capacity: %.2f, %v", got, err)
	}
}

func TestExistingEntryBrokerReplayUsesClientLookupAfterWorkingOrderFills(t *testing.T) {
	broker := &concurrentAdmissionBroker{byClient: map[string]execution.OrderResult{
		"te-stock-existing": {BrokerOrderID: "broker-existing", Status: "filled", FilledQty: 5},
	}}
	engine := &Engine{executor: broker}
	now := time.Now().UTC()
	replayed, err := engine.existingEntryBrokerReplay(context.Background(), "entry:existing", &execution.OrderRequest{
		ClientOrderID: "te-stock-existing",
	}, []store.ActiveStockEntryReservation{{
		IdempotencyKey: "entry:existing", ClientOrderID: "te-stock-existing", SubmittedAt: &now,
	}}, nil)
	if err != nil {
		t.Fatalf("existingEntryBrokerReplay error: %v", err)
	}
	if !replayed {
		t.Fatal("broker-filled durable entry was re-admitted as a fresh BUY")
	}
}

func TestExistingEntryBrokerReplayFailsClosedWhenSubmittedEntryCannotBeFound(t *testing.T) {
	broker := &concurrentAdmissionBroker{byClient: make(map[string]execution.OrderResult)}
	engine := &Engine{executor: broker}
	now := time.Now().UTC()
	replayed, err := engine.existingEntryBrokerReplay(context.Background(), "entry:existing", &execution.OrderRequest{
		ClientOrderID: "te-stock-existing",
	}, []store.ActiveStockEntryReservation{{
		IdempotencyKey: "entry:existing", ClientOrderID: "te-stock-existing", SubmittedAt: &now,
	}}, nil)
	if err != nil {
		t.Fatalf("existingEntryBrokerReplay error: %v", err)
	}
	if !replayed {
		t.Fatal("ambiguous submitted entry was re-admitted as a fresh BUY")
	}
}

func TestPausedPreparedEntryKeepsReservationRecoverableAfterBrokerMiss(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("integration database unavailable")
	}
	ctx := context.Background()
	signalID, err := store.InsertSignal(ctx, store.Signal{
		Ticker: "PAUSEDPREP", Action: "BUY", Strategy: "test", SignalPrice: 100,
		StopLoss: 90, TakeProfit: 120, Confidence: .9, SizePct: 1, Status: "blocked",
	})
	if err != nil {
		t.Fatal(err)
	}
	intentKey := "entry:" + signalID
	request := execution.OrderRequest{
		Ticker: "PAUSEDPREP", Side: execution.OrderSideBuy, Type: execution.OrderTypeMarket,
		Quantity: 1, IntentKey: intentKey, ClientOrderID: "paused-prepared-" + strings.ReplaceAll(signalID, "-", ""),
	}
	payload, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	var intentID string
	if err := store.GetDB().QueryRow(ctx, `SELECT uuid_generate_v4()::text`).Scan(&intentID); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateStockOrderIntent(ctx, store.StockOrderIntent{
		ID: intentID, IdempotencyKey: intentKey, ClientOrderID: request.ClientOrderID,
		Ticker: request.Ticker, Side: "buy", OrderType: "market", RequestedQty: request.Quantity,
		RequestPayload: payload, PayloadHash: "paused-prepared", Status: "pending",
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = store.GetDB().Exec(context.Background(), `UPDATE stock_order_intents SET purpose='superseded' WHERE id=$1::uuid`, intentID)
	})

	if active, err := store.HasPreparedStockEntry(ctx, signalID); err != nil || !active {
		t.Fatalf("paused prepared reservation is not recoverable: active=%v err=%v", active, err)
	}
	intent, err := store.GetStockOrderIntentByKey(ctx, intentKey)
	if err != nil || intent == nil || intent.Status != "pending" {
		t.Fatalf("paused prepared intent = %#v err=%v, want pending", intent, err)
	}
}

func TestAttachedPartialReservationDoesNotConsumeSecondTickerSlot(t *testing.T) {
	reservations := []store.ActiveStockEntryReservation{
		{IdempotencyKey: "entry:partial", TradeID: "trade-one", Ticker: "AAPL", RemainingQty: 5, ReferencePrice: 100},
		{IdempotencyKey: "entry:prepared", Ticker: "MSFT", RemainingQty: 1, ReferencePrice: 200},
	}
	trades := reservationTrades(reservations, "")
	if len(trades) != 1 || trades[0].Ticker != "MSFT" {
		t.Fatalf("attached partial created a second occupancy row: %+v", trades)
	}
	if got := attachedReservationNotional(reservations, "", "aapl", 120); got != 600 {
		t.Fatalf("attached partial exposure = %.2f, want 600", got)
	}
	if got := attachedReservationNotional(reservations, "entry:partial", "AAPL", 120); got != 0 {
		t.Fatalf("replayed entry counted its own remainder: %.2f", got)
	}
	if got := brokerLongNotional([]execution.BrokerPosition{{Symbol: "AAPL", Side: "long", Quantity: 5}, {Symbol: "AAPL", Side: "short", Quantity: 3}}, "AAPL", 120); got != 600 {
		t.Fatalf("broker long exposure = %.2f, want 600", got)
	}
	if got := pendingStockEntryNotional(reservations, nil, "", "AAPL", 120); got != 600 {
		t.Fatalf("pending entry exposure = %.2f, want 600", got)
	}
}

func TestMergedTickerExposureDoesNotDoubleCountPendingEntries(t *testing.T) {
	// Local exposure already includes the 200 pending shares. A stale broker
	// snapshot with 300 held shares may add those pending shares once, not twice.
	if got := mergedTickerExposure(500, 300, 200); got != 500 {
		t.Fatalf("complete local exposure = %.2f, want 500", got)
	}
	if got := mergedTickerExposure(400, 500, 200); got != 700 {
		t.Fatalf("stale local fill merge = %.2f, want 700", got)
	}
}

func TestBrokerLongPositionSuppliesPyramidOwnershipForUnadoptedLong(t *testing.T) {
	quantity, averageEntry, found, valid := brokerLongPosition([]execution.BrokerPosition{{
		Symbol: "AAPL", Side: "long", Quantity: 5, AvgEntryPrice: 100,
	}}, "AAPL")
	if !found || !valid || quantity != 5 || averageEntry != 100 {
		t.Fatalf("broker long context = (%.2f, %.2f, %t, %t)", quantity, averageEntry, found, valid)
	}
	in := buildRiskInput(config.RiskConfig{PyramidEnabled: false}, "BUY", "AAPL", .9, 1, 95, 120, 110,
		portfolio.State{TotalValue: 1_000}, nil)
	in.HoldsLong = found
	in.LongQuantity = quantity
	in.LongAvgEntryPrice = averageEntry
	in.OpenPositions = 1
	in.OpenByTicker = map[string]int{"AAPL": 1}
	if results, passed := risk.CheckAll(in); passed || !strings.Contains(risk.BlockReason(results), "pyramid_disabled") {
		t.Fatalf("unadopted broker long bypassed pyramid policy: passed=%t results=%+v", passed, results)
	}
}

func TestBrokerLongPositionFailsClosedWithoutAverageEntry(t *testing.T) {
	_, _, found, valid := brokerLongPosition([]execution.BrokerPosition{{
		Symbol: "AAPL", Side: "long", Quantity: 5,
	}}, "AAPL")
	if !found || valid {
		t.Fatalf("missing broker cost basis = found %t, valid %t; want true, false", found, valid)
	}
}

func TestMergeBrokerLongOccupancyIncludesUnadoptedSymbols(t *testing.T) {
	in := buildRiskInput(config.RiskConfig{}, "BUY", "NVDA", .9, 1, 95, 120, 110,
		portfolio.State{TotalValue: 1_000}, []store.Trade{{Ticker: "MSFT", Direction: "long", Quantity: 1, EntryPrice: 100}})
	if err := mergeBrokerLongOccupancy(&in, []execution.BrokerPosition{
		{Symbol: "MSFT", Side: "long", Quantity: 1, AvgEntryPrice: 100},
		{Symbol: "AAPL", Side: "long", Quantity: 1, AvgEntryPrice: 200},
		{Symbol: "NVDA", Side: "long", Quantity: 2, AvgEntryPrice: 100},
	}, "NVDA", []store.Trade{{Ticker: "MSFT", Direction: "long", Quantity: 1}}); err != nil {
		t.Fatal(err)
	}
	if in.OpenPositions != 3 || in.OpenByTicker["MSFT"] != 1 || in.OpenByTicker["AAPL"] != 1 || in.OpenByTicker["NVDA"] != 1 {
		t.Fatalf("broker occupancy = positions:%d by_ticker:%+v", in.OpenPositions, in.OpenByTicker)
	}
	if !in.HoldsLong || in.LongQuantity != 2 || in.LongAvgEntryPrice != 100 {
		t.Fatalf("target broker pyramid context = %+v", in)
	}
	if err := mergeBrokerLongOccupancy(&in, []execution.BrokerPosition{{Symbol: "TSLA", Side: "short", Quantity: -1}}, "NVDA", nil); err != nil {
		t.Fatal(err)
	}
	if in.OpenPositions != 4 {
		t.Fatalf("broker short did not consume overall capacity: %+v", in)
	}
	local := risk.CheckInput{HoldsLong: true, LongQuantity: 3, LongAvgEntryPrice: 125}
	if err := mergeBrokerLongOccupancy(&local, []execution.BrokerPosition{{Symbol: "NVDA", Side: "long", Quantity: 2, AvgEntryPrice: 100}}, "NVDA", nil); err != nil {
		t.Fatal(err)
	}
	if local.LongQuantity != 3 || local.LongAvgEntryPrice != 125 {
		t.Fatalf("stale broker snapshot overwrote fresher local pyramid basis: %+v", local)
	}
	for _, position := range []execution.BrokerPosition{
		{Symbol: "BADQ", Side: "long", Quantity: math.NaN()},
		{Symbol: "BADS", Side: "unknown", Quantity: 1},
	} {
		if err := mergeBrokerLongOccupancy(&risk.CheckInput{}, []execution.BrokerPosition{position}, "NVDA", nil); err == nil {
			t.Fatalf("malformed broker position passed admission: %+v", position)
		}
	}

	pending := risk.CheckInput{OpenPositions: 1, OpenByTicker: map[string]int{"AAPL": 1}}
	if err := mergeBrokerLongOccupancy(&pending, []execution.BrokerPosition{{Symbol: "AAPL", Side: "long", Quantity: 1, AvgEntryPrice: 100}}, "NVDA", nil); err != nil {
		t.Fatal(err)
	}
	if pending.OpenPositions != 2 || pending.OpenByTicker["AAPL"] != 2 {
		t.Fatalf("broker long was collapsed into pending BUY: %+v", pending)
	}
	if err := mergeBrokerLongOccupancy(&risk.CheckInput{}, []execution.BrokerPosition{{Symbol: "NVDA", Side: "short", Quantity: -1}}, "NVDA", nil); err == nil {
		t.Fatal("target broker short did not block BUY admission")
	}
}

func TestManualWorkingBuyConsumesPositionCapacityWithoutFundsDeduction(t *testing.T) {
	working := []execution.WorkingOrder{{
		OrderID: "manual", TradeID: "manual-client", Ticker: "AAPL",
		AssetClass: "us_equity", Side: "buy", Status: "pending", RemainingQty: 2,
	}, {
		OrderID: "option", TradeID: "manual-option", Ticker: "SPY260717C00551000",
		AssetClass: "us_option", Side: "buy", Status: "pending", RemainingQty: 1, Price: 2,
	}}
	trades := manualWorkingEntryTrades(working, nil, 101)
	if len(trades) != 1 || trades[0].Ticker != "AAPL" || trades[0].Quantity != 2 || trades[0].EntryPrice != 101 {
		t.Fatalf("manual working BUY absent from risk projection: %+v", trades)
	}
	if got, err := applyStockEntryReservations(500, "entry:new", nil, working); err != nil || got != 500 {
		t.Fatalf("manual working BUY was locally deducted despite broker BP hold: %.2f, %v", got, err)
	}
}

func TestDeferredSignalPathHonorsFlagExceptForPreparedRecovery(t *testing.T) {
	if deferredSignalPathEnabled(false, false) {
		t.Fatal("ordinary deferred signal remained enabled after feature flag was disabled")
	}
	if !deferredSignalPathEnabled(true, false) {
		t.Fatal("ordinary deferred signal was disabled while feature flag was enabled")
	}
	if !deferredSignalPathEnabled(false, true) {
		t.Fatal("prepared recovery was disabled with the ordinary defer flag")
	}
}

func TestStockAdmissionFreshnessRecheckedAtRelease(t *testing.T) {
	now := time.Now().UTC()
	admission := stockEntryAdmission{
		BrokerAuthoritative: true,
		Account:             execution.BrokerAccount{FetchedAt: now},
		Asset:               execution.StockAsset{FetchedAt: now},
		QuoteAt:             now,
	}
	if err := validateStockAdmissionFreshness(admission, now.Add(stockAdmissionMaxAge+time.Millisecond)); err == nil {
		t.Fatal("aged admission snapshots were released")
	}
	admission.BrokerHeldReplay = true
	if err := validateStockAdmissionFreshness(admission, now.Add(time.Hour)); err != nil {
		t.Fatalf("lookup-only broker-held replay required fresh POST admission: %v", err)
	}
}

func TestBrokerHeldReplayBypassesFreshEntryRiskRejection(t *testing.T) {
	in := risk.CheckInput{
		Cfg: config.RiskConfig{
			MinRiskReward: 10,
		},
		Action: "BUY", Ticker: "AAPL", Confidence: 0, SizePct: 100,
		SignalPrice: 100, StopLoss: 99, TakeProfit: 101,
		PortfolioValue: 1_000, Cash: 0, PeakValue: 1_000,
		ProposedQuantity: 10, ProposedNotional: 1_000,
	}
	if _, passed := checkStockEntryReleaseRisk(stockEntryAdmission{}, in); passed {
		t.Fatal("fresh entry unexpectedly passed deliberately failing risk input")
	}
	if results, passed := checkStockEntryReleaseRisk(stockEntryAdmission{BrokerHeldReplay: true}, in); !passed || len(results) != 0 {
		t.Fatalf("broker-held replay was blocked by fresh entry risk: passed=%v results=%+v", passed, results)
	}
}

func TestBrokerStockRiskStatePreservesRealizedPnL(t *testing.T) {
	current := portfolio.State{TotalValue: 900, Cash: 100, RealizedPnLDay: -25, PeakValue: 1_100}
	got := brokerStockRiskState(current, stockEntryAdmission{
		BrokerAuthoritative: true,
		Account:             execution.BrokerAccount{Equity: 800, LastEquity: 1_000, Cash: -50},
		PeakEquity:          1_200,
	})
	if got.TotalValue != 800 || got.Cash != -50 || got.PeakValue != 1_200 {
		t.Fatalf("broker risk state did not use fresh equity/cash/peak: %+v", got)
	}
	if got.RealizedPnLDay != -25 {
		t.Fatalf("broker equity movement replaced realized P&L: %.2f", got.RealizedPnLDay)
	}
}

func TestAlpacaRefreshAlwaysLoadsLocalRealizedPnL(t *testing.T) {
	if !shouldLoadLocalRealizedPnL(true, true, 0) || !shouldLoadLocalRealizedPnL(true, true, 125) {
		t.Fatal("Alpaca refresh skipped local realized P&L")
	}
	if shouldLoadLocalRealizedPnL(true, false, 125) || !shouldLoadLocalRealizedPnL(true, false, 0) {
		t.Fatal("non-authoritative realized P&L fallback changed")
	}
	if shouldLoadLocalRealizedPnL(false, true, 0) {
		t.Fatal("DB-optional refresh attempted to load local realized P&L")
	}
}

func TestDurableStockEntryReplayNeverRebuildsBracketShape(t *testing.T) {
	if bracketed, mayBuild := stockEntryBracketReplayState(true, execution.OrderRequest{}); bracketed || mayBuild {
		t.Fatalf("plain replay bracket state = (%v,%v), want (false,false)", bracketed, mayBuild)
	}
	if bracketed, mayBuild := stockEntryBracketReplayState(true, execution.OrderRequest{OrderClass: "bracket"}); !bracketed || mayBuild {
		t.Fatalf("bracket replay state = (%v,%v), want (true,false)", bracketed, mayBuild)
	}
	if bracketed, mayBuild := stockEntryBracketReplayState(false, execution.OrderRequest{}); bracketed || !mayBuild {
		t.Fatalf("new request bracket state = (%v,%v), want (false,true)", bracketed, mayBuild)
	}
}

func TestDurableStockEntryReplayRestoresPersistedExitPolicy(t *testing.T) {
	sig := store.Signal{StopLoss: 80, TakeProfit: 150}
	hydrateStockEntryReplayPolicy(&sig, execution.OrderRequest{PolicyStopLoss: 95, PolicyTakeProfit: 120})
	if sig.StopLoss != 95 || sig.TakeProfit != 120 {
		t.Fatalf("replay policy = stop %.2f target %.2f, want 95/120", sig.StopLoss, sig.TakeProfit)
	}
}

func TestStockAccountPeakResetsWithAccountIdentity(t *testing.T) {
	engine := &Engine{stockAccountPeakID: "old", stockAccountPeak: 10_000}
	account := validAdmissionAccount(time.Now().UTC())
	if engine.stockAccountPeakID == account.AccountID {
		t.Fatal("test setup reused account identity")
	}
	engine.updateStockAccountPeak(account.AccountID, account.Equity)
	if engine.stockAccountPeak != account.Equity {
		t.Fatalf("peak = %.2f, want new account equity %.2f", engine.stockAccountPeak, account.Equity)
	}
}

func TestStockAccountPeakPreservesSameAccountTrackerHighAndRejectsCrossAccountPeak(t *testing.T) {
	engine := &Engine{stockAccountPeakID: "account-a", stockAccountPeak: 8_000}
	engine.updateStockAccountPeak("account-a", 10_000)
	engine.updateStockAccountPeak("account-a", 8_000)
	if engine.stockAccountPeak != 10_000 {
		t.Fatalf("same-account peak = %.2f, want tracker high 10000", engine.stockAccountPeak)
	}
	engine.updateStockAccountPeak("account-b", 2_000)
	if engine.stockAccountPeak != 2_000 || engine.stockAccountPeakID != "account-b" {
		t.Fatalf("switched account inherited old peak: id=%q peak=%.2f", engine.stockAccountPeakID, engine.stockAccountPeak)
	}
	engine.updateStockAccountPeak("account-b", 2_100)
	if engine.stockAccountPeak != 2_100 {
		t.Fatalf("old tracker peak leaked after account switch: %.2f", engine.stockAccountPeak)
	}
}

func TestRefreshPortfolioPreservesZeroCashAndRejectsInvalidBrokerSnapshot(t *testing.T) {
	now := time.Now().UTC()
	valid := validAdmissionAccount(now)
	valid.AccountID = fmt.Sprintf("refresh-test-%d", time.Now().UnixNano())
	valid.Equity = 1_250
	valid.LastEquity = 1_200
	exec := &portfolioAlpacaExecutor{account: valid}
	tracker := portfolio.NewTracker(time.Hour)
	tracker.Update(nil, 500, 500, 0, 0)
	engine := &Engine{cfg: &config.Config{}, executor: exec, portfolio: tracker}

	engine.refreshPortfolio(context.Background())
	state := tracker.CurrentState()
	if state.Cash != 0 || state.TotalValue != 1_250 || state.DayPnL != 50 {
		t.Fatalf("valid zero-cash snapshot fell back to configured capital: %+v", state)
	}

	previous := state
	exec.account.LastEquityAvailable = false
	exec.account.Equity = 99_999
	engine.refreshPortfolio(context.Background())
	if got := tracker.CurrentState(); got.Cash != previous.Cash || got.TotalValue != previous.TotalValue || got.DayPnL != previous.DayPnL {
		t.Fatalf("invalid account overwrote prior portfolio: before=%+v after=%+v", previous, got)
	}
}

func TestRefreshPortfolioMaintainsAccountScopedBrokerPeak(t *testing.T) {
	now := time.Now().UTC()
	account := validAdmissionAccount(now)
	account.AccountID = fmt.Sprintf("refresh-peak-test-%d", time.Now().UnixNano())
	account.Equity, account.LastEquity = 10_000, 10_000
	exec := &portfolioAlpacaExecutor{account: account}
	engine := &Engine{cfg: &config.Config{}, executor: exec, portfolio: portfolio.NewTracker(time.Hour)}
	engine.refreshPortfolio(context.Background())
	exec.account.Equity, exec.account.LastEquity = 8_000, 8_000
	exec.account.FetchedAt = time.Now().UTC()
	engine.refreshPortfolio(context.Background())
	if peak := engine.stockPeak(account.AccountID); peak != 10_000 {
		t.Fatalf("same-account refresh peak = %.2f, want 10000", peak)
	}
	exec.account.AccountID, exec.account.AccountNumber = "new-id", "PA999"
	exec.account.Equity, exec.account.LastEquity = 2_000, 2_000
	exec.account.FetchedAt = time.Now().UTC()
	engine.refreshPortfolio(context.Background())
	if peak := engine.stockPeak("new-id"); peak != 2_000 {
		t.Fatalf("switched-account refresh peak = %.2f, want 2000", peak)
	}
}

func TestPreparedEntryObserverQueuesFreshAdmissionWithoutReleasingReservation(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("integration database unavailable")
	}
	ctx := context.Background()
	signalID, err := store.InsertSignal(ctx, store.Signal{
		Ticker: "PREPQUEUE", Action: "BUY", Strategy: "test", SignalPrice: 100,
		StopLoss: 90, TakeProfit: 120, Confidence: .9, SizePct: 1, Status: "approved",
	})
	if err != nil {
		t.Fatal(err)
	}
	engine := &Engine{}
	engine.PreparedStockEntryNeedsAdmission(ctx, "entry:"+signalID)
	if err := store.UpdateSignalStatus(ctx, signalID, "blocked", "temporary admission failure"); err != nil {
		t.Fatal(err)
	}
	engine.PreparedStockEntryNeedsAdmission(ctx, "entry:"+signalID)
	queuedSignal, err := store.GetSignalByID(ctx, signalID)
	if err != nil {
		t.Fatal(err)
	}
	if queuedSignal == nil || queuedSignal.Status != "pending" {
		t.Fatalf("blocked prepared signal was not requeued: %#v", queuedSignal)
	}
	deferred, err := store.GetDeferredSignals(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, signal := range deferred {
		if signal.ID == signalID {
			return
		}
	}
	t.Fatal("prepared entry was not queued for fresh admission")
}

func TestPreparedEntryObserverRetiresLegacyGenerationWhenStrictPolicyEnabled(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("integration database unavailable")
	}
	ctx := context.Background()
	signalID, err := store.InsertSignal(ctx, store.Signal{
		Ticker: "PREPLEGACY", Action: "BUY", Strategy: "test", SignalPrice: 100,
		StopLoss: 90, TakeProfit: 120, Confidence: .9, SizePct: 1, Status: "approved",
	})
	if err != nil {
		t.Fatal(err)
	}
	intentKey := "entry:" + signalID
	request := execution.OrderRequest{
		Ticker: "PREPLEGACY", Side: execution.OrderSideBuy, Type: execution.OrderTypeMarket,
		Quantity: 1, IntentKey: intentKey, ClientOrderID: "legacy-prepared-" + strings.ReplaceAll(signalID, "-", ""),
	}
	payload, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	var intentID string
	if err := store.GetDB().QueryRow(ctx, `SELECT uuid_generate_v4()::text`).Scan(&intentID); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateStockOrderIntent(ctx, store.StockOrderIntent{
		ID: intentID, IdempotencyKey: intentKey, ClientOrderID: request.ClientOrderID,
		Ticker: request.Ticker, Side: "buy", OrderType: "market", RequestedQty: request.Quantity,
		RequestPayload: payload, PayloadHash: "legacy-prepared-policy", Status: "pending",
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = store.GetDB().Exec(context.Background(), `UPDATE stock_order_intents SET purpose='superseded' WHERE id=$1::uuid`, intentID)
	})

	engine := &Engine{cfg: &config.Config{}}
	engine.cfg.Risk.BracketExitsEnabled = true
	engine.PreparedStockEntryNeedsAdmission(ctx, intentKey)

	intent, err := store.GetStockOrderIntentByKey(ctx, intentKey)
	if err != nil || intent == nil || intent.Status != "rejected" {
		t.Fatalf("legacy prepared intent = %#v err=%v, want retired", intent, err)
	}
	if active, err := store.HasPreparedStockEntry(ctx, signalID); err != nil || active {
		t.Fatalf("legacy prepared reservation remains active: active=%v err=%v", active, err)
	}
	signal, err := store.GetSignalByID(ctx, signalID)
	if err != nil || signal == nil || signal.Status != "pending" {
		t.Fatalf("signal after retiring legacy prepared generation = %#v err=%v, want queued pending", signal, err)
	}
	deferred, err := store.GetDeferredSignals(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, queued := range deferred {
		if queued.ID == signalID {
			return
		}
	}
	t.Fatal("signal was not queued for a fresh strict admission")
}

func TestExpiredSignalRetiresPreparedEntryOnlyAfterBrokerMissCallback(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("integration database unavailable")
	}
	ctx := context.Background()
	signalID, err := store.InsertSignal(ctx, store.Signal{
		Ticker: "PREPEXPIRED", Action: "BUY", Strategy: "test", SignalPrice: 100,
		StopLoss: 90, TakeProfit: 120, Confidence: .9, SizePct: 1, Status: "pending",
		Deferred: true, ExpiresAt: time.Now().Add(-time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	intentKey := "entry:" + signalID
	clientID := "te-stock-expired-" + strings.ReplaceAll(signalID, "-", "")
	req := execution.OrderRequest{
		Ticker: "PREPEXPIRED", Side: execution.OrderSideBuy, Type: execution.OrderTypeMarket,
		Quantity: 1, ReferencePrice: 100, IntentKey: intentKey, ClientOrderID: clientID,
	}
	payload, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	var intentID string
	if err := store.GetDB().QueryRow(ctx, `SELECT uuid_generate_v4()::text`).Scan(&intentID); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateStockOrderIntent(ctx, store.StockOrderIntent{
		ID: intentID, IdempotencyKey: intentKey, ClientOrderID: clientID,
		Ticker: req.Ticker, Side: "buy", OrderType: "market", RequestedQty: 1,
		RequestPayload: payload, PayloadHash: "expired-prepared", Status: "pending",
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = store.GetDB().Exec(context.Background(), `
			UPDATE stock_order_intents SET purpose='superseded' WHERE id=$1::uuid`, intentID)
	})

	(&Engine{}).expireStaleSignals(ctx)
	signal, err := store.GetSignalByID(ctx, signalID)
	if err != nil {
		t.Fatal(err)
	}
	if signal == nil || signal.Status != "expired" {
		t.Fatalf("signal = %#v, want expired", signal)
	}
	reservations, err := store.ListActiveStockEntryReservations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	preserved := false
	for _, reservation := range reservations {
		if reservation.IntentID == intentID {
			if reservation.SubmittedAt != nil || reservation.BrokerOrderID != "" {
				t.Fatalf("prepared reservation unexpectedly changed: %#v", reservation)
			}
			preserved = true
			break
		}
	}
	if !preserved {
		t.Fatal("signal expiry retired prepared intent without broker truth")
	}
	(&Engine{}).PreparedStockEntryNeedsAdmission(ctx, intentKey)
	intent, err := store.GetStockOrderIntentByKey(ctx, intentKey)
	if err != nil {
		t.Fatal(err)
	}
	if intent == nil || intent.Status != "rejected" {
		t.Fatalf("broker-missing expired intent = %#v, want rejected", intent)
	}
	if active, err := store.HasPreparedStockEntry(ctx, signalID); err != nil || active {
		t.Fatalf("broker-missing expired intent remains active: active=%v err=%v", active, err)
	}
}

func TestTerminalFilledEntryIsNeverReplayed(t *testing.T) {
	payload, err := json.Marshal(execution.OrderRequest{
		Ticker: "AAPL", Side: execution.OrderSideBuy, Type: execution.OrderTypeMarket,
		Quantity: 1, IntentKey: "entry:done",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, status := range []string{"filled", "cancelled", "rejected"} {
		_, err := stockEntryRequestFromIntent(&store.StockOrderIntent{
			Status: status, FilledQty: 0.5, RequestPayload: payload,
		}, "entry:done", "AAPL")
		if !errors.Is(err, errStockEntryIntentComplete) {
			t.Fatalf("terminal %s partial/full fill replay result = %v", status, err)
		}
	}
	req, err := stockEntryRequestFromIntent(&store.StockOrderIntent{Status: "partial", FilledQty: 0.5, RequestPayload: payload}, "entry:done", "AAPL")
	if err != nil || req == nil || req.IntentKey != "entry:done" {
		t.Fatalf("nonterminal partial did not exact-replay: %#v, %v", req, err)
	}
}

func TestDurableEntryReplayPayloadMustMatchCurrentSignal(t *testing.T) {
	base := execution.OrderRequest{
		Ticker: "AAPL", Side: execution.OrderSideBuy, Type: execution.OrderTypeMarket,
		Quantity: 1, IntentKey: "entry:one",
	}
	for _, test := range []struct {
		name string
		edit func(*execution.OrderRequest)
	}{
		{"ticker", func(r *execution.OrderRequest) { r.Ticker = "MSFT" }},
		{"side", func(r *execution.OrderRequest) { r.Side = execution.OrderSideSell }},
		{"type", func(r *execution.OrderRequest) { r.Type = execution.OrderTypeLimit }},
		{"intent", func(r *execution.OrderRequest) { r.IntentKey = "entry:other" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			req := base
			test.edit(&req)
			payload, err := json.Marshal(req)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := stockEntryRequestFromIntent(&store.StockOrderIntent{Status: "pending", RequestPayload: payload}, "entry:one", "AAPL"); err == nil {
				t.Fatal("corrupt replay payload passed")
			}
		})
	}
}

func TestPyramidTighteningFeedsExactDollarRisk(t *testing.T) {
	stop := pyramidTightenedStop(80, 90, 95, 100)
	if stop != 95 {
		t.Fatalf("tightened stop = %.2f, want 95", stop)
	}
	in := risk.CheckInput{
		Cfg: config.RiskConfig{
			ConfidenceThreshold: 0, MinRiskReward: 0, MaxPositionPct: 100,
			MaxDollarRiskPct: 0.4, MaxOpenPositions: 10, MaxDrawdownPct: 100,
		}, Action: "BUY",
		PortfolioValue: 1_000, SignalPrice: 100, StopLoss: stop,
		ProposedQuantity: 1,
	}
	results, passed := risk.CheckAll(in)
	if passed || !strings.Contains(risk.BlockReason(results), "$-at-risk") {
		t.Fatalf("tightened exact dollar risk did not block: passed=%v results=%+v", passed, results)
	}
}
