package orb

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/data"
	"github.com/romeo-folie/trade-engine/internal/store"
)

const (
	StrategyORB               = "orb"
	StrategyTrendContinuation = "trend_continuation"

	TrendFilterNone       = "none"
	TrendFilterAligned    = "aligned"
	TrendFilterContrarian = "contrarian"
)

// RangeBand is a half-open opening-range-width filter in pip_buffer units: [Min, Max).
type RangeBand struct {
	Min float64
	Max float64
}

// ParseRangeBands parses comma-separated min:max bands, e.g. "32:60,80:120".
func ParseRangeBands(s string) ([]RangeBand, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	parts := strings.Split(s, ",")
	out := make([]RangeBand, 0, len(parts))
	for _, raw := range parts {
		part := strings.TrimSpace(raw)
		if part == "" {
			return nil, fmt.Errorf("empty band")
		}
		lo, hi, ok := strings.Cut(part, ":")
		if !ok {
			return nil, fmt.Errorf("%q must use min:max", part)
		}
		min, err := strconv.ParseFloat(strings.TrimSpace(lo), 64)
		if err != nil {
			return nil, fmt.Errorf("%q min is not a number", part)
		}
		max, err := strconv.ParseFloat(strings.TrimSpace(hi), 64)
		if err != nil {
			return nil, fmt.Errorf("%q max is not a number", part)
		}
		if min < 0 || max <= 0 {
			return nil, fmt.Errorf("%q bounds must be non-negative with max > 0", part)
		}
		if min >= max {
			return nil, fmt.Errorf("%q min must be less than max", part)
		}
		out = append(out, RangeBand{Min: min, Max: max})
	}
	return out, nil
}

func FormatRangeBands(bands []RangeBand) string {
	if len(bands) == 0 {
		return "none"
	}
	parts := make([]string, 0, len(bands))
	for _, band := range bands {
		parts = append(parts, fmt.Sprintf("%.1f:%.1f", band.Min, band.Max))
	}
	return strings.Join(parts, ",")
}

func RangeExcluded(rangePips float64, bands []RangeBand) bool {
	for _, band := range bands {
		if rangePips >= band.Min && rangePips < band.Max {
			return true
		}
	}
	return false
}

type TrendContinuationParams struct {
	DelayMinutes        int
	ConfirmBars         int
	DepthMult           float64
	TrendFilter         string
	TrendLookbackMinute int
	MinRangePips        float64
	MaxRangePips        float64
	ExcludeRange        []RangeBand
}

func IsTrendContinuation(inst config.Instrument) bool {
	return inst.Strategy == StrategyTrendContinuation
}

func TrendContinuationParamsFromInstrument(inst config.Instrument) (TrendContinuationParams, error) {
	bands, err := ParseRangeBands(inst.ContinuationExcludeRangePips)
	if err != nil {
		return TrendContinuationParams{}, err
	}
	filter := inst.ContinuationTrendFilter
	if filter == "" {
		filter = TrendFilterNone
	}
	return TrendContinuationParams{
		DelayMinutes:        inst.ContinuationDelayMinutes,
		ConfirmBars:         inst.ContinuationConfirmBars,
		DepthMult:           inst.ContinuationDepthMult,
		TrendFilter:         filter,
		TrendLookbackMinute: inst.ContinuationTrendLookbackMin,
		MinRangePips:        inst.ContinuationMinRangePips,
		MaxRangePips:        inst.ContinuationMaxRangePips,
		ExcludeRange:        bands,
	}, nil
}

type TrendContinuationSignal struct {
	Bracket    BracketParams
	Side       string
	EntryTime  time.Time
	EntryIndex int
	RangePips  float64
}

