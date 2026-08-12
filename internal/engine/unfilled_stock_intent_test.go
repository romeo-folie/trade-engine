package engine

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/execution"
	"github.com/romeo-folie/trade-engine/internal/stockorders"
	"github.com/romeo-folie/trade-engine/internal/store"
)

type unfilledIntentBroker struct{ result execution.OrderResult }

func (b *unfilledIntentBroker) PlaceOrder(context.Context, execution.OrderRequest) (execution.OrderResult, error) {
	return execution.OrderResult{BrokerOrderID: "unfilled-entry", Status: "accepted"}, nil
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
	(&Engine{executor: coordinator}).pollUnfilledStockIntent(req.IntentKey, "unfilled-entry")
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		var count int
		if err := store.GetDB().QueryRow(ctx, `SELECT COUNT(*) FROM trades WHERE stock_intent_id=$1::uuid`, intent.ID).Scan(&count); err == nil && count == 1 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("3-second unfilled-entry poll did not project broker fill")
}
