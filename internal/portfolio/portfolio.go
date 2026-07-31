// Package portfolio tracks portfolio state and records snapshots + daily summaries.
package portfolio

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/romeo-folie/trade-engine/internal/logger"
	"github.com/romeo-folie/trade-engine/internal/store"
)

// State is an in-memory view of the current portfolio.
type State struct {
	TotalValue     float64
	Cash           float64
	Invested       float64
	UnrealizedPnL  float64
	DayPnL         float64 // net change in equity since last session close
	RealizedPnLDay float64 // realized P&L today (from closed trades)
	OpenPositions  int
	PeakValue      float64 // all-time high (for drawdown)
	LastLossAt     time.Time
}

// Tracker manages portfolio state and persistence. All methods are thread-safe.
type Tracker struct {
	mu               sync.RWMutex
	state            State
	positions        []ExecPosition // last set passed to Update; used by PositionMap
	snapshotEvery    time.Duration
	lastSnapshot     time.Time
	lastDailySummary time.Time

	// mode and broker tag snapshot rows written to the DB (Phase 18).
	// Set via SetTags before the first snapshot.
	mode   string // "live" | "paper"
	broker string // "alpaca" | "ibkr"
}

// NewTracker creates a Tracker with the given snapshot interval.
func NewTracker(snapshotInterval time.Duration) *Tracker {
	if snapshotInterval == 0 {
		snapshotInterval = time.Hour
	}
	return &Tracker{snapshotEvery: snapshotInterval}
}

// ExecPosition is the external-facing adapter type callers use to pass position
// data without importing execution package internals.
type ExecPosition struct {
	Ticker       string
	Quantity     float64
	AvgCostBasis float64 // per-share average entry price
	CurrentPrice float64 // per-share current price (0 if unavailable)
	CurrentValue float64 // total market value
	UnrealizedPL float64
	Side         string // "long" or "short" (empty if unavailable)
}

// PositionMap returns a snapshot of current open positions as ticker→currentValue.
// Returns nil if no positions have been recorded yet.  Safe for concurrent use.
func (t *Tracker) PositionMap() map[string]float64 {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if len(t.positions) == 0 {
		return nil
	}
	out := make(map[string]float64, len(t.positions))
	for _, p := range t.positions {
		out[p.Ticker] = p.CurrentValue
	}
	return out
}

// Update refreshes the in-memory state from executor positions + account metrics.
func (t *Tracker) Update(positions []ExecPosition, cashBalance, totalEquity, dayPnL, realizedPnLDay float64) {
	t.mu.Lock()
	defer t.mu.Unlock()

	// Snapshot for PositionMap.
	t.positions = make([]ExecPosition, len(positions))
	copy(t.positions, positions)

	var invested, unrealizedPnL float64
	for _, p := range positions {
		invested += p.CurrentValue
		unrealizedPnL += p.UnrealizedPL
	}

	peak := t.state.PeakValue
	if totalEquity > peak {
		peak = totalEquity
	}
	t.state = State{
		TotalValue:     totalEquity,
		Cash:           cashBalance,
		Invested:       invested,
		UnrealizedPnL:  unrealizedPnL,
		DayPnL:         dayPnL,
		RealizedPnLDay: realizedPnLDay,
		OpenPositions:  len(positions),
		PeakValue:      peak,
		LastLossAt:     t.state.LastLossAt,
	}
}

// RecordLoss marks the timestamp of the most-recent losing trade close.
func (t *Tracker) RecordLoss() {
	t.mu.Lock()
	t.state.LastLossAt = time.Now()
	t.mu.Unlock()
}

// CurrentState returns a copy of the current portfolio state (safe for concurrent reads).
func (t *Tracker) CurrentState() State {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.state
}

// Positions returns a snapshot of current open positions.
func (t *Tracker) Positions() []ExecPosition {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if len(t.positions) == 0 {
		return []ExecPosition{}
	}
	out := make([]ExecPosition, len(t.positions))
	copy(out, t.positions)
	return out
}

// SetTags sets the mode and broker that will be written to snapshot rows.
// Call this once after NewTracker when the engine configuration is known.
func (t *Tracker) SetTags(mode, broker string) {
	t.mu.Lock()
	t.mode = mode
	t.broker = broker
	t.mu.Unlock()
}

// MaybeSnapshot writes a snapshot if the interval has elapsed.
// Returns the snapshot state written, or nil if interval not elapsed / on error.
func (t *Tracker) MaybeSnapshot(ctx context.Context) *State {
	t.mu.RLock()
	s := t.state
	elapsed := time.Since(t.lastSnapshot)
	mode := t.mode
	broker := t.broker
	t.mu.RUnlock()

	if elapsed < t.snapshotEvery {
		return nil
	}
	if err := store.InsertPortfolioSnapshot(
		ctx, s.TotalValue, s.Cash, s.Invested, s.UnrealizedPnL, s.RealizedPnLDay,
		s.OpenPositions, true, "engine", mode, broker,
	); err != nil {
		logger.Warn("portfolio snapshot failed", "err", err)
		return nil
	}
	t.mu.Lock()
	t.lastSnapshot = time.Now()
	t.mu.Unlock()
	logger.Debug("portfolio snapshot written", "total", s.TotalValue)
	return &s
}

