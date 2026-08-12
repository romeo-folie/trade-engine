package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/data"
	"github.com/romeo-folie/trade-engine/internal/execution"
	"github.com/romeo-folie/trade-engine/internal/stockorders"
	"github.com/romeo-folie/trade-engine/internal/store"
)

type protectiveRetryBroker struct {
	posts      []execution.OrderRequest
	lookup     map[string]execution.OrderResult
	brokerToID map[string]string
}

func newProtectiveRetryBroker() *protectiveRetryBroker {
	return &protectiveRetryBroker{lookup: map[string]execution.OrderResult{}, brokerToID: map[string]string{}}
}

func (b *protectiveRetryBroker) PlaceOrder(_ context.Context, req execution.OrderRequest) (execution.OrderResult, error) {
	b.posts = append(b.posts, req)
	brokerID := "rollback-cancelled"
	if len(b.posts) > 1 {
		brokerID = "fresh-live-stop"
	}
	b.brokerToID[brokerID] = req.ClientOrderID
	return execution.OrderResult{BrokerOrderID: brokerID, Status: "accepted"}, nil
}
func (b *protectiveRetryBroker) GetOrderByClientID(_ context.Context, id string) (execution.OrderResult, error) {
	if res, ok := b.lookup[id]; ok {
		return res, nil
	}
	return execution.OrderResult{}, execution.ErrOrderNotFound
}
func (b *protectiveRetryBroker) CancelOrder(_ context.Context, id string) error {
	b.lookup[b.brokerToID[id]] = execution.OrderResult{BrokerOrderID: id, Status: "cancelled"}
	return nil
}
func (b *protectiveRetryBroker) GetOrderStatus(_ context.Context, id string) (execution.OrderResult, error) {
	return execution.OrderResult{BrokerOrderID: id, Status: "cancelled"}, nil
}
func (*protectiveRetryBroker) GetPositions(context.Context) ([]execution.Position, error) {
	return nil, nil
}
func (*protectiveRetryBroker) GetAccount(context.Context) (execution.AccountInfo, error) {
	return execution.AccountInfo{}, nil
}
func (*protectiveRetryBroker) ReplaceStopOrder(context.Context, string, float64) (execution.OrderResult, error) {
	return execution.OrderResult{}, errors.New("unused")
}

type terminalGenerationBroker struct {
	posts      []execution.OrderRequest
	lookup     map[string]execution.OrderResult
	brokerToID map[string]string
	statuses   []string
}

// marketExitGenerationBroker returns a succession of terminal zero-fill
// market responses before a live fill. It deliberately has no client-id
// lookup result, so each retry must be a freshly generated durable intent.
type marketExitGenerationBroker struct {
	posts    []execution.OrderRequest
	statuses []string
	err      error
}

func (b *marketExitGenerationBroker) PlaceOrder(_ context.Context, req execution.OrderRequest) (execution.OrderResult, error) {
	b.posts = append(b.posts, req)
	if b.err != nil {
		return execution.OrderResult{}, b.err
	}
	status := b.statuses[len(b.posts)-1]
	res := execution.OrderResult{BrokerOrderID: fmt.Sprintf("market-exit-generation-%d", len(b.posts)), Status: status}
	if status == "filled" {
		res.FilledPrice, res.FilledQty = 120, req.Quantity
	}
	return res, nil
}
func (*marketExitGenerationBroker) GetOrderByClientID(context.Context, string) (execution.OrderResult, error) {
	return execution.OrderResult{}, execution.ErrOrderNotFound
}
func (*marketExitGenerationBroker) CancelOrder(context.Context, string) error { return nil }
func (*marketExitGenerationBroker) GetOrderStatus(context.Context, string) (execution.OrderResult, error) {
	return execution.OrderResult{}, errors.New("unused")
}
func (*marketExitGenerationBroker) GetPositions(context.Context) ([]execution.Position, error) {
	return nil, nil
}
func (*marketExitGenerationBroker) GetAccount(context.Context) (execution.AccountInfo, error) {
	return execution.AccountInfo{}, nil
}
func (*marketExitGenerationBroker) ReplaceStopOrder(context.Context, string, float64) (execution.OrderResult, error) {
	return execution.OrderResult{}, errors.New("unused")
}

