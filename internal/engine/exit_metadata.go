package engine

import (
	"encoding/json"
	"time"

	"github.com/romeo-folie/trade-engine/internal/events"
	"github.com/romeo-folie/trade-engine/internal/store"
)

func newExitIntent(reason string, evidence map[string]any) store.ExitIntent {
	raw, err := json.Marshal(evidence)
	if err != nil {
		raw = nil
	}
	return store.ExitIntent{Reason: reason, Evidence: raw}
}

func (e *Engine) publishTradeClosed(realized store.TradeRealization) {
	if !realized.JustClosed {
		return
	}
	e.bus.Publish(events.TradeClosed, events.TradeClosedPayload{
		TradeID: realized.TradeID, Ticker: realized.Ticker,
		Direction: realized.Direction, EntryPrice: realized.EntryPrice,
		ExitPrice: realized.ExitPrice, PnL: realized.PnL,
		PnLPct: realized.PnLPct, Reason: realized.ExitReason,
		Evidence: realized.ExitEvidence, FilledAt: realized.ExitFilledAt,
	})
}

func filledAtPtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	t = t.UTC()
	return &t
}
