package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/cost"
	"github.com/romeo-folie/trade-engine/internal/data"
	"github.com/romeo-folie/trade-engine/internal/events"
	"github.com/romeo-folie/trade-engine/internal/execution"
	"github.com/romeo-folie/trade-engine/internal/portfolio"
	"github.com/romeo-folie/trade-engine/internal/store"
)

// fakeBracketExec serves canned order statuses and records cancels/orders.
type fakeBracketExec struct {
	statuses    map[string]execution.OrderResult
	statusErr   map[string]error
	cancelErr   map[string]error
	cancelled   []string
	placed      []execution.OrderRequest
	placeRes    execution.OrderResult
	placeErr    error
	onStatus    func(string)
	positions   []execution.Position
	positionErr error
}

type recoverFailureBracketExec struct{ *fakeBracketExec }

func (*recoverFailureBracketExec) Recover(context.Context) error {
	return errors.New("transient recovery failure")
}

func (f *fakeBracketExec) PlaceOrder(_ context.Context, req execution.OrderRequest) (execution.OrderResult, error) {
	if f.placeErr != nil {
		return execution.OrderResult{}, f.placeErr
	}
	f.placed = append(f.placed, req)
	return f.placeRes, nil
}

func (f *fakeBracketExec) CancelOrder(_ context.Context, id string) error {
	if err, ok := f.cancelErr[id]; ok {
		return err
	}
	f.cancelled = append(f.cancelled, id)
	return nil
}

func (f *fakeBracketExec) GetOrderStatus(_ context.Context, id string) (execution.OrderResult, error) {
	if f.onStatus != nil {
		f.onStatus(id)
	}
	if err, ok := f.statusErr[id]; ok {
		return execution.OrderResult{}, err
	}
	if res, ok := f.statuses[id]; ok {
		return res, nil
	}
	return execution.OrderResult{}, execution.ErrOrderNotFound
}

func (f *fakeBracketExec) GetPositions(_ context.Context) ([]execution.Position, error) {
	return f.positions, f.positionErr
}

func (f *fakeBracketExec) GetAccount(_ context.Context) (execution.AccountInfo, error) {
	return execution.AccountInfo{}, nil
}

func newBracketTestEngine(exec execution.Executor) *Engine {
	return &Engine{
		cfg:         &config.Config{},
		executor:    exec,
		portfolio:   portfolio.NewTracker(time.Hour),
		bus:         events.New(),
		costTracker: cost.NewTracker(),
	}
}

// insertBracketFixture creates an open trade with two OCO legs and returns
// (tradeID, tpLegBrokerID, slLegBrokerID).
func insertBracketFixture(t *testing.T, ticker string, qty float64) (string, string, string) {
	t.Helper()
	ctx := context.Background()
	tradeID, err := store.InsertTrade(ctx, store.Trade{
		Ticker: ticker, Direction: "long", Strategy: "momentum",
		EntryPrice: 100, Quantity: qty, StopLoss: 95, TakeProfit: 110,
		Status: "open",
	})
	if err != nil {
		t.Fatalf("InsertTrade: %v", err)
	}
	tpID := "tp-" + tradeID[:8]
	slID := "sl-" + tradeID[:8]
	for _, leg := range []store.Order{
		{TradeID: tradeID, BrokerOrderID: tpID, Ticker: ticker, Side: "sell",
			OrderType: "limit", Quantity: qty, LimitPrice: 110, Status: "pending",
			IsTakeProfit: true, ParentOrderID: "parent-" + tradeID[:8]},
		{TradeID: tradeID, BrokerOrderID: slID, Ticker: ticker, Side: "sell",
			OrderType: "stop", Quantity: qty, StopPrice: 95, Status: "pending",
			IsStopLoss: true, ParentOrderID: "parent-" + tradeID[:8]},
	} {
		if _, err := store.InsertOrder(ctx, leg); err != nil {
			t.Fatalf("InsertOrder: %v", err)
		}
	}
	return tradeID, tpID, slID
}

func getTrade(t *testing.T, id string) store.Trade {
	t.Helper()
	trades, err := store.GetOpenTrades(context.Background())
	if err != nil {
		t.Fatalf("GetOpenTrades: %v", err)
	}
	for _, tr := range trades {
		if tr.ID == id {
			return tr
		}
	}
	// Not open — fetch as closed via orders join is overkill; signal absence.
	return store.Trade{}
}

// TestReconcileBracket_StandaloneStopIsNotBrokerManaged pins the Codex P2:
// a trade whose only working exit order is a STANDALONE trailing stop
// (ParentOrderID empty) must stay app-managed, or software take-profit
// checks would silently stop for every trailing trade.
func TestReconcileBracket_StandaloneStopIsNotBrokerManaged(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("DATABASE_URL not set; skipping DB-backed engine test")
	}
	ctx := context.Background()
	tradeID, err := store.InsertTrade(ctx, store.Trade{
		Ticker: "STDA", Direction: "long", Strategy: "momentum",
		EntryPrice: 100, Quantity: 5, StopLoss: 95, TakeProfit: 110, Status: "open",
	})
	if err != nil {
		t.Fatalf("InsertTrade: %v", err)
	}
	if _, err := store.InsertOrder(ctx, store.Order{
		TradeID: tradeID, BrokerOrderID: "trail-1", Ticker: "STDA", Side: "sell",
		OrderType: "stop", Quantity: 5, StopPrice: 97, Status: "pending",
		IsStopLoss: true, // ParentOrderID deliberately empty: trailing manager's stop
	}); err != nil {
		t.Fatalf("InsertOrder: %v", err)
	}

	e := newBracketTestEngine(&fakeBracketExec{statuses: map[string]execution.OrderResult{
		"trail-1": {Status: "accepted"},
	}})
	trade := getTrade(t, tradeID)
	if handled := e.reconcileBracketTrade(ctx, trade); handled {
		t.Fatal("standalone trailing stop must not mark the trade broker-managed")
	}
	if handled := e.reconcileBrokerExitTrade(ctx, trade, true); handled {
		t.Fatal("polling a capped standalone stop must preserve software take-profit ownership")
	}
}

