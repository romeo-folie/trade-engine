package engine

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/cost"
	"github.com/romeo-folie/trade-engine/internal/events"
	"github.com/romeo-folie/trade-engine/internal/execution"
	"github.com/romeo-folie/trade-engine/internal/stockorders"
	"github.com/romeo-folie/trade-engine/internal/store"
)

type unfilledIntentBroker struct{ result execution.OrderResult }

func (b *unfilledIntentBroker) PlaceOrder(context.Context, execution.OrderRequest) (execution.OrderResult, error) {
	return execution.OrderResult{BrokerOrderID: "unfilled-entry", Status: "accepted"}, nil
}

type timedOutIntentBroker struct {
	mu            sync.Mutex
	result        execution.OrderResult
	cancelResults []execution.OrderResult
	cancels       int
}

func (b *timedOutIntentBroker) PlaceOrder(context.Context, execution.OrderRequest) (execution.OrderResult, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.result, nil
}
func (b *timedOutIntentBroker) GetOrderByClientID(context.Context, string) (execution.OrderResult, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.result, nil
}
func (b *timedOutIntentBroker) GetOrderStatus(context.Context, string) (execution.OrderResult, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.result, nil
}
func (b *timedOutIntentBroker) CancelOrder(context.Context, string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.cancels++
	if b.cancels <= len(b.cancelResults) {
		b.result = b.cancelResults[b.cancels-1]
	} else {
		b.result.Status = "cancelled"
	}
	return nil
}
func (*timedOutIntentBroker) GetPositions(context.Context) ([]execution.Position, error) {
	return nil, nil
}
func (*timedOutIntentBroker) GetAccount(context.Context) (execution.AccountInfo, error) {
	return execution.AccountInfo{}, nil
}
func (*timedOutIntentBroker) ReplaceStopOrder(context.Context, string, float64) (execution.OrderResult, error) {
	return execution.OrderResult{}, errors.New("unused")
}

func (b *timedOutIntentBroker) cancelCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.cancels
}
func (b *unfilledIntentBroker) GetOrderByClientID(context.Context, string) (execution.OrderResult, error) {
	return b.result, nil
}
func (b *unfilledIntentBroker) GetOrderStatus(context.Context, string) (execution.OrderResult, error) {
	return b.result, nil
}
func (*unfilledIntentBroker) CancelOrder(context.Context, string) error { return nil }
func (*unfilledIntentBroker) GetPositions(context.Context) ([]execution.Position, error) {
	return nil, nil
}
func (*unfilledIntentBroker) GetAccount(context.Context) (execution.AccountInfo, error) {
	return execution.AccountInfo{}, nil
}
func (*unfilledIntentBroker) ReplaceStopOrder(context.Context, string, float64) (execution.OrderResult, error) {
	return execution.OrderResult{}, errors.New("unused")
}

