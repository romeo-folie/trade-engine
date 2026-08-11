package store_test

import (
	"context"
	"encoding/json"
	"math"
	"reflect"
	"testing"
	"time"

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

func TestGetPendingExitOrdersExcludesStaleTerminalRowsOnExitingTrade(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("DATABASE_URL not set; skipping store integration test")
	}
	ctx := context.Background()
	tradeID, err := store.InsertTrade(ctx, store.Trade{
		Ticker: "TEST_XRC_STALE", Direction: "long", Strategy: "test",
		EntryPrice: 100, Quantity: 10, Status: "open",
	})
	if err != nil {
		t.Fatalf("InsertTrade: %v", err)
	}
	if _, err := store.InsertOrder(ctx, store.Order{
		TradeID: tradeID, BrokerOrderID: "brk-xrc-stale", Ticker: "TEST_XRC_STALE",
		Side: "sell", OrderType: "stop", Quantity: 10, Status: "pending", IsStopLoss: true,
	}); err != nil {
		t.Fatalf("InsertOrder stale: %v", err)
	}
	if claimed, err := store.ClaimTradeExit(ctx, tradeID); err != nil || !claimed {
		t.Fatalf("ClaimTradeExit = %v, %v", claimed, err)
	}
	assertClaimOwned := func(stage string, want, requirePresent bool) {
		owners, err := store.GetDurableExitOrderOwnership(ctx)
		if err != nil {
			t.Fatalf("%s GetDurableExitOrderOwnership: %v", stage, err)
		}
		for _, owner := range owners {
			if owner.BrokerOrderID == "brk-xrc-stale" {
				if owner.ClaimOwned != want {
					t.Fatalf("%s protective stop ownership = %v, want %v: %+v", stage, owner.ClaimOwned, want, owner)
				}
				return
			}
		}
		if requirePresent {
			t.Fatalf("%s protective stop missing from durable ownership", stage)
		}
	}
	// The pre-claim stop must be present in the broker-ID ownership snapshot
	// without owning the later software claim. That prevents duplicate order
	// insertion while still allowing orderless recovery to release the claim.
	assertClaimOwned("pending", false, true)
	// Cancelling the old protective stop after winning the exit claim updates
	// updated_at, but it must not become the current market exit order.
	orders, err := store.GetOrdersByTrade(ctx, tradeID)
	if err != nil || len(orders) != 1 {
		t.Fatalf("GetOrdersByTrade stale stop: %v rows=%d", err, len(orders))
	}
	if err := store.UpdateOrderFill(ctx, orders[0].ID, 0, 0, "cancelled"); err != nil {
		t.Fatalf("cancel stale protective stop: %v", err)
	}
	assertClaimOwned("cancelled", false, false)
	if _, err := store.InsertOrder(ctx, store.Order{
		TradeID: tradeID, BrokerOrderID: "brk-xrc-current", Ticker: "TEST_XRC_STALE",
		Side: "sell", OrderType: "market", Quantity: 10, Status: "pending", IsTakeProfit: true,
	}); err != nil {
		t.Fatalf("InsertOrder current: %v", err)
	}

	rows, err := store.GetPendingExitOrders(ctx, 100)
	if err != nil {
		t.Fatalf("GetPendingExitOrders: %v", err)
	}
	seenCurrent := false
	for _, row := range rows {
		switch row.BrokerOrderID {
		case "brk-xrc-stale":
			t.Fatal("stale zero-fill terminal row leaked into exiting reconciliation")
		case "brk-xrc-current":
			seenCurrent = true
		}
	}
	if !seenCurrent {
		t.Fatal("current pending exit row missing from reconciliation")
	}
}

