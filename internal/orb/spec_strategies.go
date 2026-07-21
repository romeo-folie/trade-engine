package orb

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"time"

	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/data"
)

const (
	StrategySweepReclaim = "sweep_reclaim"
	StrategyOpeningShock = "opening_shock"
)

type StrategySignal struct {
	Bracket   BracketParams
	Side      string
	EntryTime time.Time
}

func IsSweepReclaim(inst config.Instrument) bool {
	return inst.Strategy == StrategySweepReclaim
}

func IsOpeningShock(inst config.Instrument) bool {
	return inst.Strategy == StrategyOpeningShock
}

func strategySideAllowed(mode, side string) bool {
	switch mode {
	case "", "both":
		return true
	case "long":
		return side == "buy"
	case "short":
		return side == "sell"
	default:
		return false
	}
}

func candleHighLow(bars []data.Candle) (float64, float64) {
	hi, lo := bars[0].High, bars[0].Low
	for _, b := range bars[1:] {
		hi = math.Max(hi, b.High)
		lo = math.Min(lo, b.Low)
	}
	return hi, lo
}

func entryBarCurrent(candles []data.Candle, now, entryTime time.Time) bool {
	if !entryTimeCurrent(now, entryTime) {
		return false
	}
	var latest time.Time
	for _, c := range candles {
		if c.Timestamp.After(now) {
			continue
		}
		if latest.IsZero() || c.Timestamp.After(latest) {
			latest = c.Timestamp
		}
	}
	return latest.Equal(entryTime)
}

func entryTimeCurrent(now, entryTime time.Time) bool {
	return !entryTime.IsZero() && !now.Before(entryTime) && now.Before(entryTime.Add(time.Minute))
}

func strategyBracketConfig(cfg config.ForexORBConfig, inst config.Instrument) config.ForexORBConfig {
	if inst.RiskReward > 0 {
		cfg.RiskReward = inst.RiskReward
	}
	return cfg
}

func percentileNearestRank(values []float64, pct float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	rank := int(math.Ceil(pct/100*float64(len(sorted)))) - 1
	if rank < 0 {
		rank = 0
	}
	if rank >= len(sorted) {
		rank = len(sorted) - 1
	}
	return sorted[rank]
}

type ReclaimParams struct {
	LevelSet           string
	SweepDepthMult     float64
	EntryWindowMinutes int
	SideMode           string
}

func ReclaimParamsFromInstrument(inst config.Instrument) ReclaimParams {
	return ReclaimParams{
		LevelSet:           inst.SweepReclaimLevelSet,
		SweepDepthMult:     inst.SweepReclaimDepthMult,
		EntryWindowMinutes: inst.SweepReclaimEntryWindowMinutes,
		SideMode:           inst.SideMode,
	}
}

func EvaluateSweepReclaim(candles []data.Candle, fetchStart, levelEnd, sessionOpen, now time.Time, inst config.Instrument, cfg config.ForexORBConfig, params ReclaimParams, portfolioValue float64) (StrategySignal, bool, bool) {
	if inst.PipBuffer <= 0 || params.SweepDepthMult <= 0 || params.EntryWindowMinutes <= 0 {
		return StrategySignal{}, false, false
	}
	ordered := append([]data.Candle(nil), candles...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Timestamp.Before(ordered[j].Timestamp) })

	var levelBars, watch []data.Candle
	for _, c := range ordered {
		switch {
		case !c.Timestamp.Before(fetchStart) && c.Timestamp.Before(levelEnd):
			levelBars = append(levelBars, c)
		case !c.Timestamp.Before(sessionOpen) && !c.Timestamp.After(now):
			watch = append(watch, c)
		}
	}
	entryDeadline := sessionOpen.Add(time.Duration(params.EntryWindowMinutes) * time.Minute)
	if len(levelBars) == 0 || len(watch) < 2 {
		return StrategySignal{}, now.Before(entryDeadline), false
	}

	hi, lo := candleHighLow(levelBars)
	levels := []struct {
		price     float64
		sweepSide string
	}{
		{price: hi, sweepSide: "up"},
		{price: lo, sweepSide: "down"},
	}
	depth := params.SweepDepthMult * inst.PipBuffer
	for i := 0; i+1 < len(watch); i++ {
		bar := watch[i]
		if !bar.Timestamp.Before(entryDeadline) {
			break
		}
		for _, lvl := range levels {
			switch lvl.sweepSide {
			case "up":
				if bar.High >= lvl.price+depth && bar.Close < lvl.price {
					entryTime := watch[i+1].Timestamp
					if !entryTimeCurrent(now, entryTime) {
						continue
					}
					entry := watch[i+1].Open
					stop := bar.High + inst.PipBuffer
					if stop <= entry || !strategySideAllowed(params.SideMode, "sell") {
						continue
					}
					return StrategySignal{
						Bracket:   ComputeRetestBracket(inst.Symbol, "sell", entry, stop, portfolioValue, strategyBracketConfig(cfg, inst), inst),
						Side:      "sell",
						EntryTime: entryTime,
					}, false, true
				}
			case "down":
				if bar.Low <= lvl.price-depth && bar.Close > lvl.price {
					entryTime := watch[i+1].Timestamp
					if !entryTimeCurrent(now, entryTime) {
						continue
					}
					entry := watch[i+1].Open
					stop := bar.Low - inst.PipBuffer
					if stop >= entry || !strategySideAllowed(params.SideMode, "buy") {
						continue
					}
					return StrategySignal{
						Bracket:   ComputeRetestBracket(inst.Symbol, "buy", entry, stop, portfolioValue, strategyBracketConfig(cfg, inst), inst),
						Side:      "buy",
						EntryTime: entryTime,
					}, false, true
				}
			}
		}
	}
	return StrategySignal{}, now.Before(entryDeadline), false
}