func TestPollUnfilledStockIntentProjectsPromptBrokerFill(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("store integration DB unavailable")
	}
	ctx := context.Background()
	signalID, err := store.InsertSignal(ctx, store.Signal{Ticker: "UNFILLEDINTENT", Action: "BUY", Strategy: "test", SignalPrice: 100, StopLoss: 90, TakeProfit: 120, Confidence: .9, SizePct: 1, Status: "approved"})
	if err != nil {
		t.Fatal(err)
	}
	broker := &unfilledIntentBroker{result: execution.OrderResult{BrokerOrderID: "unfilled-entry", Status: "partially_filled", FilledPrice: 101, FilledQty: 2}}
	coordinator := stockorders.New(broker)
	req := execution.OrderRequest{Ticker: "UNFILLEDINTENT", Side: execution.OrderSideBuy, Type: execution.OrderTypeMarket, Quantity: 5, IntentKey: "entry:" + signalID, PolicyStopLoss: 90, PolicyTakeProfit: 120}
	if _, err := coordinator.PlaceOrder(ctx, req); err != nil {
		t.Fatal(err)
	}
	intent, err := store.GetStockOrderIntentByKey(ctx, req.IntentKey)
	if err != nil || intent == nil {
		t.Fatalf("intent: %#v %v", intent, err)
	}
	if _, err := store.InsertOrder(ctx, store.Order{BrokerOrderID: "unfilled-entry", Ticker: req.Ticker, Side: "buy", OrderType: "market", Quantity: 5, Status: "pending", StockIntentID: intent.ID}); err != nil {
		t.Fatal(err)
	}
	previous := unfilledEntryPollDelay
	unfilledEntryPollDelay = time.Millisecond
	defer func() { unfilledEntryPollDelay = previous }()
	bus := events.New()
	subscription := bus.Subscribe(events.TradeExecuted)
	defer bus.Unsubscribe(subscription)
	tracker := cost.NewTracker()
	(&Engine{executor: coordinator, bus: bus, costTracker: tracker}).pollUnfilledStockIntent(req.IntentKey, "unfilled-entry")
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		var count int
		if err := store.GetDB().QueryRow(ctx, `SELECT COUNT(*) FROM trades WHERE stock_intent_id=$1::uuid`, intent.ID).Scan(&count); err == nil && count == 1 {
			select {
			case event := <-subscription.C:
				payload, ok := event.Payload.(events.TradeExecutedPayload)
				if !ok || payload.SignalID != signalID || payload.Quantity != 2 {
					t.Fatalf("recovered entry event=%#v", event.Payload)
				}
				if tracker.TakeSnapshot().TradesExecuted != 1 {
					t.Fatal("recovered entry did not update the trade counter")
				}
				return
			case <-time.After(100 * time.Millisecond):
				t.Fatal("recovered entry did not publish trade execution event")
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("3-second unfilled-entry poll did not project broker fill")
}

func TestPollUnfilledStockIntentCancelsAfterDeadline(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("store integration DB unavailable")
	}
	ctx := context.Background()
	signalID, err := store.InsertSignal(ctx, store.Signal{Ticker: "UNFILLEDTIMEOUT", Action: "BUY", Strategy: "test", SignalPrice: 100, StopLoss: 90, TakeProfit: 120, Confidence: .9, SizePct: 1, Status: "approved"})
	if err != nil {
		t.Fatal(err)
	}
	broker := &timedOutIntentBroker{result: execution.OrderResult{BrokerOrderID: "timed-out-entry", Status: "accepted"}}
	coordinator := stockorders.New(broker)
	req := execution.OrderRequest{Ticker: "UNFILLEDTIMEOUT", Side: execution.OrderSideBuy, Type: execution.OrderTypeMarket, Quantity: 5, IntentKey: "entry:" + signalID, PolicyStopLoss: 90, PolicyTakeProfit: 120}
	if _, err := coordinator.PlaceOrder(ctx, req); err != nil {
		t.Fatal(err)
	}
	intent, err := store.GetStockOrderIntentByKey(ctx, req.IntentKey)
	if err != nil || intent == nil {
		t.Fatalf("intent: %#v %v", intent, err)
	}
	if _, err := store.InsertOrder(ctx, store.Order{BrokerOrderID: "timed-out-entry", Ticker: req.Ticker, Side: "buy", OrderType: "market", Quantity: 5, Status: "pending", StockIntentID: intent.ID}); err != nil {
		t.Fatal(err)
	}
	previousPoll := unfilledEntryPollDelay
	previousCancel := unfilledCancelRetryDelay
	unfilledEntryPollDelay = time.Millisecond
	unfilledCancelRetryDelay = time.Millisecond
	defer func() {
		unfilledEntryPollDelay = previousPoll
		unfilledCancelRetryDelay = previousCancel
	}()
	(&Engine{executor: coordinator}).pollUnfilledStockIntent(req.IntentKey, "timed-out-entry")

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		signal, getErr := store.GetSignalByID(ctx, signalID)
		if getErr == nil && signal != nil && signal.Status == "rejected" {
			order, orderErr := store.GetOrderByStockIntentID(ctx, intent.ID)
			if orderErr != nil || order == nil || order.Status != "cancelled" {
				t.Fatalf("timed-out entry order=%#v err=%v", order, orderErr)
			}
			if broker.cancelCount() != 1 {
				t.Fatalf("timed-out entry cancels=%d, want one", broker.cancelCount())
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("timed-out entry was not cancelled and rejected")
}

func TestTimedOutPartialStockIntentWaitsForTerminalCancellation(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("store integration DB unavailable")
	}
	ctx := context.Background()
	signalID, err := store.InsertSignal(ctx, store.Signal{
		Ticker: "TIMEOUTPARTIAL", Action: "BUY", Strategy: "test", SignalPrice: 100,
		StopLoss: 90, TakeProfit: 120, Confidence: .9, SizePct: 1, Status: "approved",
	})
	if err != nil {
		t.Fatal(err)
	}
	broker := &timedOutIntentBroker{
		result: execution.OrderResult{BrokerOrderID: "timed-partial-entry", Status: "accepted"},
		cancelResults: []execution.OrderResult{
			{BrokerOrderID: "timed-partial-entry", Status: "partially_filled", FilledPrice: 101, FilledQty: 2},
			{BrokerOrderID: "timed-partial-entry", Status: "cancelled", FilledPrice: 102, FilledQty: 3},
		},
	}
	coordinator := stockorders.New(broker)
	req := execution.OrderRequest{
		Ticker: "TIMEOUTPARTIAL", Side: execution.OrderSideBuy, Type: execution.OrderTypeMarket,
		Quantity: 5, IntentKey: "entry:" + signalID, PolicyStopLoss: 90, PolicyTakeProfit: 120,
	}
	if _, err := coordinator.PlaceOrder(ctx, req); err != nil {
		t.Fatal(err)
	}
	intent, err := store.GetStockOrderIntentByKey(ctx, req.IntentKey)
	if err != nil || intent == nil {
		t.Fatalf("intent: %#v %v", intent, err)
	}
	if _, err := store.InsertOrder(ctx, store.Order{
		BrokerOrderID: "timed-partial-entry", Ticker: req.Ticker, Side: "buy",
		OrderType: "market", Quantity: 5, Status: "pending", StockIntentID: intent.ID,
	}); err != nil {
		t.Fatal(err)
	}
	previousCancel := unfilledCancelRetryDelay
	unfilledCancelRetryDelay = time.Millisecond
	defer func() { unfilledCancelRetryDelay = previousCancel }()

	engine := &Engine{executor: coordinator}
	engine.cancelTimedOutStockIntent(req.IntentKey, "timed-partial-entry", coordinator)

	if broker.cancelCount() != 2 {
		t.Fatalf("cancel attempts=%d, want two so partial status is not treated as terminal", broker.cancelCount())
	}
	intent, err = store.GetStockOrderIntentByKey(ctx, req.IntentKey)
	if err != nil || intent == nil || intent.Status != "cancelled" || intent.FilledQty != 3 {
		t.Fatalf("terminal intent=%#v err=%v", intent, err)
	}
	trade, err := store.GetTradeByID(ctx, intent.TradeID)
	if err != nil || trade == nil || trade.Quantity != 3 || trade.Status != "open" {
		t.Fatalf("terminal partial trade=%#v err=%v", trade, err)
	}
	order, err := store.GetOrderByStockIntentID(ctx, intent.ID)
	if err != nil || order == nil || order.Status != "cancelled" || order.FilledQty != 3 {
		t.Fatalf("terminal partial order=%#v err=%v", order, err)
	}
}
