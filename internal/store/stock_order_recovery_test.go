package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/romeo-folie/trade-engine/internal/execution"
	"github.com/romeo-folie/trade-engine/internal/stockorders"
	"github.com/romeo-folie/trade-engine/internal/store"
)

// recoveryBroker is deliberately small: it models the only ambiguity that the
// coordinator must resolve, where a POST response was lost but Alpaca can find
// the accepted order by its caller-provided client id.
type recoveryBroker struct {
	posts   int
	posted  map[string]execution.OrderResult
	lookup  map[string]execution.OrderResult
	status  map[string]execution.OrderResult
	cancels []string
	patches []string
	postRes execution.OrderResult
	postErr error
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
	res := execution.OrderResult{BrokerOrderID: "patched-" + fmt.Sprint(len(b.patches)), Status: "accepted"}
	b.posted[clientID] = res
	return res, nil
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

func TestStockIntentRecoveryAcceptedBeforeProjectionFailure(t *testing.T) {
	ctx := context.Background()
	signalID := insertRecoverySignal(t, "RINTENTACCEPT")
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

func TestStockIntentDefinitiveRejectionIsTerminalButAmbiguityRecovers(t *testing.T) {
	ctx := context.Background()
	if _, err := store.GetDB().Exec(ctx, `UPDATE stock_order_intents SET status='rejected'`); err != nil {
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
	if _, err := store.GetDB().Exec(ctx, `UPDATE stock_order_intents SET status='rejected'`); err != nil {
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
	if _, err := store.GetDB().Exec(ctx, `UPDATE stock_order_intents SET status='rejected'`); err != nil {
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
	if _, err := store.GetDB().Exec(ctx, `UPDATE stock_order_intents SET status='rejected' WHERE id=$1::uuid`, parent.ID); err != nil {
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