// EvaluateTrendContinuation is the shared signal definition for the delayed
// trend-continuation strategy. EntryIndex is relative to the post-range watch slice:
// candles with Timestamp >= sessionOpen + cfg.RangeMinutes and <= now.
func EvaluateTrendContinuation(candles []data.Candle, r *OpeningRange, sessionOpen, now time.Time, inst config.Instrument, cfg config.ForexORBConfig, params TrendContinuationParams, portfolioValue float64) (TrendContinuationSignal, bool) {
	if r == nil || inst.PipBuffer <= 0 || params.DelayMinutes <= 0 || params.ConfirmBars <= 0 || params.DepthMult <= 0 {
		return TrendContinuationSignal{}, false
	}
	ordered := append([]data.Candle(nil), candles...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Timestamp.Before(ordered[j].Timestamp) })

	rangeWidth := r.High - r.Low
	if rangeWidth < inst.PipBuffer*2 {
		return TrendContinuationSignal{}, false
	}
	rangePips := rangeWidth / inst.PipBuffer
	if params.MinRangePips > 0 && rangePips < params.MinRangePips {
		return TrendContinuationSignal{}, false
	}
	if params.MaxRangePips > 0 && rangePips > params.MaxRangePips {
		return TrendContinuationSignal{}, false
	}
	if RangeExcluded(rangePips, params.ExcludeRange) {
		return TrendContinuationSignal{}, false
	}

	rangeEnd := sessionOpen.Add(time.Duration(cfg.RangeMinutes) * time.Minute)
	watch := make([]data.Candle, 0, len(ordered))
	for _, c := range ordered {
		if !c.Timestamp.Before(rangeEnd) && !c.Timestamp.After(now) {
			watch = append(watch, c)
		}
	}
	if len(watch) < params.ConfirmBars+2 {
		return TrendContinuationSignal{}, false
	}

	decisionTime := rangeEnd.Add(time.Duration(params.DelayMinutes) * time.Minute)
	if now.Before(decisionTime) {
		return TrendContinuationSignal{}, false
	}
	decisionIdx := -1
	for i, c := range watch {
		if !c.Timestamp.Before(decisionTime) {
			decisionIdx = i
			break
		}
	}
	if decisionIdx < params.ConfirmBars-1 || decisionIdx+1 >= len(watch) {
		return TrendContinuationSignal{}, false
	}

	confirm := watch[decisionIdx-params.ConfirmBars+1 : decisionIdx+1]
	depth := params.DepthMult * inst.PipBuffer
	longOK, shortOK := true, true
	for _, c := range confirm {
		if c.Close < r.High+depth {
			longOK = false
		}
		if c.Close > r.Low-depth {
			shortOK = false
		}
	}
	if longOK == shortOK {
		return TrendContinuationSignal{}, false
	}
	side := "sell"
	if longOK {
		side = "buy"
	}
	if !trendContinuationTrendAllowed(ordered, params, sessionOpen, side) {
		return TrendContinuationSignal{}, false
	}

	entryIdx := decisionIdx + 1
	entry := watch[entryIdx].Open
	stop := r.High + inst.PipBuffer
	if side == "buy" {
		stop = r.Low - inst.PipBuffer
		if stop >= entry {
			return TrendContinuationSignal{}, false
		}
	} else if stop <= entry {
		return TrendContinuationSignal{}, false
	}

	bracketCfg := cfg
	if inst.RiskReward > 0 {
		bracketCfg.RiskReward = inst.RiskReward
	}
	bracket := ComputeRetestBracket(inst.Symbol, side, entry, stop, portfolioValue, bracketCfg, inst)
	return TrendContinuationSignal{
		Bracket:    bracket,
		Side:       side,
		EntryTime:  watch[entryIdx].Timestamp,
		EntryIndex: entryIdx,
		RangePips:  rangePips,
	}, true
}

func trendContinuationTrendAllowed(candles []data.Candle, p TrendContinuationParams, sessionOpen time.Time, side string) bool {
	if p.TrendFilter == "" || p.TrendFilter == TrendFilterNone {
		return true
	}
	if p.TrendLookbackMinute <= 0 {
		return false
	}
	lookbackStart := sessionOpen.Add(-time.Duration(p.TrendLookbackMinute) * time.Minute)
	var first, last *data.Candle
	for i := range candles {
		c := &candles[i]
		if c.Timestamp.Before(lookbackStart) || !c.Timestamp.Before(sessionOpen) {
			continue
		}
		if first == nil {
			first = c
		}
		last = c
	}
	if first == nil || last == nil || first.Timestamp.Equal(last.Timestamp) {
		return false
	}
	up := last.Close > first.Close
	down := last.Close < first.Close
	if !up && !down {
		return false
	}
	aligned := (side == "buy" && up) || (side == "sell" && down)
	if p.TrendFilter == TrendFilterAligned {
		return aligned
	}
	return !aligned
}

func (s *ORBScheduler) trendContinuationEntry(ctx context.Context, ticker string, r *OpeningRange, cfg config.ForexORBConfig, inst config.Instrument, portfolioValue float64) (TrendContinuationSignal, bool) {
	if s.marketData == nil || r == nil {
		return TrendContinuationSignal{}, false
	}
	params, err := TrendContinuationParamsFromInstrument(inst)
	if err != nil {
		slog.Warn("orb: trend-continuation: invalid parameters", "ticker", ticker, "err", err)
		return TrendContinuationSignal{}, false
	}
	sessionTZ := s.sessionTimezone()
	openMinutes := s.sessionOpenMinutes()
	sessionDay := s.now().In(sessionTZ)
	sessionOpen := time.Date(sessionDay.Year(), sessionDay.Month(), sessionDay.Day(), openMinutes/60, openMinutes%60, 0, 0, sessionTZ)
	fetchStart := sessionOpen
	if params.TrendFilter != "" && params.TrendFilter != TrendFilterNone && params.TrendLookbackMinute > 0 {
		fetchStart = sessionOpen.Add(-time.Duration(params.TrendLookbackMinute) * time.Minute)
	}
	candles, err := s.marketData.FetchCandles(ctx, ticker, data.Timeframe1Min, fetchStart, s.now())
	if err != nil {
		slog.Warn("orb: trend-continuation: fetch candles failed — not entering this tick", "ticker", ticker, "err", err)
		return TrendContinuationSignal{}, false
	}
	return EvaluateTrendContinuation(candles, r, sessionOpen, s.now(), inst, cfg, params, portfolioValue)
}

func (s *ORBScheduler) continuationMonthlyStopHit(ctx context.Context, ticker string, limit float64) (bool, error) {
	if limit <= 0 || s.db == nil {
		return false, nil
	}
	now := s.now().UTC()
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	nextMonth := monthStart.AddDate(0, 1, 0)
	pnl, err := store.SumClosedORBPNLForTicker(ctx, s.db, ticker, monthStart, nextMonth)
	if err != nil {
		return false, err
	}
	return pnl <= -limit, nil
}
