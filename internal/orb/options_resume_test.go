package orb

import (
	"context"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/store"
)

func TestResumeSessionRestoresPendingOptionSiblingTrade(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("DATABASE_URL not set; skipping DB-backed ORB test")
	}
	ctx := context.Background()
	db := store.GetDB()
	date := time.Now().UTC()
	name := "options-resume-" + date.Format("150405.000000000")
	parent, err := store.CreateORBSession(ctx, db, date, name)
	if err != nil {
		t.Fatalf("CreateORBSession: %v", err)
	}
	optionSession, err := store.CreateORBSessionOwned(ctx, db, date, optionsSessionName(name), "option", store.ORBOwnerOptions)
	if err != nil {
		t.Fatalf("CreateORBSessionOwned: %v", err)
	}
	tradeID, err := store.CreateORBTrade(ctx, db, store.ORBTrade{
		SessionID: optionSession.ID, Ticker: "SPY", Side: "buy", Qty: 2,
		OrderID: "options-resume-entry", Status: "pending",
		AssetClass: "option", OwnerScope: store.ORBOwnerOptions,
		OCCSymbol: "SPY261218C00600000", Multiplier: 100,
	})
	if err != nil {
		t.Fatalf("CreateORBTrade: %v", err)
	}
	parent.Status = string(StateExecuting)
	s := &ORBScheduler{
		db: db, orderIDs: make(map[string][2]string),
		scaleOutEntryIDs: make(map[string][2]string),
		trackedTrades:    make(map[string]*trackedTrade),
	}
	if err := s.resumeSession(ctx, parent); err != nil {
		t.Fatalf("resumeSession: %v", err)
	}
	tracked := s.trackedTrades["options-resume-entry"]
	if tracked == nil || tracked.dbID != tradeID || !tracked.isOption {
		t.Fatalf("restored option tracking = %+v; want trade %s marked option", tracked, tradeID)
	}
	if got := s.orderIDs["SPY"][0]; got != "options-resume-entry" {
		t.Fatalf("restored buy order = %q, want options-resume-entry", got)
	}
}