func TestGetPendingExitOrdersIncludesProtectiveOrderPredatingClaim(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("DATABASE_URL not set; skipping store integration test")
	}
	ctx := context.Background()
	tradeID, err := store.InsertTrade(ctx, store.Trade{
		Ticker: "TEST_XRC_PROTECTIVE_RACE", Direction: "long", Strategy: "test",
		EntryPrice: 100, Quantity: 10, Status: "open",
	})
	if err != nil {
		t.Fatalf("InsertTrade: %v", err)
	}
	orderID, err := store.InsertOrder(ctx, store.Order{
		TradeID: tradeID, BrokerOrderID: "brk-xrc-protective-race", Ticker: "TEST_XRC_PROTECTIVE_RACE",
		Side: "sell", OrderType: "stop", Quantity: 10, Status: "pending", IsStopLoss: true,
	})
	if err != nil {
		t.Fatalf("InsertOrder: %v", err)
	}
	if _, err := store.GetDB().Exec(ctx, `UPDATE orders SET created_at=NOW()-INTERVAL '10 minutes' WHERE id=$1`, orderID); err != nil {
		t.Fatalf("age protective order: %v", err)
	}
	if claimed, err := store.ClaimTradeExit(ctx, tradeID); err != nil || !claimed {
		t.Fatalf("ClaimTradeExit = %v, %v", claimed, err)
	}

	rows, err := store.GetPendingExitOrders(ctx, 100)
	if err != nil {
		t.Fatalf("GetPendingExitOrders: %v", err)
	}
	found := false
	for _, row := range rows {
		if row.BrokerOrderID == "brk-xrc-protective-race" {
			found = true
		}
	}
	if !found {
		t.Fatal("protective order predating the exit claim was not retryable")
	}
	if _, err := store.GetDB().Exec(ctx, `UPDATE trades SET exit_claimed_at=NOW()-INTERVAL '3 minutes' WHERE id=$1`, tradeID); err != nil {
		t.Fatalf("age claim: %v", err)
	}
	orderless, err := store.GetOrderlessExitingTrades(ctx, 100)
	if err != nil {
		t.Fatalf("GetOrderlessExitingTrades: %v", err)
	}
	foundOrderless := false
	for _, trade := range orderless {
		foundOrderless = foundOrderless || trade.ID == tradeID
	}
	if !foundOrderless {
		t.Fatal("pre-claim protective order suppressed orderless claim recovery")
	}
}

func TestGetPendingExitOrdersIncludesOnlyUnappliedPreClaimTerminalFill(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("DATABASE_URL not set; skipping store integration test")
	}
	ctx := context.Background()
	tradeID, err := store.InsertTrade(ctx, store.Trade{
		Ticker: "TEST_XRC_UNAPPLIED", Direction: "long", Strategy: "test",
		EntryPrice: 100, Quantity: 10, Status: "open",
	})
	if err != nil {
		t.Fatalf("InsertTrade: %v", err)
	}
	orderID, err := store.InsertOrder(ctx, store.Order{
		TradeID: tradeID, BrokerOrderID: "brk-xrc-unapplied", Ticker: "TEST_XRC_UNAPPLIED",
		Side: "sell", OrderType: "stop", Quantity: 10, Status: "cancelled",
		FilledPrice: 98, FilledQty: 2, IsStopLoss: true,
	})
	if err != nil {
		t.Fatalf("InsertOrder: %v", err)
	}
	if _, err := store.GetDB().Exec(ctx, `UPDATE orders SET created_at=NOW()-INTERVAL '10 minutes' WHERE id=$1`, orderID); err != nil {
		t.Fatalf("age terminal order: %v", err)
	}
	if claimed, err := store.ClaimTradeExit(ctx, tradeID); err != nil || !claimed {
		t.Fatalf("ClaimTradeExit = %v, %v", claimed, err)
	}
	contains := func() bool {
		rows, queryErr := store.GetPendingExitOrders(ctx, 100)
		if queryErr != nil {
			t.Fatalf("GetPendingExitOrders: %v", queryErr)
		}
		for _, row := range rows {
			if row.OrderID == orderID {
				return true
			}
		}
		return false
	}
	claimOwned := func() bool {
		owners, queryErr := store.GetDurableExitOrderOwnership(ctx)
		if queryErr != nil {
			t.Fatalf("GetDurableExitOrderOwnership: %v", queryErr)
		}
		for _, owner := range owners {
			if owner.BrokerOrderID == "brk-xrc-unapplied" {
				return owner.ClaimOwned
			}
		}
		return false
	}
	if !contains() {
		t.Fatal("pre-claim terminal row with unapplied fill was not sweepable")
	}
	if !claimOwned() {
		t.Fatal("unapplied pre-claim terminal fill did not protect orderless recovery")
	}
	if _, err := store.GetDB().Exec(ctx, `UPDATE orders SET exit_fill_applied_qty=filled_qty WHERE id=$1`, orderID); err != nil {
		t.Fatalf("mark fill applied: %v", err)
	}
	if contains() {
		t.Fatal("fully applied pre-claim terminal row remained sweepable")
	}
	if claimOwned() {
		t.Fatal("settled pre-claim terminal fill suppressed recovery of the current orderless claim")
	}
}

