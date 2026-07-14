package orb

import (
	"context"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/execution"
	"github.com/romeo-folie/trade-engine/internal/store"
)

// Regression tests for the 2026-07-14 XAG phantom-exit incident: bracket leg IDs
// captured rotated at placement made the scheduler poll the ENTRY order as its
// "SL"/"TP" leg. The entry order reports filled, so the order-ID exit paths booked
// closes (one even labelled 'tp' with negative PnL) while the broker still held the
// full position — then re-entered, tripling the real position. These tests drive
// the two order-ID exit paths end-to-end against the per-package test DB and assert
// that a "filled exit leg" cannot book a close the broker hasn't actually made.

func xagForexCfg() *config.ForexORBConfig {
	return &config.ForexORBConfig{
		Enabled:     true,
		Instruments: []config.Instrument{{Symbol: "XAG/USD", SecType: "CFD"}},
	}
}

// seedFilledXAGTrade creates a london session with one filled XAG/USD buy whose
// bracket leg IDs are known, returning the trade's DB id.
func seedFilledXAGTrade(t *testing.T, ctx context.Context, entryID, tpID, slID string) (sessionID, tradeID string) {
	t.Helper()
	db := store.GetDB()
	sess, err := store.CreateORBSession(ctx, db, time.Now().UTC(), "london-test-"+entryID)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	tradeID, err = store.CreateORBTrade(ctx, db, store.ORBTrade{
		SessionID:  sess.ID,
		Ticker:     "XAG/USD",
		Side:       "buy",
		EntryStop:  58.09,
		StopLoss:   57.83,
		TakeProfit: 58.61,
		Qty:        1600,
		OrderID:    entryID,
		Status:     "pending",
		TpOrderID:  tpID,
		SlOrderID:  slID,
		AssetClass: "forex",
	})
	if err != nil {
		t.Fatalf("create trade: %v", err)
	}
	if _, err := store.FillORBTradeByOrderID(ctx, db, entryID, 58.1224); err != nil {
		t.Fatalf("fill trade: %v", err)
	}
	return sess.ID, tradeID
}

// TestPollExitLegs_GuardBlocksPhantomClose drives the live order-ID exit poll: the
// "SL" leg reports filled (in the incident it was really the entry order), but the
// broker still holds the full position — the close must be deferred, and booked only
// once the broker position actually reduces.
func TestPollExitLegs_GuardBlocksPhantomClose(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("DATABASE_URL not set; skipping DB-backed test")
	}
	ctx := context.Background()
	_, tradeID := seedFilledXAGTrade(t, ctx, "entry-poll", "tp-poll", "sl-poll")

	exec := &stubExecutor{
		statuses: map[string]execution.OrderResult{
			"sl-poll": {Status: "filled", FilledPrice: 58.105},
		},
		positions: []execution.Position{{Ticker: "XAG/USD", Quantity: 1600}}, // still held
	}
	s := &ORBScheduler{
		executor:           exec,
		db:                 store.GetDB(),
		forexCfg:           xagForexCfg(),
		reconciledSessions: make(map[string]struct{}),
		trackedTrades: map[string]*trackedTrade{
			"entry-poll": {
				dbID: tradeID, tpOrderID: "tp-poll", slOrderID: "sl-poll",
				side: "buy", ticker: "XAG/USD", qty: 1600, entryFill: 58.1224,
			},
		},
	}

	// Broker still holds the position: the filled "exit leg" must NOT book a close.
	s.pollExitLegs(ctx)
	tr, err := store.GetORBTradeByID(ctx, store.GetDB(), tradeID)
	if err != nil || tr == nil {
		t.Fatalf("read trade: %v", err)
	}
	if tr.Status != "filled" {
		t.Fatalf("status = %q after phantom exit leg; want filled (close deferred)", tr.Status)
	}
	if _, ok := s.trackedTrades["entry-poll"]; !ok {
		t.Fatal("trade dropped from tracking on a deferred close; must be retried next tick")
	}

	// Broker actually flat: the same leg fill may now close the trade.
	exec.positions = nil
	s.pollExitLegs(ctx)
	tr, err = store.GetORBTradeByID(ctx, store.GetDB(), tradeID)
	if err != nil || tr == nil {
		t.Fatalf("read trade: %v", err)
	}
	if tr.Status != "closed" || tr.ExitType != "stop" {
		t.Fatalf("status/exit_type = %q/%q after broker-confirmed exit; want closed/stop", tr.Status, tr.ExitType)
	}
	if _, ok := s.trackedTrades["entry-poll"]; ok {
		t.Fatal("trade still tracked after booked close")
	}
}

// TestReconcileDoneSession_GuardBlocksPhantomClose drives the order-ID fast path of
// the done-session reconciler with the same incident shape.
func TestReconcileDoneSession_GuardBlocksPhantomClose(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("DATABASE_URL not set; skipping DB-backed test")
	}
	ctx := context.Background()
	sessionID, tradeID := seedFilledXAGTrade(t, ctx, "entry-rec", "tp-rec", "sl-rec")

	exec := &stubExecutor{
		statuses: map[string]execution.OrderResult{
			"tp-rec": {Status: "filled", FilledPrice: 58.088}, // the incident's 'tp' at a loss
		},
		positions: []execution.Position{{Ticker: "XAG/USD", Quantity: 1600}}, // still held
	}
	s := &ORBScheduler{
		executor:           exec,
		db:                 store.GetDB(),
		forexCfg:           xagForexCfg(),
		reconciledSessions: make(map[string]struct{}),
		trackedTrades:      make(map[string]*trackedTrade),
	}

	s.reconcileDoneSessionTrades(ctx, sessionID)
	tr, err := store.GetORBTradeByID(ctx, store.GetDB(), tradeID)
	if err != nil || tr == nil {
		t.Fatalf("read trade: %v", err)
	}
	if tr.Status != "filled" {
		t.Fatalf("status = %q after phantom exit leg; want filled (close deferred)", tr.Status)
	}

	// Broker actually flat: a fresh pass books the close with the leg's type.
	exec.positions = nil
	s2 := &ORBScheduler{
		executor:           exec,
		db:                 store.GetDB(),
		forexCfg:           xagForexCfg(),
		reconciledSessions: make(map[string]struct{}),
		trackedTrades:      make(map[string]*trackedTrade),
	}
	s2.reconcileDoneSessionTrades(ctx, sessionID)
	tr, err = store.GetORBTradeByID(ctx, store.GetDB(), tradeID)
	if err != nil || tr == nil {
		t.Fatalf("read trade: %v", err)
	}
	if tr.Status != "closed" || tr.ExitType != "tp" {
		t.Fatalf("status/exit_type = %q/%q after broker-confirmed exit; want closed/tp", tr.Status, tr.ExitType)
	}
}
