package store_test

import (
	"context"
	"testing"

	"github.com/romeo-folie/trade-engine/internal/store"
)

// mustInsertClosedTradeWithPendingExit sets up the exact shape the exit-fill
// reconciler sweeps for: a closed trade whose exit order is still 'pending'
// with empty fill columns (Alpaca market exits fill async, so the close path
// records the row before the fill confirms).
func mustInsertClosedTradeWithPendingExit(t *testing.T, ctx context.Context, ticker, direction, exitSide, brokerOrderID string) string {
	t.Helper()
	tradeID, err := store.InsertTrade(ctx, store.Trade{
		Ticker:     ticker,
		Direction:  direction,
		Strategy:   "trend_following",
		EntryPrice: 3.86,
		Quantity:   480.122977,
	})
	if err != nil {
		t.Fatalf("InsertTrade: %v", err)
	}
	// Close at the (possibly wrong) trigger price, like monitorPositions does.
	if err := store.CloseTrade(ctx, tradeID, 5.50, 787.40, 42.49); err != nil {
		t.Fatalf("CloseTrade: %v", err)
	}
	if _, err := store.InsertOrder(ctx, store.Order{
		TradeID:       tradeID,
		BrokerOrderID: brokerOrderID,
		Ticker:        ticker,
		Side:          exitSide,
		OrderType:     "market",
		Quantity:      480.122977,
		Status:        "pending",
		IsTakeProfit:  true,
	}); err != nil {
		t.Fatalf("InsertOrder: %v", err)
	}
	return tradeID
}

func TestGetPendingExitOrders_ShapeAndExclusions(t *testing.T) {
	ctx := context.Background()

	// Row the sweep must find: closed long + pending sell order.
	wantTradeID := mustInsertClosedTradeWithPendingExit(t, ctx, "TEST_XRC_LAC", "long", "sell", "brk-xrc-1")

	// Exclusion 1: entry order (side matches direction) on a closed trade.
	if _, err := store.InsertOrder(ctx, store.Order{
		TradeID:       wantTradeID,
		BrokerOrderID: "brk-xrc-entry",
		Ticker:        "TEST_XRC_LAC",
		Side:          "buy",
		OrderType:     "market",
		Quantity:      480.122977,
		Status:        "pending",
	}); err != nil {
		t.Fatalf("InsertOrder entry: %v", err)
	}

	// Exclusion 2: pending exit-shaped order on a still-OPEN trade (a working
	// broker stop). Must not be swept — there is nothing to reconcile yet.
	openTradeID, err := store.InsertTrade(ctx, store.Trade{
		Ticker: "TEST_XRC_OPEN", Direction: "long", Strategy: "trend_following",
		EntryPrice: 10, Quantity: 5,
	})
	if err != nil {
		t.Fatalf("InsertTrade open: %v", err)
	}
	if _, err := store.InsertOrder(ctx, store.Order{
		TradeID: openTradeID, BrokerOrderID: "brk-xrc-stop", Ticker: "TEST_XRC_OPEN",
		Side: "sell", OrderType: "stop", Quantity: 5, Status: "pending", IsStopLoss: true,
	}); err != nil {
		t.Fatalf("InsertOrder stop: %v", err)
	}

	rows, err := store.GetPendingExitOrders(ctx, 100)
	if err != nil {
		t.Fatalf("GetPendingExitOrders: %v", err)
	}
	var found *store.PendingExitOrder
	for i := range rows {
		switch rows[i].BrokerOrderID {
		case "brk-xrc-1":
			found = &rows[i]
		case "brk-xrc-entry", "brk-xrc-stop":
			t.Fatalf("sweep returned excluded order %s", rows[i].BrokerOrderID)
		}
	}
	if found == nil {
		t.Fatal("pending exit order on closed trade not returned")
	}
	if found.TradeID != wantTradeID || found.Direction != "long" ||
		found.EntryPrice != 3.86 || found.ExitPrice != 5.50 {
		t.Fatalf("row = %+v, want trade fields joined", *found)
	}
}

