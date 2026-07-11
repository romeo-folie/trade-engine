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