func TestMonitorPositionsRotatesTerminalSoftwareExitGenerations(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("DATABASE_URL not set; skipping DB-backed engine test")
	}
	ctx := context.Background()
	ticker := fmt.Sprintf("EXITTGEN%d", time.Now().UnixNano())
	tradeID, err := store.InsertTrade(ctx, store.Trade{
		Ticker: ticker, Direction: "long", Strategy: "momentum",
		EntryPrice: 100, Quantity: 3, TakeProfit: 110, Status: "open",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertCandles(ctx, []data.Candle{{
		Ticker: ticker, Timeframe: data.Timeframe5Min, Timestamp: time.Now().UTC(),
		Open: 120, High: 121, Low: 119, Close: 120, Volume: 1000,
	}}); err != nil {
		t.Fatal(err)
	}
	broker := &marketExitGenerationBroker{statuses: []string{"cancelled", "rejected", "expired", "filled"}}
	coordinator := stockorders.New(broker)
	baseKey := "software-exit:" + tradeID + ":take-profit hit"
	// The first market exit reached the broker but finished zero-fill. This
	// simulates the released-claim retry where the stale base key now returns
	// ErrTerminalZeroFillPlacement before the engine can submit again.
	if _, err := coordinator.PlaceOrder(ctx, execution.OrderRequest{
		Ticker: ticker, Side: execution.OrderSideSell, Type: execution.OrderTypeMarket,
		Quantity: 3, ReferencePrice: 120, TradeID: tradeID, IntentKey: baseKey,
	}); err != nil {
		t.Fatal(err)
	}
	e := newBracketTestEngine(coordinator)
	// Each monitor tick may submit at most one fresh generation. Persistent
	// terminal broker responses are paced across ticks rather than consuming
	// an unbounded retry lineage in one scheduling cycle.
	for attempt := 0; attempt < 3 && len(broker.posts) < 4; attempt++ {
		e.monitorPositions(ctx)
	}

	if got := len(broker.posts); got != 4 {
		t.Fatalf("market-exit submissions=%d, want terminal generations plus live fill", got)
	}
	key := baseKey
	seenClients := map[string]bool{}
	for i, req := range broker.posts {
		if req.IntentKey != key {
			t.Fatalf("generation %d key=%q, want %q", i, req.IntentKey, key)
		}
		if req.ClientOrderID == "" || seenClients[req.ClientOrderID] {
			t.Fatalf("generation %d reused/omitted client id: %q", i, req.ClientOrderID)
		}
		seenClients[req.ClientOrderID] = true
		key = store.StockOrderIntentRetryKey(key, fmt.Sprintf("market-exit-generation-%d", i+1))
	}
	finalIntent, err := store.GetStockOrderIntentByKey(ctx, broker.posts[len(broker.posts)-1].IntentKey)
	if err != nil || finalIntent == nil {
		t.Fatalf("final market-exit intent=%#v err=%v", finalIntent, err)
	}
	var rows int
	if err := store.GetDB().QueryRow(ctx, `SELECT COUNT(*) FROM orders WHERE trade_id=$1::uuid`, tradeID).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("final market-exit orders=%d err=%v, want one", rows, err)
	}
	trade, err := store.GetTradeByID(ctx, tradeID)
	if err != nil || trade == nil || trade.Status != "closed" || trade.Quantity != 3 {
		t.Fatalf("terminal software exit trade=%#v err=%v", trade, err)
	}
	// A closed trade is no longer eligible for a fresh claim or a duplicate
	// market exit on the next monitor pass.
	e.monitorPositions(ctx)
	if got := len(broker.posts); got != 4 {
		t.Fatalf("closed trade replayed market exit: %d submissions", got)
	}
}

func TestMarketExitFreshDefinitiveRejectionWaitsForNextTick(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("DATABASE_URL not set; skipping DB-backed engine test")
	}
	broker := &marketExitGenerationBroker{
		statuses: []string{"accepted"},
		err:      &execution.BrokerRequestError{StatusCode: 422, Message: "market exit rejected"},
	}
	coordinator := stockorders.New(broker)
	e := newBracketTestEngine(coordinator)
	_, _, terminalZero, err := e.placeMarketExitWithTerminalRetries(context.Background(), execution.OrderRequest{
		Ticker: "FRESHREJECT", Side: execution.OrderSideSell, Type: execution.OrderTypeMarket,
		Quantity: 1, IntentKey: "software-exit:fresh-rejection:take-profit",
	})
	if !terminalZero || !errors.Is(err, execution.ErrFreshTerminalZeroFillPlacement) {
		t.Fatalf("fresh definitive exit result terminal=%v err=%v", terminalZero, err)
	}
	if len(broker.posts) != 1 {
		t.Fatalf("fresh definitive exit submissions=%d, want one", len(broker.posts))
	}
}

