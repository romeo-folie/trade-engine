package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/data"
	"github.com/romeo-folie/trade-engine/internal/execution"
	"github.com/romeo-folie/trade-engine/internal/execution/trailing"
	"github.com/romeo-folie/trade-engine/internal/stockorders"
	"github.com/romeo-folie/trade-engine/internal/store"
)

// recoveryBroker is deliberately small: it models the only ambiguity that the
// coordinator must resolve, where a POST response was lost but Alpaca can find
// the accepted order by its caller-provided client id.
type recoveryBroker struct {
	posts     int
	lookups   int
	lookupIDs []string
	posted    map[string]execution.OrderResult
	lookup    map[string]execution.OrderResult
	status    map[string]execution.OrderResult
	cancels   []string
	patches   []string
	postRes   execution.OrderResult
	postErr   error
	patchErr  error
	patchErrs []error
}

func newRecoveryBroker(res execution.OrderResult) *recoveryBroker {
	return &recoveryBroker{posted: map[string]execution.OrderResult{}, lookup: map[string]execution.OrderResult{}, status: map[string]execution.OrderResult{}, postRes: res}
}

func (b *recoveryBroker) PlaceOrder(_ context.Context, req execution.OrderRequest) (execution.OrderResult, error) {
	b.posts++
	if b.postErr != nil {
		return execution.OrderResult{}, b.postErr
	}
	res := b.postRes
	if res.BrokerOrderID == "" {
		res.BrokerOrderID = fmt.Sprintf("broker-%d", b.posts)
	}
	b.posted[req.ClientOrderID] = res
	return res, nil
}
func (b *recoveryBroker) GetOrderByClientID(_ context.Context, id string) (execution.OrderResult, error) {
	b.lookups++
	b.lookupIDs = append(b.lookupIDs, id)
	if res, ok := b.lookup[id]; ok {
		return res, nil
	}
	if res, ok := b.posted[id]; ok {
		return res, nil
	}
	return execution.OrderResult{}, execution.ErrOrderNotFound
}
func (b *recoveryBroker) CancelOrder(_ context.Context, id string) error {
	b.cancels = append(b.cancels, id)
	b.status[id] = execution.OrderResult{Status: "cancelled"}
	return nil
}
func (b *recoveryBroker) GetOrderStatus(_ context.Context, id string) (execution.OrderResult, error) {
	if res, ok := b.status[id]; ok {
		return res, nil
	}
	return execution.OrderResult{}, errors.New("not used")
}
func (*recoveryBroker) GetPositions(context.Context) ([]execution.Position, error) { return nil, nil }
func (*recoveryBroker) GetAccount(context.Context) (execution.AccountInfo, error) {
	return execution.AccountInfo{}, nil
}
func (*recoveryBroker) ReplaceStopOrder(context.Context, string, float64) (execution.OrderResult, error) {
	return execution.OrderResult{}, errors.New("not used")
}
func (b *recoveryBroker) ReplaceStopOrderWithClientID(_ context.Context, old string, _ float64, clientID string) (execution.OrderResult, error) {
	b.patches = append(b.patches, old)
	if n := len(b.patches); n <= len(b.patchErrs) && b.patchErrs[n-1] != nil {
		return execution.OrderResult{}, b.patchErrs[n-1]
	}
	if b.patchErr != nil {
		return execution.OrderResult{}, b.patchErr
	}
	res := execution.OrderResult{BrokerOrderID: "patched-" + fmt.Sprint(len(b.patches)), Status: "accepted"}
	b.posted[clientID] = res
	return res, nil
}

type recoveryPrices struct{ candles []data.Candle }

func (p recoveryPrices) FetchHistory(context.Context, string, data.Timeframe, int) ([]data.Candle, error) {
	return append([]data.Candle(nil), p.candles...), nil
}

func stableRecoveryCandles(price float64) []data.Candle {
	candles := make([]data.Candle, 50)
	for i := range candles {
		candles[i] = data.Candle{Open: price, High: price + 1, Low: price - 1, Close: price, Volume: 1000}
	}
	return candles
}

