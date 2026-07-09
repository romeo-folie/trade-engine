package engine

import (
	"context"
	"encoding/json"
	"time"

	"github.com/romeo-folie/trade-engine/internal/logger"
	"github.com/romeo-folie/trade-engine/internal/store"
)

// BrokerReconcileInterval is the cadence of the background broker-vs-DB
// reconcile loop. Kept coarse — this is an observability backstop for drift
// that develops between engine restarts, not a hot path.
const BrokerReconcileInterval = 30 * time.Minute

// persistedDrift is the wire shape of one drift row stored under
// EngineStateBrokerDrift. Lowercase JSON tags are a stable contract read by the
// TUI (and future web); do not rename without updating the readers.
type persistedDrift struct {
	Ticker    string  `json:"ticker"`
	BrokerQty float64 `json:"broker_qty"`
	DBQty     float64 `json:"db_qty"`
	Source    string  `json:"source"`
}

// PersistedReconciliation is the JSON blob stored under EngineStateBrokerDrift.
// An empty Drifts slice means broker and DB agreed at CheckedAt.
type PersistedReconciliation struct {
	CheckedAt time.Time        `json:"checked_at"`
	Drifts    []persistedDrift `json:"drifts"`
}

// MarshalReconciliation serialises a drift list with a checked-at stamp. Pure:
// the timestamp is passed in so callers (and tests) control it.
func MarshalReconciliation(drifts []BrokerDrift, checkedAt time.Time) (string, error) {
	pr := PersistedReconciliation{CheckedAt: checkedAt}
	for _, d := range drifts {
		pr.Drifts = append(pr.Drifts, persistedDrift{
			Ticker:    d.Ticker,
			BrokerQty: d.BrokerQty,
			DBQty:     d.DBQty,
			Source:    d.Source,
		})
	}
	b, err := json.Marshal(pr)
	return string(b), err
}

// PersistReconciliation writes the drift list to engine_state for cross-process
// readers. Best-effort — marshal/write errors are logged and swallowed so a
// reconcile never blocks or crashes the engine. No-ops when the DB is
// disconnected (store.SetEngineState → GetDB() would otherwise panic on a
// DB-less engine start).
func PersistReconciliation(ctx context.Context, drifts []BrokerDrift) {
	if !store.IsConnected() {
		return
	}
	blob, err := MarshalReconciliation(drifts, time.Now().UTC())
	if err != nil {
		logger.Warn("reconcile persist: marshal failed", "err", err)
		return
	}
	if err := store.SetEngineState(ctx, store.EngineStateBrokerDrift, blob); err != nil {
		logger.Warn("reconcile persist: write failed", "err", err)
	}
}

// RunReconcileLoop periodically reconciles broker positions against the DB and
// persists the result to engine_state — observability only, no alerts and no
// orphan adoption (those stay on the boot path). Blocks until ctx is cancelled.
// The first reconcile runs after the first interval; the caller is expected to
// have already reconciled + persisted at boot. A nil broker or non-positive
// interval disables the loop.
func RunReconcileLoop(ctx context.Context, broker reconcilerBrokerView, interval time.Duration) {
	if interval <= 0 || broker == nil {
		return
	}
	logger.Info("broker reconcile loop started", "interval", interval)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			// Only persist a reconcile that actually completed — a transient
			// broker/DB failure must not overwrite known drift with a false
			// "in sync".
			if drifts, ok := ReconcileBrokerVsDBWithStoreStatus(ctx, broker, 0); ok {
				PersistReconciliation(ctx, drifts)
			}
		}
	}
}