func TestMarketExitAmbiguityDoesNotRotateIntentGeneration(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("DATABASE_URL not set; skipping DB-backed engine test")
	}
	broker := &marketExitGenerationBroker{statuses: []string{"filled"}, err: errors.New("response lost")}
	e := newBracketTestEngine(stockorders.New(broker))
	_, key, terminalZero, err := e.placeMarketExitWithTerminalRetries(context.Background(), execution.OrderRequest{
		Ticker: "EXITAMBIG", Side: execution.OrderSideSell, Type: execution.OrderTypeMarket,
		Quantity: 1, IntentKey: "software-exit:ambiguous:test",
	})
	if err == nil || terminalZero {
		t.Fatalf("ambiguous exit err=%v terminal=%v, want nonterminal error", err, terminalZero)
	}
	if key != "software-exit:ambiguous:test" || len(broker.posts) != 2 {
		t.Fatalf("ambiguous exit rotated key=%q posts=%d, want exact replay only", key, len(broker.posts))
	}
}

func newTerminalGenerationBroker(statuses ...string) *terminalGenerationBroker {
	return &terminalGenerationBroker{lookup: map[string]execution.OrderResult{}, brokerToID: map[string]string{}, statuses: statuses}
}

func (b *terminalGenerationBroker) PlaceOrder(_ context.Context, req execution.OrderRequest) (execution.OrderResult, error) {
	b.posts = append(b.posts, req)
	brokerID := fmt.Sprintf("terminal-generation-%d", len(b.posts))
	b.brokerToID[brokerID] = req.ClientOrderID
	return execution.OrderResult{BrokerOrderID: brokerID, Status: "accepted"}, nil
}
func (b *terminalGenerationBroker) GetOrderByClientID(_ context.Context, id string) (execution.OrderResult, error) {
	if res, ok := b.lookup[id]; ok {
		return res, nil
	}
	return execution.OrderResult{}, execution.ErrOrderNotFound
}
func (b *terminalGenerationBroker) CancelOrder(_ context.Context, brokerID string) error {
	status := "cancelled"
	if n := len(b.lookup); n < len(b.statuses) {
		status = b.statuses[n]
	}
	b.lookup[b.brokerToID[brokerID]] = execution.OrderResult{BrokerOrderID: brokerID, Status: status}
	return nil
}
func (b *terminalGenerationBroker) GetOrderStatus(_ context.Context, brokerID string) (execution.OrderResult, error) {
	if res, ok := b.lookup[b.brokerToID[brokerID]]; ok {
		return res, nil
	}
	return execution.OrderResult{BrokerOrderID: brokerID, Status: "pending"}, nil
}
func (*terminalGenerationBroker) GetPositions(context.Context) ([]execution.Position, error) {
	return nil, nil
}
func (*terminalGenerationBroker) GetAccount(context.Context) (execution.AccountInfo, error) {
	return execution.AccountInfo{}, nil
}
func (*terminalGenerationBroker) ReplaceStopOrder(context.Context, string, float64) (execution.OrderResult, error) {
	return execution.OrderResult{}, errors.New("unused")
}

