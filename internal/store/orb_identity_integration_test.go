package store_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/store"
)

func TestORBDurableIdentitySurvivesLifecycleReaders(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("DATABASE_URL not set; skipping store integration test")
	}
	ctx := context.Background()
	db := store.GetDB()
	date := time.Now().UTC()
	name := fmt.Sprintf("futures-identity-%d", time.Now().UnixNano())

	sess, err := store.CreateORBSessionOwned(ctx, db, date, name, "futures", "futures_index_orb")
	if err != nil {
		t.Fatalf("CreateORBSessionOwned: %v", err)
	}
	assertSessionIdentity(t, *sess)

	byDate, err := store.GetORBSessionByDate(ctx, db, date, name)
	if err != nil || byDate == nil {
		t.Fatalf("GetORBSessionByDate: session=%v err=%v", byDate, err)
	}
	assertSessionIdentity(t, *byDate)

	recentSessions, err := store.GetRecentORBSessions(ctx, db, 1000)
	if err != nil {
		t.Fatalf("GetRecentORBSessions: %v", err)
	}
	foundSession := false
	for _, got := range recentSessions {
		if got.ID == sess.ID {
			assertSessionIdentity(t, got)
			foundSession = true
			break
		}
	}
	if !foundSession {
		t.Fatalf("recent session reader did not return session %s", sess.ID)
	}

	newTrade := func(status, suffix string) (string, string) {
		t.Helper()
		maxHold := 240
		orderID := fmt.Sprintf("identity-%s-%d", suffix, time.Now().UnixNano())
		id, err := store.CreateORBTrade(ctx, db, store.ORBTrade{
			SessionID: sess.ID, Ticker: "MYM", Side: "buy", Qty: 2,
			EntryStop: 52874, StopLoss: 52567, TakeProfit: 53500,
			OrderID: orderID, Status: status, AssetClass: "futures",
			OwnerScope: "futures_index_orb", ConID: "815824200", PointValue: 0.5, MaxHoldMinutes: &maxHold,
		})
		if err != nil {
			t.Fatalf("CreateORBTrade(%s): %v", suffix, err)
		}
		return id, orderID
	}

	pendingID, pendingOrderID := newTrade("pending", "pending")
	assertTradeInRows(t, pendingID, mustPendingTrades(t, ctx), "pending reader")
	if err := store.CancelORBTradeByOrderID(ctx, db, pendingOrderID); err != nil {
		t.Fatalf("CancelORBTradeByOrderID: %v", err)
	}
	assertTradeInRows(t, pendingID, mustCancelledTrades(t, ctx), "cancelled reader")

	filledID, filledOrderID := newTrade("pending", "filled")
	if ok, err := store.FillORBTradeByOrderID(ctx, db, filledOrderID, 52874); err != nil || !ok {
		t.Fatalf("FillORBTradeByOrderID: updated=%v err=%v", ok, err)
	}
	assertTradeInRows(t, filledID, mustFilledTrades(t, ctx), "filled reader")

	byID, err := store.GetORBTradeByID(ctx, db, filledID)
	if err != nil || byID == nil {
		t.Fatalf("GetORBTradeByID: trade=%v err=%v", byID, err)
	}
	assertTradeIdentity(t, *byID, "trade-by-id reader")

	recentTrades, err := store.GetRecentORBTrades(ctx, db, 1000)
	if err != nil {
		t.Fatalf("GetRecentORBTrades: %v", err)
	}
	assertTradeInRows(t, filledID, recentTrades, "recent reader")

	sessionTrades, err := store.GetORBTrades(ctx, db, sess.ID)
	if err != nil {
		t.Fatalf("GetORBTrades: %v", err)
	}
	assertTradeInRows(t, filledID, sessionTrades, "session-scoped reader")

	closedAt := time.Now().UTC()
	if err := store.CloseORBTradeAt(ctx, db, filledID, "broker_exit", "broker", 53000, 126, closedAt); err != nil {
		t.Fatalf("CloseORBTradeAt: %v", err)
	}
	closedTrades, err := store.GetClosedORBTradesByDateRange(ctx, db, closedAt.Add(-time.Minute), closedAt.Add(time.Minute))
	if err != nil {
		t.Fatalf("GetClosedORBTradesByDateRange: %v", err)
	}
	assertTradeInRows(t, filledID, closedTrades, "closed ledger reader")
}