type OpeningShockParams struct {
	WindowMinutes int
	Lookback      int
	Percentile    float64
	StopMult      float64
	EntryDelayMin int
	SideMode      string
}

func OpeningShockParamsFromInstrument(inst config.Instrument) OpeningShockParams {
	return OpeningShockParams{
		WindowMinutes: inst.OpeningShockWindowMinutes,
		Lookback:      inst.OpeningShockLookback,
		Percentile:    inst.OpeningShockPercentile,
		StopMult:      inst.OpeningShockStopMult,
		EntryDelayMin: inst.OpeningShockEntryDelayMin,
		SideMode:      inst.SideMode,
	}
}

func EvaluateOpeningShock(candles []data.Candle, history []float64, sessionOpen, now time.Time, inst config.Instrument, cfg config.ForexORBConfig, params OpeningShockParams, portfolioValue float64) (StrategySignal, bool, bool) {
	if inst.PipBuffer <= 0 || params.WindowMinutes <= 0 || params.Lookback <= 0 || params.Percentile <= 0 || params.Percentile >= 100 || params.StopMult <= 0 || params.EntryDelayMin < 0 {
		return StrategySignal{}, false, false
	}
	ordered := append([]data.Candle(nil), candles...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Timestamp.Before(ordered[j].Timestamp) })

	shockEnd := sessionOpen.Add(time.Duration(params.WindowMinutes) * time.Minute)
	entryTime := shockEnd.Add(time.Duration(params.EntryDelayMin) * time.Minute)
	if now.Before(entryTime) {
		return StrategySignal{}, true, false
	}

	var shockBars, watch []data.Candle
	for _, c := range ordered {
		switch {
		case !c.Timestamp.Before(sessionOpen) && c.Timestamp.Before(shockEnd):
			shockBars = append(shockBars, c)
		case !c.Timestamp.Before(shockEnd) && !c.Timestamp.After(now):
			watch = append(watch, c)
		}
	}
	if len(shockBars) == 0 || len(watch) < 1 {
		return StrategySignal{}, true, false
	}
	open := shockBars[0].Open
	close := shockBars[len(shockBars)-1].Close
	move := close - open
	absMove := math.Abs(move)
	if absMove <= 0 || len(history) < params.Lookback {
		return StrategySignal{}, false, false
	}
	if absMove < percentileNearestRank(history, params.Percentile) {
		return StrategySignal{}, false, false
	}
	side := "sell"
	if move < 0 {
		side = "buy"
	}
	if !strategySideAllowed(params.SideMode, side) {
		return StrategySignal{}, false, false
	}

	entryIdx := -1
	for i, c := range watch {
		if !c.Timestamp.Before(entryTime) {
			entryIdx = i
			break
		}
	}
	if entryIdx < 0 {
		return StrategySignal{}, true, false
	}

	hi, lo := candleHighLow(shockBars)
	entry := watch[entryIdx].Open
	stopBuffer := params.StopMult * inst.PipBuffer
	stop := hi + stopBuffer
	if side == "buy" {
		stop = lo - stopBuffer
	}
	if (side == "sell" && stop <= entry) || (side == "buy" && stop >= entry) {
		return StrategySignal{}, false, false
	}
	return StrategySignal{
		Bracket:   ComputeRetestBracket(inst.Symbol, side, entry, stop, portfolioValue, strategyBracketConfig(cfg, inst), inst),
		Side:      side,
		EntryTime: watch[entryIdx].Timestamp,
	}, false, true
}

func (s *ORBScheduler) strategySessionOpen() time.Time {
	sessionTZ := s.sessionTimezone()
	openMinutes := s.sessionOpenMinutes()
	sessionDay := s.now().In(sessionTZ)
	return time.Date(sessionDay.Year(), sessionDay.Month(), sessionDay.Day(), openMinutes/60, openMinutes%60, 0, 0, sessionTZ)
}

