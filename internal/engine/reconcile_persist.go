package engine

import (
	"context"
	"encoding/json"
	"math"
	"time"

	"github.com/romeo-folie/trade-engine/internal/logger"
	"github.com/romeo-folie/trade-engine/internal/store"
)

// BrokerReconcileInterval is the cadence of the background broker-vs-DB
// reconcile loop. Matching the exit reconciler lets a fresh exit reduction
// become alertable immediately after its two-sweep grace period.
const BrokerReconcileInterval = ExitReconcileInterval

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
// persists the result to engine_state. An optional callback receives only new
// or changed alertable drift; orphan adoption stays on the boot path. Blocks
// until ctx is cancelled.
// The first reconcile runs after the first interval; the caller is expected to
// have already reconciled + persisted at boot. A nil broker or non-positive
// interval disables the loop.
type ReconcileLoopOptions struct {
	InitialDrifts []BrokerDrift
	OnNewDrifts   func(context.Context, []BrokerDrift)
}

type driftAlertState map[string]BrokerDrift

func newDriftAlertState(initial []BrokerDrift) driftAlertState {
	state := make(driftAlertState)
	for _, d := range alertableBrokerDrifts(initial) {
		state[d.Ticker] = d
	}
	return state
}

func sameAlertableDrift(a BrokerDrift, exists bool, b BrokerDrift) bool {
	return exists && a.Source == b.Source &&
		math.Abs(a.BrokerQty-b.BrokerQty) <= DefaultBrokerDriftToleranceShares &&
		math.Abs(a.DBQty-b.DBQty) <= DefaultBrokerDriftToleranceShares
}

func (s driftAlertState) advance(drifts []BrokerDrift) (driftAlertState, []BrokerDrift) {
	next := make(driftAlertState)
	var fresh []BrokerDrift
	for _, d := range alertableBrokerDrifts(drifts) {
		previous, exists := s[d.Ticker]
		next[d.Ticker] = d
		if !sameAlertableDrift(previous, exists, d) {
			fresh = append(fresh, d)
		}
	}
	return next, fresh
}

func RunReconcileLoop(ctx context.Context, broker reconcilerBrokerView, interval time.Duration, option ...ReconcileLoopOptions) {
	if interval <= 0 || broker == nil {
		return
	}
	var opts ReconcileLoopOptions
	if len(option) > 0 {
		opts = option[0]
	}
	last := newDriftAlertState(opts.InitialDrifts)
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
				var fresh []BrokerDrift
				last, fresh = last.advance(drifts)
				if len(fresh) > 0 && opts.OnNewDrifts != nil {
					opts.OnNewDrifts(ctx, fresh)
				}
			}
		}
	}
}
