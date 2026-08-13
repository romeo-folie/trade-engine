package engine

import (
	"context"
	"fmt"

	"github.com/romeo-folie/trade-engine/internal/store"
)

// buyEntriesEnabled is intentionally evaluated immediately before a stock BUY
// is admitted. It is not a startup-only setting: an operator can pause new
// risk while the engine, reconciler, trailing stops, and all exits continue.
func buyEntriesEnabled(ctx context.Context) (bool, string) {
	if !store.IsConnected() {
		// Existing non-DB execution modes retain their established behaviour.
		// The broker-backed live admission path already requires a durable DB.
		return true, ""
	}
	control, err := store.GetBuyControl(ctx, store.BuyControlStocks)
	if err != nil {
		return false, "unable to read persistent buy control"
	}
	if control.Enabled {
		return true, ""
	}
	return false, fmt.Sprintf("new stock buys are paused (set by %s at %s)",
		control.UpdatedBy, control.UpdatedAt.UTC().Format("2006-01-02 15:04:05Z"))
}
