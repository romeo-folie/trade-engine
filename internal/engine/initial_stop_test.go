package engine

import (
	"context"
	"errors"
	"testing"

	"github.com/romeo-folie/trade-engine/internal/execution"
	"github.com/romeo-folie/trade-engine/internal/store"
)

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
	stop, err := store.LatestStopLossOrderForTrade(ctx, tradeID)
	if err != nil || stop == nil {
		t.Fatalf("LatestStopLossOrderForTrade: %v (nil=%v)", err, stop == nil)
	}
	if stop.Quantity != 5 || stop.BrokerOrderID != "uncap-stop-1" {
		t.Fatalf("stored stop = %+v", stop)
	}

	// Idempotent when the trailing manager or fill poll reaches the same fill.
	if err := e.ensureUncappedProtectiveStop(ctx, tradeID, "UCPS", 5, 95); err != nil {
		t.Fatalf("second ensure: %v", err)
	}
	if len(exec.placed) != 1 {
		t.Fatalf("idempotent ensure placed %d stops, want 1", len(exec.placed))
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
			_, err := rollbackUnrecordedProtectiveStop(context.Background(), exec, "stop-1")
			if (err != nil) != tt.wantErr {
				t.Fatalf("rollback error = %v, wantErr %v", err, tt.wantErr)
			}
			if len(exec.cancelled) != 1 || exec.cancelled[0] != "stop-1" {
				t.Fatalf("cancelled = %v, want stop-1", exec.cancelled)
			}
		})
	}
}
