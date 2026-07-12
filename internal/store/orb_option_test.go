package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/store"
)

// TestORBTrade_OptionRoundTrip verifies that an option-contract ORB trade
// persists and reads back with full instrument identity (Issue #410, migration
// 039), and that a plain equity trade defaults to asset_class='equity'/
// multiplier=1 — the zero-regression guarantee for existing paths.
func TestORBTrade_OptionRoundTrip(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("DATABASE_URL not set; skipping store integration test")
	}
	ctx := context.Background()
	db := store.GetDB()

	// Dedicated session name: the test DB is shared and orb_sessions has a unique
	// (date, session_name) constraint, so each test must use a distinct name.
	sess, err := store.CreateORBSession(ctx, db, time.Now(), "opt-roundtrip")
	if err != nil {
		t.Fatalf("CreateORBSession: %v", err)
	}

	expiry := time.Date(2026, 7, 17, 0, 0, 0, 0, time.UTC)
	optID, err := store.CreateORBTrade(ctx, db, store.ORBTrade{
		SessionID:  sess.ID,
		Ticker:     "SPY",
		Side:       "buy",
		EntryStop:  550.10,
		StopLoss:   548.00,
		TakeProfit: 555.00,
		Qty:        2,
		Status:     "filled",
		AssetClass: "option",
		Underlying: "SPY",
		Expiry:     &expiry,
		Strike:     551.0,
		Right:      "C",
		OCCSymbol:  "SPY260717C00551000",
		Multiplier: 100,
	})
	if err != nil {
		t.Fatalf("CreateORBTrade (option): %v", err)
	}

	got, err := store.GetORBTradeByID(ctx, db, optID)
	if err != nil {
		t.Fatalf("GetORBTradeByID: %v", err)
	}
	if got == nil {
		t.Fatal("GetORBTradeByID returned nil for an existing trade")
	}
	if got.AssetClass != "option" {
		t.Errorf("AssetClass = %q; want option", got.AssetClass)
	}
	if got.Underlying != "SPY" {
		t.Errorf("Underlying = %q; want SPY", got.Underlying)
	}
	if got.Expiry == nil || !got.Expiry.Equal(expiry) {
		t.Errorf("Expiry = %v; want %v", got.Expiry, expiry)
	}
	if got.Strike != 551.0 {
		t.Errorf("Strike = %v; want 551", got.Strike)
	}
	if got.Right != "C" {
		t.Errorf("Right = %q; want C", got.Right)
	}
	if got.OCCSymbol != "SPY260717C00551000" {
		t.Errorf("OCCSymbol = %q; want SPY260717C00551000", got.OCCSymbol)
	}
	if got.Multiplier != 100 {
		t.Errorf("Multiplier = %d; want 100", got.Multiplier)
	}

	// A plain equity trade must default cleanly — nothing option-specific set.
	eqID, err := store.CreateORBTrade(ctx, db, store.ORBTrade{
		SessionID: sess.ID, Ticker: "AAPL", Side: "buy",
		EntryStop: 200.0, StopLoss: 198.0, TakeProfit: 206.0, Qty: 10, Status: "filled",
	})
	if err != nil {
		t.Fatalf("CreateORBTrade (equity): %v", err)
	}
	eq, err := store.GetORBTradeByID(ctx, db, eqID)
	if err != nil {
		t.Fatalf("GetORBTradeByID (equity): %v", err)
	}
	if eq.AssetClass != "equity" {
		t.Errorf("equity AssetClass = %q; want equity", eq.AssetClass)
	}
	if eq.Multiplier != 1 {
		t.Errorf("equity Multiplier = %d; want 1", eq.Multiplier)
	}
	if eq.OCCSymbol != "" || eq.Underlying != "" || eq.Expiry != nil || eq.Strike != 0 || eq.Right != "" {
		t.Errorf("equity trade unexpectedly has option fields set: %+v", eq)
	}
}