func TestMigration079EnforcesIdentityConstraints(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("DATABASE_URL not set; skipping store integration test")
	}
	ctx := context.Background()
	db := store.GetDB()
	sess, err := store.CreateORBSessionOwned(ctx, db, time.Now().UTC(), fmt.Sprintf("migration-079-%d", time.Now().UnixNano()), "futures", "futures_index_orb")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	tradeID, err := store.CreateORBTrade(ctx, db, store.ORBTrade{SessionID: sess.ID, Ticker: "MYM", Side: "buy", Qty: 1, Status: "filled", AssetClass: "futures", OwnerScope: "futures_index_orb", PointValue: 0.5})
	if err != nil {
		t.Fatalf("create trade: %v", err)
	}
	if _, err := store.CreateORBTrade(ctx, db, store.ORBTrade{SessionID: sess.ID, Ticker: "UNKNOWN", Side: "buy", Qty: 1, Status: "pending", AssetClass: "futures", PointValue: 1}); err == nil {
		t.Fatal("futures trade without explicit launch owner was accepted")
	}

	if _, err := db.Exec(ctx, `UPDATE orb_trades SET owner_scope='unknown' WHERE id=$1`, tradeID); err == nil {
		t.Fatal("orb_trades owner_scope constraint accepted an unknown owner")
	}
	if _, err := db.Exec(ctx, `UPDATE orb_trades SET point_value=-0.5 WHERE id=$1`, tradeID); err == nil {
		t.Fatal("orb_trades point_value constraint accepted a negative value")
	}
	closed, err := store.CloseORBTradeIfFilled(ctx, db, tradeID, "broker_exit", "broker", 53000, 63)
	if err != nil || !closed {
		t.Fatalf("first durable close: closed=%v err=%v", closed, err)
	}
	closed, err = store.CloseORBTradeIfFilled(ctx, db, tradeID, "broker_exit", "broker", 53001, 64)
	if err != nil || closed {
		t.Fatalf("second durable close must lose CAS: closed=%v err=%v", closed, err)
	}
}