func TestStockIntentDefinitiveReplacementRejectionIsTerminal(t *testing.T) {
	ctx := context.Background()
	if _, err := store.GetDB().Exec(ctx, `UPDATE stock_order_intents SET status='rejected', purpose='superseded'`); err != nil {
		t.Fatal(err)
	}
	signalID := insertRecoverySignal(t, "RINTENTREPLACEREJECT")
	tradeID, err := store.InsertTrade(ctx, store.Trade{SignalID: signalID, Ticker: "RINTENTREPLACEREJECT", Direction: "long", Strategy: "test", EntryPrice: 100, Quantity: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.InsertOrder(ctx, store.Order{TradeID: tradeID, BrokerOrderID: "old-reject", Ticker: "RINTENTREPLACEREJECT", Side: "sell", OrderType: "stop", Quantity: 1, StopPrice: 90, Status: "pending", IsStopLoss: true}); err != nil {
		t.Fatal(err)
	}
	broker := newRecoveryBroker(execution.OrderResult{})
	broker.patchErr = &execution.BrokerRequestError{StatusCode: 422, Message: "invalid stop"}
	coordinator := stockorders.New(broker)
	if _, err := coordinator.ReplaceStopOrder(ctx, "old-reject", 91); err == nil {
		t.Fatal("expected definitive replacement rejection")
	}
	var status string
	if err := store.GetDB().QueryRow(ctx, `SELECT status FROM stock_order_intents WHERE idempotency_key LIKE 'replace:legacy-stop:%'`).Scan(&status); err != nil || status != "rejected" {
		t.Fatalf("replacement intent status=%q err=%v", status, err)
	}
	patches := len(broker.patches)
	if err := coordinator.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if len(broker.patches) != patches {
		t.Fatal("terminal replacement retried on recovery")
	}
}

func TestStockIntentTerminalZeroFillCannotSatisfyRetry(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name, status string
		postErr      error
		lookup       *execution.OrderResult
	}{
		{"definitive rejected", "rejected", &execution.BrokerRequestError{StatusCode: 422, Message: "invalid order"}, nil},
		{"cancelled lookup", "cancelled", nil, &execution.OrderResult{BrokerOrderID: "cancelled-entry", Status: "cancelled"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := store.GetDB().Exec(ctx, `UPDATE stock_order_intents SET status='rejected', purpose='superseded'`); err != nil {
				t.Fatal(err)
			}
			signalID := insertRecoverySignal(t, "RINTENTTERMINAL"+tc.name[:1])
			if err := store.UpdateSignalStatus(ctx, signalID, "approved", ""); err != nil {
				t.Fatal(err)
			}
			broker := newRecoveryBroker(execution.OrderResult{BrokerOrderID: "accepted-entry", Status: "accepted"})
			broker.postErr = tc.postErr
			coordinator := stockorders.New(broker)
			req := entryRequest(signalID, "RINTENTTERMINAL"+tc.name[:1], 1)
			if tc.lookup != nil {
				// First acceptance leaves the durable intent; the broker then
				// reports its client id as terminal zero-fill on retry.
				if _, err := coordinator.PlaceOrder(ctx, req); err != nil {
					t.Fatal(err)
				}
				in := intentForKey(t, req.IntentKey)
				broker.lookup[in.ClientOrderID] = *tc.lookup
			} else {
				if _, err := coordinator.PlaceOrder(ctx, req); err == nil {
					t.Fatal("first definitive rejection unexpectedly succeeded")
				}
			}
			if _, err := coordinator.PlaceOrder(ctx, req); !errors.Is(err, execution.ErrTerminalZeroFillPlacement) {
				t.Fatalf("terminal retry error=%v, want ErrTerminalZeroFillPlacement", err)
			}
			if got := countForIntent(t, "trades", intentForKey(t, req.IntentKey).ID); got != 0 {
				t.Fatalf("terminal retry projected %d trades", got)
			}
			if got := countForIntent(t, "orders", intentForKey(t, req.IntentKey).ID); got != 0 {
				t.Fatalf("terminal retry projected %d orders", got)
			}
			var status string
			if err := store.GetDB().QueryRow(ctx, `SELECT status FROM signals WHERE id=$1::uuid`, signalID).Scan(&status); err != nil || status != "approved" {
				t.Fatalf("terminal retry signal status=%q err=%v", status, err)
			}
		})
	}
}

func TestStockIntentAmbiguousReplacementDoesNotTerminalizeAndRecovers(t *testing.T) {
	ctx := context.Background()
	if _, err := store.GetDB().Exec(ctx, `UPDATE stock_order_intents SET status='rejected', purpose='superseded'`); err != nil {
		t.Fatal(err)
	}
	signalID := insertRecoverySignal(t, "RINTENTAMBIGREPLACE")
	tradeID, err := store.InsertTrade(ctx, store.Trade{SignalID: signalID, Ticker: "RINTENTAMBIGREPLACE", Direction: "long", Strategy: "test", EntryPrice: 100, Quantity: 2, StopLoss: 90})
	if err != nil {
		t.Fatal(err)
	}
	parent := store.StockOrderIntent{ID: "77777777-7777-4777-8777-777777777777", IdempotencyKey: "protective-stop:" + tradeID, ClientOrderID: "ambiguous-parent", TradeID: tradeID, Purpose: "superseded", Ticker: "RINTENTAMBIGREPLACE", Side: "sell", OrderType: "stop", RequestedQty: 2, RequestPayload: []byte(`{}`), PayloadHash: "parent", Status: "rejected"}
	if err := store.CreateStockOrderIntent(ctx, parent); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateStockOrderIntentBroker(ctx, parent.ClientOrderID, "ambiguous-old", "rejected", 0, 0, "", "", true); err != nil {
		t.Fatal(err)
	}
	orderID, err := store.InsertOrder(ctx, store.Order{TradeID: tradeID, BrokerOrderID: "ambiguous-old", Ticker: parent.Ticker, Side: "sell", OrderType: "stop", Quantity: 2, StopPrice: 90, Status: "pending", IsStopLoss: true, StockIntentID: parent.ID})
	if err != nil {
		t.Fatal(err)
	}
	broker := newRecoveryBroker(execution.OrderResult{})
	broker.patchErrs = []error{
		errors.New("PATCH response lost after broker acceptance"),
		&execution.BrokerRequestError{StatusCode: 404, Message: "predecessor no longer exists"},
	}
	coordinator := stockorders.New(broker)
	if _, err := coordinator.ReplaceStopOrder(ctx, "ambiguous-old", 95); !errors.Is(err, execution.ErrAmbiguousStopReplacement) {
		t.Fatalf("replacement error = %v, want ambiguous replacement", err)
	}
	key := "replace:" + parent.IdempotencyKey + ":95.00000000"
	child := intentForKey(t, key)
	if child.Status != "pending" {
		t.Fatalf("ambiguous replacement intent status=%q, want pending", child.Status)
	}
	if got := len(broker.patches); got != 2 {
		t.Fatalf("replacement PATCH attempts=%d, want initial plus exact replay", got)
	}

	// The broker eventually exposes the order created by the first PATCH.
	// Recovery resolves it by client id without a third PATCH and repoints the
	// one mutable stop row to the durable replacement intent.
	broker.lookup[child.ClientOrderID] = execution.OrderResult{BrokerOrderID: "ambiguous-winner", Status: "accepted"}
	if err := coordinator.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if got := len(broker.patches); got != 2 {
		t.Fatalf("recovery replayed ambiguous PATCH: attempts=%d", got)
	}
	var brokerID, intentID string
	if err := store.GetDB().QueryRow(ctx, `SELECT broker_order_id, stock_intent_id::text FROM orders WHERE id=$1::uuid`, orderID).Scan(&brokerID, &intentID); err != nil || brokerID != "ambiguous-winner" || intentID != child.ID {
		t.Fatalf("recovered replacement = broker %q intent %q err %v", brokerID, intentID, err)
	}
}

func TestStockIntentRecoveryReplacementPredecessor404RemainsAmbiguous(t *testing.T) {
	ctx := context.Background()
	if _, err := store.GetDB().Exec(ctx, `UPDATE stock_order_intents SET status='rejected', purpose='superseded'`); err != nil {
		t.Fatal(err)
	}
	signalID := insertRecoverySignal(t, "RINTENTRECOVER404")
	tradeID, err := store.InsertTrade(ctx, store.Trade{SignalID: signalID, Ticker: "RINTENTRECOVER404", Direction: "long", Strategy: "test", EntryPrice: 100, Quantity: 2, StopLoss: 90})
	if err != nil {
		t.Fatal(err)
	}
	parent := store.StockOrderIntent{ID: "88888888-8888-4888-8888-888888888888", IdempotencyKey: "protective-stop:" + tradeID, ClientOrderID: "recover-404-parent", TradeID: tradeID, Purpose: "superseded", Ticker: "RINTENTRECOVER404", Side: "sell", OrderType: "stop", RequestedQty: 2, RequestPayload: []byte(`{}`), PayloadHash: "parent", Status: "rejected"}
	if err := store.CreateStockOrderIntent(ctx, parent); err != nil {
		t.Fatal(err)
	}
	orderID, err := store.InsertOrder(ctx, store.Order{TradeID: tradeID, BrokerOrderID: "recover-404-old", Ticker: parent.Ticker, Side: "sell", OrderType: "stop", Quantity: 2, StopPrice: 90, Status: "pending", IsStopLoss: true, StockIntentID: parent.ID})
	if err != nil {
		t.Fatal(err)
	}
	req := execution.OrderRequest{Ticker: parent.Ticker, Side: execution.OrderSideSell, Type: execution.OrderTypeStop, Quantity: 2, StopPrice: 95, TradeID: tradeID, ReplacesBrokerOrderID: "recover-404-old", IntentKey: "replace:" + parent.IdempotencyKey + ":95.00000000", ClientOrderID: "recover-404-child"}
	payload, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	child := store.StockOrderIntent{ID: "99999999-9999-4999-8999-999999999998", IdempotencyKey: req.IntentKey, ClientOrderID: req.ClientOrderID, TradeID: tradeID, ParentIntentID: parent.ID, Ticker: parent.Ticker, Side: "sell", OrderType: "stop", RequestedQty: 2, RequestPayload: payload, PayloadHash: "recover-404", Status: "pending"}
	if err := store.CreateStockOrderIntent(ctx, child); err != nil {
		t.Fatal(err)
	}
	broker := newRecoveryBroker(execution.OrderResult{})
	broker.patchErr = &execution.BrokerRequestError{StatusCode: 404, Message: "predecessor already replaced"}
	coordinator := stockorders.New(broker)
	if err := coordinator.Recover(ctx); !errors.Is(err, execution.ErrAmbiguousStopReplacement) {
		t.Fatalf("recovery error = %v, want ambiguous replacement", err)
	}
	got := intentForKey(t, req.IntentKey)
	if got.Status != "pending" {
		t.Fatalf("recovery 404 terminalized intent=%q", got.Status)
	}
	broker.lookup[child.ClientOrderID] = execution.OrderResult{BrokerOrderID: "recover-404-winner", Status: "accepted"}
	if err := coordinator.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	var brokerID, intentID string
	if err := store.GetDB().QueryRow(ctx, `SELECT broker_order_id, stock_intent_id::text FROM orders WHERE id=$1::uuid`, orderID).Scan(&brokerID, &intentID); err != nil || brokerID != "recover-404-winner" || intentID != child.ID {
		t.Fatalf("recovered 404 replacement broker=%q intent=%q err=%v", brokerID, intentID, err)
	}
}

func insertRecoverySignal(t *testing.T, ticker string) string {
	t.Helper()
	id, err := store.InsertSignal(context.Background(), store.Signal{
		Ticker: ticker, Action: "BUY", Strategy: "recovery-test", SignalPrice: 100,
		StopLoss: 90, TakeProfit: 120, Confidence: 0.9, SizePct: 1, Status: "approved",
	})
	if err != nil {
		t.Fatalf("insert signal: %v", err)
	}
	return id
}

func entryRequest(signalID, ticker string, qty float64) execution.OrderRequest {
	return execution.OrderRequest{Ticker: ticker, Side: execution.OrderSideBuy, Type: execution.OrderTypeMarket,
		Quantity: qty, IntentKey: "entry:" + signalID}
}

func intentForKey(t *testing.T, key string) *store.StockOrderIntent {
	t.Helper()
	in, err := store.GetStockOrderIntentByKey(context.Background(), key)
	if err != nil || in == nil {
		t.Fatalf("load intent %q: %#v, %v", key, in, err)
	}
	return in
}

func countForIntent(t *testing.T, table, intentID string) int {
	t.Helper()
	var n int
	if err := store.GetDB().QueryRow(context.Background(), "SELECT COUNT(*) FROM "+table+" WHERE stock_intent_id=$1::uuid", intentID).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

func countStockIntentAttempts(t *testing.T, intentID string) int {
	t.Helper()
	var n int
	if err := store.GetDB().QueryRow(context.Background(), `SELECT COUNT(*) FROM stock_order_attempts WHERE intent_id=$1::uuid`, intentID).Scan(&n); err != nil {
		t.Fatalf("count stock-order attempts: %v", err)
	}
	return n
}

func isRecoverableIntent(t *testing.T, intentID string) bool {
	t.Helper()
	intents, err := store.ListRecoverableStockOrderIntents(context.Background())
	if err != nil {
		t.Fatalf("list recoverable stock intents: %v", err)
	}
	for _, intent := range intents {
		if intent.ID == intentID {
			return true
		}
	}
	return false
}

func TestStockIntentRecoveryAcceptedBeforeProjectionFailure(t *testing.T) {
	ctx := context.Background()
	if _, err := store.GetDB().Exec(ctx, `UPDATE stock_order_intents SET status='rejected', purpose='superseded'`); err != nil {
		t.Fatal(err)
	}
	signalID := insertRecoverySignal(t, "RINTENTACCEPT")
	if err := store.UpdateSignalStatus(ctx, signalID, "pending", ""); err != nil {
		t.Fatal(err)
	}
	broker := newRecoveryBroker(execution.OrderResult{BrokerOrderID: "accepted-entry", Status: "accepted"})
	coordinator := stockorders.New(broker)
	req := entryRequest(signalID, "RINTENTACCEPT", 10)
	if _, err := coordinator.PlaceOrder(ctx, req); err != nil {
		t.Fatalf("place accepted entry: %v", err)
	}
	in := intentForKey(t, req.IntentKey)
	// Simulate the process dying after broker acceptance and before either
	// projection was committed. Recovery reads the broker's cumulative truth.
	broker.lookup[in.ClientOrderID] = execution.OrderResult{BrokerOrderID: "accepted-entry", Status: "filled", FilledPrice: 101, FilledQty: 10}
	if err := coordinator.Recover(ctx); err != nil {
		t.Fatalf("recover: %v", err)
	}
	if got := countForIntent(t, "trades", in.ID); got != 1 {
		t.Fatalf("trades for intent = %d, want 1", got)
	}
	if got := countForIntent(t, "orders", in.ID); got != 1 {
		t.Fatalf("orders for intent = %d, want 1", got)
	}
	var signalStatus string
	if err := store.GetDB().QueryRow(ctx, `SELECT status FROM signals WHERE id=$1::uuid`, signalID).Scan(&signalStatus); err != nil || signalStatus != "executed" {
		t.Fatalf("recovered entry signal status=%q err=%v, want executed", signalStatus, err)
	}
	if err := coordinator.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if got := countForIntent(t, "trades", in.ID); got != 1 {
		t.Fatalf("repeated recovery trades for intent = %d, want 1", got)
	}
}

func TestStockIntentRecoveryProjectsSingleTradeMarketExitFill(t *testing.T) {
	ctx := context.Background()
	if _, err := store.GetDB().Exec(ctx, `UPDATE stock_order_intents SET status='rejected', purpose='superseded'`); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, key, ticker string
	}{
		{"software", "software-exit:%s:take-profit hit", "RINTENTSOFTEXIT"},
		{"trailing", "trailing-market-exit:%s:3.00000000", "RINTENTTRAILEXIT"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			signalID := insertRecoverySignal(t, tc.ticker)
			tradeID, err := store.InsertTrade(ctx, store.Trade{SignalID: signalID, Ticker: tc.ticker, Direction: "long", Strategy: "test", EntryPrice: 100, Quantity: 3})
			if err != nil {
				t.Fatal(err)
			}
			if claimed, err := store.ClaimTradeExitWithIntent(ctx, tradeID, store.ExitIntent{Reason: store.ExitReasonTakeProfit}); err != nil || !claimed {
				t.Fatalf("claim exit = %v, %v", claimed, err)
			}
			key := fmt.Sprintf(tc.key, tradeID)
			broker := newRecoveryBroker(execution.OrderResult{BrokerOrderID: "accepted-" + tc.name, Status: "accepted"})
			coordinator := stockorders.New(broker)
			req := execution.OrderRequest{Ticker: tc.ticker, Side: execution.OrderSideSell, Type: execution.OrderTypeMarket, Quantity: 3, TradeID: tradeID, IntentKey: key}
			if _, err := coordinator.PlaceOrder(ctx, req); err != nil {
				t.Fatal(err)
			}
			in := intentForKey(t, key)
			broker.lookup[in.ClientOrderID] = execution.OrderResult{BrokerOrderID: "accepted-" + tc.name, Status: "filled", FilledPrice: 107, FilledQty: 3}
			// The crash happened after the broker fill was made durable but
			// before the local order/trade projection. This must be selected.
			if err := store.UpdateStockOrderIntentBroker(ctx, in.ClientOrderID, "accepted-"+tc.name, "filled", 107, 3, "", "", true); err != nil {
				t.Fatal(err)
			}
			if !isRecoverableIntent(t, in.ID) {
				t.Fatal("filled market exit without a projection was not recoverable")
			}
			if err := coordinator.Recover(ctx); err != nil {
				t.Fatal(err)
			}
			order, err := store.GetOrderByStockIntentAndTrade(ctx, in.ID, tradeID)
			if err != nil || order == nil {
				t.Fatalf("recovered exit order = %#v, %v", order, err)
			}
			if order.FilledQty != 3 || order.FilledPrice != 107 || order.StockIntentID != in.ID {
				t.Fatalf("recovered exit allocation = %#v", order)
			}
			var qty float64
			var status string
			if err := store.GetDB().QueryRow(ctx, `SELECT quantity, status FROM trades WHERE id=$1::uuid`, tradeID).Scan(&qty, &status); err != nil || qty != 3 || status != "closed" {
				t.Fatalf("recovered exit trade = qty %v status %q err %v", qty, status, err)
			}
			if isRecoverableIntent(t, in.ID) {
				t.Fatal("fully projected market exit remained recoverable")
			}
			beforeLookups, beforeAttempts, beforeOrders := broker.lookups, countStockIntentAttempts(t, in.ID), countForIntent(t, "orders", in.ID)
			if err := coordinator.Recover(ctx); err != nil {
				t.Fatal(err)
			}
			if got := countForIntent(t, "orders", in.ID); got != 1 {
				t.Fatalf("recovered exit rows after replay = %d, want 1", got)
			}
			if broker.lookups != beforeLookups {
				t.Fatalf("completed market exit triggered broker lookup: %d -> %d", beforeLookups, broker.lookups)
			}
			if got := countStockIntentAttempts(t, in.ID); got != beforeAttempts {
				t.Fatalf("completed market exit created an attempt: %d -> %d", beforeAttempts, got)
			}
			if got := countForIntent(t, "orders", in.ID); got != beforeOrders {
				t.Fatalf("completed market exit created a projection: %d -> %d", beforeOrders, got)
			}
		})
	}
}

func TestStockIntentRecoveryAppliesCumulativeSingleMarketExitProjection(t *testing.T) {
	ctx := context.Background()
	if _, err := store.GetDB().Exec(ctx, `UPDATE stock_order_intents SET status='rejected', purpose='superseded'`); err != nil {
		t.Fatal(err)
	}
	signalID := insertRecoverySignal(t, "RINTENTCUMULATIVEEXIT")
	tradeID, err := store.InsertTrade(ctx, store.Trade{SignalID: signalID, Ticker: "RINTENTCUMULATIVEEXIT", Direction: "long", Strategy: "test", EntryPrice: 100, Quantity: 5})
	if err != nil {
		t.Fatal(err)
	}
	if claimed, err := store.ClaimTradeExitWithIntent(ctx, tradeID, store.ExitIntent{Reason: store.ExitReasonTakeProfit}); err != nil || !claimed {
		t.Fatalf("claim exit = %v, %v", claimed, err)
	}

	key := "software-exit:" + tradeID + ":cumulative"
	broker := newRecoveryBroker(execution.OrderResult{BrokerOrderID: "cumulative-exit", Status: "accepted"})
	coordinator := stockorders.New(broker)
	req := execution.OrderRequest{Ticker: "RINTENTCUMULATIVEEXIT", Side: execution.OrderSideSell, Type: execution.OrderTypeMarket, Quantity: 5, TradeID: tradeID, IntentKey: key}
	if _, err := coordinator.PlaceOrder(ctx, req); err != nil {
		t.Fatal(err)
	}
	in := intentForKey(t, key)
	// Simulate a crash after the durable order projection was inserted but
	// before its broker cumulative fill was applied to the trade.
	if _, err := store.InsertOrder(ctx, store.Order{TradeID: tradeID, BrokerOrderID: "cumulative-exit", Ticker: req.Ticker, Side: "sell", OrderType: "market", Quantity: 5, Status: "pending", StockIntentID: in.ID}); err != nil {
		t.Fatal(err)
	}

	broker.lookup[in.ClientOrderID] = execution.OrderResult{BrokerOrderID: "cumulative-exit", Status: "partially_filled", FilledPrice: 105, FilledQty: 2}
	if err := coordinator.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	assertCumulativeRecoveredExit(t, in.ID, tradeID, 2, 210, "partial", "exiting")

	// A repeated poll with the same cumulative quantity must not apply the
	// first two shares a second time.
	if err := coordinator.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	assertCumulativeRecoveredExit(t, in.ID, tradeID, 2, 210, "partial", "exiting")

	broker.lookup[in.ClientOrderID] = execution.OrderResult{BrokerOrderID: "cumulative-exit", Status: "filled", FilledPrice: 106, FilledQty: 5}
	if err := coordinator.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	assertCumulativeRecoveredExit(t, in.ID, tradeID, 5, 530, "filled", "closed")
	if isRecoverableIntent(t, in.ID) {
		t.Fatal("completed cumulative market exit remained recoverable")
	}
	beforeLookups, beforeAttempts := len(broker.lookupIDs), countStockIntentAttempts(t, in.ID)
	if err := coordinator.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	for _, clientID := range broker.lookupIDs[beforeLookups:] {
		if clientID == in.ClientOrderID {
			t.Fatal("completed cumulative market exit replayed broker lookup")
		}
	}
	if got := countStockIntentAttempts(t, in.ID); got != beforeAttempts {
		t.Fatalf("completed cumulative market exit created attempt: %d -> %d", beforeAttempts, got)
	}
}

