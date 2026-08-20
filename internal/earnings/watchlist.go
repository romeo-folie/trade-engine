package earnings

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/data"
	"github.com/romeo-folie/trade-engine/internal/engine"
	"github.com/romeo-folie/trade-engine/internal/store"
	"github.com/romeo-folie/trade-engine/internal/tradeprofile"
)

type MarketCalendarFetcher interface {
	FetchMarketEarningsCalendar(context.Context, time.Time, time.Time) ([]data.EarningsRelease, error)
}

// WatchlistFeeder selects liquid, next-session earnings names and attaches an
// independently expiring context to the ordinary watchlist funnel.
type WatchlistFeeder struct {
	Calendar MarketCalendarFetcher
	Bars     data.BulkDailyBarsProvider
	Cfg      config.EarningsWatchlistConfig
	Now      func() time.Time
}

type rankedRelease struct {
	release data.EarningsRelease
	dollar  float64
}

func (f *WatchlistFeeder) RunSession(ctx context.Context, completed engine.USMarketSession, _ time.Time) (err error) {
	if f.Calendar == nil || f.Bars == nil {
		return fmt.Errorf("earnings watchlist requires calendar and daily-bars providers")
	}
	if err := store.ExpireWatchlistDiscoveryContexts(ctx); err != nil {
		return fmt.Errorf("expire discovery contexts: %w", err)
	}
	target := engine.NextMarketSession(completed.Date)
	if !f.now().Before(target.Open) {
		// Startup during the target session must not pre-publish tomorrow with
		// yesterday's ranking data. The ordinary after-close run will use the
		// completed session that actually precedes tomorrow.
		return nil
	}
	attemptID, claimed, err := store.ClaimEarningsWatchlistRun(ctx, target.Date, f.now())
	if err != nil {
		return err
	}
	if !claimed {
		leaseHeld, availabilityErr := store.EarningsWatchlistRunLeaseHeld(ctx, target.Date, f.now())
		if availabilityErr != nil {
			return availabilityErr
		}
		if leaseHeld {
			return store.ErrEarningsWatchlistRunLeased
		}
		return nil
	}
	defer func() {
		if err != nil {
			_ = store.FailEarningsWatchlistRun(ctx, target.Date, attemptID, err)
		}
	}()
	releases, err := f.Calendar.FetchMarketEarningsCalendar(ctx, target.Date, target.Date)
	if err != nil {
		return fmt.Errorf("fetch market earnings calendar for %s: %w", target.Date.Format("2006-01-02"), err)
	}
	unique := nextSessionReleases(releases, target.Date)
	if len(unique) == 0 {
		return store.CompleteEarningsWatchlistRun(ctx, target.Date, attemptID)
	}
	symbols := make([]string, 0, len(unique))
	for _, release := range unique {
		symbols = append(symbols, release.Symbol)
	}
	from := completed.Date.AddDate(0, 0, -35)
	to := completed.Date.AddDate(0, 0, 1)
	barResult, err := f.Bars.FetchDailyBars(ctx, symbols, from, to, f.Cfg.BarBatchSize)
	if err != nil {
		return fmt.Errorf("fetch ranking daily bars: %w", err)
	}
	ranked := rankReleases(unique, barResult.Bars)
	cap := f.Cfg.CandidateCap
	if cap > len(ranked) {
		cap = len(ranked)
	}
	expires := engine.MarketSessionOffset(target.Date, f.Cfg.WatchlistSessions-1).Close
	candidates := make([]store.EarningsWatchlistCandidate, 0, cap)
	for index, candidate := range ranked[:cap] {
		candidates = append(candidates, store.EarningsWatchlistCandidate{
			Ticker: candidate.release.Symbol, ContextKey: "earnings_scheduled:" + target.Date.Format("2006-01-02"),
			Context: earningsDiscoveryContext(candidate.release, target, index+1, candidate.dollar), ExpiresAt: expires,
			LiquidityRank: index + 1, MedianDollarVolume: candidate.dollar,
		})
	}
	return store.PublishEarningsWatchlistRun(ctx, target.Date, attemptID, candidates)
}

func nextSessionReleases(releases []data.EarningsRelease, target time.Time) []data.EarningsRelease {
	seen := make(map[string]data.EarningsRelease)
	for _, release := range releases {
		symbol := strings.ToUpper(strings.TrimSpace(release.Symbol))
		if symbol == "" || !sameETDate(release.Date, target) {
			continue
		}
		release.Symbol = symbol
		if current, ok := seen[symbol]; !ok || strings.TrimSpace(release.Hour) < strings.TrimSpace(current.Hour) {
			seen[symbol] = release
		}
	}
	out := make([]data.EarningsRelease, 0, len(seen))
	for _, release := range seen {
		out = append(out, release)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Symbol < out[j].Symbol })
	return out
}

func rankReleases(releases []data.EarningsRelease, bars map[string][]data.Candle) []rankedRelease {
	ranked := make([]rankedRelease, 0, len(releases))
	for _, release := range releases {
		volumes := make([]float64, 0, len(bars[release.Symbol]))
		for _, bar := range bars[release.Symbol] {
			if bar.Close > 0 && bar.Volume > 0 {
				volumes = append(volumes, bar.Close*float64(bar.Volume))
			}
		}
		if len(volumes) == 0 {
			continue
		}
		sort.Float64s(volumes)
		middle := len(volumes) / 2
		median := volumes[middle]
		if len(volumes)%2 == 0 {
			median = (volumes[middle-1] + volumes[middle]) / 2
		}
		ranked = append(ranked, rankedRelease{release: release, dollar: median})
	}
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].dollar == ranked[j].dollar {
			return ranked[i].release.Symbol < ranked[j].release.Symbol
		}
		return ranked[i].dollar > ranked[j].dollar
	})
	return ranked
}

func (f *WatchlistFeeder) now() time.Time {
	if f.Now != nil {
		return f.Now()
	}
	return time.Now()
}

func earningsDiscoveryContext(release data.EarningsRelease, target engine.USMarketSession, rank int, dollar float64) tradeprofile.DiscoveryContext {
	evidence, _ := json.Marshal(map[string]any{
		"scheduled_date": target.Date.Format("2006-01-02"), "session": strings.TrimSpace(release.Hour),
		"calendar_source": "finnhub", "liquidity_rank": rank, "median_dollar_volume": dollar,
	})
	return tradeprofile.DiscoveryContext{
		Version: tradeprofile.DiscoveryContextVersion, Sources: []string{"earnings_scheduled"},
		Thesis:           "Scheduled earnings event; assess ordinary news, technical, and market evidence.",
		EvidenceBySource: map[string]json.RawMessage{"earnings_scheduled": evidence},
	}
}

func sameETDate(left, right time.Time) bool {
	// Finnhub's date field is a calendar date, parsed as UTC midnight by the
	// existing adapter. It must not be converted to an instant in ET (which
	// would turn it into the preceding date); compare its provider calendar
	// representation with the exchange-session calendar date instead.
	return left.Format("2006-01-02") == right.Format("2006-01-02")
}
