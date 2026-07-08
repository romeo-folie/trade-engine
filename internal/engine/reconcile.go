package engine

import (
	"context"
	"errors"
	"math"
	"sort"
	"strings"

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

// signedBrokerQty returns the broker quantity with the sign implied by the
// position side. Some brokers (Alpaca) already signal shorts via a negative
// Quantity; others may pass a positive Quantity with Side="short". We normalise
// to a single signed convention: negative => short, positive => long.
func signedBrokerQty(p execution.BrokerPosition) float64 {
	if p.Quantity < 0 {
		return p.Quantity
	}
	if strings.EqualFold(p.Side, "short") {
		return -math.Abs(p.Quantity)
	}
	return p.Quantity
}

// signedDBQty translates a DB-stored open trade into a signed quantity using
// the same convention as signedBrokerQty. DB stores quantity as a positive
// number with a separate Direction column ("buy"/"sell" or "long"/"short").
// "sell"/"short" are treated as a held short position.
func signedDBQty(t store.Trade) float64 {
	q := math.Abs(t.Quantity)
	d := strings.ToLower(strings.TrimSpace(t.Direction))
	if d == "sell" || d == "short" {
		return -q
	}
	return q
}

// BrokerDrift is one ticker's broker-vs-DB divergence, returned so callers can
// alert on it (Telegram at boot) or adopt orphans instead of the drift living
// and dying as a WARN line. The NOK incident: 178 orphaned shares drifted for a
// month at −29% while the reconciler dutifully logged them on every start.
type BrokerDrift struct {
	Ticker     string
	BrokerQty  float64 // signed
	DBQty      float64 // signed
	DBRowCount int
	Source     string // broker_only | db_only | mismatch | side_mismatch
	// AvgEntryPrice / CurrentPrice from the broker position (0 when db_only).
	AvgEntryPrice float64
	CurrentPrice  float64
}

// ReconcileBrokerVsDB compares broker positions to DB-tracked open trades and
// emits a WARN log line for every ticker where they diverge by more than
// `tolerance` shares, returning the structured drift list. It never mutates
// the DB or broker itself — adoption is the caller's explicit opt-in via
// AdoptBrokerOrphans.
//
// Behaviour:
//   - broker == nil OR PositionsSnapshot returns ErrBrokerUnavailable: logs an
//     INFO ("reconciler skipped — broker unavailable") and returns. Boot must
//     not fail when there is no real broker (e.g. PaperExecutor).
//   - tolerance <= 0: falls back to DefaultBrokerDriftToleranceShares.
//   - Tickers present in only one side count as drift (source=broker_only or
//     source=db_only).
//   - Tickers present in both with |Δsigned_qty| > tolerance log
//     source=mismatch. When the absolute quantities are within tolerance but
//     the signs differ (e.g. broker -50 short vs DB 50 long), the drift is
//     reported with source=side_mismatch — the most dangerous shape of drift.
//
// Mirrors the visibility-first stance of reconcileDoneSessionTrades (PR #297).
// Returns no error: failures (broker fetch, DB fetch) are logged and swallowed
// so a stale dev DB or paused broker can't block engine start.
func ReconcileBrokerVsDB(ctx context.Context, broker reconcilerBrokerView, db reconcilerStore, tolerance float64) []BrokerDrift {
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
		brokerQty     float64 // signed: negative for short, positive for long
		dbQty         float64 // signed: same convention
		dbRowCount    int
		seenBroker    bool
		seenDB        bool
		avgEntryPrice float64
		currentPrice  float64
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
		a.brokerQty += signedBrokerQty(p)
		a.seenBroker = true
		a.avgEntryPrice = p.AvgEntryPrice
		a.currentPrice = p.CurrentPrice
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
		a.dbQty += signedDBQty(t)
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

	var drifts []BrokerDrift
	for _, ticker := range tickers {
		a := byTicker[ticker]
		diff := math.Abs(a.brokerQty - a.dbQty)
		absMatchesButSidesDiffer := math.Abs(math.Abs(a.brokerQty)-math.Abs(a.dbQty)) <= tolerance &&
			a.seenBroker && a.seenDB &&
			((a.brokerQty < 0) != (a.dbQty < 0)) &&
			(a.brokerQty != 0 || a.dbQty != 0)

		if diff <= tolerance && !absMatchesButSidesDiffer {
			continue
		}

		source := "mismatch"
		switch {
		case a.seenBroker && !a.seenDB:
			source = "broker_only"
		case !a.seenBroker && a.seenDB:
			source = "db_only"
		case absMatchesButSidesDiffer:
			// Absolute quantities match within tolerance but signs differ:
			// the canonical "broker short, DB long" (or vice-versa) drift
			// that the abs-only check used to silently miss.
			source = "side_mismatch"
		}
		logger.Warn("reconciler: broker/DB drift",
			"ticker", ticker,
			"broker_qty", a.brokerQty,
			"db_qty", a.dbQty,
			"db_row_count", a.dbRowCount,
			"source", source,
			"tolerance", tolerance,
		)
		drifts = append(drifts, BrokerDrift{
			Ticker:        ticker,
			BrokerQty:     a.brokerQty,
			DBQty:         a.dbQty,
			DBRowCount:    a.dbRowCount,
			Source:        source,
			AvgEntryPrice: a.avgEntryPrice,
			CurrentPrice:  a.currentPrice,
		})
	}

	logger.Info("reconciler complete",
		"tickers_compared", len(tickers),
		"drift_warnings", len(drifts),
	)
	return drifts
}

// ReconcileBrokerVsDBWithStore is a convenience wrapper that uses the package
// store.GetOpenTrades. Call this from boot when no special test store is needed.
func ReconcileBrokerVsDBWithStore(ctx context.Context, broker reconcilerBrokerView, tolerance float64) []BrokerDrift {
	if !store.IsConnected() {
		logger.Info("reconciler skipped — db not connected")
		return nil
	}
	return ReconcileBrokerVsDB(ctx, broker, storeAdapter{}, tolerance)
}

// ─── Orphan adoption ────────────────────────────────────────────────────────────

// adoptionStore is the store surface used by AdoptBrokerOrphans; a narrow
// interface so tests can capture inserts without a database.
type adoptionStore interface {
	InsertTrade(ctx context.Context, t store.Trade) (string, error)
}

type adoptionStoreAdapter struct{}

func (adoptionStoreAdapter) InsertTrade(ctx context.Context, t store.Trade) (string, error) {
	return store.InsertTrade(ctx, t)
}

// AdoptBrokerOrphans inserts recovery trade rows for broker positions the DB
// does not track, so the engine's own machinery — stop/TP monitoring, analyst
// SELL signals, portfolio context — manages them again. Without adoption an
// orphan has NO stop-loss and the position-ownership guard actively blocks
// analyst exit attempts (NOK: two blocked SELLs at ~$15 on the way to $11.78).
//
// Adoption rules, deliberately conservative:
//   - broker_only  → adopt the full signed quantity.
//   - mismatch     → adopt only the untracked surplus (|broker| − |db|) when
//     both sides agree on direction; an over-recorded DB (broker < db) is a
//     different bug class and stays alert-only.
//   - side_mismatch / db_only → never adopted; those need a human.
//
// Stops anchor on the broker's CURRENT price, not the average entry: an orphan
// deep underwater would otherwise trip its stop on the first monitor pass and
// force an instant market exit, which is a policy decision the operator did
// not make. From adoption forward the position risks at most the configured
// default stop percent from today. When the broker reports no current price
// the row is adopted without a stop (WARN) rather than fabricating one.
func AdoptBrokerOrphans(ctx context.Context, db adoptionStore, drifts []BrokerDrift, defaultStopPct, defaultTPPct float64) []string {
	var adopted []string
	for _, d := range drifts {
		var qty float64 // signed adoption quantity
		switch d.Source {
		case "broker_only":
			qty = d.BrokerQty
		case "mismatch":
			if (d.BrokerQty < 0) != (d.DBQty < 0) && d.DBQty != 0 {
				continue // direction disagreement rides with side_mismatch: human territory
			}
			surplus := math.Abs(d.BrokerQty) - math.Abs(d.DBQty)
			if surplus <= 0 {
				continue // DB over-records the broker — alert-only
			}
			qty = math.Copysign(surplus, d.BrokerQty)
		default:
			continue
		}

		direction := "long"
		if qty < 0 {
			direction = "short"
		}

		var stop, tp float64
		if d.CurrentPrice > 0 {
			if direction == "long" {
				stop = d.CurrentPrice * (1 - defaultStopPct/100)
				tp = d.CurrentPrice * (1 + defaultTPPct/100)
			} else {
				stop = d.CurrentPrice * (1 + defaultStopPct/100)
				tp = d.CurrentPrice * (1 - defaultTPPct/100)
			}
		} else {
			logger.Warn("orphan adoption: broker reported no current price — adopting without stop/TP",
				"ticker", d.Ticker)
		}

		id, err := db.InsertTrade(ctx, store.Trade{
			Ticker:     d.Ticker,
			Direction:  direction,
			Strategy:   "recovery",
			EntryPrice: d.AvgEntryPrice,
			Quantity:   math.Abs(qty),
			StopLoss:   stop,
			TakeProfit: tp,
			Status:     "open",
		})
		if err != nil {
			logger.Warn("orphan adoption: insert recovery trade failed",
				"ticker", d.Ticker, "qty", qty, "err", err.Error())
			continue
		}
		logger.Info("orphan adoption: recovery trade created",
			"trade_id", id,
			"ticker", d.Ticker,
			"direction", direction,
			"qty", math.Abs(qty),
			"entry_price", d.AvgEntryPrice,
			"stop_loss", stop,
			"take_profit", tp,
			"source", d.Source,
		)
		adopted = append(adopted, d.Ticker)
	}
	return adopted
}

// AdoptBrokerOrphansWithStore is the production wrapper over AdoptBrokerOrphans.
func AdoptBrokerOrphansWithStore(ctx context.Context, drifts []BrokerDrift, defaultStopPct, defaultTPPct float64) []string {
	return AdoptBrokerOrphans(ctx, adoptionStoreAdapter{}, drifts, defaultStopPct, defaultTPPct)
}