func TestEnsureUncappedProtectiveStopSeedsImmediately(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("DATABASE_URL not set; skipping DB-backed engine test")
	}
	ctx := context.Background()
	tradeID, err := store.InsertTrade(ctx, store.Trade{
		Ticker: "UCPS", Direction: "long", Strategy: "momentum",
		EntryPrice: 100, Quantity: 5, StopLoss: 95, Status: "open",
	})
	if err != nil {
		t.Fatalf("InsertTrade: %v", err)
	}
	exec := &fakeBracketExec{placeRes: execution.OrderResult{
		BrokerOrderID: "uncap-stop-1", Status: "accepted",
	}}
	e := newBracketTestEngine(exec)

	if err := e.ensureUncappedProtectiveStop(ctx, tradeID, "UCPS", 5, 95); err != nil {
		t.Fatalf("ensureUncappedProtectiveStop: %v", err)
	}
	if len(exec.placed) != 1 {
		t.Fatalf("placed = %d, want one immediate stop", len(exec.placed))
	}
	req := exec.placed[0]
	if req.Type != execution.OrderTypeStop || req.Side != execution.OrderSideSell ||
		req.Quantity != 5 || req.StopPrice != 95 || req.TimeInForce != "gtc" {
		t.Fatalf("protective request = %+v", req)
	}
	if req.ExitReason != store.ExitReasonStopLoss {
		t.Fatalf("durable protective request reason=%q, want stop_loss", req.ExitReason)
	}
	stop, err := store.LatestStopLossOrderForTrade(ctx, tradeID)
	if err != nil || stop == nil {
		t.Fatalf("LatestStopLossOrderForTrade: %v (nil=%v)", err, stop == nil)
	}
	if stop.Quantity != 5 || stop.BrokerOrderID != "uncap-stop-1" {
		t.Fatalf("stored stop = %+v", stop)
	}
	var evidence map[string]any
	if err := json.Unmarshal(stop.ExitEvidence, &evidence); err != nil ||
		stop.ExitReason != store.ExitReasonStopLoss || evidence["stop_loss"] != 95.0 {
		t.Fatalf("stored stop provenance = %q %s, err=%v", stop.ExitReason, stop.ExitEvidence, err)
	}

	// Idempotent when the trailing manager or fill poll reaches the same fill.
	if err := e.ensureUncappedProtectiveStop(ctx, tradeID, "UCPS", 5, 95); err != nil {
		t.Fatalf("second ensure: %v", err)
	}
	if len(exec.placed) != 1 {
		t.Fatalf("idempotent ensure placed %d stops, want 1", len(exec.placed))
	}
}

func TestEnsureUncappedProtectiveStopRollbackCancellationRotatesIntent(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("DATABASE_URL not set; skipping DB-backed engine test")
	}
	ctx := context.Background()
	tradeID, err := store.InsertTrade(ctx, store.Trade{Ticker: "UCPRETRY", Direction: "long", Strategy: "momentum", EntryPrice: 100, Quantity: 5, StopLoss: 95, Status: "open"})
	if err != nil {
		t.Fatal(err)
	}
	broker := newProtectiveRetryBroker()
	e := newBracketTestEngine(stockorders.New(broker))
	originalInsert := insertUncappedProtectiveOrder
	insertCalls := 0
	insertUncappedProtectiveOrder = func(ctx context.Context, order store.Order) (string, error) {
		insertCalls++
		if insertCalls == 1 {
			return "", errors.New("simulated first protective projection failure")
		}
		return originalInsert(ctx, order)
	}
	defer func() { insertUncappedProtectiveOrder = originalInsert }()

	if err := e.ensureUncappedProtectiveStop(ctx, tradeID, "UCPRETRY", 5, 95); err == nil {
		t.Fatal("first accepted stop should report projection failure after rollback")
	}
	if got := len(broker.posts); got != 1 {
		t.Fatalf("first posts=%d, want 1", got)
	}
	if err := e.ensureUncappedProtectiveStop(ctx, tradeID, "UCPRETRY", 5, 95); err != nil {
		t.Fatalf("retry after confirmed cancellation: %v", err)
	}
	if got := len(broker.posts); got != 2 {
		t.Fatalf("posts=%d, want original plus fresh generation", got)
	}
	baseKey := protectiveStopIntentKey(tradeID, 95, 5)
	retryKey := protectiveStopRetryIntentKey(baseKey, "rollback-cancelled")
	if broker.posts[0].IntentKey != baseKey || broker.posts[1].IntentKey != retryKey || broker.posts[0].ClientOrderID == broker.posts[1].ClientOrderID {
		t.Fatalf("retry generation = %#v", broker.posts)
	}
	activeIntent, err := store.GetStockOrderIntentByKey(ctx, retryKey)
	if err != nil || activeIntent == nil {
		t.Fatalf("active retry intent=%#v err=%v", activeIntent, err)
	}
	var brokerID, intentID string
	if err := store.GetDB().QueryRow(ctx, `SELECT broker_order_id, stock_intent_id::text FROM orders WHERE trade_id=$1::uuid AND status='pending'`, tradeID).Scan(&brokerID, &intentID); err != nil || brokerID != "fresh-live-stop" || intentID != activeIntent.ID {
		t.Fatalf("active retry protection broker=%q intent=%q err=%v", brokerID, intentID, err)
	}
}

