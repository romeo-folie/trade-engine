package orb

import (
	"context"

	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/data"
	"github.com/romeo-folie/trade-engine/internal/earnings"
	"github.com/romeo-folie/trade-engine/internal/execution"
	"github.com/romeo-folie/trade-engine/internal/options"
)

// marketDataPricer adapts a MarketDataProvider to the UnderlyingPricer the exit
// manager needs: it returns the underlying's last trade, or the bid/ask mid.
type marketDataPricer struct{ md data.MarketDataProvider }

func (p marketDataPricer) UnderlyingPrice(ctx context.Context, underlying string) (float64, bool) {
	if p.md == nil {
		return 0, false
	}
	q, err := p.md.FetchQuote(ctx, underlying)
	if err != nil || q == nil {
		return 0, false
	}
	if q.Last > 0 {
		return q.Last, true
	}
	if q.Bid > 0 && q.Ask > 0 {
		return (q.Bid + q.Ask) / 2, true
	}
	return 0, false
}

// optionSnapshotsProvider is the read-only option quote surface required by
// the exit manager. *execution.AlpacaBroker and ReadOnlyExecutor both satisfy
// it, so dry-run retains market data without retaining a raw order adapter.
type optionSnapshotsProvider interface {
	GetOptionSnapshots(ctx context.Context, occSymbols []string) ([]execution.OptionSnapshot, error)
}

// chainQuoter adapts option snapshots to the OptionQuoter the time exit uses:
// it returns the mid of a specific contract by OCC symbol.
type chainQuoter struct{ broker optionSnapshotsProvider }

func (c chainQuoter) OptionMid(ctx context.Context, _underlying, occSymbol string) (float64, bool) {
	if c.broker == nil {
		return 0, false
	}
	// Fetch ONLY the held contract, not the entire underlying chain — the exit sweep
	// calls this per open position every 15s, so a full-chain fetch per position is
	// pure waste.
	snaps, err := c.broker.GetOptionSnapshots(ctx, []string{occSymbol})
	if err != nil {
		return 0, false
	}
	for _, s := range snaps {
		if s.OCCSymbol == occSymbol {
			if m := s.Mid(); m > 0 {
				return m, true
			}
			return 0, false
		}
	}
	return 0, false
}

// SetupOptionsORB constructs the options-ORB router and app-managed exit manager
// from config and wires them onto the scheduler. chain and snapshots are
// read-only Alpaca option-data capabilities, while placer is the execution path
// used for all option entries and exits. Keeping them separate ensures dry-run
// never exposes a raw Alpaca mutation path to the router or exit manager.
//
// The earnings provider is store-backed (earnings_calendar, migration 045): it answers
// the earnings-blackout gate from the shared table populated by `te earnings sync`
// (Finnhub coming reports) + `te earnings seed`. Single names still fail closed while the
// table has no dates for them — populate it before enabling single-name underlyings.
//
// IV-RANK SEEDING IS A PREREQUISITE: the IVRankService below reads option_iv_history
// and fails CLOSED below iv_min_samples stored days, so the gate aborts every breakout
// until that table is seeded. Run `te options backfill-iv` before/daily alongside the
// session (e.g. from te-switch) to populate it — otherwise options-ORB places zero
// trades even with a valid OPRA feed.
func (s *ORBScheduler) SetupOptionsORB(
	cfg config.OptionsORBConfig,
	chain options.ChainProvider,
	snapshots optionSnapshotsProvider,
	placer execution.Executor,
) {
	if !cfg.Enabled || chain == nil || snapshots == nil || placer == nil {
		return
	}
	ivrank := options.NewIVRankService(options.NewStoreIVStore(s.db), cfg.IVLookbackDays, cfg.IVMinSamples)
	gates := options.NewEngine(options.GateConfig{
		IVRankCeiling:        cfg.IVRankCeiling,
		MaxDTE:               cfg.MaxDTE,
		DeltaMin:             cfg.DeltaMin,
		DeltaMax:             cfg.DeltaMax,
		SpreadPctCeiling:     cfg.SpreadPctCeiling,
		EarningsBlackoutDays: cfg.EarningsBlackoutDays,
		ETFUnderlyings:       cfg.ETFUnderlyings,
	}, chain, ivrank, earnings.NewStoreProvider(s.db, nil))

	s.SetOptionsRouter(NewOptionsRouter(gates, placer, cfg.Underlyings, cfg.PremiumBudget, cfg.MaxLossPerTrade))
	s.SetOptionExitManager(NewOptionExitManager(marketDataPricer{md: s.marketData}, chainQuoter{broker: snapshots}, placer, cfg.TimeExitMinutes))
	if s.alertedOrphanOptions == nil {
		s.alertedOrphanOptions = make(map[string]struct{})
	}
}
