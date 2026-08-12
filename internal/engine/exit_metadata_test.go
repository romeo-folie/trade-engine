package engine

import (
	"context"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/events"
	"github.com/romeo-folie/trade-engine/internal/portfolio"
	"github.com/romeo-folie/trade-engine/internal/store"
)

func TestPublishTradeClosedUsesCommittedTransitionAsEventGate(t *testing.T) {
	bus := events.New()
	engine := &Engine{bus: bus}
	sub := bus.Subscribe(events.TradeClosed)
	defer bus.Unsubscribe(sub)
	filledAt := time.Date(2026, 8, 1, 14, 5, 2, 0, time.UTC)
	realized := store.TradeRealization{
		TradeID: "t1", Ticker: "PSN", Direction: "long", EntryPrice: 100,
		ExitPrice: 95, PnL: -50, PnLPct: -5,
		ExitReason:   store.ExitReasonEMA9Close,
		ExitEvidence: []byte(`{"daily_close":95,"ema9":98}`),
		ExitFilledAt: &filledAt,
	}

	engine.publishTradeClosed(realized)
	select {
	case event := <-sub.C:
		t.Fatalf("non-transition emitted close event: %+v", event)
	default:
	}
	realized.JustClosed = true
	engine.publishTradeClosed(realized)
	select {
	case event := <-sub.C:
		payload, ok := event.Payload.(events.TradeClosedPayload)
		if !ok || payload.TradeID != "t1" || payload.Reason != store.ExitReasonEMA9Close ||
			payload.FilledAt == nil || !payload.FilledAt.Equal(filledAt) ||
			string(payload.Evidence) != string(realized.ExitEvidence) {
			t.Fatalf("close payload = %#v", event.Payload)
		}
	case <-time.After(time.Second):
		t.Fatal("committed close transition emitted no event")
	}
}

func TestRecoveredStockExitRestoresCloseEffects(t *testing.T) {
	bus := events.New()
	tracker := portfolio.NewTracker(time.Hour)
	engine := &Engine{bus: bus, portfolio: tracker}
	sub := bus.Subscribe(events.TradeClosed)
	defer bus.Unsubscribe(sub)

	engine.RecoveredStockExit(context.Background(), store.TradeRealization{
		TradeID: "recovered-close", Ticker: "RCV", Direction: "long",
		EntryPrice: 100, ExitPrice: 95, PnL: -5, PnLPct: -5,
		JustClosed: true,
	})
	if tracker.CurrentState().LastLossAt.IsZero() {
		t.Fatal("recovered losing close did not record the loss cooldown")
	}
	select {
	case event := <-sub.C:
		payload, ok := event.Payload.(events.TradeClosedPayload)
		if !ok || payload.TradeID != "recovered-close" {
			t.Fatalf("recovered close event=%#v", event.Payload)
		}
	case <-time.After(time.Second):
		t.Fatal("recovered close did not publish TradeClosed")
	}
}
