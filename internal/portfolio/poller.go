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
	bv                  execution.BrokerView
	interval            time.Duration
	sigChangePct        float64 // e.g. 0.05 → write snapshot on >0.05% equity move
	mode                string  // "live" | "paper"
	broker              string  // "alpaca" | "ibkr"
	lastSnapshotEquity  float64
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

// poll fetches account state from the broker and writes a snapshot if needed.
// It always calls the broker (to track threshold changes) but only writes to
// the DB when the store is connected.
func (p *Poller) poll(ctx context.Context) {
	acct, err := p.bv.AccountSnapshot(ctx)
	if err != nil {
		logger.Warn("portfolio poller: broker AccountSnapshot failed", "err", err)
		return
	}

	equity := acct.Equity
	cash := acct.Cash
	invested := equity - cash

	// Decide whether to write a snapshot.
	write := p.lastSnapshotEquity == 0
	if !write && p.lastSnapshotEquity != 0 {
		changePct := math.Abs((equity-p.lastSnapshotEquity)/p.lastSnapshotEquity) * 100
		write = changePct >= p.sigChangePct
	}

	if !write {
		return
	}

	// Only write to DB when connected; skip silently otherwise so the poller
	// can resume writing when the DB comes back without missing threshold events.
	if !store.IsConnected() {
		return
	}

	if err := store.InsertPortfolioSnapshot(
		ctx, equity, cash, invested, 0, 0, 0,
		"poller", p.mode, p.broker,
	); err != nil {
		logger.Warn("portfolio poller: snapshot write failed", "err", err)
		return
	}
	p.lastSnapshotEquity = equity
	logger.Debug("portfolio poller: snapshot written", "equity", equity)
}