// TestGetOpenFilledOptionTrades verifies the dedicated reader returns filled
// option trades with full contract identity + underlying levels (used by the
// #415 exit watcher), and excludes non-option and non-filled rows.
func TestGetOpenFilledOptionTrades(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("DATABASE_URL not set; skipping store integration test")
	}
	ctx := context.Background()
	db := store.GetDB()
	sess, err := store.CreateORBSession(ctx, db, time.Now(), "opt-openfilled")
	if err != nil {
		t.Fatalf("CreateORBSession: %v", err)
	}
	exp := time.Date(2026, 7, 17, 0, 0, 0, 0, time.UTC)

	// An option entry, created pending then filled — mirroring how the router
	// records it (fill_price is set by the entry fill-poll, not at creation).
	occ := "MGDQTY261218C00100000"
	optID, err := store.CreateORBTrade(ctx, db, store.ORBTrade{
		SessionID: sess.ID, Ticker: "SPY", Side: "buy", OrderID: "opt-e1",
		EntryStop: 550.10, StopLoss: 548.00, TakeProfit: 555.00, Qty: 3, Status: "pending",
		AssetClass: "option", Underlying: "SPY", Expiry: &exp,
		Strike: 551, Right: "C", OCCSymbol: occ, Multiplier: 100,
	})
	if err != nil {
		t.Fatalf("create option trade: %v", err)
	}
	if _, err := store.FillORBTradeByOrderID(ctx, db, "opt-e1", 1.25); err != nil {
		t.Fatalf("fill option trade: %v", err)
	}
	// A filled equity (must be excluded) and a pending option (must be excluded).
	_, _ = store.CreateORBTrade(ctx, db, store.ORBTrade{SessionID: sess.ID, Ticker: "AAPL", Side: "buy", Qty: 10, Status: "filled"})
	_, _ = store.CreateORBTrade(ctx, db, store.ORBTrade{SessionID: sess.ID, Ticker: "QQQ", Side: "buy", Qty: 1, Status: "pending", AssetClass: "option", OCCSymbol: "QQQ260717C00500000"})

	trades, err := store.GetOpenFilledOptionTrades(ctx, db)
	if err != nil {
		t.Fatalf("GetOpenFilledOptionTrades: %v", err)
	}
	var got *store.ORBTrade
	for i := range trades {
		if trades[i].ID == optID {
			got = &trades[i]
		}
		if trades[i].AssetClass != "option" {
			t.Errorf("reader returned a non-option row: %+v", trades[i])
		}
		if trades[i].Status != "filled" {
			t.Errorf("reader returned a non-filled row: %+v", trades[i])
		}
	}
	if got == nil {
		t.Fatal("filled option trade not returned")
	}
	if got.OCCSymbol != occ || got.Underlying != "SPY" || got.Strike != 551 || got.Right != "C" || got.Multiplier != 100 {
		t.Errorf("contract identity = %+v", got)
	}
	if got.StopLoss != 548.00 || got.TakeProfit != 555.00 || got.FillPrice != 1.25 {
		t.Errorf("underlying levels/fill not read: stop=%v tp=%v fill=%v", got.StopLoss, got.TakeProfit, got.FillPrice)
	}
}

