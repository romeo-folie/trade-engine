package portfolio

import (
	"context"
	"math"
	"time"

	"github.com/romeo-folie/trade-engine/internal/execution"
	"github.com/romeo-folie/trade-engine/internal/logger"
	"github.com/romeo-folie/trade-engine/internal/store"
)

// Poller independently polls the broker for account equity and writes
// portfolio_snapshots rows at a configurable interval, regardless of whether
// the engine tick loop is running. This keeps the portfolio chart gapless
// during sleep windows and idle periods.
//
// The poller also writes an extra snapshot whenever equity moves by more than
// SignificantChangePct% since the last snapshot.
type Poller struct {
	bv                 execution.BrokerView
	interval           time.Duration
	sigChangePct       float64 // e.g. 0.05 → write snapshot on >0.05% equity move
	mode               string  // "live" | "paper"
	broker             string  // "alpaca" | "ibkr"
	lastSnapshotEquity float64
}

// NewPoller constructs a Poller. interval is the base poll cadence; zero
// disables the poller (Enabled=false in config). sigChangePct is the equity
// move threshold that triggers an extra snapshot.
func NewPoller(bv execution.BrokerView, interval time.Duration, sigChangePct float64, mode, broker string) *Poller {
	return &Poller{
		bv:           bv,
		interval:     interval,
		sigChangePct: sigChangePct,
		mode:         mode,
		broker:       broker,
	}
}

// Run starts the poll loop. It blocks until ctx is cancelled.
func (p *Poller) Run(ctx context.Context) {
	if p.interval <= 0 {
		logger.Info("portfolio poller disabled (interval=0)")
		return
	}
	logger.Info("portfolio poller started", "interval", p.interval, "mode", p.mode)

	tick := time.NewTicker(p.interval)
	defer tick.Stop()

	// Poll once immediately so the first snapshot doesn't wait a full interval.
	p.poll(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			p.poll(ctx)
		}
	}
}

// poll fetches account state from the broker and writes a snapshot.
// Called on every interval tick (unconditional write) plus whenever
// significant equity moves are detected between ticks. Only skips the
// DB write if the store is not connected — the broker is always called
// so threshold tracking stays accurate even during DB outages.
func (p *Poller) poll(ctx context.Context) {
	acct, err := p.bv.AccountSnapshot(ctx)
	if err != nil {
		logger.Warn("portfolio poller: broker AccountSnapshot failed", "err", err)
		return
	}

	equity := acct.Equity
	cash := acct.Cash
	invested := equity - cash

	// Only write to DB when connected; skip silently otherwise so the poller
	// can resume writing when the DB comes back without losing equity tracking.
	if !store.IsConnected() {
		p.lastSnapshotEquity = equity // keep threshold tracking up-to-date
		return
	}

	// realized_pnl_today stays 0: it is not derivable from a broker account
	// snapshot; the engine Tracker path is responsible for realized P&L.
	openPositions, unrealized, positionStatsAvailable := p.positionStats(ctx)
	if err := store.InsertPortfolioSnapshot(
		ctx, equity, cash, invested, unrealized, 0, openPositions,
		positionStatsAvailable, "poller", p.mode, p.broker,
	); err != nil {
		logger.Warn("portfolio poller: snapshot write failed", "err", err)
		return
	}
	p.lastSnapshotEquity = equity
	logger.Debug("portfolio poller: snapshot written", "equity", equity, "positions", openPositions)
}

// positionStats returns the open-position count and aggregate unrealized P&L
// from the broker. Best-effort: on error it logs and marks the values
// unavailable so the snapshot can still capture equity/cash/invested without
// presenting missing position metrics as legitimate zeros.
// Callers must only invoke it when about to write, to avoid a wasted broker
// call during DB outages.
func (p *Poller) positionStats(ctx context.Context) (count int, unrealized float64, available bool) {
	positions, err := p.bv.PositionsSnapshot(ctx)
	if err != nil {
		logger.Warn("portfolio poller: PositionsSnapshot failed; snapshot omits position stats", "err", err)
		return 0, 0, false
	}
	for _, pos := range positions {
		unrealized += pos.UnrealizedPL
	}
	return len(positions), unrealized, true
}

// pollIfSignificant is called at sub-interval cadence (future) or can be
// triggered by external equity events. It skips the DB write when equity
// hasn't moved past sigChangePct since the last persisted snapshot.
func (p *Poller) pollIfSignificant(ctx context.Context) {
	acct, err := p.bv.AccountSnapshot(ctx)
	if err != nil {
		return
	}
	equity := acct.Equity
	if p.lastSnapshotEquity != 0 {
		changePct := math.Abs((equity-p.lastSnapshotEquity)/p.lastSnapshotEquity) * 100
		if changePct < p.sigChangePct {
			return
		}
	}
	if !store.IsConnected() {
		return
	}
	cash := acct.Cash
	openPositions, unrealized, positionStatsAvailable := p.positionStats(ctx)
	if err := store.InsertPortfolioSnapshot(
		ctx, equity, cash, equity-cash, unrealized, 0, openPositions,
		positionStatsAvailable, "poller", p.mode, p.broker,
	); err != nil {
		logger.Warn("portfolio poller: significant-change snapshot failed", "err", err)
		return
	}
	p.lastSnapshotEquity = equity
	logger.Debug("portfolio poller: significant-change snapshot written", "equity", equity)
}