func assertCumulativeRecoveredExit(t *testing.T, intentID, tradeID string, wantFill, wantProceeds float64, wantOrderStatus, wantTradeStatus string) {
	t.Helper()
	order, err := store.GetOrderByStockIntentAndTrade(context.Background(), intentID, tradeID)
	if err != nil || order == nil || order.FilledQty != wantFill || order.Status != wantOrderStatus {
		t.Fatalf("recovered cumulative exit order = %#v, %v", order, err)
	}
	var realizedQty, realizedProceeds float64
	var tradeStatus string
	if err := store.GetDB().QueryRow(context.Background(), `SELECT realized_exit_qty, realized_exit_proceeds, status FROM trades WHERE id=$1::uuid`, tradeID).Scan(&realizedQty, &realizedProceeds, &tradeStatus); err != nil {
		t.Fatal(err)
	}
	if realizedQty != wantFill || realizedProceeds != wantProceeds || tradeStatus != wantTradeStatus {
		t.Fatalf("recovered cumulative exit realization = qty %v proceeds %v status %q, want %v %v %q", realizedQty, realizedProceeds, tradeStatus, wantFill, wantProceeds, wantTradeStatus)
	}
}

func TestStockIntentRecoveryProjectsTerminalPartialEntryFill(t *testing.T) {
	ctx := context.Background()
	for _, status := range []string{"cancelled", "rejected"} {
		t.Run(status, func(t *testing.T) {
			if _, err := store.GetDB().Exec(ctx, `UPDATE stock_order_intents SET status='rejected', purpose='superseded'`); err != nil {
				t.Fatal(err)
			}
			ticker := "RINTENTTERMINALENTRY" + status[:1]
			signalID := insertRecoverySignal(t, ticker)
			broker := newRecoveryBroker(execution.OrderResult{BrokerOrderID: "terminal-entry-" + status, Status: "accepted"})
			coordinator := stockorders.New(broker)
			req := entryRequest(signalID, ticker, 5)
			if _, err := coordinator.PlaceOrder(ctx, req); err != nil {
				t.Fatal(err)
			}
			in := intentForKey(t, req.IntentKey)
			result := execution.OrderResult{BrokerOrderID: "terminal-entry-" + status, Status: status, FilledPrice: 101, FilledQty: 2}
			broker.lookup[in.ClientOrderID] = result
			if err := store.UpdateStockOrderIntentBroker(ctx, in.ClientOrderID, result.BrokerOrderID, execution.NormalizeOrderStatus(status), result.FilledPrice, result.FilledQty, "", "", true); err != nil {
				t.Fatal(err)
			}
			if !isRecoverableIntent(t, in.ID) {
				t.Fatal("terminal partial entry without projection was not recoverable")
			}
			if err := coordinator.Recover(ctx); err != nil {
				t.Fatal(err)
			}
			if got := countForIntent(t, "trades", in.ID); got != 1 {
				t.Fatalf("terminal partial entry trades=%d, want 1", got)
			}
			if isRecoverableIntent(t, in.ID) {
				t.Fatal("applied terminal partial entry remained recoverable")
			}
			beforeLookups, beforeAttempts := broker.lookups, countStockIntentAttempts(t, in.ID)
			if err := coordinator.Recover(ctx); err != nil {
				t.Fatal(err)
			}
			if broker.lookups != beforeLookups || countStockIntentAttempts(t, in.ID) != beforeAttempts {
				t.Fatalf("completed terminal partial entry replayed lookup/attempt: lookups %d->%d attempts %d->%d", beforeLookups, broker.lookups, beforeAttempts, countStockIntentAttempts(t, in.ID))
			}
		})
	}
}

func TestStockIntentRecoveryProjectsTerminalStopFill(t *testing.T) {
	ctx := context.Background()
	for _, status := range []string{"cancelled", "rejected"} {
		t.Run(status, func(t *testing.T) {
			if _, err := store.GetDB().Exec(ctx, `UPDATE stock_order_intents SET status='rejected', purpose='superseded'`); err != nil {
				t.Fatal(err)
			}
			ticker := "RINTENTTERMINALSTOP" + status[:1]
			signalID := insertRecoverySignal(t, ticker)
			tradeID, err := store.InsertTrade(ctx, store.Trade{SignalID: signalID, Ticker: ticker, Direction: "long", Strategy: "test", EntryPrice: 100, Quantity: 2, StopLoss: 90})
			if err != nil {
				t.Fatal(err)
			}
			broker := newRecoveryBroker(execution.OrderResult{BrokerOrderID: "terminal-stop-" + status, Status: "accepted"})
			coordinator := stockorders.New(broker)
			req := execution.OrderRequest{Ticker: ticker, Side: execution.OrderSideSell, Type: execution.OrderTypeStop, Quantity: 2, StopPrice: 90, TradeID: tradeID, IntentKey: "protective-stop-terminal:" + tradeID}
			if _, err := coordinator.PlaceOrder(ctx, req); err != nil {
				t.Fatal(err)
			}
			in := intentForKey(t, req.IntentKey)
			result := execution.OrderResult{BrokerOrderID: "terminal-stop-" + status, Status: status, FilledPrice: 89, FilledQty: 2}
			broker.lookup[in.ClientOrderID] = result
			if err := store.UpdateStockOrderIntentBroker(ctx, in.ClientOrderID, result.BrokerOrderID, execution.NormalizeOrderStatus(status), result.FilledPrice, result.FilledQty, "", "", true); err != nil {
				t.Fatal(err)
			}
			if !isRecoverableIntent(t, in.ID) {
				t.Fatal("terminal stop fill without projection was not recoverable")
			}
			if err := coordinator.Recover(ctx); err != nil {
				t.Fatal(err)
			}
			order, err := store.GetOrderByStockIntentAndTrade(ctx, in.ID, tradeID)
			if err != nil || order == nil || order.FilledQty != 2 || order.Status != execution.NormalizeOrderStatus(status) {
				t.Fatalf("terminal stop projection=%#v err=%v", order, err)
			}
			if isRecoverableIntent(t, in.ID) {
				t.Fatal("applied terminal stop remained recoverable")
			}
			beforeLookups, beforeAttempts := broker.lookups, countStockIntentAttempts(t, in.ID)
			if err := coordinator.Recover(ctx); err != nil {
				t.Fatal(err)
			}
			if broker.lookups != beforeLookups || countStockIntentAttempts(t, in.ID) != beforeAttempts {
				t.Fatalf("completed terminal stop replayed lookup/attempt")
			}
		})
	}
}

func TestStockIntentRecoveryProjectsTerminalMarketExitFills(t *testing.T) {
	ctx := context.Background()
	for _, status := range []string{"cancelled", "rejected"} {
		t.Run(status, func(t *testing.T) {
			if _, err := store.GetDB().Exec(ctx, `UPDATE stock_order_intents SET status='rejected', purpose='superseded'`); err != nil {
				t.Fatal(err)
			}
			ticker := "RINTENTTERMINALEXIT" + status[:1]
			signalID := insertRecoverySignal(t, ticker)
			tradeID, err := store.InsertTrade(ctx, store.Trade{SignalID: signalID, Ticker: ticker, Direction: "long", Strategy: "test", EntryPrice: 100, Quantity: 3})
			if err != nil {
				t.Fatal(err)
			}
			if claimed, err := store.ClaimTradeExitWithIntent(ctx, tradeID, store.ExitIntent{Reason: store.ExitReasonTakeProfit}); err != nil || !claimed {
				t.Fatalf("claim exit=%v err=%v", claimed, err)
			}
			broker := newRecoveryBroker(execution.OrderResult{BrokerOrderID: "terminal-exit-" + status, Status: "accepted"})
			coordinator := stockorders.New(broker)
			req := execution.OrderRequest{Ticker: ticker, Side: execution.OrderSideSell, Type: execution.OrderTypeMarket, Quantity: 3, TradeID: tradeID, IntentKey: "software-exit:" + tradeID + ":terminal"}
			if _, err := coordinator.PlaceOrder(ctx, req); err != nil {
				t.Fatal(err)
			}
			in := intentForKey(t, req.IntentKey)
			result := execution.OrderResult{BrokerOrderID: "terminal-exit-" + status, Status: status, FilledPrice: 105, FilledQty: 3}
			broker.lookup[in.ClientOrderID] = result
			if err := store.UpdateStockOrderIntentBroker(ctx, in.ClientOrderID, result.BrokerOrderID, execution.NormalizeOrderStatus(status), result.FilledPrice, result.FilledQty, "", "", true); err != nil {
				t.Fatal(err)
			}
			if !isRecoverableIntent(t, in.ID) {
				t.Fatal("terminal market exit without projection was not recoverable")
			}
			if err := coordinator.Recover(ctx); err != nil {
				t.Fatal(err)
			}
			if isRecoverableIntent(t, in.ID) {
				t.Fatal("finalized terminal market exit remained recoverable")
			}
			beforeLookups, beforeAttempts := broker.lookups, countStockIntentAttempts(t, in.ID)
			if err := coordinator.Recover(ctx); err != nil {
				t.Fatal(err)
			}
			if broker.lookups != beforeLookups || countStockIntentAttempts(t, in.ID) != beforeAttempts {
				t.Fatalf("completed terminal market exit replayed lookup/attempt")
			}
		})
	}
}

func TestStockIntentRecoveryProjectsTerminalAggregateMarketExitFills(t *testing.T) {
	ctx := context.Background()
	for _, status := range []string{"cancelled", "rejected"} {
		t.Run(status, func(t *testing.T) {
			if _, err := store.GetDB().Exec(ctx, `UPDATE stock_order_intents SET status='rejected', purpose='superseded'`); err != nil {
				t.Fatal(err)
			}
			ticker := "RINTENTTERMINALAGG" + status[:1]
			signalID := insertRecoverySignal(t, ticker)
			tradeA, err := store.InsertTrade(ctx, store.Trade{SignalID: signalID, Ticker: ticker, Direction: "long", Strategy: "test", EntryPrice: 100, Quantity: 2})
			if err != nil {
				t.Fatal(err)
			}
			tradeB, err := store.InsertTrade(ctx, store.Trade{SignalID: signalID, Ticker: ticker, Direction: "long", Strategy: "test", EntryPrice: 101, Quantity: 3})
			if err != nil {
				t.Fatal(err)
			}
			claim := store.ExitIntent{Reason: store.ExitReasonSellSignal, Evidence: json.RawMessage(fmt.Sprintf(`{"signal_id":%q}`, signalID))}
			for _, tradeID := range []string{tradeA, tradeB} {
				if claimed, err := store.ClaimTradeExitWithIntent(ctx, tradeID, claim); err != nil || !claimed {
					t.Fatalf("claim aggregate exit=%v err=%v", claimed, err)
				}
			}
			broker := newRecoveryBroker(execution.OrderResult{BrokerOrderID: "terminal-aggregate-" + status, Status: "accepted"})
			coordinator := stockorders.New(broker)
			key := "analyst-exit:" + signalID + ":" + ticker
			if _, err := coordinator.PlaceOrder(ctx, execution.OrderRequest{Ticker: ticker, Side: execution.OrderSideSell, Type: execution.OrderTypeMarket, Quantity: 5, IntentKey: key}); err != nil {
				t.Fatal(err)
			}
			in := intentForKey(t, key)
			result := execution.OrderResult{BrokerOrderID: "terminal-aggregate-" + status, Status: status, FilledPrice: 106, FilledQty: 5}
			broker.lookup[in.ClientOrderID] = result
			if err := store.UpdateStockOrderIntentBroker(ctx, in.ClientOrderID, result.BrokerOrderID, execution.NormalizeOrderStatus(status), result.FilledPrice, result.FilledQty, "", "", true); err != nil {
				t.Fatal(err)
			}
			if !isRecoverableIntent(t, in.ID) {
				t.Fatal("terminal aggregate exit without allocation was not recoverable")
			}
			if err := coordinator.Recover(ctx); err != nil {
				t.Fatal(err)
			}
			if isRecoverableIntent(t, in.ID) {
				t.Fatal("finalized terminal aggregate exit remained recoverable")
			}
			beforeLookups, beforeAttempts := broker.lookups, countStockIntentAttempts(t, in.ID)
			if err := coordinator.Recover(ctx); err != nil {
				t.Fatal(err)
			}
			if broker.lookups != beforeLookups || countStockIntentAttempts(t, in.ID) != beforeAttempts {
				t.Fatalf("completed terminal aggregate exit replayed lookup/attempt")
			}
		})
	}
}