// TestOptionReconcileScoping verifies the #416 scope-out plumbing: the managed-OCC
// query returns pending+filled option OCCs, and the shared GetOpenFilledORBTrades
// reader EXCLUDES option rows (they have their own reader + exit path).
func TestOptionReconcileScoping(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("DATABASE_URL not set; skipping store integration test")
	}
	ctx := context.Background()
	db := store.GetDB()
	sess, err := store.CreateORBSession(ctx, db, time.Now(), "opt-reconcile-scope")
	if err != nil {
		t.Fatalf("CreateORBSession: %v", err)
	}
	exp := time.Date(2026, 7, 17, 0, 0, 0, 0, time.UTC)
	filledOCC := "SPY260717C00551000"
	pendingOCC := "QQQ260717P00500000"

	filledOptID, _ := store.CreateORBTrade(ctx, db, store.ORBTrade{
		SessionID: sess.ID, Ticker: "SPY", Side: "buy", Status: "filled", Qty: 2,
		AssetClass: "option", Underlying: "SPY", Expiry: &exp, Strike: 551, Right: "C",
		OCCSymbol: filledOCC, Multiplier: 100,
	})
	_, _ = store.CreateORBTrade(ctx, db, store.ORBTrade{
		SessionID: sess.ID, Ticker: "QQQ", Side: "sell", Status: "pending", Qty: 1,
		AssetClass: "option", Underlying: "QQQ", Expiry: &exp, Strike: 500, Right: "P",
		OCCSymbol: pendingOCC, Multiplier: 100,
	})
	eqID, _ := store.CreateORBTrade(ctx, db, store.ORBTrade{
		SessionID: sess.ID, Ticker: "AAPLSCOPE", Side: "buy", Status: "filled", Qty: 10,
	})

	// Managed set includes both option OCCs (pending + filled).
	managed, err := store.GetManagedOptionOCCSymbols(ctx, db)
	if err != nil {
		t.Fatalf("GetManagedOptionOCCSymbols: %v", err)
	}
	if !managed[filledOCC] || !managed[pendingOCC] {
		t.Errorf("managed set missing OCCs: %v", managed)
	}

	// GetOpenFilledORBTrades excludes the option row, includes the equity row.
	filled, err := store.GetOpenFilledORBTrades(ctx, db)
	if err != nil {
		t.Fatalf("GetOpenFilledORBTrades: %v", err)
	}
	var sawOption, sawEquity bool
	for _, tr := range filled {
		if tr.ID == filledOptID {
			sawOption = true
		}
		if tr.ID == eqID {
			sawEquity = true
		}
	}
	if sawOption {
		t.Error("GetOpenFilledORBTrades must exclude option rows")
	}
	if !sawEquity {
		t.Error("GetOpenFilledORBTrades must still return equity rows")
	}
}

// TestGetORBTrades_CarriesOptionIdentity verifies the session-scoped TUI reader
// now surfaces option identity (asset_class/occ_symbol/right/strike) while leaving
// equity rows as plain 'equity'.
func TestGetORBTrades_CarriesOptionIdentity(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("DATABASE_URL not set; skipping store integration test")
	}
	ctx := context.Background()
	db := store.GetDB()
	sess, err := store.CreateORBSession(ctx, db, time.Now(), "opt-getorbtrades")
	if err != nil {
		t.Fatalf("CreateORBSession: %v", err)
	}
	exp := time.Date(2026, 7, 17, 0, 0, 0, 0, time.UTC)
	_, _ = store.CreateORBTrade(ctx, db, store.ORBTrade{
		SessionID: sess.ID, Ticker: "SPY", Side: "buy", Status: "pending", Qty: 2,
		AssetClass: "option", Underlying: "SPY", Expiry: &exp, Strike: 551, Right: "C",
		OCCSymbol: "SPY260717C00551000", Multiplier: 100,
	})
	_, _ = store.CreateORBTrade(ctx, db, store.ORBTrade{
		SessionID: sess.ID, Ticker: "AAPLTUI", Side: "buy", Status: "pending", Qty: 10,
	})

	trades, err := store.GetORBTrades(ctx, db, sess.ID)
	if err != nil {
		t.Fatalf("GetORBTrades: %v", err)
	}
	var opt, eq *store.ORBTrade
	for i := range trades {
		switch trades[i].Ticker {
		case "SPY":
			opt = &trades[i]
		case "AAPLTUI":
			eq = &trades[i]
		}
	}
	if opt == nil || opt.AssetClass != "option" || opt.OCCSymbol != "SPY260717C00551000" || opt.Right != "C" || opt.Strike != 551 {
		t.Errorf("option row identity = %+v", opt)
	}
	if eq == nil || eq.AssetClass != "equity" || eq.OCCSymbol != "" || eq.Right != "" {
		t.Errorf("equity row should carry no option identity: %+v", eq)
	}
}

// TestGetORBTradeByID_NotFound confirms a missing id yields (nil, nil), not an error.
func TestGetORBTradeByID_NotFound(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("DATABASE_URL not set; skipping store integration test")
	}
	got, err := store.GetORBTradeByID(context.Background(), store.GetDB(),
		"00000000-0000-0000-0000-000000000000")
	if err != nil {
		t.Fatalf("GetORBTradeByID: unexpected error %v", err)
	}
	if got != nil {
		t.Errorf("expected nil for missing id, got %+v", got)
	}
}