func TestWorkingBrokerStopCoversTradeRequiresAnActiveFullStop(t *testing.T) {
	tests := []struct {
		name   string
		states []legState
		want   bool
	}{
		{
			name: "held child is inactive",
			states: []legState{{ok: true, leg: store.Order{IsStopLoss: true, Quantity: 5}, res: execution.OrderResult{
				Status: "held", Quantity: 5, RemainingQty: 5,
			}}},
		},
		{
			name: "take profit alone is insufficient",
			states: []legState{{ok: true, leg: store.Order{IsTakeProfit: true, Quantity: 5}, res: execution.OrderResult{
				Status: "accepted", Quantity: 5, RemainingQty: 5,
			}}},
		},
		{
			name: "undersized active stop",
			states: []legState{{ok: true, leg: store.Order{IsStopLoss: true, Quantity: 5}, res: execution.OrderResult{
				Status: "accepted", Quantity: 4, RemainingQty: 4,
			}}},
		},
		{
			name: "full active stop",
			states: []legState{{ok: true, leg: store.Order{IsStopLoss: true, Quantity: 5}, res: execution.OrderResult{
				Status: "accepted", Quantity: 5, RemainingQty: 5,
			}}},
			want: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := workingBrokerStopCoversTrade(tc.states, 5); got != tc.want {
				t.Fatalf("covered = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestBrokerStopSnapshotUnknown(t *testing.T) {
	if !brokerStopSnapshotUnknown([]legState{{leg: store.Order{IsStopLoss: true}}}) {
		t.Fatal("unreadable stop snapshot must remain unknown")
	}
	if brokerStopSnapshotUnknown([]legState{{ok: true, leg: store.Order{IsStopLoss: true}, res: execution.OrderResult{Status: "accepted"}}}) {
		t.Fatal("readable stop snapshot must not remain unknown")
	}
}

func TestCancelBrokerBracketChildren_RetainsParentReportedFillWhenLookupMisses(t *testing.T) {
	exec := &fakeBracketExec{statusErr: map[string]error{
		"filled-child": execution.ErrOrderNotFound,
	}}
	e := newBracketTestEngine(exec)
	if e.cancelBrokerBracketChildren(context.Background(), "", []execution.OrderLeg{{
		BrokerOrderID: "filled-child", Status: "filled", FilledQty: 1,
	}}) {
		t.Fatal("parent-reported child fill must block containment until reconciliation")
	}
	if len(exec.cancelled) != 0 {
		t.Fatalf("cancellations = %v, want none after parent reports a child fill", exec.cancelled)
	}
}

func TestVerifyBrokerEntry_RetriesUnknownTradeProjectionWithoutContainment(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("DATABASE_URL not set; skipping DB-backed engine test")
	}
	checks := make(chan struct{}, 3)
	exec := &fakeBracketExec{
		statuses: map[string]execution.OrderResult{
			"healthy-parent": {
				Status: "filled", Quantity: 2, FilledQty: 2,
				ChildOrders: []execution.OrderLeg{{Type: "stop", Status: "accepted", Quantity: 2, RemainingQty: 2}},
			},
		},
		onStatus: func(string) { checks <- struct{}{} },
	}
	e := newBracketTestEngine(exec)
	e.verifyOrContainBrokerEntryAsync("missing-trade", "healthy-parent", "TEST", 2)
	for i := 0; i < 3; i++ {
		select {
		case <-checks:
		case <-time.After(time.Second):
			t.Fatalf("verification attempt %d did not run", i+1)
		}
	}
	if len(exec.cancelled) != 0 || len(exec.placed) != 0 {
		t.Fatalf("unknown trade projection must retry without containment; cancellations=%v placements=%v", exec.cancelled, exec.placed)
	}
}

func TestContainRecoveredUnprotectedBracket_RetriesUnknownParentSnapshot(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("DATABASE_URL not set; skipping DB-backed engine test")
	}
	ctx := context.Background()
	tradeID, err := store.InsertTrade(ctx, store.Trade{
		Ticker: "STRICT_UNKNOWN_PARENT", Direction: "long", Strategy: "test",
		EntryPrice: 100, Quantity: 2, StopLoss: 95, TakeProfit: 110, Status: "open",
	})
	if err != nil {
		t.Fatalf("InsertTrade: %v", err)
	}
	var intentID string
	if err := store.GetDB().QueryRow(ctx, `SELECT uuid_generate_v4()::text`).Scan(&intentID); err != nil {
		t.Fatalf("create intent UUID: %v", err)
	}
	intentKey := "entry:strict-unknown-parent-" + intentID
	clientOrderID := "strict-unknown-parent-client-" + intentID
	request, err := json.Marshal(execution.OrderRequest{
		Ticker: "STRICT_UNKNOWN_PARENT", Side: execution.OrderSideBuy, Type: execution.OrderTypeMarket,
		Quantity: 2, IntentKey: intentKey, ClientOrderID: clientOrderID,
		TimeInForce: "gtc", RequireBrokerProtection: true,
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	intent := store.StockOrderIntent{
		ID: intentID, IdempotencyKey: intentKey, ClientOrderID: clientOrderID, TradeID: tradeID,
		Ticker: "STRICT_UNKNOWN_PARENT", Side: "buy", OrderType: "market", RequestedQty: 2,
		RequestPayload: request, PayloadHash: "strict-unknown-parent", Status: "pending",
	}
	if err := store.CreateStockOrderIntent(ctx, intent); err != nil {
		t.Fatalf("CreateStockOrderIntent: %v", err)
	}
	t.Cleanup(func() {
		if _, err := store.GetDB().Exec(ctx, `DELETE FROM stock_order_intents WHERE id=$1::uuid`, intent.ID); err != nil {
			t.Errorf("cleanup strict intent: %v", err)
		}
	})
	if err := store.UpdateStockOrderIntentBroker(ctx, intent.ClientOrderID, "strict-unknown-parent", "filled", 100, 2, "", "", true); err != nil {
		t.Fatalf("record strict intent: %v", err)
	}
	exec := &fakeBracketExec{statusErr: map[string]error{
		"strict-unknown-parent": fmt.Errorf("transient parent status failure"),
	}}
	e := newBracketTestEngine(exec)
	e.cfg.Risk.BracketExitsEnabled = true
	e.containRecoveredUnprotectedBracket(ctx, intentKey)
	if len(exec.cancelled) != 0 || len(exec.placed) != 0 {
		t.Fatalf("unknown parent snapshot must retry without containment; cancellations=%v placements=%v", exec.cancelled, exec.placed)
	}
}

func TestContainRecoveredUnprotectedBracket_LegacyIntentStaysApplicationManaged(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("DATABASE_URL not set; skipping DB-backed engine test")
	}
	ctx := context.Background()
	tradeID, err := store.InsertTrade(ctx, store.Trade{
		Ticker: "LEGACY_RECOVERY", Direction: "long", Strategy: "test",
		EntryPrice: 100, Quantity: 2, StopLoss: 95, TakeProfit: 110, Status: "open",
	})
	if err != nil {
		t.Fatalf("InsertTrade: %v", err)
	}
	var intentID string
	if err := store.GetDB().QueryRow(ctx, `SELECT uuid_generate_v4()::text`).Scan(&intentID); err != nil {
		t.Fatalf("create intent UUID: %v", err)
	}
	intentKey := "entry:legacy-recovery-" + intentID
	clientOrderID := "legacy-recovery-client-" + intentID
	request, err := json.Marshal(execution.OrderRequest{
		Ticker: "LEGACY_RECOVERY", Side: execution.OrderSideBuy, Type: execution.OrderTypeMarket,
		Quantity: 2, IntentKey: intentKey, ClientOrderID: clientOrderID, TimeInForce: "gtc",
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	intent := store.StockOrderIntent{
		ID: intentID, IdempotencyKey: intentKey, ClientOrderID: clientOrderID, TradeID: tradeID,
		Ticker: "LEGACY_RECOVERY", Side: "buy", OrderType: "market", RequestedQty: 2,
		RequestPayload: request, PayloadHash: "legacy-recovery", Status: "pending",
	}
	if err := store.CreateStockOrderIntent(ctx, intent); err != nil {
		t.Fatalf("CreateStockOrderIntent: %v", err)
	}
	t.Cleanup(func() {
		if _, err := store.GetDB().Exec(ctx, `DELETE FROM stock_order_intents WHERE id=$1::uuid`, intent.ID); err != nil {
			t.Errorf("cleanup legacy intent: %v", err)
		}
	})
	if err := store.UpdateStockOrderIntentBroker(ctx, intent.ClientOrderID, "legacy-recovery-parent", "filled", 100, 2, "", "", true); err != nil {
		t.Fatalf("record legacy intent: %v", err)
	}
	exec := &fakeBracketExec{statuses: map[string]execution.OrderResult{
		"legacy-recovery-parent": {Status: "filled", Quantity: 2, FilledQty: 2},
	}}
	e := newBracketTestEngine(exec)
	e.cfg.Risk.BracketExitsEnabled = true
	e.containRecoveredUnprotectedBracket(ctx, intentKey)
	if len(exec.cancelled) != 0 || len(exec.placed) != 0 {
		t.Fatalf("legacy recovery must remain application-managed; cancellations=%v placements=%v", exec.cancelled, exec.placed)
	}
}

func TestReconcileBracket_LegacyTradeWithoutIntentRemainsApplicationManaged(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("DATABASE_URL not set; skipping DB-backed engine test")
	}
	ctx := context.Background()
	tradeID, err := store.InsertTrade(ctx, store.Trade{
		Ticker: "LEGACY_NO_INTENT", Direction: "long", Strategy: "test",
		EntryPrice: 100, Quantity: 2, StopLoss: 95, TakeProfit: 110, Status: "open",
	})
	if err != nil {
		t.Fatalf("InsertTrade: %v", err)
	}
	e := newBracketTestEngine(&fakeBracketExec{})
	e.cfg.Risk.BracketExitsEnabled = true
	if handled := e.reconcileBracketTrade(ctx, getTrade(t, tradeID)); handled {
		t.Fatal("legacy trade without a broker-protection intent must retain application-managed exits")
	}
}

func TestReconcileBracket_PolicyReadFailureRetainsBrokerOwnershipAfterAdmissionDisabled(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("DATABASE_URL not set; skipping DB-backed engine test")
	}
	ctx := context.Background()
	tradeID, err := store.InsertTrade(ctx, store.Trade{
		Ticker: "STRICT_POLICY_READ_FAILURE", Direction: "long", Strategy: "test",
		EntryPrice: 100, Quantity: 2, StopLoss: 95, TakeProfit: 110, Status: "open",
	})
	if err != nil {
		t.Fatalf("InsertTrade: %v", err)
	}

	original := stockEntryBrokerProtection
	stockEntryBrokerProtection = func(context.Context, string) (bool, string, error) {
		return false, "", fmt.Errorf("transient policy read failure")
	}
	t.Cleanup(func() { stockEntryBrokerProtection = original })

	e := newBracketTestEngine(&fakeBracketExec{})
	// This flag only governs admission of new bracket entries. An existing
	// strict trade cannot safely fall back to software exits while its durable
	// ownership policy is temporarily unreadable.
	e.cfg.Risk.BracketExitsEnabled = false
	if handled, reconcileErr := e.reconcileBrokerExitTradeStatus(ctx, getTrade(t, tradeID), false); !handled || reconcileErr == nil {
		t.Fatalf("policy read failure after admission disabled = handled %v err %v; want broker ownership retained", handled, reconcileErr)
	}
}

func TestReconcileBracket_MissingProjectionVerifiesHealthyStrictParent(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("DATABASE_URL not set; skipping DB-backed engine test")
	}
	ctx := context.Background()
	tradeID, err := store.InsertTrade(ctx, store.Trade{
		Ticker: "STRICT_HEALTHY_PARENT", Direction: "long", Strategy: "test",
		EntryPrice: 100, Quantity: 2, StopLoss: 95, TakeProfit: 110, Status: "open",
	})
	if err != nil {
		t.Fatal(err)
	}
	original := stockEntryBrokerProtection
	stockEntryBrokerProtection = func(context.Context, string) (bool, string, error) {
		return true, "healthy-missing-projection-parent", nil
	}
	t.Cleanup(func() { stockEntryBrokerProtection = original })
	checked := make(chan struct{}, 1)
	exec := &fakeBracketExec{statuses: map[string]execution.OrderResult{
		"healthy-missing-projection-parent": {
			Status: "filled", Quantity: 2, FilledQty: 2,
			ChildOrders: []execution.OrderLeg{{Type: "stop", Status: "accepted", Quantity: 2, RemainingQty: 2}},
		},
	}, onStatus: func(string) { checked <- struct{}{} }}
	e := newBracketTestEngine(exec)
	if handled := e.reconcileBracketTrade(ctx, getTrade(t, tradeID)); !handled {
		t.Fatal("strict trade with missing local legs must retain broker ownership")
	}
	select {
	case <-checked:
	case <-time.After(time.Second):
		t.Fatal("missing projection did not verify the broker parent")
	}
	if len(exec.cancelled) != 0 || len(exec.placed) != 0 {
		t.Fatalf("healthy broker bracket was contained: cancellations=%v placements=%v", exec.cancelled, exec.placed)
	}
}

func TestCancelLegacyBrokerHeldEntry_GTCDoneForDayRemainsWorking(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("DATABASE_URL not set; skipping DB-backed engine test")
	}
	ctx := context.Background()
	var intentID string
	if err := store.GetDB().QueryRow(ctx, `SELECT uuid_generate_v4()::text`).Scan(&intentID); err != nil {
		t.Fatalf("create intent UUID: %v", err)
	}
	intentKey := "entry:gtc-legacy-" + intentID
	clientOrderID := "gtc-legacy-client-" + intentID
	request, err := json.Marshal(execution.OrderRequest{
		Ticker: "GTCLEGACY", Side: execution.OrderSideBuy, Type: execution.OrderTypeMarket,
		Quantity: 2, IntentKey: intentKey, ClientOrderID: clientOrderID, TimeInForce: "gtc",
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	intent := store.StockOrderIntent{
		ID: intentID, IdempotencyKey: intentKey, ClientOrderID: clientOrderID, Ticker: "GTCLEGACY",
		Side: "buy", OrderType: "market", RequestedQty: 2,
		RequestPayload: request, PayloadHash: "gtc-legacy", BrokerOrderID: "gtc-legacy-parent", Status: "pending",
	}
	if err := store.CreateStockOrderIntent(ctx, intent); err != nil {
		t.Fatalf("CreateStockOrderIntent: %v", err)
	}
	t.Cleanup(func() {
		if _, err := store.GetDB().Exec(ctx, `DELETE FROM stock_order_intents WHERE id=$1::uuid`, intent.ID); err != nil {
			t.Errorf("cleanup legacy intent: %v", err)
		}
	})
	if err := store.UpdateStockOrderIntentBroker(ctx, intent.ClientOrderID, "gtc-legacy-parent", "pending", 0, 0, "", "", true); err != nil {
		t.Fatalf("record accepted legacy intent: %v", err)
	}
	exec := &fakeBracketExec{statuses: map[string]execution.OrderResult{
		"gtc-legacy-parent": {Status: "done_for_day"},
	}}
	e := newBracketTestEngine(exec)
	e.cancelLegacyBrokerHeldEntry(ctx, intent.IdempotencyKey)
	if got := len(exec.cancelled); got != 3 {
		t.Fatalf("cancellation attempts = %d, want 3 while GTC parent remains done_for_day", got)
	}
	updated, err := store.GetStockOrderIntentByKey(ctx, intent.IdempotencyKey)
	if err != nil || updated == nil || updated.Status != "pending" {
		t.Fatalf("GTC legacy intent = %+v err=%v, want pending", updated, err)
	}
}

func TestCancelLegacyBrokerHeldEntry_PurgedParentRemainsRecoverable(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("DATABASE_URL not set; skipping DB-backed engine test")
	}
	ctx := context.Background()
	var intentID string
	if err := store.GetDB().QueryRow(ctx, `SELECT uuid_generate_v4()::text`).Scan(&intentID); err != nil {
		t.Fatalf("create intent UUID: %v", err)
	}
	intentKey := "entry:purged-legacy-" + intentID
	clientOrderID := "purged-legacy-client-" + intentID
	request, err := json.Marshal(execution.OrderRequest{
		Ticker: "PURGEDLEGACY", Side: execution.OrderSideBuy, Type: execution.OrderTypeMarket,
		Quantity: 2, IntentKey: intentKey, ClientOrderID: clientOrderID, TimeInForce: "gtc",
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	intent := store.StockOrderIntent{
		ID: intentID, IdempotencyKey: intentKey, ClientOrderID: clientOrderID, Ticker: "PURGEDLEGACY",
		Side: "buy", OrderType: "market", RequestedQty: 2,
		RequestPayload: request, PayloadHash: "purged-legacy", Status: "pending",
	}
	if err := store.CreateStockOrderIntent(ctx, intent); err != nil {
		t.Fatalf("CreateStockOrderIntent: %v", err)
	}
	t.Cleanup(func() {
		if _, err := store.GetDB().Exec(ctx, `DELETE FROM stock_order_intents WHERE id=$1::uuid`, intent.ID); err != nil {
			t.Errorf("cleanup legacy intent: %v", err)
		}
	})
	if err := store.UpdateStockOrderIntentBroker(ctx, intent.ClientOrderID, "purged-legacy-parent", "pending", 0, 0, "", "", true); err != nil {
		t.Fatalf("record accepted legacy intent: %v", err)
	}
	exec := &fakeBracketExec{statusErr: map[string]error{
		"purged-legacy-parent": execution.ErrOrderNotFound,
	}}
	e := newBracketTestEngine(exec)
	e.cancelLegacyBrokerHeldEntry(ctx, intent.IdempotencyKey)
	if got := len(exec.cancelled); got != 3 {
		t.Fatalf("cancellation attempts = %d, want 3 while fill remains unknown", got)
	}
	updated, err := store.GetStockOrderIntentByKey(ctx, intent.IdempotencyKey)
	if err != nil || updated == nil || updated.Status != "pending" {
		t.Fatalf("purged legacy intent = %+v err=%v, want pending for recovery", updated, err)
	}
}

func TestCancelLegacyBrokerHeldEntry_PersistsContainmentAfterRecoveryFailure(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("DATABASE_URL not set; skipping DB-backed engine test")
	}
	ctx := context.Background()
	tradeID, err := store.InsertTrade(ctx, store.Trade{
		Ticker: "LEGACY_FORCE", Direction: "long", Strategy: "test", EntryPrice: 100, Quantity: 2, Status: "open",
	})
	if err != nil {
		t.Fatal(err)
	}
	var intentID string
	if err := store.GetDB().QueryRow(ctx, `SELECT uuid_generate_v4()::text`).Scan(&intentID); err != nil {
		t.Fatal(err)
	}
	intentKey := "entry:legacy-force-" + intentID
	request, err := json.Marshal(execution.OrderRequest{
		Ticker: "LEGACY_FORCE", Side: execution.OrderSideBuy, Type: execution.OrderTypeMarket,
		Quantity: 2, IntentKey: intentKey, ClientOrderID: "legacy-force-" + intentID, TimeInForce: "gtc",
	})
	if err != nil {
		t.Fatal(err)
	}
	intent := store.StockOrderIntent{
		ID: intentID, IdempotencyKey: intentKey, ClientOrderID: "legacy-force-" + intentID, TradeID: tradeID,
		Ticker: "LEGACY_FORCE", Side: "buy", OrderType: "market", RequestedQty: 2,
		RequestPayload: request, PayloadHash: "legacy-force", Status: "pending",
	}
	if err := store.CreateStockOrderIntent(ctx, intent); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = store.GetDB().Exec(context.Background(), `DELETE FROM stock_order_intents WHERE parent_intent_id=$1::uuid`, intent.ID)
		_, _ = store.GetDB().Exec(context.Background(), `DELETE FROM stock_order_intents WHERE id=$1::uuid`, intent.ID)
	})
	if err := store.UpdateStockOrderIntentBroker(ctx, intent.ClientOrderID, "legacy-force-parent", "pending", 0, 0, "", "", true); err != nil {
		t.Fatal(err)
	}
	exec := &recoverFailureBracketExec{fakeBracketExec: &fakeBracketExec{statuses: map[string]execution.OrderResult{
		"legacy-force-parent": {Status: "filled", Quantity: 2, FilledQty: 2, FilledPrice: 100},
	}}}
	e := newBracketTestEngine(exec)
	e.cancelLegacyBrokerHeldEntry(ctx, intentKey)
	required, brokerID, err := store.StockEntryBrokerProtection(ctx, tradeID)
	if err != nil || !required || brokerID != "legacy-force-parent" {
		t.Fatalf("durable legacy containment policy = required:%v broker:%q err:%v", required, brokerID, err)
	}
}

func TestApplyBracketChildSnapshotPersistsFill(t *testing.T) {
	filledAt := time.Date(2026, time.August, 13, 11, 0, 0, 0, time.UTC)
	order := store.Order{BrokerOrderID: "filled-take-profit", Status: "pending"}
	applyBracketChildSnapshot(&order, []execution.OrderLeg{{
		BrokerOrderID: "filled-take-profit", Status: "filled", FilledQty: 1, FilledPrice: 120, FilledAt: filledAt,
	}})
	if order.FilledQty != 1 || order.FilledPrice != 120 || order.Status != "filled" || order.FilledAt == nil || !order.FilledAt.Equal(filledAt) {
		t.Fatalf("persisted child snapshot = %+v", order)
	}
}

func TestContainUnprotectedBrokerEntry_PersistsParentOnlyFillForProjectedChild(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("DATABASE_URL not set; skipping DB-backed engine test")
	}
	ctx := context.Background()
	tradeID, err := store.InsertTrade(ctx, store.Trade{
		Ticker: "PARENT_ONLY_CHILD_FILL", Direction: "long", Strategy: "test", EntryPrice: 100, Quantity: 5, Status: "open",
	})
	if err != nil {
		t.Fatal(err)
	}
	parentID := "parent-only-fill-" + tradeID[:8]
	if _, err := store.InsertOrder(ctx, store.Order{
		TradeID: tradeID, BrokerOrderID: parentID, Ticker: "PARENT_ONLY_CHILD_FILL", Side: "buy", OrderType: "market",
		Quantity: 5, FilledPrice: 100, FilledQty: 5, Status: "filled",
	}); err != nil {
		t.Fatal(err)
	}
	_, err = store.InsertOrder(ctx, store.Order{
		TradeID: tradeID, BrokerOrderID: "parent-only-tp", Ticker: "PARENT_ONLY_CHILD_FILL", Side: "sell", OrderType: "limit",
		Quantity: 5, LimitPrice: 120, Status: "pending", IsTakeProfit: true, ParentOrderID: parentID,
	})
	if err != nil {
		t.Fatal(err)
	}
	exec := &fakeBracketExec{statuses: map[string]execution.OrderResult{
		parentID: {Status: "filled", FilledPrice: 100, FilledQty: 5, ChildOrders: []execution.OrderLeg{
			{BrokerOrderID: "parent-only-tp", Type: "limit", Status: "filled", FilledQty: 2, FilledPrice: 120},
		}},
	}}
	e := newBracketTestEngine(exec)
	e.containUnprotectedBrokerEntry(ctx, tradeID, parentID, "PARENT_ONLY_CHILD_FILL", "missing stop")
	trade, err := store.GetTradeByID(ctx, tradeID)
	if err != nil || trade == nil || trade.Quantity != 3 {
		t.Fatalf("parent-only fill trade = %#v err=%v, want remaining quantity 3", trade, err)
	}
	child, err := store.GetOrderByBrokerOrderID(ctx, "parent-only-tp")
	if err != nil || child == nil || child.FilledQty != 2 || child.FilledPrice != 120 || child.Status != "filled" {
		t.Fatalf("parent-only fill order = %#v err=%v", child, err)
	}
}