func TestProtectiveReplayWithoutNewFillPreservesCurrentExitIntent(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("DATABASE_URL not set; skipping store integration test")
	}
	ctx := context.Background()
	tradeID, err := store.InsertTrade(ctx, store.Trade{
		Ticker: "TEST_XRC_INTENT", Direction: "long", Strategy: "test",
		EntryPrice: 100, Quantity: 10, Status: "open",
	})
	if err != nil {
		t.Fatalf("InsertTrade: %v", err)
	}
	orderID, err := store.InsertOrder(ctx, store.Order{
		TradeID: tradeID, BrokerOrderID: "brk-xrc-intent-stop", Ticker: "TEST_XRC_INTENT",
		Side: "sell", OrderType: "stop", Quantity: 10, Status: "partial",
		StopPrice: 98, IsStopLoss: true,
	})
	if err != nil {
		t.Fatalf("InsertOrder: %v", err)
	}
	protective := store.ExitIntent{Reason: store.ExitReasonTrailingATR, Evidence: json.RawMessage(`{"stop":98}`)}
	if _, applied, err := store.ApplyProtectiveExitAtForBrokerOrder(
		ctx, tradeID, orderID, "brk-xrc-intent-stop", 98, 2, "partial", time.Now(), protective,
	); err != nil || applied != 2 {
		t.Fatalf("seed protective fill: applied=%.2f err=%v", applied, err)
	}
	software := store.ExitIntent{Reason: store.ExitReasonSellSignal, Evidence: json.RawMessage(`{"signal":"manual"}`)}
	if claimed, err := store.ClaimTradeExitWithIntent(ctx, tradeID, software); err != nil || !claimed {
		t.Fatalf("ClaimTradeExitWithIntent = %v, %v", claimed, err)
	}
	if _, applied, err := store.ApplyProtectiveExitAtForBrokerOrder(
		ctx, tradeID, orderID, "brk-xrc-intent-stop", 98, 2, "cancelled", time.Now(), protective,
	); err != nil || applied != 0 {
		t.Fatalf("replay protective fill: applied=%.2f err=%v", applied, err)
	}
	trade, err := store.GetTradeByID(ctx, tradeID)
	if err != nil || trade == nil {
		t.Fatalf("GetTradeByID: %v", err)
	}
	var gotEvidence, wantEvidence any
	if err := json.Unmarshal(trade.ExitEvidence, &gotEvidence); err != nil {
		t.Fatalf("decode stored exit evidence: %v", err)
	}
	if err := json.Unmarshal(software.Evidence, &wantEvidence); err != nil {
		t.Fatalf("decode expected exit evidence: %v", err)
	}
	if trade.ExitReason != software.Reason || !reflect.DeepEqual(gotEvidence, wantEvidence) {
		t.Fatalf("exit intent = %q %s, want software intent %q %s",
			trade.ExitReason, trade.ExitEvidence, software.Reason, software.Evidence)
	}
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

func TestApplyPartialExitCapsConcurrentProtectiveFillsToTradeQuantity(t *testing.T) {
	ctx := context.Background()
	tradeID, err := store.InsertTrade(ctx, store.Trade{
		Ticker: "TEST_XRC_OCO_CAP", Direction: "long", Strategy: "test",
		EntryPrice: 10, Quantity: 5,
	})
	if err != nil {
		t.Fatalf("InsertTrade: %v", err)
	}
	insertLeg := func(brokerID string) string {
		t.Helper()
		id, insertErr := store.InsertOrder(ctx, store.Order{
			TradeID: tradeID, BrokerOrderID: brokerID, Ticker: "TEST_XRC_OCO_CAP",
			Side: "sell", OrderType: "stop", Quantity: 5, Status: "cancelled",
			FilledPrice: 9.5, FilledQty: 5, IsStopLoss: true,
		})
		if insertErr != nil {
			t.Fatalf("InsertOrder %s: %v", brokerID, insertErr)
		}
		return id
	}
	firstID := insertLeg("brk-xrc-oco-1")
	secondID := insertLeg("brk-xrc-oco-2")
	if claimed, err := store.ClaimTradeExit(ctx, tradeID); err != nil || !claimed {
		t.Fatalf("ClaimTradeExit = %v, %v", claimed, err)
	}
	type result struct {
		remaining, applied float64
		err                error
	}
	results := make(chan result, 2)
	go func() {
		remaining, applied, applyErr := store.ApplyPartialExit(ctx, tradeID, firstID, 9.5, 5, "cancelled")
		results <- result{remaining: remaining, applied: applied, err: applyErr}
	}()
	go func() {
		remaining, applied, applyErr := store.ApplyPartialExit(ctx, tradeID, secondID, 9.5, 5, "cancelled")
		results <- result{remaining: remaining, applied: applied, err: applyErr}
	}()
	var totalApplied float64
	for range 2 {
		got := <-results
		if got.err != nil || got.remaining != 0 {
			t.Fatalf("concurrent OCO fill = remaining %v applied %v err %v, want remaining 0 and no error", got.remaining, got.applied, got.err)
		}
		totalApplied += got.applied
	}
	if totalApplied != 5 {
		t.Fatalf("concurrent OCO fills applied %.6f shares, want 5", totalApplied)
	}
	trade, err := store.GetTradeByID(ctx, tradeID)
	if err != nil || trade == nil {
		t.Fatalf("GetTradeByID: %v (nil=%v)", err, trade == nil)
	}
	if trade.Quantity != 0 {
		t.Fatalf("trade after dual OCO fills = qty %.6f, want 0", trade.Quantity)
	}
	final, err := store.FinalizeTradeExit(ctx, tradeID)
	if err != nil {
		t.Fatalf("FinalizeTradeExit: %v", err)
	}
	if final.ExitQty != 5 {
		t.Fatalf("finalized exit qty = %.6f, want 5", final.ExitQty)
	}
	orders, err := store.GetOrdersByTrade(ctx, tradeID)
	if err != nil {
		t.Fatalf("GetOrdersByTrade: %v", err)
	}
	seenSecond := false
	for _, order := range orders {
		if order.ID == secondID {
			seenSecond = true
			if order.FilledQty != 5 {
				t.Fatalf("second OCO audit fill = %.6f, want 5", order.FilledQty)
			}
		}
	}
	if !seenSecond {
		t.Fatal("second OCO order missing from audit")
	}
}