func TestStockIntentRecoveryProjectsAggregateAnalystExitAllocations(t *testing.T) {
	ctx := context.Background()
	if _, err := store.GetDB().Exec(ctx, `UPDATE stock_order_intents SET status='rejected', purpose='superseded'`); err != nil {
		t.Fatal(err)
	}
	signalID := insertRecoverySignal(t, "RINTENTANALYSTEXIT")
	tradeA, err := store.InsertTrade(ctx, store.Trade{SignalID: signalID, Ticker: "RINTENTANALYSTEXIT", Direction: "long", Strategy: "test", EntryPrice: 100, Quantity: 2})
	if err != nil {
		t.Fatal(err)
	}
	tradeB, err := store.InsertTrade(ctx, store.Trade{SignalID: signalID, Ticker: "RINTENTANALYSTEXIT", Direction: "long", Strategy: "test", EntryPrice: 101, Quantity: 3})
	if err != nil {
		t.Fatal(err)
	}
	claim := store.ExitIntent{Reason: store.ExitReasonSellSignal, Evidence: json.RawMessage(fmt.Sprintf(`{"signal_id":%q}`, signalID))}
	for _, tradeID := range []string{tradeA, tradeB} {
		if claimed, err := store.ClaimTradeExitWithIntent(ctx, tradeID, claim); err != nil || !claimed {
			t.Fatalf("claim aggregate exit %s = %v, %v", tradeID, claimed, err)
		}
	}
	if claimed, err := store.GetExitingTradesForSellSignal(ctx, "RINTENTANALYSTEXIT", signalID); err != nil || len(claimed) != 2 {
		t.Fatalf("claimed aggregate exit links = %#v, %v", claimed, err)
	}
	key := "analyst-exit:" + signalID + ":RINTENTANALYSTEXIT"
	broker := newRecoveryBroker(execution.OrderResult{BrokerOrderID: "accepted-aggregate-exit", Status: "accepted"})
	coordinator := stockorders.New(broker)
	if _, err := coordinator.PlaceOrder(ctx, execution.OrderRequest{Ticker: "RINTENTANALYSTEXIT", Side: execution.OrderSideSell, Type: execution.OrderTypeMarket, Quantity: 5, IntentKey: key}); err != nil {
		t.Fatal(err)
	}
	in := intentForKey(t, key)
	broker.lookup[in.ClientOrderID] = execution.OrderResult{BrokerOrderID: "accepted-aggregate-exit", Status: "filled", FilledPrice: 108, FilledQty: 5}
	if err := store.UpdateStockOrderIntentBroker(ctx, in.ClientOrderID, "accepted-aggregate-exit", "filled", 108, 5, "", "", true); err != nil {
		t.Fatal(err)
	}
	if !isRecoverableIntent(t, in.ID) {
		t.Fatal("filled aggregate analyst exit without allocations was not recoverable")
	}
	if err := coordinator.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	for tradeID, want := range map[string]float64{tradeA: 2, tradeB: 3} {
		order, err := store.GetOrderByStockIntentAndTrade(ctx, in.ID, tradeID)
		if err != nil || order == nil || order.FilledQty != want || order.FilledPrice != 108 {
			t.Fatalf("aggregate allocation for %s = %#v, %v; want %v @ 108", tradeID, order, err, want)
		}
	}
	var total float64
	var rows int
	if err := store.GetDB().QueryRow(ctx, `SELECT COUNT(*), COALESCE(SUM(filled_qty),0) FROM orders WHERE stock_intent_id=$1::uuid`, in.ID).Scan(&rows, &total); err != nil || rows != 2 || total != 5 {
		t.Fatalf("aggregate recovered rows=%d total=%v err=%v", rows, total, err)
	}
	if isRecoverableIntent(t, in.ID) {
		t.Fatal("fully allocated analyst exit remained recoverable")
	}
	beforeLookups, beforeAttempts, beforeOrders := broker.lookups, countStockIntentAttempts(t, in.ID), rows
	if err := coordinator.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if err := store.GetDB().QueryRow(ctx, `SELECT COUNT(*), COALESCE(SUM(filled_qty),0) FROM orders WHERE stock_intent_id=$1::uuid`, in.ID).Scan(&rows, &total); err != nil || rows != 2 || total != 5 {
		t.Fatalf("aggregate replay rows=%d total=%v err=%v", rows, total, err)
	}
	if broker.lookups != beforeLookups {
		t.Fatalf("completed aggregate exit triggered broker lookup: %d -> %d", beforeLookups, broker.lookups)
	}
	if got := countStockIntentAttempts(t, in.ID); got != beforeAttempts {
		t.Fatalf("completed aggregate exit created an attempt: %d -> %d", beforeAttempts, got)
	}
	if rows != beforeOrders {
		t.Fatalf("completed aggregate exit created allocations: %d -> %d", beforeOrders, rows)
	}
}