func TestReconcileBracket_UnknownStrictStopSnapshotRetriesWithoutContainment(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("DATABASE_URL not set; skipping DB-backed engine test")
	}
	ctx := context.Background()
	tradeID, tpID, slID := insertBracketFixture(t, "STRICT_UNKNOWN_STOP", 2)
	var intentID string
	if err := store.GetDB().QueryRow(ctx, `SELECT uuid_generate_v4()::text`).Scan(&intentID); err != nil {
		t.Fatalf("create intent UUID: %v", err)
	}
	intentKey := "entry:strict-unknown-" + intentID
	clientOrderID := "strict-unknown-client-" + intentID
	request, err := json.Marshal(execution.OrderRequest{
		Ticker: "STRICT_UNKNOWN_STOP", Side: execution.OrderSideBuy, Type: execution.OrderTypeMarket,
		Quantity: 2, IntentKey: intentKey, ClientOrderID: clientOrderID,
		TimeInForce: "gtc", RequireBrokerProtection: true,
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	intent := store.StockOrderIntent{
		ID: intentID, IdempotencyKey: intentKey, ClientOrderID: clientOrderID, TradeID: tradeID,
		Ticker: "STRICT_UNKNOWN_STOP", Side: "buy", OrderType: "market", RequestedQty: 2,
		RequestPayload: request, PayloadHash: "strict-unknown", Status: "pending",
	}
	if err := store.CreateStockOrderIntent(ctx, intent); err != nil {
		t.Fatalf("CreateStockOrderIntent: %v", err)
	}
	t.Cleanup(func() {
		if _, err := store.GetDB().Exec(ctx, `DELETE FROM stock_order_intents WHERE id=$1::uuid`, intent.ID); err != nil {
			t.Errorf("cleanup strict intent: %v", err)
		}
	})
	if err := store.UpdateStockOrderIntentBroker(ctx, intent.ClientOrderID, "strict-unknown-parent", "filled", 100, 2, "", "", true); err != nil {
		t.Fatalf("record strict intent: %v", err)
	}
	exec := &fakeBracketExec{
		statuses:  map[string]execution.OrderResult{tpID: {Status: "accepted", Quantity: 2, RemainingQty: 2}},
		statusErr: map[string]error{slID: fmt.Errorf("transient stop status failure")},
	}
	e := newBracketTestEngine(exec)
	if handled := e.reconcileBracketTrade(ctx, getTrade(t, tradeID)); !handled {
		t.Fatal("unknown strict stop snapshot must retain broker ownership")
	}
	if len(exec.cancelled) != 0 {
		t.Fatalf("containment cancellations = %v, want no flatten on unknown stop snapshot", exec.cancelled)
	}
}

func TestBrokerSnapshotRemainingTradeQtyAccountsForNewPartialTakeProfit(t *testing.T) {
	states := []legState{
		{ok: true, leg: store.Order{IsTakeProfit: true, FilledQty: 1}, res: execution.OrderResult{FilledQty: 3}},
		{ok: true, leg: store.Order{IsStopLoss: true, FilledQty: 0}, res: execution.OrderResult{FilledQty: 0}},
	}
	if got := brokerSnapshotRemainingTradeQty(5, states); got != 3 {
		t.Fatalf("remaining quantity = %v, want 3 after cumulative TP fill", got)
	}
}

func TestContainUnprotectedBrokerEntryCancelsParentBeforeDurableFlatten(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("DATABASE_URL not set; skipping DB-backed engine test")
	}
	ctx := context.Background()
	tradeID, err := store.InsertTrade(ctx, store.Trade{
		Ticker: "TEST_UNPROTECTED_BRACKET", Direction: "long", Strategy: "test",
		EntryPrice: 100, Quantity: 2, StopLoss: 95, TakeProfit: 110, Status: "open",
	})
	if err != nil {
		t.Fatalf("InsertTrade: %v", err)
	}
	parentID := "unprotected-parent-" + tradeID[:8]
	if _, err := store.InsertOrder(ctx, store.Order{
		TradeID: tradeID, BrokerOrderID: parentID, Ticker: "TEST_UNPROTECTED_BRACKET",
		Side: "buy", OrderType: "market", Quantity: 2, Status: "filled",
	}); err != nil {
		t.Fatalf("InsertOrder(entry): %v", err)
	}
	exec := &fakeBracketExec{
		statuses: map[string]execution.OrderResult{
			parentID: {Status: "filled", ChildOrders: []execution.OrderLeg{
				{BrokerOrderID: "unprojected-tp", Type: "limit", Status: "accepted", Quantity: 2, RemainingQty: 2},
				{BrokerOrderID: "unprojected-sl", Type: "stop", Status: "accepted", Quantity: 2, RemainingQty: 2},
			}},
			"unprojected-tp": {Status: "cancelled"},
			"unprojected-sl": {Status: "cancelled"},
		},
		placeRes: execution.OrderResult{BrokerOrderID: "containment-exit", Status: "accepted"},
	}
	e := newBracketTestEngine(exec)
	e.containUnprotectedBrokerEntry(ctx, tradeID, parentID, "TEST_UNPROTECTED_BRACKET", "missing stop")
	if got, want := exec.cancelled, []string{parentID, "unprojected-tp", "unprojected-sl"}; len(got) != len(want) || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("cancelled = %v, want parent and every unprojected child before flatten", got)
	}
	if len(exec.placed) != 1 || exec.placed[0].Side != execution.OrderSideSell || exec.placed[0].Quantity != 2 {
		t.Fatalf("containment flatten = %+v", exec.placed)
	}
	trade, err := store.GetTradeByID(ctx, tradeID)
	if err != nil || trade == nil || trade.Status != "exiting" {
		t.Fatalf("contained trade = %+v err=%v; want exiting", trade, err)
	}
}

