// Package pullbackfreq measures the deterministic pullback-in-strength setup
// frequency without invoking the LLM analysis pipeline.
package pullbackfreq

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/romeo-folie/trade-engine/internal/data"
	"github.com/romeo-folie/trade-engine/internal/strategy/pullback"
)

// CandleProvider is the only market-data capability required by the study.
type CandleProvider interface {
	FetchCandles(ctx context.Context, ticker string, tf data.Timeframe, from, to time.Time) ([]data.Candle, error)
}

// Member defines when a ticker belongs to the supplied research cohort. Empty
// bounds mean the full study range. The study ranks only members eligible on a
// given date, preventing future cohort additions from leaking backwards when
// point-in-time membership is available.
type Member struct {
	Ticker       string
	EligibleFrom time.Time
	EligibleTo   time.Time
}

type Config struct {
	From               time.Time
	To                 time.Time
	MinDollarVolume    float64
	TopFraction        float64
	EpisodeGapSessions int
	Pullback           pullback.Config
	Progress           func(current, total int, ticker string)
}

type Episode struct {
	Ticker       string
	Date         time.Time
	Return1M     float64
	Return3M     float64
	Return6M     float64
	Support      float64
	StopLoss     float64
	StopWidthPct float64
}

type TickerStats struct {
	ObservedSessions   int
	LiquidityQualified int
	MomentumQualified  int
	SelectedDays       int
	EligibleDays       int
	Episodes           int
	Blocks             map[string]int
}

type Result struct {
	CohortSize         int
	ObservedSessions   int
	LiquidityQualified int
	MomentumQualified  int
	SelectedDays       int
	EligibleDays       int
	Episodes           int
	Blocks             map[string]int
	ByTicker           map[string]*TickerStats
	EpisodeRows        []Episode
	LoadErrors         map[string]string
}

type evaluator func([]data.Candle, float64, pullback.Config) pullback.Result

type observation struct {
	ticker      string
	candleIndex int
	candle      data.Candle
	setup       pullback.Result
	composite   float64
}

// Run loads one warm-up year before From and analyzes each date using only the
// candle prefix available on that date.
func Run(ctx context.Context, provider CandleProvider, members []Member, cfg Config) (Result, error) {
	if provider == nil {
		return Result{}, fmt.Errorf("pullback frequency: candle provider is nil")
	}
	if cfg.From.IsZero() || cfg.To.IsZero() || cfg.To.Before(cfg.From) {
		return Result{}, fmt.Errorf("pullback frequency: invalid date range")
	}
	series := make(map[string][]data.Candle, len(members))
	loadErrors := make(map[string]string)
	normalized := normalizeMembers(members)
	if len(normalized) == 0 {
		return Result{}, fmt.Errorf("pullback frequency: cohort is empty")
	}
	for i, member := range normalized {
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		candles, err := provider.FetchCandles(
			ctx, member.Ticker, data.Timeframe1Day,
			cfg.From.AddDate(-1, 0, 0), cfg.To.AddDate(0, 0, 1),
		)
		if cfg.Progress != nil {
			cfg.Progress(i+1, len(normalized), member.Ticker)
		}
		if err != nil {
			loadErrors[member.Ticker] = err.Error()
			continue
		}
		if len(candles) == 0 {
			loadErrors[member.Ticker] = "no daily candles returned"
			continue
		}
		series[member.Ticker] = candles
	}
	if len(loadErrors) > 0 {
		return Result{
			CohortSize: len(normalized),
			LoadErrors: loadErrors,
		}, cohortLoadError(loadErrors, len(normalized))
	}
	result := analyze(series, members, cfg, pullback.Evaluate)
	result.LoadErrors = loadErrors
	return result, nil
}

func cohortLoadError(loadErrors map[string]string, cohortSize int) error {
	tickers := make([]string, 0, len(loadErrors))
	for ticker := range loadErrors {
		tickers = append(tickers, ticker)
	}
	sort.Strings(tickers)
	details := make([]string, 0, len(tickers))
	for _, ticker := range tickers {
		details = append(details, fmt.Sprintf("%s: %s", ticker, loadErrors[ticker]))
	}
	return fmt.Errorf(
		"pullback frequency: incomplete cohort (%d of %d ticker loads failed): %s",
		len(loadErrors), cohortSize, strings.Join(details, "; "),
	)
}