func TestStockIntentRecoveryCompletesPartialAggregateMarketExitProjection(t *testing.T) {
	ctx := context.Background()
	if _, err := store.GetDB().Exec(ctx, `UPDATE stock_order_intents SET status='rejected', purpose='superseded'`); err != nil {
		t.Fatal(err)
	}
	signalID := insertRecoverySignal(t, "RINTENTPARTIALAGGEXIT")
	tradeA, err := store.InsertTrade(ctx, store.Trade{SignalID: signalID, Ticker: "RINTENTPARTIALAGGEXIT", Direction: "long", Strategy: "test", EntryPrice: 100, Quantity: 2})
	if err != nil {
		t.Fatal(err)
	}
	tradeB, err := store.InsertTrade(ctx, store.Trade{SignalID: signalID, Ticker: "RINTENTPARTIALAGGEXIT", Direction: "long", Strategy: "test", EntryPrice: 101, Quantity: 3})
	if err != nil {
		t.Fatal(err)
	}
	claim := store.ExitIntent{Reason: store.ExitReasonSellSignal, Evidence: json.RawMessage(fmt.Sprintf(`{"signal_id":%q}`, signalID))}
	for _, tradeID := range []string{tradeA, tradeB} {
		if claimed, err := store.ClaimTradeExitWithIntent(ctx, tradeID, claim); err != nil || !claimed {
			t.Fatalf("claim aggregate exit %s = %v, %v", tradeID, claimed, err)
		}
	}
	key := "analyst-exit:" + signalID + ":RINTENTPARTIALAGGEXIT"
	broker := newRecoveryBroker(execution.OrderResult{BrokerOrderID: "partial-aggregate-exit", Status: "accepted"})
	coordinator := stockorders.New(broker)
	if _, err := coordinator.PlaceOrder(ctx, execution.OrderRequest{Ticker: "RINTENTPARTIALAGGEXIT", Side: execution.OrderSideSell, Type: execution.OrderTypeMarket, Quantity: 5, IntentKey: key}); err != nil {
		t.Fatal(err)
	}
	in := intentForKey(t, key)

	// Simulate the process dying after it durably allocated the first lot, but
	// before it wrote or applied the second. The closed lot is no longer in the
	// sell-signal query, so recovery must preserve its capacity and allocate the
	// remaining broker fill to the still-exiting lot.
	orderID, err := store.InsertOrder(ctx, store.Order{TradeID: tradeA, BrokerOrderID: "partial-aggregate-exit", Ticker: "RINTENTPARTIALAGGEXIT", Side: "sell", OrderType: "market", Quantity: 2, FilledPrice: 108, FilledQty: 2, Status: "filled", StockIntentID: in.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.ApplyPartialExit(ctx, tradeA, orderID, 108, 2, "filled"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.FinalizeTradeExit(ctx, tradeA); err != nil {
		t.Fatal(err)
	}
	broker.lookup[in.ClientOrderID] = execution.OrderResult{BrokerOrderID: "partial-aggregate-exit", Status: "filled", FilledPrice: 108, FilledQty: 5}
	if err := store.UpdateStockOrderIntentBroker(ctx, in.ClientOrderID, "partial-aggregate-exit", "filled", 108, 5, "", "", true); err != nil {
		t.Fatal(err)
	}
	if !isRecoverableIntent(t, in.ID) {
		t.Fatal("partially projected aggregate exit was not recoverable")
	}
	if err := coordinator.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	order, err := store.GetOrderByStockIntentAndTrade(ctx, in.ID, tradeB)
	if err != nil || order == nil || order.FilledQty != 3 {
		t.Fatalf("recovered second aggregate allocation = %#v, %v; want 3", order, err)
	}
	if isRecoverableIntent(t, in.ID) {
		t.Fatal("completed partial aggregate projection remained recoverable")
	}
	beforeLookups, beforeAttempts, beforeOrders := broker.lookups, countStockIntentAttempts(t, in.ID), countForIntent(t, "orders", in.ID)
	if err := coordinator.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if broker.lookups != beforeLookups {
		t.Fatalf("completed partial aggregate exit triggered broker lookup: %d -> %d", beforeLookups, broker.lookups)
	}
	if got := countStockIntentAttempts(t, in.ID); got != beforeAttempts {
		t.Fatalf("completed partial aggregate exit created an attempt: %d -> %d", beforeAttempts, got)
	}
	if got := countForIntent(t, "orders", in.ID); got != beforeOrders {
		t.Fatalf("completed partial aggregate exit created allocations: %d -> %d", beforeOrders, got)
	}
}

func TestTrailingManagerRotatesCoordinatorTerminalStopGeneration(t *testing.T) {
	ctx := context.Background()
	for _, terminalStatus := range []string{"cancelled", "rejected", "expired"} {
		t.Run(terminalStatus, func(t *testing.T) {
			if _, err := store.GetDB().Exec(ctx, `UPDATE stock_order_intents SET status='rejected', purpose='superseded'`); err != nil {
				t.Fatal(err)
			}
			ticker := "RINTENTTRAIL" + terminalStatus
			signalID := insertRecoverySignal(t, ticker)
			tradeID, err := store.InsertTrade(ctx, store.Trade{SignalID: signalID, Ticker: ticker, Direction: "long", Strategy: "test", EntryPrice: 100, Quantity: 2, StopLoss: 95, HighWaterMark: 100})
			if err != nil {
				t.Fatal(err)
			}
			broker := newRecoveryBroker(execution.OrderResult{Status: "accepted"})
			coordinator := stockorders.New(broker)
			originalKey := "trailing-stop:" + tradeID + ":new:97.00000000:2.00000000"
			original := execution.OrderRequest{Ticker: ticker, Side: execution.OrderSideSell, Type: execution.OrderTypeStop, Quantity: 2, StopPrice: 97, TradeID: tradeID, TimeInForce: "gtc", IntentKey: originalKey}
			if _, err := coordinator.PlaceOrder(ctx, original); err != nil {
				t.Fatalf("place first trailing generation: %v", err)
			}
			old := intentForKey(t, originalKey)
			broker.lookup[old.ClientOrderID] = execution.OrderResult{BrokerOrderID: "terminal-" + terminalStatus, Status: terminalStatus}
			if _, err := coordinator.PlaceOrder(ctx, original); !errors.Is(err, execution.ErrTerminalZeroFillPlacement) {
				t.Fatalf("coordinator terminal result = %v, want ErrTerminalZeroFillPlacement", err)
			}

			manager := trailing.NewManager(
				trailing.Config{Enabled: true, ATRMultiple: 1.5, MarketOpen: func(time.Time) bool { return true }},
				trailing.LiveStore(), recoveryPrices{candles: stableRecoveryCandles(100)}, coordinator,
			)
			manager.UpdateAll(ctx)

			retryKey := originalKey + ":retry:terminal-" + terminalStatus
			fresh := intentForKey(t, retryKey)
			if fresh.ClientOrderID == old.ClientOrderID || fresh.BrokerOrderID == "" || fresh.Status != "pending" {
				t.Fatalf("fresh stop generation = %#v, old client=%q", fresh, old.ClientOrderID)
			}
			order, err := store.GetOrderByStockIntentAndTrade(ctx, fresh.ID, tradeID)
			if err != nil || order == nil {
				t.Fatalf("fresh trailing stop projection = %#v, %v", order, err)
			}
			if order.BrokerOrderID != fresh.BrokerOrderID || order.Status != "pending" || !order.IsStopLoss {
				t.Fatalf("fresh trailing stop projection = %#v", order)
			}
			if got := countForIntent(t, "orders", old.ID); got != 0 {
				t.Fatalf("terminal generation retained a live projection: %d", got)
			}
		})
	}
}

func TestTrailingManagerFollowsTerminalStopGenerations(t *testing.T) {
	ctx := context.Background()
	if _, err := store.GetDB().Exec(ctx, `UPDATE stock_order_intents SET status='rejected', purpose='superseded'`); err != nil {
		t.Fatal(err)
	}
	ticker := "RINTENTTRAILMULTIGEN"
	signalID := insertRecoverySignal(t, ticker)
	tradeID, err := store.InsertTrade(ctx, store.Trade{SignalID: signalID, Ticker: ticker, Direction: "long", Strategy: "test", EntryPrice: 100, Quantity: 2, StopLoss: 95, HighWaterMark: 100})
	if err != nil {
		t.Fatal(err)
	}
	broker := newRecoveryBroker(execution.OrderResult{Status: "accepted"})
	coordinator := stockorders.New(broker)
	baseKey := "trailing-stop:" + tradeID + ":new:97.00000000:2.00000000"
	stopRequest := func(key string) execution.OrderRequest {
		return execution.OrderRequest{Ticker: ticker, Side: execution.OrderSideSell, Type: execution.OrderTypeStop, Quantity: 2, StopPrice: 97, TradeID: tradeID, TimeInForce: "gtc", IntentKey: key}
	}
	if _, err := coordinator.PlaceOrder(ctx, stopRequest(baseKey)); err != nil {
		t.Fatalf("place base trailing generation: %v", err)
	}
	base := intentForKey(t, baseKey)
	broker.lookup[base.ClientOrderID] = execution.OrderResult{BrokerOrderID: "terminal-base", Status: "cancelled"}
	if _, err := coordinator.PlaceOrder(ctx, stopRequest(baseKey)); !errors.Is(err, execution.ErrTerminalZeroFillPlacement) {
		t.Fatalf("terminal base result=%v, want ErrTerminalZeroFillPlacement", err)
	}

	manager := trailing.NewManager(
		trailing.Config{Enabled: true, ATRMultiple: 1.5, MarketOpen: func(time.Time) bool { return true }},
		trailing.LiveStore(), recoveryPrices{candles: stableRecoveryCandles(100)}, coordinator,
	)
	manager.UpdateAll(ctx) // terminal base -> child 1
	firstKey := store.StockOrderIntentRetryKey(baseKey, "terminal-base")
	terminalize := func(key, brokerID, status string) {
		t.Helper()
		intent := intentForKey(t, key)
		broker.lookup[intent.ClientOrderID] = execution.OrderResult{BrokerOrderID: brokerID, Status: status}
		if _, err := coordinator.PlaceOrder(ctx, stopRequest(key)); !errors.Is(err, execution.ErrTerminalZeroFillPlacement) {
			t.Fatalf("terminalize %q: %v", key, err)
		}
		if _, err := store.GetDB().Exec(ctx, `UPDATE orders SET status='cancelled' WHERE stock_intent_id=$1::uuid`, intent.ID); err != nil {
			t.Fatal(err)
		}
	}
	terminalize(firstKey, "terminal-rejected", "rejected")
	manager.UpdateAll(ctx) // terminal child 1 -> child 2
	secondKey := store.StockOrderIntentRetryKey(firstKey, "terminal-rejected")
	terminalize(secondKey, "terminal-expired", "expired")
	manager.UpdateAll(ctx) // terminal child 2 -> child 3, which stays live
	thirdKey := store.StockOrderIntentRetryKey(secondKey, "terminal-expired")

	if got := len(broker.posted); got != 4 {
		t.Fatalf("broker generations=%d, want base plus three distinct successors", got)
	}
	keys := []string{baseKey, firstKey, secondKey, thirdKey}
	clients := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		intent := intentForKey(t, key)
		if _, duplicate := clients[intent.ClientOrderID]; duplicate {
			t.Fatalf("generation %q reused client id %q", key, intent.ClientOrderID)
		}
		clients[intent.ClientOrderID] = struct{}{}
	}
	active := intentForKey(t, thirdKey)
	if active.Status != "pending" || active.BrokerOrderID == "" {
		t.Fatalf("active trailing generation=%#v", active)
	}
	order, err := store.GetOrderByStockIntentAndTrade(ctx, active.ID, tradeID)
	if err != nil || order == nil || order.Status != "pending" || !order.IsStopLoss || order.BrokerOrderID != active.BrokerOrderID {
		t.Fatalf("active trailing stop projection=%#v err=%v", order, err)
	}
	var pending int
	if err := store.GetDB().QueryRow(ctx, `SELECT COUNT(*) FROM orders WHERE trade_id=$1::uuid AND status='pending' AND is_stop_loss`, tradeID).Scan(&pending); err != nil || pending != 1 {
		t.Fatalf("live trailing stops=%d err=%v, want exactly one", pending, err)
	}
}

func TestStockOrderIntentRetryGenerationDoesNotAdvanceAmbiguousSuccessor(t *testing.T) {
	ctx := context.Background()
	if _, err := store.GetDB().Exec(ctx, `UPDATE stock_order_intents SET status='rejected', purpose='superseded'`); err != nil {
		t.Fatal(err)
	}
	signalID := insertRecoverySignal(t, "RINTENTAMBIGGEN")
	tradeID, err := store.InsertTrade(ctx, store.Trade{SignalID: signalID, Ticker: "RINTENTAMBIGGEN", Direction: "long", Strategy: "test", EntryPrice: 100, Quantity: 1, StopLoss: 90})
	if err != nil {
		t.Fatal(err)
	}
	broker := newRecoveryBroker(execution.OrderResult{BrokerOrderID: "base-stop", Status: "accepted"})
	coordinator := stockorders.New(broker)
	baseKey := "protective-stop:" + tradeID + ":90.00000000:1.00000000"
	request := func(key string) execution.OrderRequest {
		return execution.OrderRequest{Ticker: "RINTENTAMBIGGEN", Side: execution.OrderSideSell, Type: execution.OrderTypeStop, Quantity: 1, StopPrice: 90, TradeID: tradeID, TimeInForce: "gtc", IntentKey: key}
	}
	if _, err := coordinator.PlaceOrder(ctx, request(baseKey)); err != nil {
		t.Fatal(err)
	}
	base := intentForKey(t, baseKey)
	broker.lookup[base.ClientOrderID] = execution.OrderResult{BrokerOrderID: "base-terminal", Status: "cancelled"}
	if _, err := coordinator.PlaceOrder(ctx, request(baseKey)); !errors.Is(err, execution.ErrTerminalZeroFillPlacement) {
		t.Fatalf("terminal base result=%v", err)
	}
	childKey := store.StockOrderIntentRetryKey(baseKey, "base-terminal")
	broker.postErr = errors.New("ambiguous broker response")
	if _, err := coordinator.PlaceOrder(ctx, request(childKey)); err == nil {
		t.Fatal("ambiguous child submission unexpectedly succeeded")
	}
	child := intentForKey(t, childKey)
	if child.Status != "pending" {
		t.Fatalf("ambiguous child status=%q, want pending", child.Status)
	}
	latest, submitKey, err := store.ResolveStockOrderIntentRetryGeneration(ctx, baseKey)
	if err != nil {
		t.Fatal(err)
	}
	if latest == nil || latest.ID != child.ID || submitKey != childKey {
		t.Fatalf("ambiguous successor resolution latest=%#v submit=%q, want child %q", latest, submitKey, childKey)
	}
	grandchild := store.StockOrderIntentRetryKey(childKey, child.BrokerOrderID)
	if grandchild != "" {
		if next, err := store.GetStockOrderIntentByKey(ctx, grandchild); err != nil || next != nil {
			t.Fatalf("ambiguous successor advanced to %#v, err=%v", next, err)
		}
	}
}

func TestStockIntentRecoveryAfterTrailingCancelResubmitRepointIsClean(t *testing.T) {
	ctx := context.Background()
	if _, err := store.GetDB().Exec(ctx, `UPDATE stock_order_intents SET status='rejected', purpose='superseded'`); err != nil {
		t.Fatal(err)
	}
	signalID := insertRecoverySignal(t, "RINTENTTRAILREPOINT")
	tradeID, err := store.InsertTrade(ctx, store.Trade{SignalID: signalID, Ticker: "RINTENTTRAILREPOINT", Direction: "long", Strategy: "test", EntryPrice: 100, Quantity: 2, StopLoss: 90})
	if err != nil {
		t.Fatal(err)
	}
	parent := store.StockOrderIntent{ID: "99999999-9999-4999-8999-999999999999", IdempotencyKey: "protective-stop:" + tradeID, ClientOrderID: "trail-parent", TradeID: tradeID, Ticker: "RINTENTTRAILREPOINT", Side: "sell", OrderType: "stop", RequestedQty: 2, RequestPayload: []byte(`{}`), PayloadHash: "parent", Status: "rejected"}
	if err := store.CreateStockOrderIntent(ctx, parent); err != nil {
		t.Fatal(err)
	}
	orderID, err := store.InsertOrder(ctx, store.Order{TradeID: tradeID, BrokerOrderID: "stale-trail-stop", Ticker: "RINTENTTRAILREPOINT", Side: "sell", OrderType: "stop", Quantity: 2, StopPrice: 90, Status: "pending", IsStopLoss: true, StockIntentID: parent.ID})
	if err != nil {
		t.Fatal(err)
	}

	// This models the trailing manager's cancel-confirm-resubmit fallback:
	// the coordinator durably submits the new STOP and the manager mutates
	// its one live order row before repointing it to that new intent.
	broker := newRecoveryBroker(execution.OrderResult{BrokerOrderID: "fresh-trail-stop", Status: "accepted"})
	coordinator := stockorders.New(broker)
	req := execution.OrderRequest{Ticker: "RINTENTTRAILREPOINT", Side: execution.OrderSideSell, Type: execution.OrderTypeStop, Quantity: 2, StopPrice: 95, TradeID: tradeID, IntentKey: "trailing-stop-fallback:" + tradeID}
	if _, err := coordinator.PlaceOrder(ctx, req); err != nil {
		t.Fatal(err)
	}
	child := intentForKey(t, req.IntentKey)
	if err := store.UpdateOrderStopReplacement(ctx, orderID, "stale-trail-stop", "fresh-trail-stop", 0, 2, 95, 2, "pending", 0, 0, time.Time{}, store.ExitIntent{Reason: store.ExitReasonTrailingATR}); err != nil {
		t.Fatal(err)
	}
	if err := store.RepointOrderStockIntentAndSupersede(ctx, orderID, child.ID); err != nil {
		t.Fatal(err)
	}
	broker.lookup[child.ClientOrderID] = execution.OrderResult{BrokerOrderID: "fresh-trail-stop", Status: "accepted"}
	if err := coordinator.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if err := coordinator.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	var brokerID, intentID string
	if err := store.GetDB().QueryRow(ctx, `SELECT broker_order_id, stock_intent_id::text FROM orders WHERE id=$1::uuid`, orderID).Scan(&brokerID, &intentID); err != nil || brokerID != "fresh-trail-stop" || intentID != child.ID {
		t.Fatalf("fallback repoint after recovery = broker %q intent %q err %v", brokerID, intentID, err)
	}
	if got := countForIntent(t, "orders", child.ID); got != 1 || broker.posts != 1 {
		t.Fatalf("fallback recovery duplicated projection: rows=%d posts=%d", got, broker.posts)
	}
	var parentPurpose string
	if err := store.GetDB().QueryRow(ctx, `SELECT purpose FROM stock_order_intents WHERE id=$1::uuid`, parent.ID).Scan(&parentPurpose); err != nil || parentPurpose != "superseded" {
		t.Fatalf("fallback predecessor purpose=%q err=%v", parentPurpose, err)
	}
}

func TestStockIntentRecoveryWorkingZeroFillCreatesNoPhantomTrade(t *testing.T) {
	ctx := context.Background()
	signalID := insertRecoverySignal(t, "RINTENTZERO")
	broker := newRecoveryBroker(execution.OrderResult{BrokerOrderID: "working-entry", Status: "accepted"})
	coordinator := stockorders.New(broker)
	req := entryRequest(signalID, "RINTENTZERO", 10)
	if _, err := coordinator.PlaceOrder(ctx, req); err != nil {
		t.Fatalf("place working entry: %v", err)
	}
	in := intentForKey(t, req.IntentKey)
	if err := coordinator.Recover(ctx); err != nil {
		t.Fatalf("recover working entry: %v", err)
	}
	if got := countForIntent(t, "trades", in.ID); got != 0 {
		t.Fatalf("working zero-fill created %d trades, want none", got)
	}
}

func TestStockIntentRecoveryTerminalZeroFillClosesOrderOnlyProjection(t *testing.T) {
	ctx := context.Background()
	signalID := insertRecoverySignal(t, "RINTENTTERMINALZERO")
	broker := newRecoveryBroker(execution.OrderResult{BrokerOrderID: "terminal-zero", Status: "accepted"})
	coordinator := stockorders.New(broker)
	req := entryRequest(signalID, "RINTENTTERMINALZERO", 1)
	if _, err := coordinator.PlaceOrder(ctx, req); err != nil {
		t.Fatal(err)
	}
	in := intentForKey(t, req.IntentKey)
	if _, err := store.InsertOrder(ctx, store.Order{BrokerOrderID: "terminal-zero", Ticker: req.Ticker, Side: "buy", OrderType: "market", Quantity: 1, Status: "pending", StockIntentID: in.ID}); err != nil {
		t.Fatal(err)
	}
	broker.lookup[in.ClientOrderID] = execution.OrderResult{BrokerOrderID: "terminal-zero", Status: "cancelled"}
	if err := coordinator.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	order, err := store.GetOrderByStockIntentID(ctx, in.ID)
	if err != nil || order == nil || order.Status != "cancelled" {
		t.Fatalf("terminal zero order = %#v, %v", order, err)
	}
}

func TestStockIntentTerminalZeroFillTerminalizesOrderOnlyProjections(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name, brokerStatus, storedStatus string
		entry                            bool
	}{
		{"done-for-day entry", "done_for_day", "cancelled", true},
		{"done-for-day fractional stop", "done_for_day", "cancelled", false},
		{"rejected entry", "rejected", "rejected", true},
		{"rejected standalone stop", "rejected", "rejected", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := store.GetDB().Exec(ctx, `UPDATE stock_order_intents SET status='rejected', purpose='superseded'`); err != nil {
				t.Fatal(err)
			}
			ticker := "RINTENTDONE" + fmt.Sprintf("%d", time.Now().UnixNano())
			signalID := insertRecoverySignal(t, ticker)
			broker := newRecoveryBroker(execution.OrderResult{BrokerOrderID: "done-for-day-" + tc.name, Status: "accepted"})
			coordinator := stockorders.New(broker)
			var req execution.OrderRequest
			if tc.entry {
				req = entryRequest(signalID, ticker, 1)
			} else {
				tradeID, err := store.InsertTrade(ctx, store.Trade{SignalID: signalID, Ticker: ticker, Direction: "long", Strategy: "test", EntryPrice: 100, Quantity: 1.5, StopLoss: 90})
				if err != nil {
					t.Fatal(err)
				}
				req = execution.OrderRequest{Ticker: ticker, Side: execution.OrderSideSell, Type: execution.OrderTypeStop, Quantity: 1.5, StopPrice: 90, TradeID: tradeID, TimeInForce: "day", IntentKey: "protective-stop-done:" + tradeID}
			}
			if _, err := coordinator.PlaceOrder(ctx, req); err != nil {
				t.Fatal(err)
			}
			in := intentForKey(t, req.IntentKey)
			if _, err := store.InsertOrder(ctx, store.Order{TradeID: req.TradeID, BrokerOrderID: "done-for-day-" + tc.name, Ticker: ticker, Side: string(req.Side), OrderType: string(req.Type), Quantity: req.Quantity, StopPrice: req.StopPrice, Status: "pending", IsStopLoss: !tc.entry, StockIntentID: in.ID}); err != nil {
				t.Fatal(err)
			}
			broker.lookup[in.ClientOrderID] = execution.OrderResult{BrokerOrderID: "done-for-day-" + tc.name, Status: tc.brokerStatus}
			if _, err := coordinator.PlaceOrder(ctx, req); !errors.Is(err, execution.ErrTerminalZeroFillPlacement) {
				t.Fatalf("%s client lookup result=%v, want ErrTerminalZeroFillPlacement", tc.brokerStatus, err)
			}
			in = intentForKey(t, req.IntentKey)
			if in.Status != tc.storedStatus {
				t.Fatalf("%s intent status=%q, want %q", tc.brokerStatus, in.Status, tc.storedStatus)
			}
			if !isRecoverableIntent(t, in.ID) {
				t.Fatal("terminal order-only projection was not recoverable")
			}
			if err := coordinator.Recover(ctx); err != nil {
				t.Fatal(err)
			}
			order, err := store.GetOrderByStockIntentID(ctx, in.ID)
			if err != nil || order == nil || order.Status != tc.storedStatus {
				t.Fatalf("%s projection=%#v err=%v", tc.brokerStatus, order, err)
			}
			if isRecoverableIntent(t, in.ID) {
				t.Fatal("terminalized done_for_day intent remained recoverable")
			}
			beforeLookups, beforeAttempts := len(broker.lookupIDs), countStockIntentAttempts(t, in.ID)
			if err := coordinator.Recover(ctx); err != nil {
				t.Fatal(err)
			}
			for _, clientID := range broker.lookupIDs[beforeLookups:] {
				if clientID == in.ClientOrderID {
					t.Fatal("terminalized done_for_day intent replayed broker lookup")
				}
			}
			if got := countStockIntentAttempts(t, in.ID); got != beforeAttempts {
				t.Fatalf("terminalized done_for_day intent created attempt: %d -> %d", beforeAttempts, got)
			}
		})
	}
}