func TestStartupReconcileAppliesWorkingCappedStopPartialAndFinalFill(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("DATABASE_URL not set; skipping DB-backed engine test")
	}
	ctx := context.Background()
	tradeID, err := store.InsertTrade(ctx, store.Trade{
		Ticker: "TEST_CAPPED_LIVE_PARTIAL", Direction: "long", Strategy: "test",
		EntryPrice: 100, Quantity: 5, StopLoss: 95, TakeProfit: 110, Status: "open",
	})
	if err != nil {
		t.Fatalf("InsertTrade: %v", err)
	}
	const brokerID = "capped-live-partial"
	if _, err := store.InsertOrder(ctx, store.Order{
		TradeID: tradeID, BrokerOrderID: brokerID, Ticker: "TEST_CAPPED_LIVE_PARTIAL",
		Side: "sell", OrderType: "stop", Quantity: 5, StopPrice: 95,
		Status: "pending", IsStopLoss: true,
	}); err != nil {
		t.Fatalf("InsertOrder: %v", err)
	}
	partialAt := time.Date(2026, 8, 11, 14, 0, 0, 0, time.UTC)
	exec := &fakeBracketExec{statuses: map[string]execution.OrderResult{
		brokerID: {Status: "partial", FilledPrice: 96, FilledQty: 2, FilledAt: partialAt},
	}}
	e := newBracketTestEngine(exec)
	if handled := e.reconcileBrokerExitTrade(ctx, getTrade(t, tradeID), true); handled {
		t.Fatal("working capped stop must preserve software take-profit ownership")
	}
	var quantity, realizedQty, realizedProceeds float64
	var status string
	if err := store.GetDB().QueryRow(ctx, `
		SELECT quantity, realized_exit_qty, realized_exit_proceeds, status
		FROM trades WHERE id=$1`, tradeID,
	).Scan(&quantity, &realizedQty, &realizedProceeds, &status); err != nil {
		t.Fatalf("load partially exited trade: %v", err)
	}
	if quantity != 3 || realizedQty != 2 || realizedProceeds != 192 || status != "open" {
		t.Fatalf("partial trade = qty %.2f realized %.2f proceeds %.2f status %s; want 3, 2, 192, open",
			quantity, realizedQty, realizedProceeds, status)
	}

	exec.statuses[brokerID] = execution.OrderResult{
		Status: "filled", FilledPrice: 95, FilledQty: 5,
		FilledAt: partialAt.Add(time.Minute),
	}
	if handled := e.reconcileBrokerExitTrade(ctx, getTrade(t, tradeID), true); !handled {
		t.Fatal("terminal capped stop fill must close the trade")
	}
	closed, err := store.GetTradeByID(ctx, tradeID)
	if err != nil || closed == nil {
		t.Fatalf("GetTradeByID: trade=%+v err=%v", closed, err)
	}
	if closed.Status != "closed" || closed.Quantity != 5 || closed.ExitPrice != 95 || closed.PnL != -25 {
		t.Fatalf("closed trade = %+v; want qty 5, exit 95, pnl -25", closed)
	}
}

func TestStartupReconcileDoesNotOverwriteConcurrentStopReplacement(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("DATABASE_URL not set; skipping DB-backed engine test")
	}
	ctx := context.Background()
	tradeID, err := store.InsertTrade(ctx, store.Trade{
		Ticker: "TEST_CAPPED_REPLACE_RACE", Direction: "long", Strategy: "test",
		EntryPrice: 100, Quantity: 5, StopLoss: 95, TakeProfit: 110, Status: "open",
	})
	if err != nil {
		t.Fatalf("InsertTrade: %v", err)
	}
	const oldBrokerID = "capped-stop-old"
	orderID, err := store.InsertOrder(ctx, store.Order{
		TradeID: tradeID, BrokerOrderID: oldBrokerID, Ticker: "TEST_CAPPED_REPLACE_RACE",
		Side: "sell", OrderType: "stop", Quantity: 5, StopPrice: 95,
		Status: "pending", IsStopLoss: true,
	})
	if err != nil {
		t.Fatalf("InsertOrder: %v", err)
	}
	const replacementID = "capped-stop-replacement"
	replaced := false
	exec := &fakeBracketExec{
		statusErr: map[string]error{oldBrokerID: execution.ErrOrderNotFound},
		positions: []execution.Position{{Ticker: "TEST_CAPPED_REPLACE_RACE", Quantity: 5, Side: "long"}},
		onStatus: func(id string) {
			if id != oldBrokerID || replaced {
				return
			}
			replaced = true
			if updateErr := store.UpdateOrderStopReplacement(
				ctx, orderID, oldBrokerID, replacementID, 0, 5, 96, 5, "pending", 0, 0, time.Time{}, store.ExitIntent{},
			); updateErr != nil {
				t.Fatalf("replace stop during broker poll: %v", updateErr)
			}
		},
	}
	e := newBracketTestEngine(exec)
	if handled := e.reconcileBrokerExitTrade(ctx, getTrade(t, tradeID), true); handled {
		t.Fatal("working capped replacement must preserve software take-profit ownership")
	}
	orders, err := store.GetOrdersByTrade(ctx, tradeID)
	if err != nil || len(orders) != 1 {
		t.Fatalf("GetOrdersByTrade: orders=%+v err=%v", orders, err)
	}
	if orders[0].BrokerOrderID != replacementID || orders[0].Status != "pending" {
		t.Fatalf("replacement row = %+v; want broker id %s pending", orders[0], replacementID)
	}
}