func TestApplyPartialExitRebasesRevisedCumulativeAverage(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name, direction string
		firstPrice      float64
		revisedPrice    float64
		wantPnL         float64
	}{
		{name: "long", direction: "long", firstPrice: 90, revisedPrice: 95, wantPnL: -20},
		{name: "short", direction: "short", firstPrice: 110, revisedPrice: 105, wantPnL: -20},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			exitSide := "sell"
			if tt.direction == "short" {
				exitSide = "buy"
			}
			tradeID, err := store.InsertTrade(ctx, store.Trade{
				Ticker: "TEST_XRC_REPRICE_" + tt.name, Direction: tt.direction,
				Strategy: "test", EntryPrice: 100, Quantity: 4,
			})
			if err != nil {
				t.Fatalf("InsertTrade: %v", err)
			}
			if claimed, err := store.ClaimTradeExit(ctx, tradeID); err != nil || !claimed {
				t.Fatalf("ClaimTradeExit = %v, %v", claimed, err)
			}
			orderID, err := store.InsertOrder(ctx, store.Order{
				TradeID: tradeID, BrokerOrderID: "brk-xrc-reprice-" + tt.name,
				Ticker: "TEST_XRC_REPRICE_" + tt.name, Side: exitSide,
				OrderType: "market", Quantity: 4, Status: "partial",
			})
			if err != nil {
				t.Fatalf("InsertOrder: %v", err)
			}

			remaining, applied, err := store.ApplyPartialExit(
				ctx, tradeID, orderID, tt.firstPrice, 4, "partial",
			)
			if err != nil || remaining != 0 || applied != 4 {
				t.Fatalf("first apply = remaining %v applied %v err %v, want 0/4/nil",
					remaining, applied, err)
			}
			remaining, applied, err = store.ApplyPartialExit(
				ctx, tradeID, orderID, tt.revisedPrice, 4, "filled",
			)
			if err != nil || remaining != 0 || applied != 0 {
				t.Fatalf("reprice = remaining %v applied %v err %v, want 0/0/nil",
					remaining, applied, err)
			}

			result, err := store.FinalizeTradeExit(ctx, tradeID)
			if err != nil {
				t.Fatalf("FinalizeTradeExit: %v", err)
			}
			if result.ExitQty != 4 || result.ExitPrice != tt.revisedPrice ||
				result.PnL != tt.wantPnL || result.PnLPct != -5 {
				t.Fatalf("repriced result = %+v, want qty=4 exit=%v pnl=%v pct=-5",
					result, tt.revisedPrice, tt.wantPnL)
			}
		})
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

func TestCloseTradeRealizedIncludesEarlierPartialExit(t *testing.T) {
	ctx := context.Background()
	tradeID, err := store.InsertTrade(ctx, store.Trade{
		Ticker: "TEST_XRC_CUMULATIVE", Direction: "long", Strategy: "test",
		EntryPrice: 100, Quantity: 10,
	})
	if err != nil {
		t.Fatalf("InsertTrade: %v", err)
	}
	orderID, err := store.InsertOrder(ctx, store.Order{
		TradeID: tradeID, BrokerOrderID: "brk-xrc-cumulative-partial",
		Ticker: "TEST_XRC_CUMULATIVE", Side: "sell", OrderType: "limit",
		Quantity: 4, LimitPrice: 110, Status: "partial",
	})
	if err != nil {
		t.Fatalf("InsertOrder: %v", err)
	}
	if remaining, applied, err := store.ApplyPartialExit(
		ctx, tradeID, orderID, 110, 4, "cancelled",
	); err != nil || remaining != 6 || applied != 4 {
		t.Fatalf("ApplyPartialExit = remaining %v applied %v err %v", remaining, applied, err)
	}

	result, err := store.CloseTradeRealized(ctx, tradeID, 90)
	if err != nil {
		t.Fatalf("CloseTradeRealized: %v", err)
	}
	if result.ExitQty != 10 || result.ExitPrice != 98 ||
		result.PnL != -20 || result.PnLPct != -2 {
		t.Fatalf("cumulative result = %+v, want qty=10 exit=98 pnl=-20 pct=-2", result)
	}
	trade, err := store.GetTradeByID(ctx, tradeID)
	if err != nil || trade == nil {
		t.Fatalf("GetTradeByID: %v", err)
	}
	if trade.Quantity != 10 || trade.ExitPrice != 98 ||
		trade.PnL != -20 || trade.PnLPct != -2 {
		t.Fatalf("closed trade = %+v", trade)
	}
}

