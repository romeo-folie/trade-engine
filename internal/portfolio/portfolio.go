// Package portfolio tracks portfolio state and records snapshots + daily summaries.
package portfolio

import (
	"context"
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

// Tracker manages portfolio state and persistence.
type Tracker struct {
	state            State
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

// Update refreshes the in-memory state from the executor positions + realized P&L today.
func (t *Tracker) Update(positions []position, cashBalance, realizedPnLDay float64) {
	var invested, unrealizedPnL float64
	for _, p := range positions {
		invested += p.CurrentValue
		unrealizedPnL += p.UnrealizedPL
	}
	total := cashBalance + invested
	if total > t.state.PeakValue {
		t.state.PeakValue = total
	}
	t.state = State{
		TotalValue:     total,
		Cash:           cashBalance,
		Invested:       invested,
		UnrealizedPnL:  unrealizedPnL,
		RealizedPnLDay: realizedPnLDay,
		OpenPositions:  len(positions),
		PeakValue:      t.state.PeakValue,
		LastLossAt:     t.state.LastLossAt,
	}
}

// RecordLoss marks the timestamp of the most-recent losing trade close.
func (t *Tracker) RecordLoss() {
	t.state.LastLossAt = time.Now()
}

// CurrentState returns a copy of the current portfolio state.
func (t *Tracker) CurrentState() State { return t.state }

// MaybeSnapshot writes a snapshot if the interval has elapsed.
func (t *Tracker) MaybeSnapshot(ctx context.Context) {
	if time.Since(t.lastSnapshot) < t.snapshotEvery {
		return
	}
	s := t.state
	_, err := store.GetDB().Exec(ctx, `
		INSERT INTO portfolio_snapshots
		  (total_value, cash, invested, unrealized_pnl, realized_pnl_today, open_positions)
		VALUES ($1,$2,$3,$4,$5,$6)`,
		s.TotalValue, s.Cash, s.Invested, s.UnrealizedPnL, s.RealizedPnLDay, s.OpenPositions,
	)
	if err != nil {
		logger.Warn("portfolio snapshot failed", "err", err)
		return
	}
	t.lastSnapshot = time.Now()
	logger.Debug("portfolio snapshot written", "total", s.TotalValue)
}

// MaybeDailySummary writes a daily_summaries row when the calendar date changes.
func (t *Tracker) MaybeDailySummary(ctx context.Context, startingValue float64,
	tradesOpened, tradesClosed, wins, losses, sigGenerated, sigBlocked int,
	tokIn, tokOut int64,
) {
	today := time.Now().UTC().Truncate(24 * time.Hour)
	if today.Equal(t.lastDailySummary) {
		return
	}
	s := t.state
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
	t.lastDailySummary = today
	logger.Info("daily summary written", "date", today.Format("2006-01-02"), "pnl_pct", pnlPct)
}

// position is a local alias so this package doesn't import execution directly.
type position struct {
	Ticker       string
	CurrentValue float64
	UnrealizedPL float64
}

// FromExecutionPositions converts execution.Position slice → local position slice.
// Call site: portfolio.FromExecutionPositions(executionPositions) before Update().
func FromExecutionPositions(eps interface{ toLocalPositions() []position }) []position {
	return eps.toLocalPositions()
}

// ExecPosition is an external-facing adapter type so callers don't need to import this package's internals.
type ExecPosition struct {
	Ticker       string
	CurrentValue float64
	UnrealizedPL float64
}

// ToPositions converts a slice of ExecPosition to the internal type for Update().
func ToPositions(eps []ExecPosition) []position {
	out := make([]position, len(eps))
	for i, ep := range eps {
		out[i] = position{Ticker: ep.Ticker, CurrentValue: ep.CurrentValue, UnrealizedPL: ep.UnrealizedPL}
	}
	return out
}