func TestMonitorKeepsBrokerOwnershipWhenUncappedStopWasReplaced(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("DATABASE_URL not set; skipping DB-backed engine test")
	}
	ctx := context.Background()
	tradeID, err := store.InsertTrade(ctx, store.Trade{
		Ticker: "TEST_UNCAPPED_REPLACE_RACE", Direction: "long", Strategy: "test",
		EntryPrice: 100, Quantity: 5, StopLoss: 95, TakeProfit: 0, Status: "open",
	})
	if err != nil {
		t.Fatalf("InsertTrade: %v", err)
	}
	const oldBrokerID = "uncapped-stop-old"
	orderID, err := store.InsertOrder(ctx, store.Order{
		TradeID: tradeID, BrokerOrderID: oldBrokerID, Ticker: "TEST_UNCAPPED_REPLACE_RACE",
		Side: "sell", OrderType: "stop", Quantity: 5, StopPrice: 95,
		Status: "pending", IsStopLoss: true,
	})
	if err != nil {
		t.Fatalf("InsertOrder: %v", err)
	}
	exec := &fakeBracketExec{
		statusErr: map[string]error{oldBrokerID: execution.ErrOrderNotFound},
		onStatus: func(id string) {
			if id != oldBrokerID {
				return
			}
			if updateErr := store.UpdateOrderStopReplacement(
				ctx, orderID, oldBrokerID, "uncapped-stop-replacement", 0, 5, 96, 5, "pending", 0, 0, time.Time{},
				store.ExitIntent{Reason: store.ExitReasonStopLoss},
			); updateErr != nil {
				t.Fatalf("replace stop during broker poll: %v", updateErr)
			}
		},
	}
	e := newBracketTestEngine(exec)
	if handled := e.reconcileBracketTrade(ctx, getTrade(t, tradeID)); !handled {
		t.Fatal("uncapped replacement stop must retain broker exit ownership")
	}
}

func TestMonitorKeepsBrokerOwnershipWhenTerminalPollRacesReplacement(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("DATABASE_URL not set; skipping DB-backed engine test")
	}
	ctx := context.Background()
	tradeID, err := store.InsertTrade(ctx, store.Trade{
		Ticker: "TEST_TERMINAL_REPLACE_RACE", Direction: "long", Strategy: "test",
		EntryPrice: 100, Quantity: 5, StopLoss: 95, TakeProfit: 0, Status: "open",
	})
	if err != nil {
		t.Fatalf("InsertTrade: %v", err)
	}
	const oldBrokerID = "terminal-stop-old"
	orderID, err := store.InsertOrder(ctx, store.Order{
		TradeID: tradeID, BrokerOrderID: oldBrokerID, Ticker: "TEST_TERMINAL_REPLACE_RACE",
		Side: "sell", OrderType: "stop", Quantity: 5, StopPrice: 95,
		Status: "pending", IsStopLoss: true,
	})
	if err != nil {
		t.Fatalf("InsertOrder: %v", err)
	}
	exec := &fakeBracketExec{
		statuses: map[string]execution.OrderResult{oldBrokerID: {Status: "cancelled"}},
		onStatus: func(id string) {
			if id != oldBrokerID {
				return
			}
			if updateErr := store.UpdateOrderStopReplacement(
				ctx, orderID, oldBrokerID, "terminal-stop-replacement", 0, 5, 96, 5, "pending", 0, 0, time.Time{},
				store.ExitIntent{Reason: store.ExitReasonStopLoss},
			); updateErr != nil {
				t.Fatalf("replace stop during terminal broker poll: %v", updateErr)
			}
		},
	}
	e := newBracketTestEngine(exec)
	if handled := e.reconcileBracketTrade(ctx, getTrade(t, tradeID)); !handled {
		t.Fatal("replacement stop must retain broker exit ownership after stale terminal poll")
	}
}

func TestStartupReconcilePreservesPurgedProtectivePartialFill(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("DATABASE_URL not set; skipping DB-backed engine test")
	}
	ctx := context.Background()
	tradeID, err := store.InsertTrade(ctx, store.Trade{
		Ticker: "TEST_PURGED_STOP_PARTIAL", Direction: "long", Strategy: "test",
		EntryPrice: 100, Quantity: 5, StopLoss: 95, TakeProfit: 110, Status: "open",
	})
	if err != nil {
		t.Fatalf("InsertTrade: %v", err)
	}
	const brokerID = "purged-stop-partial"
	if _, err := store.InsertOrder(ctx, store.Order{
		TradeID: tradeID, BrokerOrderID: brokerID, Ticker: "TEST_PURGED_STOP_PARTIAL",
		Side: "sell", OrderType: "stop", Quantity: 5, StopPrice: 95,
		FilledPrice: 94, FilledQty: 2, Status: "partial", IsStopLoss: true,
	}); err != nil {
		t.Fatalf("InsertOrder: %v", err)
	}
	e := newBracketTestEngine(&fakeBracketExec{statusErr: map[string]error{brokerID: execution.ErrOrderNotFound}})
	e.reconcileBrokerExitTrade(ctx, getTrade(t, tradeID), true)
	trade, err := store.GetTradeByID(ctx, tradeID)
	if err != nil || trade == nil || trade.Quantity != 3 {
		t.Fatalf("trade after purged partial = %+v err=%v; want remaining quantity 3", trade, err)
	}
	orders, err := store.GetOrdersByTrade(ctx, tradeID)
	if err != nil || len(orders) != 1 || orders[0].Status != "cancelled" || orders[0].FilledQty != 2 || orders[0].FilledPrice != 94 {
		t.Fatalf("purged partial order = %+v err=%v; want durable 2 @ 94 cancelled", orders, err)
	}
}

func TestStartupReconcileTreatsEveryPurgedProtectiveLegAsUnresolvedWithoutPositionProof(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("DATABASE_URL not set; skipping DB-backed engine test")
	}
	ctx := context.Background()
	tradeID, err := store.InsertTrade(ctx, store.Trade{
		Ticker: "TEST_PURGED_PROTECTIVE", Direction: "long", Strategy: "test",
		EntryPrice: 100, Quantity: 5, StopLoss: 95, Status: "open",
	})
	if err != nil {
		t.Fatalf("InsertTrade: %v", err)
	}
	const brokerID = "purged-protective-stop"
	if _, err := store.InsertOrder(ctx, store.Order{
		TradeID: tradeID, BrokerOrderID: brokerID, Ticker: "TEST_PURGED_PROTECTIVE",
		Side: "sell", OrderType: "stop", Quantity: 5, StopPrice: 95,
		Status: "pending", IsStopLoss: true,
	}); err != nil {
		t.Fatalf("InsertOrder: %v", err)
	}
	exec := &fakeBracketExec{statusErr: map[string]error{brokerID: execution.ErrOrderNotFound}}
	e := newBracketTestEngine(exec)
	if handled, reconcileErr := e.reconcileBrokerExitTradeStatus(ctx, getTrade(t, tradeID), false); !handled || reconcileErr == nil {
		t.Fatalf("purged protective leg without position proof = handled %v err %v; want unresolved", handled, reconcileErr)
	}
	legs, err := store.OpenExitLegsForTrade(ctx, tradeID)
	if err != nil || len(legs) != 1 || legs[0].Status != "pending" {
		t.Fatalf("unresolved stop was terminalized: legs=%+v err=%v", legs, err)
	}

	exec.positions = []execution.Position{{Ticker: "TEST_PURGED_PROTECTIVE", Quantity: 5, Side: "long"}}
	if handled, reconcileErr := e.reconcileBrokerExitTradeStatus(ctx, getTrade(t, tradeID), false); handled || reconcileErr != nil {
		t.Fatalf("purged protective leg with full holding proof = handled %v err %v; want safe software handoff", handled, reconcileErr)
	}
}

func TestStartupReconcileRetiresPurgedCappedPaperStop(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("DATABASE_URL not set; skipping DB-backed engine test")
	}
	ctx := context.Background()
	tradeID, err := store.InsertTrade(ctx, store.Trade{
		Ticker: "TEST_PURGED_PAPER_STOP", Direction: "long", Strategy: "test",
		EntryPrice: 100, Quantity: 5, StopLoss: 95, TakeProfit: 110, Status: "open",
	})
	if err != nil {
		t.Fatalf("InsertTrade: %v", err)
	}
	if _, err := store.InsertOrder(ctx, store.Order{
		TradeID: tradeID, BrokerOrderID: "PAPER-000001", Ticker: "TEST_PURGED_PAPER_STOP",
		Side: "sell", OrderType: "stop", Quantity: 5, StopPrice: 95,
		Status: "pending", IsStopLoss: true,
	}); err != nil {
		t.Fatalf("InsertOrder: %v", err)
	}
	e := newBracketTestEngine(execution.NewPaperExecutor())
	if handled, reconcileErr := e.reconcileBrokerExitTradeStatus(ctx, getTrade(t, tradeID), true); handled || reconcileErr != nil {
		t.Fatalf("purged paper stop = handled %v err %v; want safe software handoff", handled, reconcileErr)
	}
}