func TestFinalizeTradeExitUsesEveryAppliedSlice(t *testing.T) {
	ctx := context.Background()
	tradeID, err := store.InsertTrade(ctx, store.Trade{
		Ticker: "TEST_XRC_FINALIZE", Direction: "long", Strategy: "test",
		EntryPrice: 100, Quantity: 10,
	})
	if err != nil {
		t.Fatalf("InsertTrade: %v", err)
	}
	if claimed, err := store.ClaimTradeExit(ctx, tradeID); err != nil || !claimed {
		t.Fatalf("ClaimTradeExit = %v, %v", claimed, err)
	}
	firstID, err := store.InsertOrder(ctx, store.Order{
		TradeID: tradeID, BrokerOrderID: "brk-xrc-finalize-1",
		Ticker: "TEST_XRC_FINALIZE", Side: "sell", OrderType: "limit",
		Quantity: 4, LimitPrice: 110, Status: "partial",
	})
	if err != nil {
		t.Fatalf("InsertOrder first: %v", err)
	}
	secondID, err := store.InsertOrder(ctx, store.Order{
		TradeID: tradeID, BrokerOrderID: "brk-xrc-finalize-2",
		Ticker: "TEST_XRC_FINALIZE", Side: "sell", OrderType: "stop",
		Quantity: 6, StopPrice: 90, Status: "pending", IsStopLoss: true,
	})
	if err != nil {
		t.Fatalf("InsertOrder second: %v", err)
	}
	if _, _, err := store.ApplyPartialExit(
		ctx, tradeID, firstID, 110, 4, "cancelled",
	); err != nil {
		t.Fatalf("ApplyPartialExit first: %v", err)
	}
	if _, _, err := store.ApplyPartialExit(
		ctx, tradeID, secondID, 90, 6, "filled",
	); err != nil {
		t.Fatalf("ApplyPartialExit second: %v", err)
	}

	result, err := store.FinalizeTradeExit(ctx, tradeID)
	if err != nil {
		t.Fatalf("FinalizeTradeExit: %v", err)
	}
	if result.ExitQty != 10 || result.ExitPrice != 98 ||
		result.PnL != -20 || result.PnLPct != -2 {
		t.Fatalf("finalized result = %+v, want qty=10 exit=98 pnl=-20 pct=-2", result)
	}
}

func TestFinalizeTradeExitPreservesReasonEvidenceAndBrokerFillTime(t *testing.T) {
	ctx := context.Background()
	tradeID, err := store.InsertTrade(ctx, store.Trade{
		Ticker: "TEST_XRC_META", Direction: "long", Strategy: "test",
		EntryPrice: 100, Quantity: 2,
	})
	if err != nil {
		t.Fatalf("InsertTrade: %v", err)
	}
	intent := store.ExitIntent{
		Reason:   store.ExitReasonEMA9Close,
		Evidence: []byte(`{"daily_close":95,"ema9":98}`),
	}
	if claimed, err := store.ClaimTradeExitWithIntent(ctx, tradeID, intent); err != nil || !claimed {
		t.Fatalf("ClaimTradeExitWithIntent = %v, %v", claimed, err)
	}
	orderID, err := store.InsertOrder(ctx, store.Order{
		TradeID: tradeID, BrokerOrderID: "brk-xrc-meta", Ticker: "TEST_XRC_META",
		Side: "sell", OrderType: "market", Quantity: 2, Status: "pending",
	})
	if err != nil {
		t.Fatalf("InsertOrder: %v", err)
	}
	filledAt := time.Date(2026, 8, 1, 14, 5, 6, 0, time.UTC)
	if _, _, err := store.ApplyPartialExitAt(ctx, tradeID, orderID, 95, 2, "filled", filledAt); err != nil {
		t.Fatalf("ApplyPartialExitAt: %v", err)
	}
	realized, err := store.FinalizeTradeExit(ctx, tradeID)
	if err != nil {
		t.Fatalf("FinalizeTradeExit: %v", err)
	}
	var evidence map[string]float64
	if err := json.Unmarshal(realized.ExitEvidence, &evidence); err != nil {
		t.Fatalf("unmarshal exit evidence: %v (%s)", err, realized.ExitEvidence)
	}
	if realized.ExitReason != store.ExitReasonEMA9Close || evidence["daily_close"] != 95 || evidence["ema9"] != 98 ||
		realized.ExitFilledAt == nil || !realized.ExitFilledAt.Equal(filledAt) || !realized.JustClosed {
		t.Fatalf("realized metadata = %+v evidence=%s", realized, realized.ExitEvidence)
	}
	trade, err := store.GetTradeByID(ctx, tradeID)
	if err != nil || trade == nil || trade.ClosedAt.IsZero() || trade.ExitFilledAt == nil || !trade.ExitFilledAt.Equal(filledAt) {
		t.Fatalf("trade timestamps = trade=%+v err=%v", trade, err)
	}
	if replay, err := store.FinalizeTradeExit(ctx, tradeID); err == nil || replay.JustClosed {
		t.Fatalf("replayed finalization = %+v err=%v, want no second close transition", replay, err)
	}
}