func TestGetPendingExitOrders_IncludesPartialRows(t *testing.T) {
	ctx := context.Background()
	tradeID, err := store.InsertTrade(ctx, store.Trade{
		Ticker: "TEST_XRC_PART", Direction: "long", Strategy: "trend_following",
		EntryPrice: 5, Quantity: 10,
	})
	if err != nil {
		t.Fatalf("InsertTrade: %v", err)
	}
	if err := store.CloseTrade(ctx, tradeID, 6.0, 10, 20); err != nil {
		t.Fatalf("CloseTrade: %v", err)
	}
	if _, err := store.InsertOrder(ctx, store.Order{
		TradeID: tradeID, BrokerOrderID: "brk-xrc-part", Ticker: "TEST_XRC_PART",
		Side: "sell", OrderType: "market", Quantity: 10, Status: "partial",
	}); err != nil {
		t.Fatalf("InsertOrder: %v", err)
	}

	rows, err := store.GetPendingExitOrders(ctx, 100)
	if err != nil {
		t.Fatalf("GetPendingExitOrders: %v", err)
	}
	for _, r := range rows {
		if r.BrokerOrderID == "brk-xrc-part" {
			return
		}
	}
	t.Fatal("partial exit order on closed trade not swept — a partially_filled PlaceOrder result would stay priceless forever")
}

func TestInsertOrder_PersistsFillColumns(t *testing.T) {
	ctx := context.Background()
	tradeID, err := store.InsertTrade(ctx, store.Trade{
		Ticker: "TEST_XRC_FILL", Direction: "long", Strategy: "trend_following",
		EntryPrice: 5, Quantity: 10,
	})
	if err != nil {
		t.Fatalf("InsertTrade: %v", err)
	}
	if _, err := store.InsertOrder(ctx, store.Order{
		TradeID: tradeID, BrokerOrderID: "brk-xrc-fill", Ticker: "TEST_XRC_FILL",
		Side: "sell", OrderType: "market", Quantity: 10, Status: "filled",
		FilledPrice: 5.25, FilledQty: 10,
	}); err != nil {
		t.Fatalf("InsertOrder: %v", err)
	}

	orders, err := store.GetOrdersByTrade(ctx, tradeID)
	if err != nil {
		t.Fatalf("GetOrdersByTrade: %v", err)
	}
	for _, o := range orders {
		if o.BrokerOrderID == "brk-xrc-fill" {
			if o.FilledPrice != 5.25 || o.FilledQty != 10 {
				t.Fatalf("fill columns = %.4f/%.4f, want 5.25/10 — an already-filled insert must carry its fill", o.FilledPrice, o.FilledQty)
			}
			return
		}
	}
	t.Fatal("inserted order not found")
}

func TestInsertOrderRejectsSecondActiveStopForTrade(t *testing.T) {
	ctx := context.Background()
	tradeID, err := store.InsertTrade(ctx, store.Trade{
		Ticker: "TEST_XRC_STOP_OWNER", Direction: "long",
		Strategy: "test", EntryPrice: 10, Quantity: 5,
	})
	if err != nil {
		t.Fatalf("InsertTrade: %v", err)
	}
	first := store.Order{
		TradeID: tradeID, BrokerOrderID: "brk-stop-owner-1",
		Ticker: "TEST_XRC_STOP_OWNER", Side: "sell", OrderType: "stop",
		Quantity: 5, StopPrice: 9, Status: "pending", IsStopLoss: true,
	}
	if _, err := store.InsertOrder(ctx, first); err != nil {
		t.Fatalf("InsertOrder first stop: %v", err)
	}
	first.BrokerOrderID = "brk-stop-owner-2"
	if _, err := store.InsertOrder(ctx, first); err == nil {
		t.Fatal("second active STOP row was accepted for the same trade")
	}
}