func TestStartupReconcileDoesNotRetireBrokerStopThroughPaperFallback(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("DATABASE_URL not set; skipping DB-backed engine test")
	}
	ctx := context.Background()
	tradeID, err := store.InsertTrade(ctx, store.Trade{
		Ticker: "TEST_PAPER_BROKER_STOP", Direction: "long", Strategy: "test",
		EntryPrice: 100, Quantity: 5, StopLoss: 95, TakeProfit: 110, Status: "open",
	})
	if err != nil {
		t.Fatalf("InsertTrade: %v", err)
	}
	if _, err := store.InsertOrder(ctx, store.Order{
		TradeID: tradeID, BrokerOrderID: "alpaca-live-stop", Ticker: "TEST_PAPER_BROKER_STOP",
		Side: "sell", OrderType: "stop", Quantity: 5, StopPrice: 95,
		Status: "pending", IsStopLoss: true,
	}); err != nil {
		t.Fatalf("InsertOrder: %v", err)
	}
	e := newBracketTestEngine(execution.NewPaperExecutor())
	if handled, reconcileErr := e.reconcileBrokerExitTradeStatus(ctx, getTrade(t, tradeID), true); !handled || reconcileErr == nil {
		t.Fatalf("broker stop through paper fallback = handled %v err %v; want unresolved", handled, reconcileErr)
	}
	legs, err := store.OpenExitLegsForTrade(ctx, tradeID)
	if err != nil || len(legs) != 1 || legs[0].Status != "pending" {
		t.Fatalf("broker stop was terminalized through paper fallback: legs=%+v err=%v", legs, err)
	}
}

func TestProtectiveFillJoinsConcurrentExitClaim(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("DATABASE_URL not set; skipping DB-backed engine test")
	}
	ctx := context.Background()
	tradeID, err := store.InsertTrade(ctx, store.Trade{
		Ticker: "TEST_STOP_FILL_EXISTING_CLAIM", Direction: "long", Strategy: "test",
		EntryPrice: 100, Quantity: 5, StopLoss: 95, TakeProfit: 110, Status: "open",
	})
	if err != nil {
		t.Fatalf("InsertTrade: %v", err)
	}
	const brokerID = "stop-fill-existing-claim"
	if _, err := store.InsertOrder(ctx, store.Order{
		TradeID: tradeID, BrokerOrderID: brokerID, Ticker: "TEST_STOP_FILL_EXISTING_CLAIM",
		Side: "sell", OrderType: "stop", Quantity: 5, StopPrice: 95,
		Status: "pending", IsStopLoss: true,
	}); err != nil {
		t.Fatalf("InsertOrder: %v", err)
	}
	trade := getTrade(t, tradeID)
	if claimed, err := store.ClaimTradeExitWithIntent(ctx, tradeID, store.ExitIntent{
		Reason: store.ExitReasonTakeProfit,
	}); err != nil || !claimed {
		t.Fatalf("ClaimTradeExitWithIntent = %v, %v", claimed, err)
	}
	exec := &fakeBracketExec{statuses: map[string]execution.OrderResult{
		brokerID: {Status: "filled", FilledPrice: 94, FilledQty: 5},
	}}
	e := newBracketTestEngine(exec)

	if handled := e.reconcileBrokerExitTrade(ctx, trade, true); !handled {
		t.Fatal("protective fill under an existing exit claim must be handled")
	}
	closed, err := store.GetTradeByID(ctx, tradeID)
	if err != nil || closed == nil || closed.Status != "closed" || closed.ExitPrice != 94 {
		t.Fatalf("claimed protective fill trade = %+v err=%v; want closed at 94", closed, err)
	}
	if closed.ExitReason != store.ExitReasonStopLoss {
		t.Fatalf("exit reason = %q, want protective stop reason %q", closed.ExitReason, store.ExitReasonStopLoss)
	}
}

func TestBracketFillKeepsClaimWhenLaterSiblingWasReplaced(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("DATABASE_URL not set; skipping DB-backed engine test")
	}
	ctx := context.Background()
	tradeID, err := store.InsertTrade(ctx, store.Trade{
		Ticker: "TEST_BRACKET_PARTIAL_REPLACE", Direction: "long", Strategy: "test",
		EntryPrice: 100, Quantity: 5, StopLoss: 95, TakeProfit: 110, Status: "open",
	})
	if err != nil {
		t.Fatalf("InsertTrade: %v", err)
	}
	const parentID = "bracket-partial-replace-parent"
	partialID, err := store.InsertOrder(ctx, store.Order{
		TradeID: tradeID, BrokerOrderID: "bracket-partial-leg", ParentOrderID: parentID,
		Ticker: "TEST_BRACKET_PARTIAL_REPLACE", Side: "sell", OrderType: "limit",
		Quantity: 2, LimitPrice: 110, Status: "pending", IsTakeProfit: true,
	})
	if err != nil {
		t.Fatalf("InsertOrder(partial): %v", err)
	}
	filledID, err := store.InsertOrder(ctx, store.Order{
		TradeID: tradeID, BrokerOrderID: "bracket-filled-old", ParentOrderID: parentID,
		Ticker: "TEST_BRACKET_PARTIAL_REPLACE", Side: "sell", OrderType: "stop",
		Quantity: 3, StopPrice: 95, Status: "pending", IsStopLoss: true,
	})
	if err != nil {
		t.Fatalf("InsertOrder(filled): %v", err)
	}
	exec := &fakeBracketExec{statuses: map[string]execution.OrderResult{
		"bracket-partial-leg": {Status: "filled", FilledPrice: 109, FilledQty: 2},
		"bracket-filled-old":  {Status: "cancelled", FilledPrice: 94, FilledQty: 3},
	}}
	exec.onStatus = func(brokerOrderID string) {
		if brokerOrderID != "bracket-filled-old" {
			return
		}
		if updateErr := store.UpdateOrderStopReplacement(
			ctx, filledID, "bracket-filled-old", "bracket-filled-replacement", 0, 5, 96, 3, "pending", 0, 0, time.Time{},
			store.ExitIntent{Reason: store.ExitReasonStopLoss},
		); updateErr != nil {
			t.Fatalf("UpdateOrderStopReplacement: %v", updateErr)
		}
	}
	e := newBracketTestEngine(exec)
	if handled := e.reconcileBrokerExitTrade(ctx, getTrade(t, tradeID), true); !handled {
		t.Fatal("bracket reconciliation must retain ownership after a durable sibling fill")
	}
	trade, err := store.GetTradeByID(ctx, tradeID)
	if err != nil || trade == nil || trade.Status != "exiting" || trade.Quantity != 3 {
		t.Fatalf("trade after sibling replacement = %+v err=%v; want exiting with quantity 3", trade, err)
	}
	orders, err := store.GetOrdersByTrade(ctx, tradeID)
	if err != nil {
		t.Fatalf("GetOrdersByTrade: %v", err)
	}
	var partial store.Order
	for _, order := range orders {
		if order.ID == partialID {
			partial = order
		}
	}
	if partial.Status != "filled" || partial.FilledQty != 2 {
		t.Fatalf("durable winning leg = %+v; want filled with two filled", partial)
	}
}

func TestMonitorSoftwareTakeProfitCancelsStandaloneStopBeforeSell(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("DATABASE_URL not set; skipping DB-backed engine test")
	}
	ctx := context.Background()
	tradeID, err := store.InsertTrade(ctx, store.Trade{
		Ticker: "CAPA", Direction: "long", Strategy: "momentum",
		EntryPrice: 100, Quantity: 5, StopLoss: 95, TakeProfit: 110,
		Status: "open",
	})
	if err != nil {
		t.Fatalf("InsertTrade: %v", err)
	}
	if _, err := store.InsertOrder(ctx, store.Order{
		TradeID: tradeID, BrokerOrderID: "standalone-stop", Ticker: "CAPA",
		Side: "sell", OrderType: "stop", Quantity: 5, StopPrice: 97,
		Status: "pending", IsStopLoss: true,
	}); err != nil {
		t.Fatalf("InsertOrder: %v", err)
	}
	if err := store.UpsertCandles(ctx, []data.Candle{{
		Ticker: "CAPA", Timeframe: data.Timeframe1Hour,
		Timestamp: time.Now(), Open: 110, High: 112, Low: 109, Close: 111,
	}}); err != nil {
		t.Fatalf("UpsertCandles: %v", err)
	}

	exec := &fakeBracketExec{
		statuses: map[string]execution.OrderResult{
			"standalone-stop": {Status: "cancelled"},
		},
		placeRes: execution.OrderResult{
			BrokerOrderID: "tp-market", Status: "filled",
			FilledPrice: 111, FilledQty: 5,
		},
	}
	e := newBracketTestEngine(exec)
	e.monitorPositions(ctx)

	if len(exec.cancelled) != 1 || exec.cancelled[0] != "standalone-stop" {
		t.Fatalf("standalone stop was not cancelled first: %+v", exec.cancelled)
	}
	if len(exec.placed) != 1 || exec.placed[0].Type != execution.OrderTypeMarket {
		t.Fatalf("market TP exit = %+v, want one market order", exec.placed)
	}
	if tr := getTrade(t, tradeID); tr.ID != "" {
		t.Fatal("trade remained open after confirmed software TP fill")
	}
}

func TestReconcileBracket_UncappedStandaloneStopIsBrokerManaged(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("DATABASE_URL not set; skipping DB-backed engine test")
	}
	ctx := context.Background()
	tradeID, err := store.InsertTrade(ctx, store.Trade{
		Ticker: "USTP", Direction: "long", Strategy: "momentum",
		EntryPrice: 100, Quantity: 5, StopLoss: 95, TakeProfit: 0, Status: "open",
	})
	if err != nil {
		t.Fatalf("InsertTrade: %v", err)
	}
	if _, err := store.InsertOrder(ctx, store.Order{
		TradeID: tradeID, BrokerOrderID: "uncapped-stop", Ticker: "USTP", Side: "sell",
		OrderType: "stop", Quantity: 5, StopPrice: 95, Status: "pending",
		IsStopLoss: true,
	}); err != nil {
		t.Fatalf("InsertOrder: %v", err)
	}

	exec := &fakeBracketExec{statuses: map[string]execution.OrderResult{
		"uncapped-stop": {Status: "accepted"},
	}}
	e := newBracketTestEngine(exec)
	if handled := e.reconcileBracketTrade(ctx, getTrade(t, tradeID)); !handled {
		t.Fatal("uncapped standalone stop must suppress the software stop path")
	}
	if tr := getTrade(t, tradeID); tr.ID == "" {
		t.Fatal("working standalone stop must leave the trade open")
	}
}

