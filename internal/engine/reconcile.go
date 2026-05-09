package engine

import (
	"context"
	"errors"
	"math"
	"sort"

	"github.com/romeo-folie/trade-engine/internal/execution"
	"github.com/romeo-folie/trade-engine/internal/logger"
	"github.com/romeo-folie/trade-engine/internal/store"
)

// DefaultBrokerDriftToleranceShares is the default fractional-share tolerance
// when comparing broker quantity to summed DB quantity. Alpaca rounds fractional
// shares to ~9 decimal places, so a 0.01-share tolerance is generous enough to
// suppress noise without hiding real drift.
const DefaultBrokerDriftToleranceShares = 0.01

// reconcilerBrokerView is the minimum BrokerView surface used by the reconciler.
// Defining it inline keeps the reconciler decoupled from the full BrokerView
// interface and makes the function easy to fake from tests.
type reconcilerBrokerView interface {
	PositionsSnapshot(ctx context.Context) ([]execution.BrokerPosition, error)
}

// reconcilerStore is the minimum store surface used by the reconciler. Tests
// supply an in-memory implementation; production wires the real store through
// store.GetOpenTrades.
type reconcilerStore interface {
	GetOpenTrades(ctx context.Context) ([]store.Trade, error)
}

// storeAdapter adapts the package-level store.GetOpenTrades to the
// reconcilerStore interface. Holding it as a value makes the call site in
// run.go obvious without forcing the engine package to depend on store internals.
type storeAdapter struct{}

func (storeAdapter) GetOpenTrades(ctx context.Context) ([]store.Trade, error) {
	return store.GetOpenTrades(ctx)
}

// ReconcileBrokerVsDB compares broker positions to DB-tracked open trades and
// emits a WARN log line for every ticker where they diverge by more than
// `tolerance` shares. It is observability-only: no DB or broker mutation.
//
// Behaviour:
//   - broker == nil OR PositionsSnapshot returns ErrBrokerUnavailable: logs an
//     INFO ("reconciler skipped — broker unavailable") and returns nil. Boot
//     must not fail when there is no real broker (e.g. PaperExecutor).
//   - tolerance <= 0: falls back to DefaultBrokerDriftToleranceShares.
//   - Tickers present in only one side count as drift (source=broker_only or
//     source=db_only). Tickers present in both with |Δqty| > tolerance log
//     source=mismatch.
//
// Mirrors the visibility-first stance of reconcileDoneSessionTrades (PR #297).
func ReconcileBrokerVsDB(ctx context.Context, broker reconcilerBrokerView, db reconcilerStore, tolerance float64) error {
	if tolerance <= 0 {
		tolerance = DefaultBrokerDriftToleranceShares
	}

	if broker == nil {
		logger.Info("reconciler skipped — broker unavailable", "reason", "nil_broker_view")
		return nil
	}

	positions, err := broker.PositionsSnapshot(ctx)
	if err != nil {
		if errors.Is(err, execution.ErrBrokerUnavailable) {
			logger.Info("reconciler skipped — broker unavailable", "reason", "ErrBrokerUnavailable")
			return nil
		}
		logger.Warn("reconciler: broker positions fetch failed", "err", err.Error())
		return nil
	}

	if db == nil {
		logger.Warn("reconciler: db unavailable — skipping")
		return nil
	}
	openTrades, err := db.GetOpenTrades(ctx)
	if err != nil {
		logger.Warn("reconciler: db open trades fetch failed", "err", err.Error())
		return nil
	}

	type aggregate struct {
		brokerQty   float64
		dbQty       float64
		dbRowCount  int
		seenBroker  bool
		seenDB      bool
	}
	byTicker := make(map[string]*aggregate)

	for _, p := range positions {
		if p.Symbol == "" {
			continue
		}
		a, ok := byTicker[p.Symbol]
		if !ok {
			a = &aggregate{}
			byTicker[p.Symbol] = a
		}
		// BrokerPosition.Quantity is signed for short positions; use absolute
		// value because DB-side quantity is stored as a positive number with
		// a separate `direction` column.
		a.brokerQty += math.Abs(p.Quantity)
		a.seenBroker = true
	}

	for _, t := range openTrades {
		if t.Ticker == "" {
			continue
		}
		a, ok := byTicker[t.Ticker]
		if !ok {
			a = &aggregate{}
			byTicker[t.Ticker] = a
		}
		a.dbQty += t.Quantity
		a.dbRowCount++
		a.seenDB = true
	}

	// Stable iteration order so logs are deterministic across boots — easier
	// to diff in cron tails and grep-friendly for ops.
	tickers := make([]string, 0, len(byTicker))
	for k := range byTicker {
		tickers = append(tickers, k)
	}
	sort.Strings(tickers)

	driftCount := 0
	for _, ticker := range tickers {
		a := byTicker[ticker]
		diff := math.Abs(a.brokerQty - a.dbQty)
		if diff <= tolerance {
			continue
		}
		source := "mismatch"
		switch {
		case a.seenBroker && !a.seenDB:
			source = "broker_only"
		case !a.seenBroker && a.seenDB:
			source = "db_only"
		}
		logger.Warn("reconciler: broker/DB drift",
			"ticker", ticker,
			"broker_qty", a.brokerQty,
			"db_qty", a.dbQty,
			"db_row_count", a.dbRowCount,
			"source", source,
			"tolerance", tolerance,
		)
		driftCount++
	}

	logger.Info("reconciler complete",
		"tickers_compared", len(tickers),
		"drift_warnings", driftCount,
	)
	return nil
}

// ReconcileBrokerVsDBWithStore is a convenience wrapper that uses the package
// store.GetOpenTrades. Call this from boot when no special test store is needed.
func ReconcileBrokerVsDBWithStore(ctx context.Context, broker reconcilerBrokerView, tolerance float64) error {
	if !store.IsConnected() {
		logger.Info("reconciler skipped — db not connected")
		return nil
	}
	return ReconcileBrokerVsDB(ctx, broker, storeAdapter{}, tolerance)
}
