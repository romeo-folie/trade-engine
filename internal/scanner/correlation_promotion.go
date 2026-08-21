package scanner

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/romeo-folie/trade-engine/internal/store"
)

// enrichPromotionCorrelations attaches pairwise daily-return evidence to the
// scored candidates. Transport/provider failures fail closed, while individual
// symbols with insufficient bars are left without correlation evidence and the
// candidate remains in the auditable funnel.
type promotionExposure struct {
	watchlist map[string]struct{}
	positions map[string]struct{}
	all       []string
}

const correlationCalendarBufferDays = 14

func (s *MarketScanner) enrichPromotionCorrelations(ctx context.Context, scored []scoredCandidate, asOf time.Time, exposure promotionExposure) error {
	if !s.Cfg.PromotionCorrelationEnabled || len(scored) == 0 || s.DailyBars == nil {
		return nil
	}
	lookback := s.Cfg.PromotionCorrelationLookbackDays
	if lookback <= 0 {
		lookback = correlationLookbackDays
	}
	minOverlap := s.Cfg.PromotionCorrelationMinOverlap
	if minOverlap <= 0 {
		minOverlap = correlationMinOverlap
	}
	symbols := make([]string, 0, len(scored)+len(exposure.all))
	seen := make(map[string]struct{}, len(scored)+len(exposure.all))
	add := func(symbol string) {
		key := normalizePromotionKey(symbol)
		if key == "" {
			return
		}
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		symbols = append(symbols, key)
	}
	for _, item := range scored {
		add(item.candidate.Ticker)
	}
	for _, symbol := range exposure.all {
		add(symbol)
	}
	sort.Strings(symbols)
	if len(symbols) < 2 {
		return nil
	}
	// A lookback is measured in trading sessions, while the provider range is
	// calendar-based. Leave room for weekends, holidays, and a partial final
	// session so short configured lookbacks still receive enough completed bars.
	from := asOf.AddDate(0, 0, -(lookback*2 + correlationCalendarBufferDays))
	// Daily bars must be completed sessions. The scanner may run intraday, so
	// bound the provider request at the prior UTC day to exclude a partial bar.
	to := asOf.UTC().Truncate(24 * time.Hour).Add(-time.Nanosecond)
	batchSize := s.Cfg.Universe.BatchSize
	if batchSize <= 0 {
		batchSize = 200
	}
	result, err := s.DailyBars.FetchDailyBars(ctx, symbols, from, to, batchSize)
	if err != nil {
		return fmt.Errorf("fetch promotion correlation bars: %w", err)
	}
	for i := range scored {
		key := normalizePromotionKey(scored[i].candidate.Ticker)
		scored[i].correlations = make(map[string]float64)
		scored[i].exposureCorrelations = make(map[string]float64)
		scored[i].correlationUnavailable = make(map[string]string)
		for _, peer := range symbols {
			if peer == key {
				continue
			}
			if failure, failed := result.Failures[peer]; failed {
				scored[i].correlationUnavailable[peer] = failure.Error()
				continue
			}
			correlation, ok := dailyReturnPearsonWindow(
				result.Bars[key], result.Bars[peer], lookback, minOverlap,
			)
			if !ok {
				continue
			}
			if _, ok := exposure.watchlist[peer]; ok {
				scored[i].exposureCorrelations[peer] = correlation
			} else if _, ok := exposure.positions[peer]; ok {
				scored[i].exposureCorrelations[peer] = correlation
			} else {
				scored[i].correlations[peer] = correlation
			}
		}
		if failure, failed := result.Failures[key]; failed {
			scored[i].correlationUnavailable[key] = failure.Error()
		}
	}
	return nil
}

func loadPromotionExposure(ctx context.Context) (promotionExposure, error) {
	items, err := store.GetWatchlistItems(ctx, true)
	if err != nil {
		return promotionExposure{}, err
	}
	trades, err := store.GetOwnedPositionTrades(ctx)
	if err != nil {
		return promotionExposure{}, err
	}
	exposure := promotionExposure{
		watchlist: make(map[string]struct{}, len(items)),
		positions: make(map[string]struct{}, len(trades)),
	}
	seen := make(map[string]struct{}, len(items)+len(trades))
	add := func(target map[string]struct{}, symbol string) {
		key := normalizePromotionKey(symbol)
		if key != "" {
			target[key] = struct{}{}
			if _, ok := seen[key]; !ok {
				seen[key] = struct{}{}
				exposure.all = append(exposure.all, key)
			}
		}
	}
	for _, item := range items {
		if expiredScannerResidency(item, time.Now()) {
			continue
		}
		add(exposure.watchlist, item.Ticker)
	}
	for _, trade := range trades {
		add(exposure.positions, trade.Ticker)
	}
	sort.Strings(exposure.all)
	return exposure, nil
}