func (s *ORBScheduler) sweepReclaimEntry(ctx context.Context, ticker string, cfg config.ForexORBConfig, inst config.Instrument, portfolioValue float64) (StrategySignal, bool, bool) {
	if s.marketData == nil {
		return StrategySignal{}, false, false
	}
	params := ReclaimParamsFromInstrument(inst)
	sessionOpen := s.strategySessionOpen()
	fetchStart, levelEnd := reclaimLookbackWindow(sessionOpen, params.LevelSet)
	candles, err := s.marketData.FetchCandles(ctx, ticker, data.Timeframe1Min, fetchStart, s.now())
	if err != nil {
		slog.Warn("orb: sweep-reclaim: fetch candles failed — not entering this tick", "ticker", ticker, "err", err)
		return StrategySignal{}, true, false
	}
	signal, wait, ok := EvaluateSweepReclaim(candles, fetchStart, levelEnd, sessionOpen, s.now(), inst, cfg, params, portfolioValue)
	if !ok {
		return StrategySignal{}, wait, false
	}
	if !entryBarCurrent(candles, s.now(), signal.EntryTime) {
		slog.Info("orb: sweep-reclaim: stale entry bar — waiting for next session", "ticker", ticker, "entry_time", signal.EntryTime, "now", s.now())
		return StrategySignal{}, false, false
	}
	return signal, false, true
}

func reclaimLookbackWindow(sessionOpen time.Time, levelSet string) (time.Time, time.Time) {
	day := time.Date(sessionOpen.Year(), sessionOpen.Month(), sessionOpen.Day(), 0, 0, 0, 0, sessionOpen.Location())
	switch levelSet {
	case "prior_day":
		return day.AddDate(0, 0, -1), day
	case "overnight":
		if sessionOpen.Hour() < 12 {
			return day, sessionOpen
		}
		return day.Add(22*time.Hour).AddDate(0, 0, -1), sessionOpen
	default:
		return sessionOpen.Add(-24 * time.Hour), sessionOpen
	}
}

func (s *ORBScheduler) openingShockEntry(ctx context.Context, ticker string, cfg config.ForexORBConfig, inst config.Instrument, portfolioValue float64) (StrategySignal, bool, bool) {
	if s.marketData == nil {
		return StrategySignal{}, false, false
	}
	params := OpeningShockParamsFromInstrument(inst)
	sessionOpen := s.strategySessionOpen()
	candles, err := s.marketData.FetchCandles(ctx, ticker, data.Timeframe1Min, sessionOpen, s.now())
	if err != nil {
		slog.Warn("orb: opening-shock: fetch candles failed — not entering this tick", "ticker", ticker, "err", err)
		return StrategySignal{}, true, false
	}
	history, err := s.openingShockHistory(ctx, ticker, sessionOpen, params)
	if err != nil {
		slog.Warn("orb: opening-shock: history fetch failed — not entering this tick", "ticker", ticker, "err", err)
		return StrategySignal{}, true, false
	}
	signal, wait, ok := EvaluateOpeningShock(candles, history, sessionOpen, s.now(), inst, cfg, params, portfolioValue)
	if !ok {
		return StrategySignal{}, wait, false
	}
	if !entryBarCurrent(candles, s.now(), signal.EntryTime) {
		slog.Info("orb: opening-shock: stale entry bar — waiting for next session", "ticker", ticker, "entry_time", signal.EntryTime, "now", s.now())
		return StrategySignal{}, false, false
	}
	return signal, false, true
}

func (s *ORBScheduler) openingShockHistory(ctx context.Context, ticker string, sessionOpen time.Time, params OpeningShockParams) ([]float64, error) {
	if params.Lookback <= 0 || params.WindowMinutes <= 0 {
		return nil, nil
	}
	history := make([]float64, 0, params.Lookback)
	sessionTZ := s.sessionTimezone()
	openMinutes := s.sessionOpenMinutes()
	for day := sessionOpen.AddDate(0, 0, -1); len(history) < params.Lookback; day = day.AddDate(0, 0, -1) {
		if sessionOpen.Sub(day) > time.Duration((params.Lookback+30)*24)*time.Hour {
			break
		}
		probe := day.In(sessionTZ)
		if probe.Weekday() == time.Saturday || probe.Weekday() == time.Sunday {
			continue
		}
		priorOpen := time.Date(probe.Year(), probe.Month(), probe.Day(), openMinutes/60, openMinutes%60, 0, 0, sessionTZ)
		bars, err := s.marketData.FetchCandles(ctx, ticker, data.Timeframe1Min, priorOpen, priorOpen.Add(time.Duration(params.WindowMinutes)*time.Minute))
		if err != nil {
			return nil, fmt.Errorf("%s @ %s: %w", ticker, priorOpen.Format(time.RFC3339), err)
		}
		if len(bars) == 0 {
			continue
		}
		sort.Slice(bars, func(i, j int) bool { return bars[i].Timestamp.Before(bars[j].Timestamp) })
		history = append(history, math.Abs(bars[len(bars)-1].Close-bars[0].Open))
	}
	return history, nil
}