func TestEnsureUncappedProtectiveStopFollowsTerminalGenerations(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("DATABASE_URL not set; skipping DB-backed engine test")
	}
	ctx := context.Background()
	tradeID, err := store.InsertTrade(ctx, store.Trade{Ticker: "UCMULTIGEN", Direction: "long", Strategy: "momentum", EntryPrice: 100, Quantity: 5, StopLoss: 95, Status: "open"})
	if err != nil {
		t.Fatal(err)
	}
	broker := newTerminalGenerationBroker("cancelled", "rejected", "expired")
	e := newBracketTestEngine(stockorders.New(broker))
	originalInsert := insertUncappedProtectiveOrder
	insertCalls := 0
	insertUncappedProtectiveOrder = func(ctx context.Context, order store.Order) (string, error) {
		insertCalls++
		if insertCalls <= 3 {
			return "", fmt.Errorf("simulated protective projection failure %d", insertCalls)
		}
		return originalInsert(ctx, order)
	}
	defer func() { insertUncappedProtectiveOrder = originalInsert }()

	for attempt := 1; attempt <= 3; attempt++ {
		if err := e.ensureUncappedProtectiveStop(ctx, tradeID, "UCMULTIGEN", 5, 95); err == nil {
			t.Fatalf("terminal generation %d unexpectedly projected", attempt)
		}
	}
	if err := e.ensureUncappedProtectiveStop(ctx, tradeID, "UCMULTIGEN", 5, 95); err != nil {
		t.Fatalf("fourth generation should provide live protection: %v", err)
	}
	if got := len(broker.posts); got != 4 {
		t.Fatalf("stop submissions=%d, want four distinct generations", got)
	}
	key := protectiveStopIntentKey(tradeID, 95, 5)
	activeKey := ""
	for i, req := range broker.posts {
		if req.IntentKey != key {
			t.Fatalf("generation %d key=%q, want %q", i+1, req.IntentKey, key)
		}
		if i > 0 && req.ClientOrderID == broker.posts[i-1].ClientOrderID {
			t.Fatalf("generation %d reused client id %q", i+1, req.ClientOrderID)
		}
		if i == len(broker.posts)-1 {
			activeKey = key
		}
		key = protectiveStopRetryIntentKey(key, fmt.Sprintf("terminal-generation-%d", i+1))
	}
	active, err := store.GetStockOrderIntentByKey(ctx, activeKey)
	if err != nil || active == nil || active.Status != "pending" || active.BrokerOrderID != "terminal-generation-4" {
		t.Fatalf("active protective generation=%#v err=%v", active, err)
	}
	var rows int
	var linkedIntent string
	if err := store.GetDB().QueryRow(ctx, `SELECT COUNT(*), COALESCE(MAX(stock_intent_id::text),'') FROM orders WHERE trade_id=$1::uuid AND status='pending'`, tradeID).Scan(&rows, &linkedIntent); err != nil || rows != 1 || linkedIntent != active.ID {
		t.Fatalf("live protective rows=%d intent=%q err=%v, want one linked to %q", rows, linkedIntent, err, active.ID)
	}
}

type definitiveStopRejector struct{ calls int }

func (b *definitiveStopRejector) PlaceOrder(context.Context, execution.OrderRequest) (execution.OrderResult, error) {
	b.calls++
	return execution.OrderResult{}, &execution.BrokerRequestError{StatusCode: 422, Message: "invalid protective stop"}
}
func (*definitiveStopRejector) CancelOrder(context.Context, string) error { return nil }
func (*definitiveStopRejector) GetOrderStatus(context.Context, string) (execution.OrderResult, error) {
	return execution.OrderResult{}, execution.ErrOrderNotFound
}
func (*definitiveStopRejector) GetPositions(context.Context) ([]execution.Position, error) {
	return nil, nil
}
func (*definitiveStopRejector) GetAccount(context.Context) (execution.AccountInfo, error) {
	return execution.AccountInfo{}, nil
}
func (*definitiveStopRejector) ReplaceStopOrder(context.Context, string, float64) (execution.OrderResult, error) {
	return execution.OrderResult{}, errors.New("unused")
}