func TestUpdateTradeExit_RewritesPnLPreservesClosedAt(t *testing.T) {
	ctx := context.Background()
	tradeID := mustInsertClosedTradeWithPendingExit(t, ctx, "TEST_XRC_UPD", "long", "sell", "brk-xrc-upd")

	before, err := store.GetTradeByID(ctx, tradeID)
	if err != nil || before == nil {
		t.Fatalf("GetTradeByID before: %v", err)
	}

	if err := store.UpdateTradeExit(ctx, tradeID, 3.82, -19.20, -1.04); err != nil {
		t.Fatalf("UpdateTradeExit: %v", err)
	}

	after, err := store.GetTradeByID(ctx, tradeID)
	if err != nil || after == nil {
		t.Fatalf("GetTradeByID after: %v", err)
	}
	if after.ExitPrice != 3.82 || after.PnL != -19.20 {
		t.Fatalf("trade = exit %.4f pnl %.4f, want 3.82 / -19.20", after.ExitPrice, after.PnL)
	}
	if after.Status != "closed" {
		t.Fatalf("status = %q, want closed", after.Status)
	}
	if !after.ClosedAt.Equal(before.ClosedAt) {
		t.Fatalf("closed_at changed %v → %v — correction must preserve close time", before.ClosedAt, after.ClosedAt)
	}
}

func TestMarkOrderStatus(t *testing.T) {
	ctx := context.Background()
	tradeID := mustInsertClosedTradeWithPendingExit(t, ctx, "TEST_XRC_CXL", "long", "sell", "brk-xrc-cxl")

	if err := store.MarkOrderStatus(ctx, "brk-xrc-cxl", "cancelled"); err != nil {
		t.Fatalf("MarkOrderStatus: %v", err)
	}
	orders, err := store.GetOrdersByTrade(ctx, tradeID)
	if err != nil {
		t.Fatalf("GetOrdersByTrade: %v", err)
	}
	var got string
	for _, o := range orders {
		if o.BrokerOrderID == "brk-xrc-cxl" {
			got = o.Status
		}
	}
	if got != "cancelled" {
		t.Fatalf("order status = %q, want cancelled", got)
	}
}

func TestApplyPartialExitIsAtomicAndIdempotent(t *testing.T) {
	ctx := context.Background()
	tradeID, err := store.InsertTrade(ctx, store.Trade{
		Ticker: "TEST_XRC_ATOMIC", Direction: "long", Strategy: "test",
		EntryPrice: 10, Quantity: 5,
	})
	if err != nil {
		t.Fatalf("InsertTrade: %v", err)
	}
	orderID, err := store.InsertOrder(ctx, store.Order{
		TradeID: tradeID, BrokerOrderID: "brk-xrc-atomic",
		Ticker: "TEST_XRC_ATOMIC", Side: "sell", OrderType: "stop",
		Quantity: 5, Status: "partial", IsStopLoss: true,
	})
	if err != nil {
		t.Fatalf("InsertOrder: %v", err)
	}

	remaining, applied, err := store.ApplyPartialExit(
		ctx, tradeID, orderID, 9.50, 2, "cancelled",
	)
	if err != nil || remaining != 3 || applied != 2 {
		t.Fatalf("first apply = remaining %v applied %v err %v, want 3/2/nil",
			remaining, applied, err)
	}
	remaining, applied, err = store.ApplyPartialExit(
		ctx, tradeID, orderID, 9.50, 2, "cancelled",
	)
	if err != nil || remaining != 3 || applied != 0 {
		t.Fatalf("replay = remaining %v applied %v err %v, want 3/0/nil",
			remaining, applied, err)
	}
}