// MaybeDailySummary writes a daily_summaries row when the calendar date changes.
//
// counters carries the Issue #325 observability rollup (signals_total /
// signals_executed / executed_winning|losing / blocked_by_reason). It is
// always upserted alongside the legacy P&L columns so the dashboard always
// has a fresh row to read. Pass an empty store.DailyCounters{} when the
// caller can't aggregate (no DB / pre-#325 paths) — the row will still write,
// the new columns will simply hold their zero values.
func (t *Tracker) MaybeDailySummary(ctx context.Context, startingValue float64,
	tradesOpened, tradesClosed, wins, losses, sigGenerated, sigBlocked int,
	tokIn, tokOut int64,
	counters store.DailyCounters,
) {
	today := time.Now().UTC().Truncate(24 * time.Hour)
	t.mu.RLock()
	s := t.state
	t.mu.RUnlock()

	// Use the broker-reported DayPnL (equity - last_equity) when available, or fall
	// back to realized trade PnL. Avoids distorting the daily figure with intraday
	// unrealized swings from open positions. A legitimately flat day (DayPnL == 0)
	// is indistinguishable from "unavailable" here; RealizedPnLDay is 0 too in that
	// case, so both produce the correct result.
	dayPnL := s.DayPnL
	if dayPnL == 0 {
		dayPnL = s.RealizedPnLDay
	}
	pnlPct := 0.0
	if startingValue > 0 {
		pnlPct = (dayPnL / startingValue) * 100
	}
	// Marshal the block-reason histogram for JSONB storage. A nil/empty map
	// becomes the empty object so the column is always valid JSONB rather
	// than NULL — the schema default already supplies '{}', this just keeps
	// the explicit insert path consistent.
	blockedJSON := []byte("{}")
	if len(counters.BlockedByReason) > 0 {
		if b, err := json.Marshal(counters.BlockedByReason); err == nil {
			blockedJSON = b
		}
	}
	// pgx encodes a raw []byte as bytea. blocked_by_reason is JSONB, so the
	// placeholder is cast explicitly with ::jsonb (the same pattern UpdateORBSession
	// uses for the candidates column). Without the cast, INSERT fails at runtime
	// with "column ... is of type jsonb but expression is of type bytea".
	_, err := store.GetDB().Exec(ctx, `
		INSERT INTO daily_summaries (
			date, starting_value, ending_value, pnl, pnl_pct,
			trades_opened, trades_closed, wins, losses,
			signals_generated, signals_blocked,
			model_tokens_input, model_tokens_output,
			signals_total, signals_executed,
			executed_winning, executed_losing, blocked_by_reason
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18::jsonb)
		ON CONFLICT (date) DO UPDATE SET
			ending_value=EXCLUDED.ending_value, pnl=EXCLUDED.pnl, pnl_pct=EXCLUDED.pnl_pct,
			trades_opened=EXCLUDED.trades_opened, trades_closed=EXCLUDED.trades_closed,
			wins=EXCLUDED.wins, losses=EXCLUDED.losses,
			signals_generated=EXCLUDED.signals_generated, signals_blocked=EXCLUDED.signals_blocked,
			model_tokens_input=EXCLUDED.model_tokens_input, model_tokens_output=EXCLUDED.model_tokens_output,
			signals_total=EXCLUDED.signals_total, signals_executed=EXCLUDED.signals_executed,
			executed_winning=EXCLUDED.executed_winning, executed_losing=EXCLUDED.executed_losing,
			blocked_by_reason=EXCLUDED.blocked_by_reason`,
		today.Format("2006-01-02"),
		startingValue, s.TotalValue, dayPnL, pnlPct,
		tradesOpened, tradesClosed, wins, losses,
		sigGenerated, sigBlocked, tokIn, tokOut,
		counters.SignalsTotal, counters.SignalsExecuted,
		counters.ExecutedWinning, counters.ExecutedLosing, blockedJSON,
	)
	if err != nil {
		logger.Warn("daily summary failed", "err", err)
		return
	}
	t.mu.Lock()
	t.lastDailySummary = today
	t.mu.Unlock()
	logger.Info("daily summary written",
		"date", today.Format("2006-01-02"),
		"pnl_pct", pnlPct,
		"signals_total", counters.SignalsTotal,
		"signals_blocked", counters.SignalsBlocked,
		"signals_executed", counters.SignalsExecuted,
	)
}
