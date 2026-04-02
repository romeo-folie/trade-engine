// Package portfolio tracks portfolio state and records snapshots + daily summaries.
package portfolio

import (
	"context"
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
	RealizedPnLDay float64
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
	AvgCostBasis float64
	CurrentValue float64
	UnrealizedPL float64
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

// Update refreshes the in-memory state from executor positions + realized P&L today.
func (t *Tracker) Update(positions []ExecPosition, cashBalance, realizedPnLDay float64) {
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
	total := cashBalance + invested
	peak := t.state.PeakValue
	if total > peak {
		peak = total
	}
	t.state = State{
		TotalValue:     total,
		Cash:           cashBalance,
		Invested:       invested,
		UnrealizedPnL:  unrealizedPnL,
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

// MaybeSnapshot writes a snapshot if the interval has elapsed.
// Returns the snapshot state written, or nil if interval not elapsed / on error.
func (t *Tracker) MaybeSnapshot(ctx context.Context) *State {
	t.mu.RLock()
	s := t.state
	elapsed := time.Since(t.lastSnapshot)
	t.mu.RUnlock()

	if elapsed < t.snapshotEvery {
		return nil
	}
	_, err := store.GetDB().Exec(ctx, `
		INSERT INTO portfolio_snapshots
		  (total_value, cash, invested, unrealized_pnl, realized_pnl_today, open_positions)
		VALUES ($1,$2,$3,$4,$5,$6)`,
		s.TotalValue, s.Cash, s.Invested, s.UnrealizedPnL, s.RealizedPnLDay, s.OpenPositions,
	)
	if err != nil {
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
func (t *Tracker) MaybeDailySummary(ctx context.Context, startingValue float64,
	tradesOpened, tradesClosed, wins, losses, sigGenerated, sigBlocked int,
	tokIn, tokOut int64,
) {
	today := time.Now().UTC().Truncate(24 * time.Hour)
	t.mu.RLock()
	alreadyWritten := today.Equal(t.lastDailySummary)
	s := t.state
	t.mu.RUnlock()

	if alreadyWritten {
		return
	}
	pnlPct := 0.0
	if startingValue > 0 {
		pnlPct = ((s.TotalValue - startingValue) / startingValue) * 100
	}
	_, err := store.GetDB().Exec(ctx, `
		INSERT INTO daily_summaries (
			date, starting_value, ending_value, pnl, pnl_pct,
			trades_opened, trades_closed, wins, losses,
			signals_generated, signals_blocked,
			model_tokens_input, model_tokens_output
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
		ON CONFLICT (date) DO UPDATE SET
			ending_value=EXCLUDED.ending_value, pnl=EXCLUDED.pnl, pnl_pct=EXCLUDED.pnl_pct,
			trades_opened=EXCLUDED.trades_opened, trades_closed=EXCLUDED.trades_closed,
			wins=EXCLUDED.wins, losses=EXCLUDED.losses,
			signals_generated=EXCLUDED.signals_generated, signals_blocked=EXCLUDED.signals_blocked,
			model_tokens_input=EXCLUDED.model_tokens_input, model_tokens_output=EXCLUDED.model_tokens_output`,
		today.Format("2006-01-02"),
		startingValue, s.TotalValue, s.TotalValue-startingValue, pnlPct,
		tradesOpened, tradesClosed, wins, losses,
		sigGenerated, sigBlocked, tokIn, tokOut,
	)
	if err != nil {
		logger.Warn("daily summary failed", "err", err)
		return
	}
	t.mu.Lock()
	t.lastDailySummary = today
	t.mu.Unlock()
	logger.Info("daily summary written", "date", today.Format("2006-01-02"), "pnl_pct", pnlPct)
}
