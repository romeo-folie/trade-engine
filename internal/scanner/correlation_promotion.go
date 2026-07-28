package scanner

import (
	"context"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/romeo-folie/trade-engine/internal/store"
)

// enrichPromotionCorrelations attaches pairwise daily-return evidence to the
// scored candidates. Data failures fail open: a missing pair cannot be treated
// as correlated, and the candidate remains in the auditable funnel.
func (s *MarketScanner) enrichPromotionCorrelations(ctx context.Context, scored []scoredCandidate, asOf time.Time) {
	if !s.Cfg.PromotionCorrelationEnabled || len(scored) == 0 || s.DailyBars == nil {
		return
	}
	lookback := s.Cfg.PromotionCorrelationLookbackDays
	if lookback <= 0 {
		lookback = correlationLookbackDays
	}
	minOverlap := s.Cfg.PromotionCorrelationMinOverlap
	if minOverlap <= 0 {
		minOverlap = correlationMinOverlap
	}
	exposure, err := promotionExposureSymbols(ctx)
	if err != nil {
		slog.Warn("scanner: load promotion exposure for correlation", "err", err)
		return
	}
	symbols := make([]string, 0, len(scored)+len(exposure))
	seen := make(map[string]struct{}, len(scored)+len(exposure))
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
	for _, symbol := range exposure {
		add(symbol)
	}
	sort.Strings(symbols)
	if len(symbols) < 2 {
		return
	}
	from := asOf.AddDate(0, 0, -(lookback + 7))
	result, err := s.DailyBars.FetchDailyBars(ctx, symbols, from, asOf, 200)
	if err != nil {
		slog.Warn("scanner: fetch promotion correlation bars", "err", err)
		return
	}
	for i := range scored {
		key := normalizePromotionKey(scored[i].candidate.Ticker)
		scored[i].correlations = make(map[string]float64)
		scored[i].exposureCorrelations = make(map[string]float64)
		for _, peer := range symbols {
			if peer == key {
				continue
			}
			correlation, ok := dailyReturnPearsonWindow(
				result.Bars[key], result.Bars[peer], lookback, minOverlap,
			)
			if !ok {
				continue
			}
			if containsPromotionSymbol(exposure, peer) {
				scored[i].exposureCorrelations[peer] = correlation
			} else {
				scored[i].correlations[peer] = correlation
			}
		}
	}
}

func promotionExposureSymbols(ctx context.Context) ([]string, error) {
	items, err := store.GetWatchlistItems(ctx, true)
	if err != nil {
		return nil, err
	}
	trades, err := store.GetOwnedPositionTrades(ctx)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]struct{}, len(items)+len(trades))
	var out []string
	add := func(symbol string) {
		key := normalizePromotionKey(symbol)
		if key != "" {
			if _, ok := seen[key]; !ok {
				seen[key] = struct{}{}
				out = append(out, key)
			}
		}
	}
	for _, item := range items {
		add(item.Ticker)
	}
	for _, trade := range trades {
		add(trade.Ticker)
	}
	sort.Strings(out)
	return out, nil
}

func containsPromotionSymbol(symbols []string, target string) bool {
	target = normalizePromotionKey(target)
	for _, symbol := range symbols {
		if strings.EqualFold(normalizePromotionKey(symbol), target) {
			return true
		}
	}
	return false
}