func TestStockIntentRecoveryProjectsAcceptedStandaloneStop(t *testing.T) {
	ctx := context.Background()
	if _, err := store.GetDB().Exec(ctx, `UPDATE stock_order_intents SET status='rejected', purpose='superseded'`); err != nil {
		t.Fatal(err)
	}
	signalID := insertRecoverySignal(t, "RINTENTRECOVERSTOP")
	tradeID, err := store.InsertTrade(ctx, store.Trade{SignalID: signalID, Ticker: "RINTENTRECOVERSTOP", Direction: "long", Strategy: "test", EntryPrice: 100, Quantity: 2, StopLoss: 90})
	if err != nil {
		t.Fatal(err)
	}
	broker := newRecoveryBroker(execution.OrderResult{BrokerOrderID: "recovered-stop", Status: "accepted"})
	coordinator := stockorders.New(broker)
	req := execution.OrderRequest{Ticker: "RINTENTRECOVERSTOP", Side: execution.OrderSideSell, Type: execution.OrderTypeStop, Quantity: 2, StopPrice: 90, TradeID: tradeID, IntentKey: "protective-stop:" + tradeID}
	if _, err := coordinator.PlaceOrder(ctx, req); err != nil {
		t.Fatal(err)
	}
	in := intentForKey(t, req.IntentKey)
	if err := coordinator.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if got := countForIntent(t, "orders", in.ID); got != 1 {
		t.Fatalf("recovered stop orders = %d", got)
	}
	if err := coordinator.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if got := countForIntent(t, "orders", in.ID); got != 1 {
		t.Fatalf("replay duplicated recovered stop: %d", got)
	}
}

func TestStockIntentDefinitiveRejectionIsTerminalButAmbiguityRecovers(t *testing.T) {
	ctx := context.Background()
	if _, err := store.GetDB().Exec(ctx, `UPDATE stock_order_intents SET status='rejected', purpose='superseded'`); err != nil {
		t.Fatal(err)
	}
	rejectedSignal := insertRecoverySignal(t, "RINTENTREJECTED")
	rejectedBroker := newRecoveryBroker(execution.OrderResult{})
	rejectedBroker.postErr = &execution.BrokerRequestError{StatusCode: 422, Message: "insufficient buying power"}
	rejected := stockorders.New(rejectedBroker)
	req := entryRequest(rejectedSignal, "RINTENTREJECTED", 1)
	if _, err := rejected.PlaceOrder(ctx, req); err == nil {
		t.Fatal("expected definitive rejection")
	}
	in := intentForKey(t, req.IntentKey)
	if in.Status != "rejected" {
		t.Fatalf("definitive rejection status = %q", in.Status)
	}
	posts := rejectedBroker.posts
	if err := rejected.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if rejectedBroker.posts != posts {
		t.Fatal("terminal rejection replayed on recovery")
	}

	ambiguousSignal := insertRecoverySignal(t, "RINTENTAMBIGUOUS")
	ambiguousBroker := newRecoveryBroker(execution.OrderResult{})
	ambiguousBroker.postErr = errors.New("transport reset")
	ambiguous := stockorders.New(ambiguousBroker)
	ambiguousReq := entryRequest(ambiguousSignal, "RINTENTAMBIGUOUS", 1)
	if _, err := ambiguous.PlaceOrder(ctx, ambiguousReq); err == nil {
		t.Fatal("expected ambiguous submit error")
	}
	ambiguousIntent := intentForKey(t, ambiguousReq.IntentKey)
	if ambiguousIntent.Status == "rejected" {
		t.Fatal("transport ambiguity was terminalized")
	}
}

type delayedDuplicateBroker struct {
	lookups, posts int
	message        string
}

func (b *delayedDuplicateBroker) PlaceOrder(context.Context, execution.OrderRequest) (execution.OrderResult, error) {
	b.posts++
	message := b.message
	if message == "" {
		message = "duplicate client order id"
	}
	return execution.OrderResult{}, &execution.BrokerRequestError{StatusCode: 422, Message: message}
}
func (b *delayedDuplicateBroker) GetOrderByClientID(context.Context, string) (execution.OrderResult, error) {
	b.lookups++
	if b.lookups < 3 {
		return execution.OrderResult{}, execution.ErrOrderNotFound
	}
	return execution.OrderResult{BrokerOrderID: "late-accepted", Status: "filled", FilledPrice: 100, FilledQty: 1}, nil
}
func (*delayedDuplicateBroker) CancelOrder(context.Context, string) error { return nil }
func (*delayedDuplicateBroker) GetOrderStatus(context.Context, string) (execution.OrderResult, error) {
	return execution.OrderResult{}, nil
}
func (*delayedDuplicateBroker) GetPositions(context.Context) ([]execution.Position, error) {
	return nil, nil
}
func (*delayedDuplicateBroker) GetAccount(context.Context) (execution.AccountInfo, error) {
	return execution.AccountInfo{}, nil
}
func (*delayedDuplicateBroker) ReplaceStopOrder(context.Context, string, float64) (execution.OrderResult, error) {
	return execution.OrderResult{}, nil
}

func TestStockIntentDuplicateClient422RemainsRecoverableUntilLookupFinds(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct{ name, message string }{
		{"duplicate", "duplicate client order id"},
		{"already exists", "order with client_order_id replay-id already exists"},
		{"must be unique", "client_order_id must be unique"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := store.GetDB().Exec(ctx, `UPDATE stock_order_intents SET status='rejected', purpose='superseded'`); err != nil {
				t.Fatal(err)
			}
			ticker := "RINTENTDUPCLIENT" + tc.name[:1]
			signalID := insertRecoverySignal(t, ticker)
			b := &delayedDuplicateBroker{message: tc.message}
			c := stockorders.New(b)
			req := entryRequest(signalID, ticker, 1)
			if _, err := c.PlaceOrder(ctx, req); err == nil {
				t.Fatal("duplicate response must remain ambiguous")
			}
			in := intentForKey(t, req.IntentKey)
			if in.Status == "rejected" {
				t.Fatal("duplicate client 422 terminalized intent")
			}
			if err := c.Recover(ctx); err != nil {
				t.Fatal(err)
			}
			in = intentForKey(t, req.IntentKey)
			if in.Status != "filled" {
				t.Fatalf("later lookup status=%q", in.Status)
			}
			if got := countForIntent(t, "trades", in.ID); got != 1 {
				t.Fatalf("late accepted projection trades=%d", got)
			}
		})
	}
}

func TestStockIntentRecoveryPartialFillIsCumulativeAndIdempotent(t *testing.T) {
	ctx := context.Background()
	signalID := insertRecoverySignal(t, "RINTENTPARTIAL")
	broker := newRecoveryBroker(execution.OrderResult{BrokerOrderID: "partial-entry", Status: "accepted"})
	coordinator := stockorders.New(broker)
	req := entryRequest(signalID, "RINTENTPARTIAL", 10)
	if _, err := coordinator.PlaceOrder(ctx, req); err != nil {
		t.Fatalf("place partial entry: %v", err)
	}
	in := intentForKey(t, req.IntentKey)
	broker.lookup[in.ClientOrderID] = execution.OrderResult{BrokerOrderID: "partial-entry", Status: "partially_filled", FilledPrice: 101, FilledQty: 3}
	if err := coordinator.Recover(ctx); err != nil {
		t.Fatalf("recover first partial: %v", err)
	}
	broker.lookup[in.ClientOrderID] = execution.OrderResult{BrokerOrderID: "partial-entry", Status: "partially_filled", FilledPrice: 102, FilledQty: 5}
	if err := coordinator.Recover(ctx); err != nil {
		t.Fatalf("recover revised partial: %v", err)
	}
	if err := coordinator.Recover(ctx); err != nil {
		t.Fatalf("replay revised partial: %v", err)
	}
	var requested, filled, held float64
	if err := store.GetDB().QueryRow(ctx, `
		SELECT o.quantity, o.filled_qty, t.quantity
		FROM orders o JOIN trades t ON t.id=o.trade_id
		WHERE o.stock_intent_id=$1::uuid`, in.ID).Scan(&requested, &filled, &held); err != nil {
		t.Fatalf("load partial projection: %v", err)
	}
	if requested != 10 || filled != 5 || held != 5 {
		t.Fatalf("requested/filled/held = %.2f/%.2f/%.2f, want 10/5/5", requested, filled, held)
	}
}

func TestStockIntentConcurrentSameTickerSignalsAndReplayDeduplicate(t *testing.T) {
	ctx := context.Background()
	broker := newRecoveryBroker(execution.OrderResult{Status: "accepted"})
	coordinator := stockorders.New(broker)
	first := entryRequest(insertRecoverySignal(t, "RINTENTCONCURRENT"), "RINTENTCONCURRENT", 1)
	second := entryRequest(insertRecoverySignal(t, "RINTENTCONCURRENT"), "RINTENTCONCURRENT", 2)
	if _, err := coordinator.PlaceOrder(ctx, first); err != nil {
		t.Fatalf("first same-ticker entry: %v", err)
	}
	if _, err := coordinator.PlaceOrder(ctx, second); err != nil {
		t.Fatalf("second same-ticker entry: %v", err)
	}
	if _, err := coordinator.PlaceOrder(ctx, first); err != nil {
		t.Fatalf("replay first entry: %v", err)
	}
	if broker.posts != 2 {
		t.Fatalf("broker posts = %d, want one per distinct signal", broker.posts)
	}
	for _, req := range []execution.OrderRequest{first, second} {
		if in := intentForKey(t, req.IntentKey); in.RequestedQty != req.Quantity {
			t.Fatalf("intent %s requested qty = %v, want %v", req.IntentKey, in.RequestedQty, req.Quantity)
		}
	}
}

func TestStockIntentRecoverySkipsBracketProjectionChildren(t *testing.T) {
	ctx := context.Background()
	if _, err := store.GetDB().Exec(ctx, `UPDATE stock_order_intents SET status='rejected', purpose='superseded'`); err != nil {
		t.Fatal(err)
	}
	signalID := insertRecoverySignal(t, "RINTENTBRACKET")
	broker := newRecoveryBroker(execution.OrderResult{BrokerOrderID: "bracket-parent", Status: "accepted", ChildOrderIDs: []string{"bracket-tp", "bracket-sl"}})
	coordinator := stockorders.New(broker)
	req := entryRequest(signalID, "RINTENTBRACKET", 10)
	req.OrderClass, req.TakeProfitPrice, req.StopLossPrice = "bracket", 120, 90
	if _, err := coordinator.PlaceOrder(ctx, req); err != nil {
		t.Fatalf("place bracket: %v", err)
	}
	posts := broker.posts
	if err := coordinator.Recover(ctx); err != nil {
		t.Fatalf("recover bracket children: %v", err)
	}
	if broker.posts != posts {
		t.Fatalf("recovery submitted a bracket child: posts %d -> %d", posts, broker.posts)
	}
	var n int
	if err := store.GetDB().QueryRow(ctx, `SELECT COUNT(*) FROM stock_order_intents WHERE parent_intent_id=$1::uuid AND purpose='broker_projection'`, intentForKey(t, req.IntentKey).ID).Scan(&n); err != nil || n != 2 {
		t.Fatalf("child projection count = %d, %v; want 2", n, err)
	}
}

func TestStockIntentRecoveryFilledProjectionEligibility(t *testing.T) {
	ctx := context.Background()
	signalID := insertRecoverySignal(t, "RINTENTDONE")
	broker := newRecoveryBroker(execution.OrderResult{BrokerOrderID: "done-entry", Status: "accepted"})
	coordinator := stockorders.New(broker)
	req := entryRequest(signalID, "RINTENTDONE", 1)
	if _, err := coordinator.PlaceOrder(ctx, req); err != nil {
		t.Fatal(err)
	}
	in := intentForKey(t, req.IntentKey)
	broker.lookup[in.ClientOrderID] = execution.OrderResult{BrokerOrderID: "done-entry", Status: "filled", FilledPrice: 100, FilledQty: 1}
	if err := coordinator.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	intents, err := store.ListRecoverableStockOrderIntents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range intents {
		if candidate.ID == in.ID {
			t.Fatal("completed filled entry remained recoverable")
		}
	}
	if _, err := store.GetDB().Exec(ctx, `UPDATE orders SET trade_id=NULL WHERE stock_intent_id=$1::uuid`, in.ID); err != nil {
		t.Fatal(err)
	}
	intents, err = store.ListRecoverableStockOrderIntents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range intents {
		if candidate.ID == in.ID {
			return
		}
	}
	t.Fatal("partially projected filled entry was not recoverable")
}