// TestFillORBTradePartialByOrderID verifies a partial entry fill persists as filled
// with the ACTUAL filled quantity (not the ordered size), so the exit sweep's
// fallback can't over-sell.
func TestFillORBTradePartialByOrderID(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("DATABASE_URL not set; skipping store integration test")
	}
	ctx := context.Background()
	db := store.GetDB()
	sess, err := store.CreateORBSession(ctx, db, time.Now(), "opt-partialfill")
	if err != nil {
		t.Fatalf("CreateORBSession: %v", err)
	}
	exp := time.Date(2026, 7, 17, 0, 0, 0, 0, time.UTC)
	id, err := store.CreateORBTrade(ctx, db, store.ORBTrade{
		SessionID: sess.ID, Ticker: "SPY", Side: "buy", OrderID: "opt-pf1", Status: "pending",
		Qty: 10, AssetClass: "option", Underlying: "SPY", Expiry: &exp, Strike: 551, Right: "C",
		OCCSymbol: "SPY260717C00551000", Multiplier: 100,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Only 3 of 10 filled.
	updated, err := store.FillORBTradePartialByOrderID(ctx, db, "opt-pf1", 1.30, 3)
	if err != nil || !updated {
		t.Fatalf("partial fill = (%v,%v); want true,nil", updated, err)
	}
	got, _ := store.GetORBTradeByID(ctx, db, id)
	if got == nil || got.Status != "filled" || got.Qty != 3 || got.FillPrice != 1.30 {
		t.Errorf("row = %+v; want filled, qty 3, fill 1.30", got)
	}
}

// TestGetManagedOptionQuantities_SumsPerOCC verifies the orphan detector's managed
// side sums contract quantities per OCC (so a qty mismatch can be detected), and
// that SetORBTradeExitOrderID round-trips via GetOpenFilledOptionTrades (restart
// recovery).
func TestGetManagedOptionQuantities_SumsPerOCC(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("DATABASE_URL not set; skipping store integration test")
	}
	ctx := context.Background()
	db := store.GetDB()
	sess, err := store.CreateORBSession(ctx, db, time.Now(), "opt-managedqty")
	if err != nil {
		t.Fatalf("CreateORBSession: %v", err)
	}
	exp := time.Date(2026, 7, 17, 0, 0, 0, 0, time.UTC)
	occ := "MGDQTY261218C00100000" // unique OCC so the global SUM isn't polluted by other tests
	mk := func(qty int, status, orderID string) string {
		id, err := store.CreateORBTrade(ctx, db, store.ORBTrade{
			SessionID: sess.ID, Ticker: "MGDQTY", Side: "buy", OrderID: orderID, Status: status, Qty: qty,
			AssetClass: "option", Underlying: "MGDQTY", Expiry: &exp, Strike: 100, Right: "C",
			OCCSymbol: occ, Multiplier: 100,
		})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		return id
	}
	// Measure the DELTA so the shared/persistent test DB (rows accumulate across
	// runs; the SUM is global across sessions) doesn't make this non-idempotent.
	before, err := store.GetManagedOptionQuantities(ctx, db)
	if err != nil {
		t.Fatalf("GetManagedOptionQuantities (before): %v", err)
	}
	filledID := mk(1, "filled", "mq-1")
	mk(2, "pending", "mq-2") // pending also counts toward managed

	after, err := store.GetManagedOptionQuantities(ctx, db)
	if err != nil {
		t.Fatalf("GetManagedOptionQuantities: %v", err)
	}
	if after[occ]-before[occ] != 3 {
		t.Errorf("managed qty delta for %s = %v; want 3 (1 filled + 2 pending)", occ, after[occ]-before[occ])
	}

	// Exit-order-id persistence round-trips for the filled row.
	if err := store.SetORBTradeExitOrderID(ctx, db, filledID, "exit-ord-9"); err != nil {
		t.Fatalf("SetORBTradeExitOrderID: %v", err)
	}
	filled, err := store.GetOpenFilledOptionTrades(ctx, db)
	if err != nil {
		t.Fatalf("GetOpenFilledOptionTrades: %v", err)
	}
	var found bool
	for _, tr := range filled {
		if tr.ID == filledID {
			found = true
			if tr.ExitOrderID != "exit-ord-9" {
				t.Errorf("ExitOrderID = %q; want exit-ord-9", tr.ExitOrderID)
			}
		}
	}
	if !found {
		t.Error("filled option row not returned by reader")
	}
}