func TestApplyPartialExitAppliesPreviouslyRecordedButUnappliedFill(t *testing.T) {
	ctx := context.Background()
	tradeID, err := store.InsertTrade(ctx, store.Trade{
		Ticker: "TEST_XRC_LEGACY_APPLY", Direction: "long", Strategy: "test",
		EntryPrice: 10, Quantity: 5,
	})
	if err != nil {
		t.Fatalf("InsertTrade: %v", err)
	}
	orderID, err := store.InsertOrder(ctx, store.Order{
		TradeID: tradeID, BrokerOrderID: "brk-xrc-legacy-apply",
		Ticker: "TEST_XRC_LEGACY_APPLY", Side: "sell", OrderType: "stop",
		Quantity: 5, Status: "pending", IsStopLoss: true,
	})
	if err != nil {
		t.Fatalf("InsertOrder: %v", err)
	}
	if err := store.UpdateOrderFill(ctx, orderID, 9.50, 2, "cancelled"); err != nil {
		t.Fatalf("record split fill: %v", err)
	}
	remaining, applied, err := store.ApplyPartialExit(
		ctx, tradeID, orderID, 9.50, 2, "cancelled",
	)
	if err != nil || remaining != 3 || applied != 2 {
		t.Fatalf("recovery apply = remaining %v applied %v err %v, want 3/2/nil",
			remaining, applied, err)
	}
}

func TestApplyPartialExitClampsAggregateFillToLotQuantity(t *testing.T) {
	ctx := context.Background()
	tradeID, err := store.InsertTrade(ctx, store.Trade{
		Ticker: "TEST_XRC_CLAMP", Direction: "long", Strategy: "test",
		EntryPrice: 10, Quantity: 10,
	})
	if err != nil {
		t.Fatalf("InsertTrade: %v", err)
	}
	orderID, err := store.InsertOrder(ctx, store.Order{
		TradeID: tradeID, BrokerOrderID: "brk-xrc-clamp",
		Ticker: "TEST_XRC_CLAMP", Side: "sell", OrderType: "market",
		Quantity: 4, Status: "partial",
	})
	if err != nil {
		t.Fatalf("InsertOrder: %v", err)
	}

	remaining, applied, err := store.ApplyPartialExit(
		ctx, tradeID, orderID, 9.5, 9, "cancelled",
	)
	if err != nil || remaining != 6 || applied != 4 {
		t.Fatalf("clamped apply = remaining %v applied %v err %v, want 6/4/nil",
			remaining, applied, err)
	}
}

func TestApplyEntryFillKeepsPreviouslyExitedSharesSubtracted(t *testing.T) {
	ctx := context.Background()
	tradeID, err := store.InsertTrade(ctx, store.Trade{
		Ticker: "TEST_XRC_ENTRY_NET", Direction: "long", Strategy: "test",
		EntryPrice: 10, Quantity: 2,
	})
	if err != nil {
		t.Fatalf("InsertTrade: %v", err)
	}
	if _, err := store.InsertOrder(ctx, store.Order{
		TradeID: tradeID, BrokerOrderID: "brk-xrc-entry-net",
		Ticker: "TEST_XRC_ENTRY_NET", Side: "buy", OrderType: "market",
		Quantity: 5, FilledPrice: 10, FilledQty: 2, Status: "partial",
	}); err != nil {
		t.Fatalf("InsertOrder entry: %v", err)
	}
	exitOrderID, err := store.InsertOrder(ctx, store.Order{
		TradeID: tradeID, BrokerOrderID: "brk-xrc-entry-net-stop",
		Ticker: "TEST_XRC_ENTRY_NET", Side: "sell", OrderType: "stop",
		Quantity: 2, Status: "partial", IsStopLoss: true,
	})
	if err != nil {
		t.Fatalf("InsertOrder exit: %v", err)
	}
	if _, _, err := store.ApplyPartialExit(
		ctx, tradeID, exitOrderID, 9.5, 1, "cancelled",
	); err != nil {
		t.Fatalf("ApplyPartialExit: %v", err)
	}

	if err := store.ApplyEntryFill(
		ctx, tradeID, "brk-xrc-entry-net", 10.2, 5, "filled",
	); err != nil {
		t.Fatalf("ApplyEntryFill: %v", err)
	}
	trade, err := store.GetTradeByID(ctx, tradeID)
	if err != nil || trade == nil {
		t.Fatalf("GetTradeByID: %v", err)
	}
	if trade.Quantity != 4 {
		t.Fatalf("net quantity = %v, want cumulative entry 5 minus exited 1", trade.Quantity)
	}
	if trade.EntryPrice != 10.2 {
		t.Fatalf("entry price = %v, want broker cumulative average 10.2", trade.EntryPrice)
	}
}