func TestReconcileBracket_UncappedStandaloneStopFillClosesTrade(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("DATABASE_URL not set; skipping DB-backed engine test")
	}
	ctx := context.Background()
	tradeID, err := store.InsertTrade(ctx, store.Trade{
		Ticker: "USFL", Direction: "long", Strategy: "momentum",
		EntryPrice: 100, Quantity: 5, StopLoss: 95, TakeProfit: 0, Status: "open",
	})
	if err != nil {
		t.Fatalf("InsertTrade: %v", err)
	}
	if _, err := store.InsertOrder(ctx, store.Order{
		TradeID: tradeID, BrokerOrderID: "uncapped-stop-fill", Ticker: "USFL", Side: "sell",
		OrderType: "stop", Quantity: 5, StopPrice: 95, Status: "pending",
		IsStopLoss: true,
	}); err != nil {
		t.Fatalf("InsertOrder: %v", err)
	}

	exec := &fakeBracketExec{statuses: map[string]execution.OrderResult{
		"uncapped-stop-fill": {Status: "filled", FilledPrice: 94.75, FilledQty: 5},
	}}
	e := newBracketTestEngine(exec)
	if handled := e.reconcileBracketTrade(ctx, getTrade(t, tradeID)); !handled {
		t.Fatal("filled uncapped standalone stop must be reconciled")
	}
	if tr := getTrade(t, tradeID); tr.ID != "" {
		t.Fatal("standalone broker stop fill must close the DB trade")
	}
	if e.portfolio.CurrentState().LastLossAt.IsZero() {
		t.Fatal("standalone stop loss must record loss cooldown")
	}
	closed, err := store.GetTradeByID(ctx, tradeID)
	if err != nil || closed == nil {
		t.Fatalf("GetTradeByID: %v", err)
	}
	if closed.ExitReason != store.ExitReasonStopLoss {
		t.Fatalf("legacy standalone stop reason = %q, want stop_loss", closed.ExitReason)
	}
}

// TestReconcileBracket_WorkingLegsSkipSoftwareExit: pending OCO legs →
// handled=true, trade stays open, nothing cancelled.
func TestReconcileBracket_WorkingLegsSkipSoftwareExit(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("DATABASE_URL not set; skipping DB-backed engine test")
	}
	ctx := context.Background()
	tradeID, tpID, slID := insertBracketFixture(t, "WRKG", 3)
	exec := &fakeBracketExec{statuses: map[string]execution.OrderResult{
		tpID: {Status: "accepted"},
		slID: {Status: "accepted"},
	}}
	e := newBracketTestEngine(exec)
	if handled := e.reconcileBracketTrade(ctx, getTrade(t, tradeID)); !handled {
		t.Fatal("working OCO legs must mark the trade broker-managed")
	}
	if tr := getTrade(t, tradeID); tr.ID == "" {
		t.Fatal("trade must remain open while legs work")
	}
}

func TestReconcileBracket_WorkingStatusWithFullFillFinalizesAfterLegsTerminal(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("DATABASE_URL not set; skipping DB-backed engine test")
	}
	ctx := context.Background()
	tradeID, tpID, slID := insertBracketFixture(t, "WFUL", 5)
	exec := &fakeBracketExec{statuses: map[string]execution.OrderResult{
		tpID: {Status: "partially_filled", FilledPrice: 110, FilledQty: 5},
		slID: {Status: "accepted"},
	}}
	statusCalls := map[string]int{}
	exec.onStatus = func(id string) {
		statusCalls[id]++
		if statusCalls[id] != 2 {
			return
		}
		if id == tpID {
			exec.statuses[id] = execution.OrderResult{Status: "filled", FilledPrice: 110, FilledQty: 5}
		} else if id == slID {
			exec.statuses[id] = execution.OrderResult{Status: "cancelled"}
		}
	}

	e := newBracketTestEngine(exec)
	if handled, err := e.reconcileBrokerExitTradeStatus(ctx, getTrade(t, tradeID), false); !handled || err != nil {
		t.Fatalf("full working fill reconciliation = handled %v err %v", handled, err)
	}
	if tr := getTrade(t, tradeID); tr.ID != "" {
		t.Fatalf("fully sold trade remained open: %+v", tr)
	}
	if len(exec.cancelled) != 2 {
		t.Fatalf("cancelled legs = %v, want both full-fill lagging legs confirmed terminal", exec.cancelled)
	}
}

// TestReconcileBracket_StopLegFillClosesTrade: SL fills at the broker →
// trade closed at the real fill, TP sibling marked cancelled, loss cooldown
// recorded (parity with the software exit path).
func TestReconcileBracket_StopLegFillClosesTrade(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("DATABASE_URL not set; skipping DB-backed engine test")
	}
	ctx := context.Background()
	tradeID, tpID, slID := insertBracketFixture(t, "SLFL", 3)
	exec := &fakeBracketExec{statuses: map[string]execution.OrderResult{
		tpID: {Status: "canceled"},
		slID: {Status: "filled", FilledPrice: 94.8, FilledQty: 3},
	}}
	e := newBracketTestEngine(exec)
	if handled := e.reconcileBracketTrade(ctx, getTrade(t, tradeID)); !handled {
		t.Fatal("a filled leg must be handled by the bracket path")
	}
	if tr := getTrade(t, tradeID); tr.ID != "" {
		t.Fatal("trade must be closed after the stop leg filled")
	}
	legs, err := store.GetOrdersByTrade(ctx, tradeID)
	if err != nil {
		t.Fatalf("GetOrdersByTrade: %v", err)
	}
	for _, o := range legs {
		switch o.BrokerOrderID {
		case slID:
			if o.Status != "filled" || o.FilledPrice != 94.8 {
				t.Fatalf("SL leg = %s @ %.2f, want filled @ 94.80", o.Status, o.FilledPrice)
			}
		case tpID:
			if o.Status != "cancelled" {
				t.Fatalf("TP sibling = %s, want cancelled", o.Status)
			}
		}
	}
	if e.portfolio.CurrentState().LastLossAt.IsZero() {
		t.Fatal("losing bracket stop-out must record the loss cooldown (RecordLoss)")
	}
}

func TestReconcileBracket_RatchetedStopRetainsTrailingATRReason(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("DATABASE_URL not set; skipping DB-backed engine test")
	}
	ctx := context.Background()
	tradeID, tpID, slID := insertBracketFixture(t, "ATRF", 3)
	legs, err := store.GetOrdersByTrade(ctx, tradeID)
	if err != nil {
		t.Fatalf("GetOrdersByTrade: %v", err)
	}
	var stopLeg store.Order
	for _, leg := range legs {
		if leg.BrokerOrderID == slID {
			stopLeg = leg
			break
		}
	}
	intent := newExitIntent(store.ExitReasonTrailingATR, map[string]any{
		"trailing_stop": 97.5, "high_water_mark": 105.0, "atr": 5.0, "atr_multiple": 1.5,
	})
	staleFillAt := time.Date(2026, 7, 31, 15, 0, 0, 0, time.UTC)
	if err := store.UpdateOrderFillAt(ctx, stopLeg.ID, 96, 1, "partial", staleFillAt); err != nil {
		t.Fatalf("seed stale replacement state: %v", err)
	}
	if _, err := store.GetDB().Exec(ctx, `UPDATE orders SET exit_fill_applied_qty=1 WHERE id=$1`, stopLeg.ID); err != nil {
		t.Fatalf("seed stale applied quantity: %v", err)
	}
	ratchetedID := slID + "-ratcheted"
	if err := store.UpdateOrderStopReplacement(ctx, stopLeg.ID, stopLeg.BrokerOrderID, ratchetedID, 1, 3, 97.5, 3, "pending", 0, 0, time.Time{}, intent); err != nil {
		t.Fatalf("tag ratcheted stop: %v", err)
	}
	legs, err = store.GetOrdersByTrade(ctx, tradeID)
	if err != nil {
		t.Fatalf("reload ratcheted stop: %v", err)
	}
	for _, leg := range legs {
		if leg.ID == stopLeg.ID {
			if leg.BrokerOrderID != ratchetedID || leg.StopPrice != 97.5 || leg.Quantity != 3 ||
				leg.Status != "pending" || leg.FilledPrice != 0 || leg.FilledQty != 0 || leg.FilledAt != nil ||
				leg.ExitReason != store.ExitReasonTrailingATR {
				t.Fatalf("durable ratcheted stop = %+v", leg)
			}
		}
	}
	var appliedQty float64
	if err := store.GetDB().QueryRow(ctx, `SELECT exit_fill_applied_qty FROM orders WHERE id=$1`, stopLeg.ID).Scan(&appliedQty); err != nil || appliedQty != 0 {
		t.Fatalf("replacement applied quantity = %v, err=%v; want reset to zero", appliedQty, err)
	}
	exec := &fakeBracketExec{statuses: map[string]execution.OrderResult{
		tpID:        {Status: "canceled"},
		ratchetedID: {Status: "filled", FilledPrice: 97.25, FilledQty: 3},
	}}
	e := newBracketTestEngine(exec)
	if handled := e.reconcileBracketTrade(ctx, getTrade(t, tradeID)); !handled {
		t.Fatal("ratcheted stop fill must be handled")
	}
	closed, err := store.GetTradeByID(ctx, tradeID)
	if err != nil || closed == nil {
		t.Fatalf("GetTradeByID: %v", err)
	}
	if closed.ExitReason != store.ExitReasonTrailingATR {
		t.Fatalf("exit reason = %q, want trailing_atr", closed.ExitReason)
	}
	var evidence map[string]any
	if err := json.Unmarshal(closed.ExitEvidence, &evidence); err != nil || evidence["trailing_stop"] != 97.5 || evidence["broker_order_id"] != ratchetedID {
		t.Fatalf("exit evidence = %s, err=%v", closed.ExitEvidence, err)
	}
}