func TestEnsureUncappedProtectiveStopDoesNotRetryFreshDefinitiveRejection(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("DATABASE_URL not set; skipping DB-backed engine test")
	}
	ctx := context.Background()
	tradeID, err := store.InsertTrade(ctx, store.Trade{Ticker: "UCREJECT", Direction: "long", Strategy: "momentum", EntryPrice: 100, Quantity: 5, StopLoss: 95, Status: "open"})
	if err != nil {
		t.Fatal(err)
	}
	broker := &definitiveStopRejector{}
	e := newBracketTestEngine(stockorders.New(broker))
	if err := e.ensureUncappedProtectiveStop(ctx, tradeID, "UCREJECT", 5, 95); err == nil {
		t.Fatal("fresh definitive rejection unexpectedly succeeded")
	}
	if got := broker.calls; got != 1 {
		t.Fatalf("fresh definitive rejection submitted %d stops, want one", got)
	}
	intent, err := store.GetStockOrderIntentByKey(ctx, protectiveStopIntentKey(tradeID, 95, 5))
	if err != nil || intent == nil || intent.Status != "rejected" {
		t.Fatalf("rejected protective intent=%#v err=%v", intent, err)
	}
}

func TestEnsureUncappedProtectiveStopImmediateFillTransfersOwnership(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("DATABASE_URL not set; skipping DB-backed engine test")
	}
	ctx := context.Background()
	tradeID, err := store.InsertTrade(ctx, store.Trade{
		Ticker: "UCFI", Direction: "long", Strategy: "momentum",
		EntryPrice: 100, Quantity: 5, StopLoss: 95, Status: "open",
	})
	if err != nil {
		t.Fatalf("InsertTrade: %v", err)
	}
	exec := &fakeBracketExec{placeRes: execution.OrderResult{
		BrokerOrderID: "instant-stop-fill", Status: "filled",
		FilledPrice: 94.75, FilledQty: 5,
	}}
	e := newBracketTestEngine(exec)

	err = e.ensureUncappedProtectiveStop(ctx, tradeID, "UCFI", 5, 95)
	if !errors.Is(err, errProtectiveStopFilledDuringRollback) {
		t.Fatalf("error = %v, want immediate-fill sentinel", err)
	}
	trade, getErr := store.GetTradeByID(ctx, tradeID)
	if getErr != nil || trade == nil {
		t.Fatalf("GetTradeByID: %v", getErr)
	}
	if trade.Status != "exiting" {
		t.Fatalf("trade status = %q, want exiting for fill reconciliation", trade.Status)
	}
	orders, getErr := store.GetOrdersByTrade(ctx, tradeID)
	if getErr != nil || len(orders) != 1 {
		t.Fatalf("orders = %+v err=%v", orders, getErr)
	}
	if orders[0].Status != "filled" || orders[0].FilledQty != 5 ||
		orders[0].FilledPrice != 94.75 {
		t.Fatalf("immediate stop fill not durable: %+v", orders[0])
	}
}

func TestEnsureUncappedProtectiveStopGrowsWithPartialFill(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("DATABASE_URL not set; skipping DB-backed engine test")
	}
	ctx := context.Background()
	tradeID, err := store.InsertTrade(ctx, store.Trade{
		Ticker: "UCPG", Direction: "long", Strategy: "momentum",
		EntryPrice: 100, Quantity: 5, StopLoss: 95, Status: "open",
	})
	if err != nil {
		t.Fatalf("InsertTrade: %v", err)
	}
	if _, err := store.InsertOrder(ctx, store.Order{
		TradeID: tradeID, BrokerOrderID: "partial-stop", Ticker: "UCPG",
		Side: "sell", OrderType: "stop", Quantity: 2, StopPrice: 95,
		Status: "pending", IsStopLoss: true,
	}); err != nil {
		t.Fatalf("InsertOrder: %v", err)
	}
	exec := &fakeBracketExec{
		statuses: map[string]execution.OrderResult{
			"partial-stop": {Status: "cancelled"},
		},
		placeRes: execution.OrderResult{BrokerOrderID: "full-stop", Status: "accepted"},
	}
	e := newBracketTestEngine(exec)

	if err := e.ensureUncappedProtectiveStop(ctx, tradeID, "UCPG", 5, 95); err != nil {
		t.Fatalf("resize protective stop: %v", err)
	}
	if len(exec.cancelled) != 1 || exec.cancelled[0] != "partial-stop" {
		t.Fatalf("cancelled = %v, want partial-stop", exec.cancelled)
	}
	if len(exec.placed) != 1 || exec.placed[0].Quantity != 5 {
		t.Fatalf("replacement = %+v, want qty 5", exec.placed)
	}
	stop, err := store.LatestStopLossOrderForTrade(ctx, tradeID)
	if err != nil || stop == nil {
		t.Fatalf("LatestStopLossOrderForTrade: %v (nil=%v)", err, stop == nil)
	}
	if stop.BrokerOrderID != "full-stop" || stop.Quantity != 5 {
		t.Fatalf("full stop = %+v", stop)
	}
}