func analyze(series map[string][]data.Candle, members []Member, cfg Config, evaluate evaluator) Result {
	cfg = withDefaults(cfg)
	result := Result{
		CohortSize: len(normalizeMembers(members)),
		Blocks:     map[string]int{},
		ByTicker:   map[string]*TickerStats{},
		LoadErrors: map[string]string{},
	}
	memberByTicker := make(map[string]Member, len(members))
	for _, member := range normalizeMembers(members) {
		memberByTicker[member.Ticker] = member
		result.ByTicker[member.Ticker] = &TickerStats{Blocks: map[string]int{}}
	}

	byDate := map[string][]observation{}
	for ticker, candles := range series {
		member, ok := memberByTicker[strings.ToUpper(ticker)]
		if !ok {
			continue
		}
		for i, candle := range candles {
			if candle.Timestamp.Before(cfg.From) || candle.Timestamp.After(endOfDay(cfg.To)) ||
				!memberEligible(member, candle.Timestamp) {
				continue
			}
			stats := result.ByTicker[member.Ticker]
			stats.ObservedSessions++
			result.ObservedSessions++

			window := candles[:i+1]
			setup := evaluate(window, candle.Close, cfg.Pullback)
			dollarVolume := candle.Close * float64(candle.Volume)
			if dollarVolume < cfg.MinDollarVolume {
				stats.Blocks["dollar_volume"]++
				result.Blocks["dollar_volume"]++
				continue
			}
			stats.LiquidityQualified++
			result.LiquidityQualified++
			if maxReturn(setup) < resolvedMomentum(cfg.Pullback) {
				stats.Blocks["momentum"]++
				result.Blocks["momentum"]++
				continue
			}
			stats.MomentumQualified++
			result.MomentumQualified++
			byDate[dateKey(candle.Timestamp)] = append(byDate[dateKey(candle.Timestamp)], observation{
				ticker: member.Ticker, candleIndex: i, candle: candle, setup: setup,
				composite: (setup.Return1M + setup.Return3M + setup.Return6M) / 3,
			})
		}
	}

	dates := make([]string, 0, len(byDate))
	for date := range byDate {
		dates = append(dates, date)
	}
	sort.Strings(dates)
	lastEligibleIndex := map[string]int{}
	for _, date := range dates {
		candidates := byDate[date]
		sort.SliceStable(candidates, func(i, j int) bool {
			if candidates[i].composite == candidates[j].composite {
				return candidates[i].ticker < candidates[j].ticker
			}
			return candidates[i].composite > candidates[j].composite
		})
		keep := int(math.Ceil(float64(len(candidates)) * cfg.TopFraction))
		if keep < 1 {
			keep = 1
		}
		for _, candidate := range candidates[keep:] {
			stats := result.ByTicker[candidate.ticker]
			stats.Blocks["below_top_quartile"]++
			result.Blocks["below_top_quartile"]++
		}
		for _, candidate := range candidates[:keep] {
			stats := result.ByTicker[candidate.ticker]
			stats.SelectedDays++
			result.SelectedDays++
			if !candidate.setup.Eligible {
				check := candidate.setup.FailedCheck
				if check == "" {
					check = "unknown"
				}
				stats.Blocks[check]++
				result.Blocks[check]++
				continue
			}
			stats.EligibleDays++
			result.EligibleDays++
			last, seen := lastEligibleIndex[candidate.ticker]
			if !seen || candidate.candleIndex-last > cfg.EpisodeGapSessions+1 {
				stats.Episodes++
				result.Episodes++
				result.EpisodeRows = append(result.EpisodeRows, Episode{
					Ticker: candidate.ticker, Date: candidate.candle.Timestamp,
					Return1M: candidate.setup.Return1M, Return3M: candidate.setup.Return3M,
					Return6M: candidate.setup.Return6M, Support: candidate.setup.Support,
					StopLoss: candidate.setup.StopLoss, StopWidthPct: candidate.setup.StopWidthPct,
				})
			}
			lastEligibleIndex[candidate.ticker] = candidate.candleIndex
		}
	}
	return result
}

func withDefaults(cfg Config) Config {
	if cfg.MinDollarVolume <= 0 {
		cfg.MinDollarVolume = 1_000_000
	}
	if cfg.TopFraction <= 0 || cfg.TopFraction > 1 {
		cfg.TopFraction = 0.25
	}
	if cfg.EpisodeGapSessions < 0 {
		cfg.EpisodeGapSessions = 0
	}
	return cfg
}

func normalizeMembers(members []Member) []Member {
	seen := map[string]bool{}
	out := make([]Member, 0, len(members))
	for _, member := range members {
		member.Ticker = strings.ToUpper(strings.TrimSpace(member.Ticker))
		if member.Ticker == "" || seen[member.Ticker] {
			continue
		}
		seen[member.Ticker] = true
		out = append(out, member)
	}
	return out
}

func memberEligible(member Member, date time.Time) bool {
	if !member.EligibleFrom.IsZero() && date.Before(startOfDay(member.EligibleFrom)) {
		return false
	}
	return member.EligibleTo.IsZero() || !date.After(endOfDay(member.EligibleTo))
}

func resolvedMomentum(cfg pullback.Config) float64 {
	if cfg.MinMomentumReturnPct <= 0 {
		return 30
	}
	return cfg.MinMomentumReturnPct
}

func maxReturn(setup pullback.Result) float64 {
	return math.Max(setup.Return1M, math.Max(setup.Return3M, setup.Return6M))
}

func dateKey(t time.Time) string { return t.UTC().Format("2006-01-02") }

func startOfDay(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, t.Location())
}

func endOfDay(t time.Time) time.Time { return startOfDay(t).Add(24*time.Hour - time.Nanosecond) }