func TestStockIntentRecoveryRestoresBracketAndUncappedPolicy(t *testing.T) {
	ctx := context.Background()
	bracketSignal := insertRecoverySignal(t, "RINTENTRECOVERBRACKET")
	broker := newRecoveryBroker(execution.OrderResult{BrokerOrderID: "recovered-parent", Status: "accepted"})
	coordinator := stockorders.New(broker)
	bracket := entryRequest(bracketSignal, "RINTENTRECOVERBRACKET", 2)
	bracket.OrderClass, bracket.TakeProfitPrice, bracket.StopLossPrice, bracket.PolicyTakeProfit, bracket.PolicyStopLoss = "bracket", 120, 90, 120, 90
	if _, err := coordinator.PlaceOrder(ctx, bracket); err != nil {
		t.Fatal(err)
	}
	in := intentForKey(t, bracket.IntentKey)
	broker.lookup[in.ClientOrderID] = execution.OrderResult{BrokerOrderID: "recovered-parent", Status: "filled", FilledPrice: 100, FilledQty: 2, ChildOrderIDs: []string{"recover-tp", "recover-sl"}}
	if err := coordinator.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if got := countForIntent(t, "orders", in.ID); got != 1 {
		t.Fatalf("parent orders=%d, want 1", got)
	}
	var children int
	if err := store.GetDB().QueryRow(ctx, `SELECT COUNT(*) FROM orders o JOIN stock_order_intents i ON i.id=o.stock_intent_id WHERE i.parent_intent_id=$1::uuid`, in.ID).Scan(&children); err != nil || children != 2 {
		t.Fatalf("bracket child orders=%d err=%v", children, err)
	}

	uncappedSignal := insertRecoverySignal(t, "RINTENTRECOVERUNCAP")
	uncapped := entryRequest(uncappedSignal, "RINTENTRECOVERUNCAP", 3)
	uncapped.PolicyStopLoss, uncapped.PolicyTakeProfit, uncapped.UncappedTakeProfit = 90, 120, true
	if _, err := coordinator.PlaceOrder(ctx, uncapped); err != nil {
		t.Fatal(err)
	}
	uncappedIntent := intentForKey(t, uncapped.IntentKey)
	broker.lookup[uncappedIntent.ClientOrderID] = execution.OrderResult{BrokerOrderID: "uncapped-entry", Status: "partially_filled", FilledPrice: 100, FilledQty: 3}
	if err := coordinator.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	var takeProfit float64
	if err := store.GetDB().QueryRow(ctx, `SELECT COALESCE(take_profit,0) FROM trades WHERE stock_intent_id=$1::uuid`, uncappedIntent.ID).Scan(&takeProfit); err != nil || takeProfit != 0 {
		t.Fatalf("uncapped take profit=%v err=%v", takeProfit, err)
	}
	var stops int
	if err := store.GetDB().QueryRow(ctx, `SELECT COUNT(*) FROM stock_order_intents WHERE idempotency_key LIKE 'recovered-protective-stop:%' AND trade_id=(SELECT id FROM trades WHERE stock_intent_id=$1::uuid)`, uncappedIntent.ID).Scan(&stops); err != nil || stops != 1 {
		t.Fatalf("recovered protective stops=%d err=%v", stops, err)
	}
	// A later cumulative fill resizes the same protection lineage only after
	// the old stop is broker-confirmed cancelled.
	broker.lookup[uncappedIntent.ClientOrderID] = execution.OrderResult{BrokerOrderID: "uncapped-entry", Status: "partially_filled", FilledPrice: 100, FilledQty: 5}
	if err := coordinator.Recover(ctx); err != nil {
		t.Fatalf("recover grown uncapped fill: %v", err)
	}
	var activeStops, protectedQty float64
	if err := store.GetDB().QueryRow(ctx, `
		SELECT COUNT(*), COALESCE(MAX(o.quantity),0) FROM orders o
		JOIN stock_order_intents i ON i.id=o.stock_intent_id
		WHERE o.trade_id=(SELECT id FROM trades WHERE stock_intent_id=$1::uuid)
		  AND i.idempotency_key LIKE 'recovered-protective-stop:%'
		  AND o.status IN ('pending','partial')`, uncappedIntent.ID).Scan(&activeStops, &protectedQty); err != nil {
		t.Fatal(err)
	}
	if activeStops != 1 || protectedQty != 5 || len(broker.cancels) == 0 {
		t.Fatalf("active protected stops/qty/cancels = %.0f/%.1f/%v, want 1/5/cancel", activeStops, protectedQty, broker.cancels)
	}
}