func TestEnsureUncappedProtectiveStopReprotectsRemainderAfterFilledOldStop(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("DATABASE_URL not set; skipping DB-backed engine test")
	}
	ctx := context.Background()
	tradeID, err := store.InsertTrade(ctx, store.Trade{
		Ticker: "UCPF", Direction: "long", Strategy: "momentum",
		EntryPrice: 100, Quantity: 5, StopLoss: 95, Status: "open",
	})
	if err != nil {
		t.Fatalf("InsertTrade: %v", err)
	}
	if _, err := store.InsertOrder(ctx, store.Order{
		TradeID: tradeID, BrokerOrderID: "old-filled-stop", Ticker: "UCPF",
		Side: "sell", OrderType: "stop", Quantity: 2, StopPrice: 95,
		Status: "pending", IsStopLoss: true,
	}); err != nil {
		t.Fatalf("InsertOrder: %v", err)
	}
	exec := &fakeBracketExec{
		statuses: map[string]execution.OrderResult{
			"old-filled-stop": {
				Status: "filled", FilledPrice: 94, FilledQty: 2,
			},
		},
		placeRes: execution.OrderResult{
			BrokerOrderID: "remainder-stop", Status: "accepted",
		},
	}
	e := newBracketTestEngine(exec)

	if err := e.ensureUncappedProtectiveStop(ctx, tradeID, "UCPF", 5, 95); err != nil {
		t.Fatalf("resize after old stop fill: %v", err)
	}
	if len(exec.placed) != 1 || exec.placed[0].Quantity != 3 {
		t.Fatalf("replacement = %+v, want remainder qty 3", exec.placed)
	}
	trade, err := store.GetTradeByID(ctx, tradeID)
	if err != nil || trade == nil {
		t.Fatalf("GetTradeByID: %v", err)
	}
	if trade.Status != "open" || trade.Quantity != 3 {
		t.Fatalf("trade = status %s qty %v, want open/3", trade.Status, trade.Quantity)
	}
}

func TestCancelProtectiveStopDoesNotReleaseAnotherWorkersClaim(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("DATABASE_URL not set; skipping DB-backed engine test")
	}
	ctx := context.Background()
	tradeID, err := store.InsertTrade(ctx, store.Trade{
		Ticker: "UCPO", Direction: "long", Strategy: "momentum",
		EntryPrice: 100, Quantity: 5, StopLoss: 95, Status: "open",
	})
	if err != nil {
		t.Fatalf("InsertTrade: %v", err)
	}
	orderID, err := store.InsertOrder(ctx, store.Order{
		TradeID: tradeID, BrokerOrderID: "owned-stop", Ticker: "UCPO",
		Side: "sell", OrderType: "stop", Quantity: 5, StopPrice: 95,
		Status: "pending", IsStopLoss: true,
	})
	if err != nil {
		t.Fatalf("InsertOrder: %v", err)
	}
	if claimed, err := store.ClaimTradeExit(ctx, tradeID); err != nil || !claimed {
		t.Fatalf("pre-claim trade = %v, %v", claimed, err)
	}
	exec := &fakeBracketExec{statuses: map[string]execution.OrderResult{
		"owned-stop": {Status: "cancelled", FilledPrice: 94, FilledQty: 1},
	}}
	e := newBracketTestEngine(exec)
	_, err = e.cancelProtectiveStopForResize(ctx, store.Order{
		ID: orderID, TradeID: tradeID, BrokerOrderID: "owned-stop",
		Quantity: 5,
	}, 5)
	if err == nil {
		t.Fatal("resize mutated a stop while another exit worker owned the trade")
	}
	trade, getErr := store.GetTradeByID(ctx, tradeID)
	if getErr != nil || trade == nil {
		t.Fatalf("GetTradeByID: %v", getErr)
	}
	if trade.Status != "exiting" || trade.Quantity != 5 {
		t.Fatalf("other claim was disturbed: status %s qty %v", trade.Status, trade.Quantity)
	}
}