// TestReconcileBracket_PartialTPPlusStopWeightedExit: TP partially fills
// 2/3 shares, OCO cancels it, the ratcheted stop fills the remaining 1 —
// the close must be the fill-weighted aggregate, not the stop price alone.
func TestReconcileBracket_PartialTPPlusStopWeightedExit(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("DATABASE_URL not set; skipping DB-backed engine test")
	}
	ctx := context.Background()
	tradeID, tpID, slID := insertBracketFixture(t, "PRTL", 3)
	exec := &fakeBracketExec{statuses: map[string]execution.OrderResult{
		tpID: {Status: "canceled", FilledPrice: 110, FilledQty: 2},
		slID: {Status: "filled", FilledPrice: 95, FilledQty: 1},
	}}
	e := newBracketTestEngine(exec)
	if handled := e.reconcileBracketTrade(ctx, getTrade(t, tradeID)); !handled {
		t.Fatal("filled leg must be handled")
	}
	// Weighted exit: (2×110 + 1×95) / 3 = 105. Entry 100, qty 3 → pnl +15.
	var exitPrice, pnl float64
	err := store.GetDB().QueryRow(ctx,
		`SELECT exit_price, pnl FROM trades WHERE id=$1 AND status='closed'`, tradeID,
	).Scan(&exitPrice, &pnl)
	if err != nil {
		t.Fatalf("closed trade lookup: %v", err)
	}
	if exitPrice < 104.99 || exitPrice > 105.01 {
		t.Fatalf("exit_price = %.4f, want fill-weighted 105", exitPrice)
	}
	if pnl < 14.9 || pnl > 15.1 {
		t.Fatalf("pnl = %.4f, want ~+15 (partial TP proceeds included)", pnl)
	}
}

// TestReconcileBracket_LegsDeadWithPartial_ReducesQuantity: both legs
// terminal, one carried a partial fill, remainder unsold — the tracked
// quantity must shrink before reverting to app-managed monitoring, or the
// software exit would over-sell shares the broker no longer holds.
func TestReconcileBracket_LegsDeadWithPartial_ReducesQuantity(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("DATABASE_URL not set; skipping DB-backed engine test")
	}
	ctx := context.Background()
	tradeID, tpID, slID := insertBracketFixture(t, "DEAD", 3)
	exec := &fakeBracketExec{statuses: map[string]execution.OrderResult{
		tpID: {Status: "canceled", FilledPrice: 110, FilledQty: 1},
		slID: {Status: "canceled"},
	}}
	e := newBracketTestEngine(exec)
	if handled := e.reconcileBracketTrade(ctx, getTrade(t, tradeID)); handled {
		t.Fatal("dead legs must revert the trade to app-managed monitoring")
	}
	tr := getTrade(t, tradeID)
	if tr.ID == "" {
		t.Fatal("trade must remain open (only 1 of 3 shares sold)")
	}
	if tr.Quantity != 2 {
		t.Fatalf("Quantity = %.2f, want 2 after the 1-share partial exit", tr.Quantity)
	}
}

func TestReconcileBracket_GTCDoneForDayRemainsBrokerManaged(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("DATABASE_URL not set; skipping DB-backed engine test")
	}
	ctx := context.Background()
	tradeID, tpID, slID := insertBracketFixture(t, "GTCDFD", 3)
	exec := &fakeBracketExec{statuses: map[string]execution.OrderResult{
		tpID: {Status: "done_for_day"},
		slID: {Status: "done_for_day"},
	}}
	e := newBracketTestEngine(exec)
	if handled := e.reconcileBracketTrade(ctx, getTrade(t, tradeID)); !handled {
		t.Fatal("GTC bracket legs reported done_for_day must remain broker-managed")
	}
	if len(exec.placed) != 0 {
		t.Fatalf("software exit submissions = %d, want 0", len(exec.placed))
	}
	legs, err := store.GetOrdersByTrade(ctx, tradeID)
	if err != nil {
		t.Fatalf("GetOrdersByTrade: %v", err)
	}
	for _, leg := range legs {
		if leg.Status != "pending" {
			t.Fatalf("GTC bracket leg %s status = %q, want pending", leg.BrokerOrderID, leg.Status)
		}
	}
}

func TestReconcileBrokerExit_FractionalDoneForDayTerminalizes(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("DATABASE_URL not set; skipping DB-backed engine test")
	}
	ctx := context.Background()
	tradeID, err := store.InsertTrade(ctx, store.Trade{
		Ticker: "FRACDFD", Direction: "long", Strategy: "momentum",
		EntryPrice: 100, Quantity: 1.5, StopLoss: 95, Status: "open",
	})
	if err != nil {
		t.Fatalf("InsertTrade: %v", err)
	}
	if _, err := store.InsertOrder(ctx, store.Order{
		TradeID: tradeID, BrokerOrderID: "frac-dfd", Ticker: "FRACDFD", Side: "sell",
		OrderType: "stop", Quantity: 1.5, StopPrice: 95, Status: "pending", IsStopLoss: true,
	}); err != nil {
		t.Fatalf("InsertOrder: %v", err)
	}
	e := newBracketTestEngine(&fakeBracketExec{statuses: map[string]execution.OrderResult{
		"frac-dfd": {Status: "done_for_day"},
	}})
	if handled := e.reconcileBrokerExitTrade(ctx, getTrade(t, tradeID), false); handled {
		t.Fatal("fractional DAY protective stop reported done_for_day must return to app-managed exits")
	}
	legs, err := store.GetOrdersByTrade(ctx, tradeID)
	if err != nil {
		t.Fatalf("GetOrdersByTrade: %v", err)
	}
	if len(legs) != 1 || legs[0].Status != "cancelled" {
		t.Fatalf("fractional DAY stop status = %+v, want cancelled", legs)
	}
}

func TestReconcileBracket_PreviouslyAppliedPartialDoesNotCloseRemainder(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("DATABASE_URL not set; skipping DB-backed engine test")
	}
	ctx := context.Background()
	tradeID, tpID, slID := insertBracketFixture(t, "DAPR", 10)
	legs, err := store.GetOrdersByTrade(ctx, tradeID)
	if err != nil {
		t.Fatalf("GetOrdersByTrade: %v", err)
	}
	var tpLeg store.Order
	for _, leg := range legs {
		if leg.BrokerOrderID == tpID {
			tpLeg = leg
			break
		}
	}
	if tpLeg.ID == "" {
		t.Fatal("take-profit leg not found")
	}
	if remaining, _, err := store.ApplyPartialExitAtForBrokerOrder(
		ctx, tradeID, tpLeg.ID, tpID, 110, 6, "partial", time.Now(),
	); err != nil || remaining != 4 {
		t.Fatalf("seed applied partial: remaining=%.2f err=%v; want 4", remaining, err)
	}

	exec := &fakeBracketExec{statuses: map[string]execution.OrderResult{
		tpID: {Status: "canceled", FilledPrice: 110, FilledQty: 6},
		slID: {Status: "canceled"},
	}}
	e := newBracketTestEngine(exec)
	if handled := e.reconcileBracketTrade(ctx, getTrade(t, tradeID)); handled {
		t.Fatal("terminal legs with only a previously applied partial must return the remainder to app monitoring")
	}
	tr := getTrade(t, tradeID)
	if tr.ID == "" || tr.Status != "open" || tr.Quantity != 4 {
		t.Fatalf("trade after terminal partial = %+v; want open with quantity 4", tr)
	}
}

func TestStartupReconcileFinalizesOpenTradeAfterTerminalFillWasApplied(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("DATABASE_URL not set; skipping DB-backed engine test")
	}
	ctx := context.Background()
	tradeID, _, slID := insertBracketFixture(t, fmt.Sprintf("TREC_%d", time.Now().UnixNano()), 3)
	orders, err := store.GetOrdersByTrade(ctx, tradeID)
	if err != nil {
		t.Fatalf("GetOrdersByTrade: %v", err)
	}
	var stop store.Order
	for _, order := range orders {
		if order.BrokerOrderID == slID {
			stop = order
			break
		}
	}
	if stop.ID == "" {
		t.Fatal("stop leg not found")
	}
	filledAt := time.Date(2026, 8, 11, 15, 30, 0, 0, time.UTC)
	remaining, applied, err := store.ApplyPartialExitAtForBrokerOrder(
		ctx, tradeID, stop.ID, slID, 94, 3, "filled", filledAt,
	)
	if err != nil || remaining != 0 || applied != 3 {
		t.Fatalf("seed terminal applied fill: remaining=%.2f applied=%.2f err=%v", remaining, applied, err)
	}
	if trade := getTrade(t, tradeID); trade.Status != "open" || trade.Quantity != 0 {
		t.Fatalf("pre-recovery trade = %+v; want open zero-quantity crash state", trade)
	}

	exec := &fakeBracketExec{statusErr: map[string]error{slID: execution.ErrOrderNotFound}}
	e := newBracketTestEngine(exec)
	if handled, reconcileErr := e.reconcileBrokerExitTradeStatus(ctx, getTrade(t, tradeID), true); !handled || reconcileErr != nil {
		t.Fatalf("startup terminal-fill recovery = handled %v err %v", handled, reconcileErr)
	}
	closed, err := store.GetTradeByID(ctx, tradeID)
	if err != nil || closed == nil || closed.Status != "closed" || closed.ExitPrice != 94 || closed.PnL != -18 {
		t.Fatalf("recovered trade = %+v err=%v; want closed at 94 with -18 pnl", closed, err)
	}
}

// TestCancelBracketLegs_AllClearSemantics: a failed cancel must report NOT
// clear (the SELL caller blocks), and the failed leg's row must stay pending
// so the monitor keeps treating the trade as broker-managed.
func TestCancelBracketLegs_AllClearSemantics(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("DATABASE_URL not set; skipping DB-backed engine test")
	}
	ctx := context.Background()
	tradeID, tpID, slID := insertBracketFixture(t, "CNCL", 3)

	exec := &fakeBracketExec{cancelErr: map[string]error{slID: fmt.Errorf("simulated cancel failure")}}
	e := newBracketTestEngine(exec)
	if clear := e.cancelBracketLegsForTicker(ctx, "CNCL"); clear {
		t.Fatal("failed leg cancel must report not-clear")
	}
	legs, err := store.OpenExitLegsForTrade(ctx, tradeID)
	if err != nil {
		t.Fatalf("OpenExitLegsForTrade: %v", err)
	}
	if len(legs) != 1 || legs[0].BrokerOrderID != slID {
		t.Fatalf("working legs = %+v, want only the un-cancelled SL leg %s", legs, slID)
	}

	// Second attempt succeeds → all clear, no working legs remain.
	exec.cancelErr = nil
	if clear := e.cancelBracketLegsForTicker(ctx, "CNCL"); !clear {
		t.Fatal("successful cancels must report all-clear")
	}
	legs, _ = store.OpenExitLegsForTrade(ctx, tradeID)
	if len(legs) != 0 {
		t.Fatalf("working legs = %+v, want none", legs)
	}
	_ = tpID
}