func TestStopReplacementAdoptsFillCommittedAfterSnapshot(t *testing.T) {
	ctx := context.Background()
	tradeID, err := store.InsertTrade(ctx, store.Trade{
		Ticker: "TEST_XRC_REPLACE_CAS", Direction: "long", Strategy: "test",
		EntryPrice: 100, Quantity: 5,
	})
	if err != nil {
		t.Fatalf("InsertTrade: %v", err)
	}
	orderID, err := store.InsertOrder(ctx, store.Order{
		TradeID: tradeID, BrokerOrderID: "replace-cas-old", Ticker: "TEST_XRC_REPLACE_CAS",
		Side: "sell", OrderType: "stop", Quantity: 5, StopPrice: 95,
		Status: "pending", IsStopLoss: true,
	})
	if err != nil {
		t.Fatalf("InsertOrder: %v", err)
	}
	if _, _, err := store.ApplyPartialExitAtForBrokerOrder(
		ctx, tradeID, orderID, "replace-cas-old", 94, 2, "partial", time.Now(),
	); err != nil {
		t.Fatalf("ApplyPartialExitAtForBrokerOrder: %v", err)
	}
	err = store.UpdateOrderStopReplacement(
		ctx, orderID, "replace-cas-old", "replace-cas-new", 0, 5,
		96, 5, "pending", 0, 0, time.Time{}, store.ExitIntent{Reason: store.ExitReasonStopLoss},
	)
	if err != nil {
		t.Fatalf("UpdateOrderStopReplacement after concurrent fill: %v", err)
	}
	trade, err := store.GetTradeByID(ctx, tradeID)
	if err != nil || trade == nil || trade.Quantity != 3 {
		t.Fatalf("trade after adopted replacement = %+v err=%v; want quantity 3", trade, err)
	}
	orders, err := store.GetOrdersByTrade(ctx, tradeID)
	if err != nil || len(orders) != 1 || orders[0].BrokerOrderID != "replace-cas-new" ||
		orders[0].Quantity != 3 || orders[0].FilledQty != 0 || orders[0].Status != "pending" {
		t.Fatalf("order after adopted replacement = %+v err=%v", orders, err)
	}
}

func TestStopReplacementAdoptsAppliedDurableFillAfterSnapshot(t *testing.T) {
	ctx := context.Background()
	tradeID, err := store.InsertTrade(ctx, store.Trade{
		Ticker: "TEST_XRC_APPLIED_CAS", Direction: "long", Strategy: "test",
		EntryPrice: 100, Quantity: 5,
	})
	if err != nil {
		t.Fatalf("InsertTrade: %v", err)
	}
	orderID, err := store.InsertOrder(ctx, store.Order{
		TradeID: tradeID, BrokerOrderID: "applied-cas-old", Ticker: "TEST_XRC_APPLIED_CAS",
		Side: "sell", OrderType: "stop", Quantity: 5, StopPrice: 95,
		FilledPrice: 94, FilledQty: 2, Status: "partial", IsStopLoss: true,
	})
	if err != nil {
		t.Fatalf("InsertOrder: %v", err)
	}
	// The replacement snapshot sees filled_qty=2 and trade quantity=5. A
	// reconciler then applies that already-recorded fill without changing
	// filled_qty, so only the locked trade-quantity guard can detect the race.
	if _, _, err := store.ApplyPartialExitAtForBrokerOrder(
		ctx, tradeID, orderID, "applied-cas-old", 94, 2, "partial", time.Now(),
	); err != nil {
		t.Fatalf("ApplyPartialExitAtForBrokerOrder: %v", err)
	}
	err = store.UpdateOrderStopReplacement(
		ctx, orderID, "applied-cas-old", "applied-cas-new", 2, 5,
		96, 5, "pending", 0, 0, time.Time{}, store.ExitIntent{Reason: store.ExitReasonStopLoss},
	)
	if err != nil {
		t.Fatalf("UpdateOrderStopReplacement after applied durable fill: %v", err)
	}
	trade, err := store.GetTradeByID(ctx, tradeID)
	if err != nil || trade == nil || trade.Quantity != 3 {
		t.Fatalf("trade after adopted replacement = %+v err=%v; want quantity 3", trade, err)
	}
	orders, err := store.GetOrdersByTrade(ctx, tradeID)
	if err != nil || len(orders) != 1 || orders[0].BrokerOrderID != "applied-cas-new" ||
		orders[0].Quantity != 3 || orders[0].FilledQty != 0 || orders[0].Status != "pending" {
		t.Fatalf("order after adopted applied-fill replacement = %+v err=%v", orders, err)
	}
}