func TestAdvanceProtectedQuantityRetriesFailedCoverage(t *testing.T) {
	attempts := 0
	ensure := func() error {
		attempts++
		if attempts == 1 {
			return errors.New("temporary cancel confirmation failure")
		}
		return nil
	}

	protected, err := advanceProtectedQuantity(2, 5, ensure)
	if err == nil {
		t.Fatal("first coverage attempt unexpectedly succeeded")
	}
	if protected != 2 {
		t.Fatalf("failed coverage advanced quantity to %v, want 2", protected)
	}

	protected, err = advanceProtectedQuantity(protected, 5, ensure)
	if err != nil {
		t.Fatalf("retry coverage: %v", err)
	}
	if protected != 5 || attempts != 2 {
		t.Fatalf("retry produced protected=%v attempts=%d, want 5/2", protected, attempts)
	}

	protected, err = advanceProtectedQuantity(protected, 5, ensure)
	if err != nil || protected != 5 || attempts != 2 {
		t.Fatalf("already-covered fill retried: protected=%v attempts=%d err=%v", protected, attempts, err)
	}
}

func TestShouldPollEntryFill(t *testing.T) {
	tests := []struct {
		name   string
		result execution.OrderResult
		want   bool
	}{
		{
			name: "immediate partial fill with price",
			result: execution.OrderResult{
				BrokerOrderID: "partial-1",
				Status:        "partially_filled",
				FilledPrice:   100,
				FilledQty:     2,
			},
			want: true,
		},
		{
			name:   "accepted with no fill",
			result: execution.OrderResult{BrokerOrderID: "pending-1", Status: "accepted"},
			want:   true,
		},
		{
			name:   "fully filled",
			result: execution.OrderResult{BrokerOrderID: "filled-1", Status: "filled", FilledPrice: 100, FilledQty: 5},
			want:   false,
		},
		{
			name:   "cancelled",
			result: execution.OrderResult{BrokerOrderID: "cancelled-1", Status: "cancelled"},
			want:   false,
		},
		{
			name:   "no broker order",
			result: execution.OrderResult{Status: "accepted"},
			want:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldPollEntryFill(tt.result); got != tt.want {
				t.Fatalf("shouldPollEntryFill(%+v) = %v, want %v", tt.result, got, tt.want)
			}
		})
	}
}

func TestRollbackUnrecordedProtectiveStop(t *testing.T) {
	tests := []struct {
		name      string
		status    execution.OrderResult
		statusErr error
		wantErr   bool
	}{
		{
			name:   "confirmed cancelled",
			status: execution.OrderResult{Status: "cancelled"},
		},
		{
			name:    "cancelled after partial fill is reported",
			status:  execution.OrderResult{Status: "cancelled", FilledPrice: 94, FilledQty: 1},
			wantErr: false,
		},
		{
			name:      "broker already purged order",
			statusErr: execution.ErrOrderNotFound,
		},
		{
			name:    "order still working",
			status:  execution.OrderResult{Status: "pending"},
			wantErr: true,
		},
		{
			name:    "GTC done-for-day remains working",
			status:  execution.OrderResult{Status: "done_for_day"},
			wantErr: true,
		},
		{
			name:    "order filled during rollback",
			status:  execution.OrderResult{Status: "filled"},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			exec := &fakeBracketExec{
				statuses:  map[string]execution.OrderResult{"stop-1": tt.status},
				statusErr: map[string]error{},
				cancelErr: map[string]error{},
			}
			if tt.statusErr != nil {
				exec.statusErr["stop-1"] = tt.statusErr
			}
			_, err := rollbackUnrecordedProtectiveStop(context.Background(), exec, "stop-1", "gtc")
			if (err != nil) != tt.wantErr {
				t.Fatalf("rollback error = %v, wantErr %v", err, tt.wantErr)
			}
			if len(exec.cancelled) != 1 || exec.cancelled[0] != "stop-1" {
				t.Fatalf("cancelled = %v, want stop-1", exec.cancelled)
			}
		})
	}
}