func TestStockIntentRecoveryAdoptsNormalUncappedProtectiveStop(t *testing.T) {
	ctx := context.Background()
	if _, err := store.GetDB().Exec(ctx, `UPDATE stock_order_intents SET status='rejected', purpose='superseded'`); err != nil {
		t.Fatal(err)
	}
	signalID := insertRecoverySignal(t, "RINTENTADOPTSTOP")
	broker := newRecoveryBroker(execution.OrderResult{BrokerOrderID: "adopt-entry", Status: "accepted"})
	coordinator := stockorders.New(broker)
	req := entryRequest(signalID, "RINTENTADOPTSTOP", 3)
	req.PolicyStopLoss, req.PolicyTakeProfit, req.UncappedTakeProfit = 90, 120, true
	if _, err := coordinator.PlaceOrder(ctx, req); err != nil {
		t.Fatal(err)
	}
	in := intentForKey(t, req.IntentKey)
	tradeID, err := store.InsertTradeForStockIntent(ctx, store.Trade{SignalID: signalID, Ticker: req.Ticker, Direction: "long", Strategy: "recovery-test", EntryPrice: 100, Quantity: 3, StopLoss: 90, StockIntentID: in.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AttachStockOrderIntentTradeByKey(ctx, req.IntentKey, tradeID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.InsertOrder(ctx, store.Order{TradeID: tradeID, BrokerOrderID: "adopt-entry", Ticker: req.Ticker, Side: "buy", OrderType: "market", Quantity: 3, FilledPrice: 100, FilledQty: 3, Status: "partial", StockIntentID: in.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.InsertOrder(ctx, store.Order{TradeID: tradeID, BrokerOrderID: "normal-stop", Ticker: req.Ticker, Side: "sell", OrderType: "stop", Quantity: 3, StopPrice: 90, Status: "pending", IsStopLoss: true}); err != nil {
		t.Fatal(err)
	}
	broker.lookup[in.ClientOrderID] = execution.OrderResult{BrokerOrderID: "adopt-entry", Status: "partially_filled", FilledPrice: 100, FilledQty: 3}
	posts := broker.posts
	if err := coordinator.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if broker.posts != posts {
		t.Fatalf("recovery created duplicate protective stop: posts %d -> %d", posts, broker.posts)
	}
	var active int
	if err := store.GetDB().QueryRow(ctx, `SELECT COUNT(*) FROM orders WHERE trade_id=$1::uuid AND is_stop_loss AND status IN ('pending','partial')`, tradeID).Scan(&active); err != nil || active != 1 {
		t.Fatalf("active protection = %d, %v", active, err)
	}
}

func TestStockIntentRecoveryLookupProjectsReplacementOrder(t *testing.T) {
	ctx := context.Background()
	signalID := insertRecoverySignal(t, "RINTENTREPLACE")
	tradeID, err := store.InsertTrade(ctx, store.Trade{SignalID: signalID, Ticker: "RINTENTREPLACE", Direction: "long", Strategy: "recovery-test", EntryPrice: 100, Quantity: 5, StopLoss: 90})
	if err != nil {
		t.Fatal(err)
	}
	parent := store.StockOrderIntent{ID: "11111111-1111-4111-8111-111111111111", IdempotencyKey: "protective-stop:" + tradeID, ClientOrderID: "parent-stop", TradeID: tradeID, Ticker: "RINTENTREPLACE", Side: "sell", OrderType: "stop", RequestedQty: 5, RequestPayload: []byte(`{}`), PayloadHash: "parent", Status: "filled"}
	if err := store.CreateStockOrderIntent(ctx, parent); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateStockOrderIntentBroker(ctx, parent.ClientOrderID, "old-stop", "pending", 0, 0, "", "", true); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetDB().Exec(ctx, `UPDATE stock_order_intents SET status='rejected', purpose='superseded' WHERE id=$1::uuid`, parent.ID); err != nil {
		t.Fatal(err)
	}
	orderID, err := store.InsertOrder(ctx, store.Order{TradeID: tradeID, BrokerOrderID: "old-stop", Ticker: "RINTENTREPLACE", Side: "sell", OrderType: "stop", Quantity: 5, StopPrice: 90, Status: "pending", IsStopLoss: true, StockIntentID: parent.ID})
	if err != nil {
		t.Fatal(err)
	}
	req := execution.OrderRequest{Ticker: "RINTENTREPLACE", Side: execution.OrderSideSell, Type: execution.OrderTypeStop, Quantity: 5, StopPrice: 91, TradeID: tradeID, IntentKey: "replace:protective:91", ClientOrderID: "replacement-stop", ReplacesBrokerOrderID: "old-stop"}
	payload, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	child := store.StockOrderIntent{ID: "22222222-2222-4222-8222-222222222222", IdempotencyKey: req.IntentKey, ClientOrderID: req.ClientOrderID, TradeID: tradeID, ParentIntentID: parent.ID, Ticker: req.Ticker, Side: "sell", OrderType: "stop", RequestedQty: 5, RequestPayload: payload, PayloadHash: "replacement", Status: "pending"}
	if err := store.CreateStockOrderIntent(ctx, child); err != nil {
		t.Fatal(err)
	}
	broker := newRecoveryBroker(execution.OrderResult{})
	broker.lookup[child.ClientOrderID] = execution.OrderResult{BrokerOrderID: "new-stop", Status: "accepted"}
	coordinator := stockorders.New(broker)
	if err := coordinator.Recover(ctx); err != nil {
		t.Fatalf("recover replacement: %v", err)
	}
	var brokerID, intentID string
	if err := store.GetDB().QueryRow(ctx, `SELECT broker_order_id, stock_intent_id::text FROM orders WHERE id=$1::uuid`, orderID).Scan(&brokerID, &intentID); err != nil {
		t.Fatal(err)
	}
	if brokerID != "new-stop" || intentID != child.ID {
		t.Fatalf("replacement projection = %s/%s", brokerID, intentID)
	}
	if _, err := coordinator.ReplaceStopOrder(ctx, "new-stop", 92); err != nil {
		t.Fatalf("ratchet replacement: %v", err)
	}
	if err := store.GetDB().QueryRow(ctx, `SELECT broker_order_id FROM orders WHERE id=$1::uuid`, orderID).Scan(&brokerID); err != nil {
		t.Fatal(err)
	}
	if brokerID != "new-stop" {
		t.Fatalf("direct replacement must leave mutable row for trailing owner, got %s", brokerID)
	}
	var count int
	if err := store.GetDB().QueryRow(ctx, `SELECT COUNT(*) FROM orders WHERE trade_id=$1::uuid AND is_stop_loss`, tradeID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("stop rows=%d err=%v, want one", count, err)
	}
}

func TestStockIntentRecoveryAlreadyLinkedReplacementIsIdempotent(t *testing.T) {
	ctx := context.Background()
	if _, err := store.GetDB().Exec(ctx, `UPDATE stock_order_intents SET status='rejected', purpose='superseded'`); err != nil {
		t.Fatal(err)
	}
	signalID := insertRecoverySignal(t, "RINTENTLINKEDREPLACE")
	tradeID, _ := store.InsertTrade(ctx, store.Trade{SignalID: signalID, Ticker: "RINTENTLINKEDREPLACE", Direction: "long", Strategy: "test", EntryPrice: 100, Quantity: 1})
	parent := store.StockOrderIntent{ID: "55555555-5555-4555-8555-555555555555", IdempotencyKey: "parent-linked", ClientOrderID: "parent-linked-id", TradeID: tradeID, Ticker: "RINTENTLINKEDREPLACE", Side: "sell", OrderType: "stop", RequestedQty: 1, RequestPayload: []byte(`{}`), PayloadHash: "p", Status: "rejected"}
	if err := store.CreateStockOrderIntent(ctx, parent); err != nil {
		t.Fatal(err)
	}
	req := execution.OrderRequest{Ticker: parent.Ticker, Side: execution.OrderSideSell, Type: execution.OrderTypeStop, Quantity: 1, StopPrice: 91, TradeID: tradeID, IntentKey: "linked-replacement", ClientOrderID: "linked-replacement-id", ReplacesBrokerOrderID: "old-linked"}
	payload, _ := json.Marshal(req)
	child := store.StockOrderIntent{ID: "66666666-6666-4666-8666-666666666666", IdempotencyKey: req.IntentKey, ClientOrderID: req.ClientOrderID, TradeID: tradeID, ParentIntentID: parent.ID, Ticker: parent.Ticker, Side: "sell", OrderType: "stop", RequestedQty: 1, RequestPayload: payload, PayloadHash: "c", Status: "pending"}
	if err := store.CreateStockOrderIntent(ctx, child); err != nil {
		t.Fatal(err)
	}
	if _, err := store.InsertOrder(ctx, store.Order{TradeID: tradeID, BrokerOrderID: "new-linked", Ticker: parent.Ticker, Side: "sell", OrderType: "stop", Quantity: 1, StopPrice: 91, Status: "pending", IsStopLoss: true, StockIntentID: child.ID}); err != nil {
		t.Fatal(err)
	}
	broker := newRecoveryBroker(execution.OrderResult{})
	broker.lookup[child.ClientOrderID] = execution.OrderResult{BrokerOrderID: "new-linked", Status: "accepted"}
	if err := stockorders.New(broker).Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if len(broker.patches) != 0 {
		t.Fatal("already-linked replacement was resubmitted")
	}
}

func TestStockIntentFilledStandaloneStopMissingProjectionRecovers(t *testing.T) {
	ctx := context.Background()
	if _, err := store.GetDB().Exec(ctx, `UPDATE stock_order_intents SET status='rejected', purpose='superseded'`); err != nil {
		t.Fatal(err)
	}
	signalID := insertRecoverySignal(t, "RINTENTFILLEDSTOP")
	tradeID, _ := store.InsertTrade(ctx, store.Trade{SignalID: signalID, Ticker: "RINTENTFILLEDSTOP", Direction: "long", Strategy: "test", EntryPrice: 100, Quantity: 1})
	broker := newRecoveryBroker(execution.OrderResult{BrokerOrderID: "filled-stop", Status: "filled", FilledPrice: 90, FilledQty: 1})
	c := stockorders.New(broker)
	req := execution.OrderRequest{Ticker: "RINTENTFILLEDSTOP", Side: execution.OrderSideSell, Type: execution.OrderTypeStop, Quantity: 1, StopPrice: 90, TradeID: tradeID, IntentKey: "protective-stop-filled:" + tradeID}
	if _, err := c.PlaceOrder(ctx, req); err != nil {
		t.Fatal(err)
	}
	in := intentForKey(t, req.IntentKey)
	if err := c.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	o, err := store.GetOrderByStockIntentID(ctx, in.ID)
	if err != nil || o == nil || o.Status != "filled" || o.FilledQty != 1 {
		t.Fatalf("filled stop projection=%#v err=%v", o, err)
	}
}

func TestStockIntentReplacementRecoveryCompletesIntentOnlyCrash(t *testing.T) {
	ctx := context.Background()
	if _, err := store.GetDB().Exec(ctx, `UPDATE stock_order_intents SET status='rejected', purpose='superseded'`); err != nil {
		t.Fatal(err)
	}
	signalID := insertRecoverySignal(t, "RINTENTREPLACECRASH")
	tradeID, err := store.InsertTrade(ctx, store.Trade{SignalID: signalID, Ticker: "RINTENTREPLACECRASH", Direction: "long", Strategy: "test", EntryPrice: 100, Quantity: 1})
	if err != nil {
		t.Fatal(err)
	}
	parent := store.StockOrderIntent{ID: "33333333-3333-4333-8333-333333333333", IdempotencyKey: "protective-stop:" + tradeID, ClientOrderID: "crash-parent", TradeID: tradeID, Purpose: "superseded", Ticker: "RINTENTREPLACECRASH", Side: "sell", OrderType: "stop", RequestedQty: 1, RequestPayload: []byte(`{}`), PayloadHash: "p", Status: "rejected"}
	if err := store.CreateStockOrderIntent(ctx, parent); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateStockOrderIntentBroker(ctx, parent.ClientOrderID, "old", "rejected", 0, 0, "", "", true); err != nil {
		t.Fatal(err)
	}
	orderID, err := store.InsertOrder(ctx, store.Order{TradeID: tradeID, BrokerOrderID: "new", Ticker: parent.Ticker, Side: "sell", OrderType: "stop", Quantity: 1, StopPrice: 91, Status: "pending", IsStopLoss: true, StockIntentID: parent.ID})
	if err != nil {
		t.Fatal(err)
	}
	req := execution.OrderRequest{Ticker: parent.Ticker, Side: execution.OrderSideSell, Type: execution.OrderTypeStop, Quantity: 1, StopPrice: 91, TradeID: tradeID, IntentKey: "replace-crash", ClientOrderID: "crash-replacement", ReplacesBrokerOrderID: "old"}
	payload, _ := json.Marshal(req)
	child := store.StockOrderIntent{ID: "44444444-4444-4444-8444-444444444444", IdempotencyKey: req.IntentKey, ClientOrderID: req.ClientOrderID, TradeID: tradeID, ParentIntentID: parent.ID, Ticker: parent.Ticker, Side: "sell", OrderType: "stop", RequestedQty: 1, RequestPayload: payload, PayloadHash: "c", Status: "pending"}
	if err := store.CreateStockOrderIntent(ctx, child); err != nil {
		t.Fatal(err)
	}
	broker := newRecoveryBroker(execution.OrderResult{})
	broker.lookup[child.ClientOrderID] = execution.OrderResult{BrokerOrderID: "new", Status: "accepted"}
	if err := stockorders.New(broker).Recover(ctx); err != nil {
		t.Fatal(err)
	}
	var got string
	if err := store.GetDB().QueryRow(ctx, `SELECT stock_intent_id::text FROM orders WHERE id=$1::uuid`, orderID).Scan(&got); err != nil || got != child.ID {
		t.Fatalf("repoint = %q %v", got, err)
	}
}

func TestStockIntentAdoptsLegacyBracketStopForAtomicReplacement(t *testing.T) {
	ctx := context.Background()
	signalID := insertRecoverySignal(t, "RINTENTLEGACYBRACKET")
	tradeID, err := store.InsertTrade(ctx, store.Trade{SignalID: signalID, Ticker: "RINTENTLEGACYBRACKET", Direction: "long", Strategy: "test", EntryPrice: 100, Quantity: 2, StopLoss: 90})
	if err != nil {
		t.Fatal(err)
	}
	orderID, err := store.InsertOrder(ctx, store.Order{TradeID: tradeID, BrokerOrderID: "legacy-bracket-stop", ParentOrderID: "legacy-parent", Ticker: "RINTENTLEGACYBRACKET", Side: "sell", OrderType: "stop", Quantity: 2, StopPrice: 90, Status: "pending", IsStopLoss: true})
	if err != nil {
		t.Fatal(err)
	}
	broker := newRecoveryBroker(execution.OrderResult{})
	coordinator := stockorders.New(broker)
	res, err := coordinator.ReplaceStopOrder(ctx, "legacy-bracket-stop", 91)
	if err != nil {
		t.Fatalf("replace legacy bracket stop: %v", err)
	}
	if res.BrokerOrderID == "" || len(broker.patches) != 1 {
		t.Fatalf("legacy replacement = %+v patches=%v", res, broker.patches)
	}
	var intentID string
	if err := store.GetDB().QueryRow(ctx, `SELECT stock_intent_id::text FROM orders WHERE id=$1::uuid`, orderID).Scan(&intentID); err != nil || intentID == "" {
		t.Fatalf("legacy stop was not linked: %q %v", intentID, err)
	}
}

func TestStockIntentLegacyStopAdoptionIsNeverReplayedAfterAmbiguousReplacement(t *testing.T) {
	ctx := context.Background()
	if _, err := store.GetDB().Exec(ctx, `UPDATE stock_order_intents SET status='rejected', purpose='superseded'`); err != nil {
		t.Fatal(err)
	}
	signalID := insertRecoverySignal(t, "RINTENTLEGACYAMBIG")
	tradeID, err := store.InsertTrade(ctx, store.Trade{SignalID: signalID, Ticker: "RINTENTLEGACYAMBIG", Direction: "long", Strategy: "test", EntryPrice: 100, Quantity: 1, StopLoss: 90})
	if err != nil {
		t.Fatal(err)
	}
	orderID, err := store.InsertOrder(ctx, store.Order{TradeID: tradeID, BrokerOrderID: "legacy-ambiguous-stop", Ticker: "RINTENTLEGACYAMBIG", Side: "sell", OrderType: "stop", Quantity: 1, StopPrice: 90, Status: "pending", IsStopLoss: true})
	if err != nil {
		t.Fatal(err)
	}
	broker := newRecoveryBroker(execution.OrderResult{})
	broker.patchErr = errors.New("ambiguous patch response")
	coordinator := stockorders.New(broker)
	if _, err := coordinator.ReplaceStopOrder(ctx, "legacy-ambiguous-stop", 91); !errors.Is(err, execution.ErrAmbiguousStopReplacement) {
		t.Fatalf("legacy ambiguous replacement=%v, want ambiguity", err)
	}
	var predecessorID, purpose string
	if err := store.GetDB().QueryRow(ctx, `SELECT stock_intent_id::text FROM orders WHERE id=$1::uuid`, orderID).Scan(&predecessorID); err != nil {
		t.Fatal(err)
	}
	if err := store.GetDB().QueryRow(ctx, `SELECT purpose FROM stock_order_intents WHERE id=$1::uuid`, predecessorID).Scan(&purpose); err != nil || purpose != "broker_projection" {
		t.Fatalf("adopted legacy predecessor purpose=%q err=%v", purpose, err)
	}
	posts, patches := broker.posts, len(broker.patches)
	if err := coordinator.Recover(ctx); err == nil {
		t.Fatal("ambiguous replacement recovery unexpectedly resolved")
	}
	if broker.posts != posts {
		t.Fatalf("recovery replayed adopted legacy submit: posts %d -> %d", posts, broker.posts)
	}
	if len(broker.patches) != patches+2 {
		t.Fatalf("recovery replacement attempts=%d, want unresolved replacement retry only", len(broker.patches))
	}
	if got := countForIntent(t, "orders", predecessorID); got != 1 {
		t.Fatalf("legacy adoption created duplicate stop projections=%d", got)
	}
}

func TestStockIntentReplacementSupersedesPredecessorAfterRecoveryProjection(t *testing.T) {
	ctx := context.Background()
	if _, err := store.GetDB().Exec(ctx, `UPDATE stock_order_intents SET status='rejected', purpose='superseded'`); err != nil {
		t.Fatal(err)
	}
	signalID := insertRecoverySignal(t, "RINTENTSUPERSEDE")
	tradeID, err := store.InsertTrade(ctx, store.Trade{SignalID: signalID, Ticker: "RINTENTSUPERSEDE", Direction: "long", Strategy: "test", EntryPrice: 100, Quantity: 1, StopLoss: 90})
	if err != nil {
		t.Fatal(err)
	}
	var parentID, childID string
	if err := store.GetDB().QueryRow(ctx, `SELECT uuid_generate_v4(), uuid_generate_v4()`).Scan(&parentID, &childID); err != nil {
		t.Fatal(err)
	}
	parentReq := execution.OrderRequest{Ticker: "RINTENTSUPERSEDE", Side: execution.OrderSideSell, Type: execution.OrderTypeStop, Quantity: 1, StopPrice: 90, TradeID: tradeID, IntentKey: "protective-stop-supersede:" + tradeID, ClientOrderID: "supersede-parent"}
	parentPayload, _ := json.Marshal(parentReq)
	parent := store.StockOrderIntent{ID: parentID, IdempotencyKey: parentReq.IntentKey, ClientOrderID: "supersede-parent-" + parentID[:8], TradeID: tradeID, Ticker: parentReq.Ticker, Side: "sell", OrderType: "stop", RequestedQty: 1, RequestPayload: parentPayload, PayloadHash: "parent", Status: "pending"}
	parentReq.ClientOrderID = parent.ClientOrderID
	parentPayload, _ = json.Marshal(parentReq)
	parent.RequestPayload = parentPayload
	if err := store.CreateStockOrderIntent(ctx, parent); err != nil {
		t.Fatal(err)
	}
	orderID, err := store.InsertOrder(ctx, store.Order{TradeID: tradeID, BrokerOrderID: "old-superseded-stop", Ticker: parent.Ticker, Side: "sell", OrderType: "stop", Quantity: 1, StopPrice: 90, Status: "pending", IsStopLoss: true, StockIntentID: parent.ID})
	if err != nil {
		t.Fatal(err)
	}
	childReq := execution.OrderRequest{Ticker: parent.Ticker, Side: execution.OrderSideSell, Type: execution.OrderTypeStop, Quantity: 1, StopPrice: 91, TradeID: tradeID, IntentKey: "replace-supersede:" + tradeID, ClientOrderID: "supersede-child-" + childID[:8], ReplacesBrokerOrderID: "old-superseded-stop"}
	childPayload, _ := json.Marshal(childReq)
	child := store.StockOrderIntent{ID: childID, IdempotencyKey: childReq.IntentKey, ClientOrderID: childReq.ClientOrderID, TradeID: tradeID, ParentIntentID: parent.ID, Ticker: childReq.Ticker, Side: "sell", OrderType: "stop", RequestedQty: 1, RequestPayload: childPayload, PayloadHash: "child", Status: "pending"}
	if err := store.CreateStockOrderIntent(ctx, child); err != nil {
		t.Fatal(err)
	}
	broker := newRecoveryBroker(execution.OrderResult{})
	broker.lookup[parent.ClientOrderID] = execution.OrderResult{BrokerOrderID: "old-superseded-stop", Status: "accepted"}
	broker.lookup[child.ClientOrderID] = execution.OrderResult{BrokerOrderID: "new-active-stop", Status: "accepted"}
	coordinator := stockorders.New(broker)
	if err := coordinator.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	var purpose, linkedIntent string
	if err := store.GetDB().QueryRow(ctx, `SELECT purpose FROM stock_order_intents WHERE id=$1::uuid`, parent.ID).Scan(&purpose); err != nil || purpose != "superseded" {
		t.Fatalf("predecessor purpose=%q err=%v", purpose, err)
	}
	if err := store.GetDB().QueryRow(ctx, `SELECT stock_intent_id::text FROM orders WHERE id=$1::uuid`, orderID).Scan(&linkedIntent); err != nil || linkedIntent != child.ID {
		t.Fatalf("active replacement link=%q err=%v", linkedIntent, err)
	}
	beforeLookups, beforeAttempts := len(broker.lookupIDs), countStockIntentAttempts(t, parent.ID)
	if err := coordinator.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	for _, clientID := range broker.lookupIDs[beforeLookups:] {
		if clientID == parent.ClientOrderID {
			t.Fatal("superseded predecessor replayed lookup")
		}
	}
	if countStockIntentAttempts(t, parent.ID) != beforeAttempts {
		t.Fatalf("superseded predecessor created attempt")
	}
	if got := countForIntent(t, "orders", parent.ID); got != 0 {
		t.Fatalf("superseded predecessor retained obsolete projection=%d", got)
	}
}