func TestStopReplacementMatchesNullBrokerIDSnapshot(t *testing.T) {
	ctx := context.Background()
	tradeID, err := store.InsertTrade(ctx, store.Trade{
		Ticker: "TEST_XRC_REPLACE_NULL", Direction: "long", Strategy: "test",
		EntryPrice: 100, Quantity: 5,
	})
	if err != nil {
		t.Fatalf("InsertTrade: %v", err)
	}
	orderID, err := store.InsertOrder(ctx, store.Order{
		TradeID: tradeID, Ticker: "TEST_XRC_REPLACE_NULL", Side: "sell",
		OrderType: "stop", Quantity: 5, StopPrice: 95, Status: "pending", IsStopLoss: true,
	})
	if err != nil {
		t.Fatalf("InsertOrder: %v", err)
	}
	if err := store.UpdateOrderStopReplacement(
		ctx, orderID, "", "replace-null-new", 0, 5,
		96, 5, "pending", 0, 0, time.Time{}, store.ExitIntent{Reason: store.ExitReasonStopLoss},
	); err != nil {
		t.Fatalf("UpdateOrderStopReplacement(NULL broker snapshot): %v", err)
	}
	orders, err := store.GetOrdersByTrade(ctx, tradeID)
	if err != nil || len(orders) != 1 || orders[0].BrokerOrderID != "replace-null-new" {
		t.Fatalf("replacement order = %+v err=%v", orders, err)
	}
}