func TestFuturesOwnerSessionsCoexistAndLookupIndependently(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("DATABASE_URL not set; skipping store integration test")
	}
	ctx := context.Background()
	db := store.GetDB()
	date := time.Now().UTC().AddDate(10, 0, 0)
	index, err := store.CreateORBSessionOwned(ctx, db, date, "futures", "futures", "futures_index_orb")
	if err != nil {
		t.Fatalf("create index session: %v", err)
	}
	metals, err := store.CreateORBSessionOwned(ctx, db, date, "futures", "futures", "futures_metals_orb")
	if err != nil {
		t.Fatalf("create metals session: %v", err)
	}
	gotIndex, err := store.GetORBSessionByDateOwned(ctx, db, date, "futures", "futures_index_orb")
	if err != nil || gotIndex == nil || gotIndex.ID != index.ID {
		t.Fatalf("index lookup: got=%v err=%v", gotIndex, err)
	}
	gotMetals, err := store.GetORBSessionByDateOwned(ctx, db, date, "futures", "futures_metals_orb")
	if err != nil || gotMetals == nil || gotMetals.ID != metals.ID {
		t.Fatalf("metals lookup: got=%v err=%v", gotMetals, err)
	}

	oldDate := date.AddDate(0, 0, -1)
	oldIndex, err := store.CreateORBSessionOwned(ctx, db, oldDate, "futures", "futures", "futures_index_orb")
	if err != nil {
		t.Fatalf("create old index session: %v", err)
	}
	oldMetals, err := store.CreateORBSessionOwned(ctx, db, oldDate, "futures", "futures", "futures_metals_orb")
	if err != nil {
		t.Fatalf("create old metals session: %v", err)
	}
	if err := store.UpdateORBSession(ctx, db, oldIndex.ID, "scanning", nil, nil); err != nil {
		t.Fatalf("mark old index session scanning: %v", err)
	}
	if err := store.UpdateORBSession(ctx, db, oldMetals.ID, "scanning", nil, nil); err != nil {
		t.Fatalf("mark old metals session scanning: %v", err)
	}
	indexTrade, err := store.CreateORBTrade(ctx, db, store.ORBTrade{SessionID: oldIndex.ID, Ticker: "MYM", Side: "buy", Qty: 1, Status: "pending", AssetClass: "futures", OwnerScope: "futures_index_orb", PointValue: 0.5})
	if err != nil {
		t.Fatalf("create old index trade: %v", err)
	}
	metalsTrade, err := store.CreateORBTrade(ctx, db, store.ORBTrade{SessionID: oldMetals.ID, Ticker: "MGC", Side: "buy", Qty: 1, Status: "pending", AssetClass: "futures", OwnerScope: "futures_metals_orb", PointValue: 10})
	if err != nil {
		t.Fatalf("create old metals trade: %v", err)
	}
	if err := store.MarkStaleORBSessionsDoneOwned(ctx, db, date, "futures", "futures_index_orb"); err != nil {
		t.Fatalf("owned stale sweep: %v", err)
	}
	gotIndexTrade, err := store.GetORBTradeByID(ctx, db, indexTrade)
	if err != nil || gotIndexTrade == nil || gotIndexTrade.Status != "expired" {
		t.Fatalf("index stale trade: got=%v err=%v", gotIndexTrade, err)
	}
	gotMetalsTrade, err := store.GetORBTradeByID(ctx, db, metalsTrade)
	if err != nil || gotMetalsTrade == nil || gotMetalsTrade.Status != "pending" {
		t.Fatalf("metals trade was swept by index owner: got=%v err=%v", gotMetalsTrade, err)
	}
}

func mustPendingTrades(t *testing.T, ctx context.Context) []store.ORBTrade {
	t.Helper()
	rows, err := store.GetOpenPendingORBTrades(ctx, store.GetDB())
	if err != nil {
		t.Fatalf("GetOpenPendingORBTrades: %v", err)
	}
	return rows
}

func mustFilledTrades(t *testing.T, ctx context.Context) []store.ORBTrade {
	t.Helper()
	rows, err := store.GetOpenFilledORBTrades(ctx, store.GetDB())
	if err != nil {
		t.Fatalf("GetOpenFilledORBTrades: %v", err)
	}
	return rows
}

func mustCancelledTrades(t *testing.T, ctx context.Context) []store.ORBTrade {
	t.Helper()
	rows, err := store.GetRecentlyCancelledORBTrades(ctx, store.GetDB(), time.Now().UTC().Add(-time.Minute))
	if err != nil {
		t.Fatalf("GetRecentlyCancelledORBTrades: %v", err)
	}
	return rows
}

func assertSessionIdentity(t *testing.T, got store.ORBSession) {
	t.Helper()
	if got.AssetClass != "futures" || got.OwnerScope != "futures_index_orb" {
		t.Fatalf("session reader lost durable identity: asset_class=%q owner_scope=%q", got.AssetClass, got.OwnerScope)
	}
}

func assertTradeInRows(t *testing.T, id string, rows []store.ORBTrade, reader string) {
	t.Helper()
	for _, got := range rows {
		if got.ID == id {
			assertTradeIdentity(t, got, reader)
			return
		}
	}
	t.Fatalf("%s did not return trade %s", reader, id)
}

func assertTradeIdentity(t *testing.T, got store.ORBTrade, reader string) {
	t.Helper()
	if got.AssetClass != "futures" || got.OwnerScope != "futures_index_orb" ||
		got.ConID != "815824200" || got.PointValue != 0.5 || got.MaxHoldMinutes == nil || *got.MaxHoldMinutes != 240 {
		t.Fatalf("%s lost durable identity: asset_class=%q owner_scope=%q conid=%q point_value=%v",
			reader, got.AssetClass, got.OwnerScope, got.ConID, got.PointValue)
	}
}