func TestGetOrderlessExitingTradesTracksClaimWindow(t *testing.T) {
	ctx := context.Background()
	tradeID, err := store.InsertTrade(ctx, store.Trade{
		Ticker: "TEST_XRC_ORDERLESS", Direction: "long", Strategy: "test",
		EntryPrice: 10, Quantity: 5,
	})
	if err != nil {
		t.Fatalf("InsertTrade: %v", err)
	}
	if claimed, err := store.ClaimTradeExit(ctx, tradeID); err != nil || !claimed {
		t.Fatalf("ClaimTradeExit = %v, %v", claimed, err)
	}
	rows, err := store.GetOrderlessExitingTrades(ctx, 100)
	if err != nil {
		t.Fatalf("GetOrderlessExitingTrades: %v", err)
	}
	for _, row := range rows {
		if row.ID == tradeID {
			t.Fatal("fresh claim was recovered while its submitting worker may still be active")
		}
	}
	if _, err := store.GetDB().Exec(ctx, `
		UPDATE trades SET exit_claimed_at=NOW()-INTERVAL '3 minutes'
		WHERE id=$1`, tradeID); err != nil {
		t.Fatalf("age exit claim: %v", err)
	}
	rows, err = store.GetOrderlessExitingTrades(ctx, 100)
	if err != nil {
		t.Fatalf("aged GetOrderlessExitingTrades: %v", err)
	}
	found := false
	for _, row := range rows {
		found = found || row.ID == tradeID
	}
	if !found {
		t.Fatal("aged exit claim without an order was not discoverable")
	}

	if _, err := store.InsertOrder(ctx, store.Order{
		TradeID: tradeID, BrokerOrderID: "brk-xrc-recovered",
		Ticker: "TEST_XRC_ORDERLESS", Side: "sell", OrderType: "market",
		Quantity: 5, Status: "pending",
	}); err != nil {
		t.Fatalf("InsertOrder: %v", err)
	}
	rows, err = store.GetOrderlessExitingTrades(ctx, 100)
	if err != nil {
		t.Fatalf("second GetOrderlessExitingTrades: %v", err)
	}
	for _, row := range rows {
		if row.ID == tradeID {
			t.Fatal("claim remained orderless after its exit order became durable")
		}
	}
}

func TestGetDurableExitOrderOwnershipIsNotBatchLimited(t *testing.T) {
	ctx := context.Background()
	tradeID, err := store.InsertTrade(ctx, store.Trade{
		Ticker: "TEST_XRC_OWNER", Direction: "long", Strategy: "test",
		EntryPrice: 10, Quantity: 5,
	})
	if err != nil {
		t.Fatalf("InsertTrade: %v", err)
	}
	if claimed, err := store.ClaimTradeExit(ctx, tradeID); err != nil || !claimed {
		t.Fatalf("ClaimTradeExit = %v, %v", claimed, err)
	}
	if _, err := store.InsertOrder(ctx, store.Order{
		TradeID: tradeID, BrokerOrderID: "brk-xrc-owner",
		Ticker: "TEST_XRC_OWNER", Side: "sell", OrderType: "market",
		Quantity: 5, Status: "pending",
	}); err != nil {
		t.Fatalf("InsertOrder: %v", err)
	}

	owners, err := store.GetDurableExitOrderOwnership(ctx)
	if err != nil {
		t.Fatalf("GetDurableExitOrderOwnership: %v", err)
	}
	for _, owner := range owners {
		if owner.BrokerOrderID == "brk-xrc-owner" {
			if owner.Ticker != "TEST_XRC_OWNER" || owner.Side != "sell" ||
				owner.TradeStatus != "exiting" || !owner.ClaimOwned {
				t.Fatalf("owner = %+v", owner)
			}
			return
		}
	}
	t.Fatal("durable exiting order missing from complete ownership snapshot")
}