func TestApplyPartialExitAtKeepsLatestBrokerTimestamp(t *testing.T) {
	ctx := context.Background()
	tradeID, err := store.InsertTrade(ctx, store.Trade{
		Ticker: "TEST_XRC_TIME", Direction: "long", Strategy: "test",
		EntryPrice: 100, Quantity: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if claimed, err := store.ClaimTradeExitWithIntent(ctx, tradeID, store.ExitIntent{Reason: store.ExitReasonSellSignal}); err != nil || !claimed {
		t.Fatalf("claim = %v, %v", claimed, err)
	}
	orderID, err := store.InsertOrder(ctx, store.Order{
		TradeID: tradeID, BrokerOrderID: "brk-xrc-time", Ticker: "TEST_XRC_TIME",
		Side: "sell", OrderType: "market", Quantity: 2, Status: "partial",
	})
	if err != nil {
		t.Fatal(err)
	}
	later := time.Date(2026, 8, 1, 14, 5, 2, 0, time.UTC)
	earlier := later.Add(-time.Minute)
	latest := later.Add(time.Minute)
	if _, _, err := store.ApplyPartialExitAt(ctx, tradeID, orderID, 99, 1, "partial", later); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.ApplyPartialExitAt(ctx, tradeID, orderID, 99, 1, "partial", earlier); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.ApplyPartialExitAt(ctx, tradeID, orderID, 98, 2, "filled", latest); err != nil {
		t.Fatal(err)
	}
	orders, err := store.GetOrdersByTrade(ctx, tradeID)
	if err != nil || len(orders) != 1 || orders[0].FilledAt == nil || !orders[0].FilledAt.Equal(latest) {
		t.Fatalf("order fill time = %+v err=%v, want %v", orders, err, latest)
	}
	realized, err := store.FinalizeTradeExit(ctx, tradeID)
	if err != nil || realized.ExitFilledAt == nil || !realized.ExitFilledAt.Equal(latest) {
		t.Fatalf("realized fill time = %+v err=%v, want %v", realized, err, latest)
	}
}

func TestClaimTradeExitRejectsInvalidEvidenceWithoutClaiming(t *testing.T) {
	ctx := context.Background()
	tradeID, err := store.InsertTrade(ctx, store.Trade{
		Ticker: "TEST_XRC_BAD_JSON", Direction: "long", Strategy: "test",
		EntryPrice: 100, Quantity: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if claimed, err := store.ClaimTradeExitWithIntent(ctx, tradeID, store.ExitIntent{
		Reason: store.ExitReasonEMA9Close, Evidence: []byte(`{"broken":`),
	}); err == nil || claimed {
		t.Fatalf("invalid evidence claim = %v, %v; want false/error", claimed, err)
	}
	trade, err := store.GetTradeByID(ctx, tradeID)
	if err != nil || trade == nil || trade.Status != "open" || trade.ExitReason != "" {
		t.Fatalf("invalid evidence mutated trade: %+v err=%v", trade, err)
	}
}

func TestApplyRecoveredExitAccumulatesAndReopensRemainder(t *testing.T) {
	ctx := context.Background()
	tradeID, err := store.InsertTrade(ctx, store.Trade{
		Ticker: "TEST_XRC_RECOVER_REALIZED", Direction: "short", Strategy: "test",
		EntryPrice: 100, Quantity: 10,
	})
	if err != nil {
		t.Fatalf("InsertTrade: %v", err)
	}
	if claimed, err := store.ClaimTradeExit(ctx, tradeID); err != nil || !claimed {
		t.Fatalf("ClaimTradeExit = %v, %v", claimed, err)
	}

	partial, err := store.ApplyRecoveredExit(ctx, tradeID, 90, 4)
	if err != nil {
		t.Fatalf("ApplyRecoveredExit partial: %v", err)
	}
	if partial.Status != "open" || partial.Remaining != 6 ||
		partial.ExitQty != 4 || partial.ExitPrice != 90 ||
		partial.PnL != 40 || partial.PnLPct != 10 {
		t.Fatalf("partial recovery = %+v", partial)
	}
	if claimed, err := store.ClaimTradeExit(ctx, tradeID); err != nil || !claimed {
		t.Fatalf("second ClaimTradeExit = %v, %v", claimed, err)
	}
	closed, err := store.ApplyRecoveredExit(ctx, tradeID, 110, 6)
	if err != nil {
		t.Fatalf("ApplyRecoveredExit final: %v", err)
	}
	if closed.Status != "closed" || closed.ExitQty != 10 ||
		closed.ExitPrice != 102 || closed.PnL != -20 || closed.PnLPct != -2 {
		t.Fatalf("closed recovery = %+v", closed)
	}
	if !closed.JustClosed || closed.ExitReason != store.ExitReasonRecovery {
		t.Fatalf("closed recovery provenance = %+v", closed)
	}
	var evidence map[string]any
	if err := json.Unmarshal(closed.ExitEvidence, &evidence); err != nil {
		t.Fatalf("decode recovery evidence: %v", err)
	}
	if evidence["source"] != "broker_position_delta" || evidence["recovered_quantity"] != float64(6) {
		t.Fatalf("recovery evidence = %#v", evidence)
	}
}

func TestApplyRecoveredExitPreservesExistingPolicyIntent(t *testing.T) {
	ctx := context.Background()
	tradeID, err := store.InsertTrade(ctx, store.Trade{
		Ticker: "TEST_XRC_RECOVER_INTENT", Direction: "long", Strategy: "test",
		EntryPrice: 100, Quantity: 3,
	})
	if err != nil {
		t.Fatalf("InsertTrade: %v", err)
	}
	intent := store.ExitIntent{
		Reason: store.ExitReasonEMA9Close, Evidence: json.RawMessage(`{"daily_close":95}`),
	}
	if claimed, err := store.ClaimTradeExitWithIntent(ctx, tradeID, intent); err != nil || !claimed {
		t.Fatalf("ClaimTradeExitWithIntent = %v, %v", claimed, err)
	}
	recovered, err := store.ApplyRecoveredExit(ctx, tradeID, 95, 3)
	if err != nil {
		t.Fatalf("ApplyRecoveredExit: %v", err)
	}
	var gotEvidence, wantEvidence map[string]any
	gotErr := json.Unmarshal(recovered.ExitEvidence, &gotEvidence)
	wantErr := json.Unmarshal(intent.Evidence, &wantEvidence)
	if recovered.ExitReason != intent.Reason || gotErr != nil || wantErr != nil ||
		!reflect.DeepEqual(gotEvidence, wantEvidence) {
		t.Fatalf("recovered intent = %q %#v, want %q %#v",
			recovered.ExitReason, gotEvidence, intent.Reason, wantEvidence)
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
	result, err := store.CloseTradeRealized(ctx, tradeID, 11)
	if err != nil {
		t.Fatalf("CloseTradeRealized: %v", err)
	}
	if result.ExitQty != 5 || result.ExitPrice != 10.7 ||
		result.PnL != 2.5 || math.Abs(result.PnLPct-4.902) > 1e-8 {
		t.Fatalf("rebased cumulative result = %+v", result)
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
				owner.TradeStatus != "exiting" || !owner.ClaimOwned ||
				owner.TradeID != tradeID || owner.TradeQuantity != 5 || owner.ExitClaimedAt == nil {
				t.Fatalf("owner = %+v", owner)
			}
			return
		}
	}
	t.Fatal("durable exiting order missing from complete ownership snapshot")
}
