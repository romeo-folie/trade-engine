package main

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strconv"
	"time"

	"github.com/romeo-folie/trade-engine/internal/backtest/orbreplay"
	"github.com/romeo-folie/trade-engine/internal/backtest/sweep"
	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/data"
	"github.com/romeo-folie/trade-engine/internal/logger"
	"github.com/romeo-folie/trade-engine/internal/orb"
	"github.com/spf13/cobra"
)

var (
	btFadeSession      string
	btFadeRanges       string
	btFadeRiskRewards  string
	btFadeMaxHolds     string
	btFadeSweepDepths  string
	btFadeTargetModes  string
	btFadeEntryWindow  int
	btFadeTop          int
	btFadePrintMinimum int
)

func init() {
	orbFadeCmd.Flags().StringVar(&btORBFrom, "from", "", "Start date YYYY-MM-DD (required)")
	orbFadeCmd.Flags().StringVar(&btORBTo, "to", "", "End date YYYY-MM-DD (required)")
	orbFadeCmd.Flags().StringVar(&btFadeSession, "session", "newyork", "Session: london | newyork | newyork_et (default newyork; futures maps newyork to newyork_et)")
	orbFadeCmd.Flags().StringVar(&btORBInstruments, "instruments", "", "Comma-separated symbols (default: all configured)")
	orbFadeCmd.Flags().StringVar(&btORBProvider, "provider", "", "Data provider: histdata (default) | dukascopy | exness | oanda")
	orbFadeCmd.Flags().StringVar(&btORBAssetClass, "asset-class", "forex", "Instrument universe: forex (forex_orb.instruments) | futures (futures_orb/default micro-index instruments via index-CFD proxy data)")
	orbFadeCmd.Flags().Float64Var(&btORBCapital, "capital", 100000, "Initial capital for position sizing")
	orbFadeCmd.Flags().StringVar(&btFadeRanges, "ranges", "5,15,30,60", "Opening-range minute levels to test")
	orbFadeCmd.Flags().StringVar(&btFadeRiskRewards, "risk-rewards", "0.75,1,1.25,1.5,2", "Target R multiples to test")
	orbFadeCmd.Flags().StringVar(&btFadeMaxHolds, "max-holds", "60,90,120,180", "Max-hold minute levels to test")
	orbFadeCmd.Flags().StringVar(&btFadeSweepDepths, "sweep-depths", "0.5,1,2,3", "Minimum sweep depth in pip-buffer multiples")
	orbFadeCmd.Flags().StringVar(&btFadeTargetModes, "target-modes", "r,mid,opposite", "Target styles to test: r, mid, opposite")
	orbFadeCmd.Flags().IntVar(&btFadeEntryWindow, "entry-window", 60, "Minutes after session open to watch for a failed range break")
	orbFadeCmd.Flags().IntVar(&btFadeTop, "top", 25, "How many ranked candidates to print")
	orbFadeCmd.Flags().IntVar(&btFadePrintMinimum, "min-trades", 100, "Minimum trades for printed candidates")
	_ = orbFadeCmd.MarkFlagRequired("from")
	_ = orbFadeCmd.MarkFlagRequired("to")
	backtestCmd.AddCommand(orbFadeCmd)
}

var orbFadeCmd = &cobra.Command{
	Use:   "orb-fade",
	Short: "Replay an opening-range failed-break fade over forex/CFD data",
	Long: `Replay a mean-reversion variant for instruments like XAG/USD:
after the opening range locks, wait for price to sweep beyond a range edge and then
close back inside the range. Enter on the next bar open, stop beyond the sweep extreme,
and target a configured R multiple.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runORBFade(cmd.Context())
	},
}

type fadeParam struct {
	Session       string
	RangeMinutes  int
	RiskReward    float64
	MaxHold       int
	SweepDepthMul float64
	TargetMode    string
}

func (p fadeParam) label() string {
	return fmt.Sprintf("%s/or%d/fade/%s/depth%.2f/rr%.2f/mh%d", p.Session, p.RangeMinutes, p.TargetMode, p.SweepDepthMul, p.RiskReward, p.MaxHold)
}

type fadeCandidateResult struct {
	Param  fadeParam
	Result orbreplay.Result
	Score  float64
	Gate   bool
}

func runORBFade(ctx context.Context) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if err := logger.Init(cfg.Logging.Level, cfg.Logging.Format); err != nil {
		return err
	}
	from, err := time.Parse("2006-01-02", btORBFrom)
	if err != nil {
		return fmt.Errorf("invalid --from date %q: %w", btORBFrom, err)
	}
	to, err := time.Parse("2006-01-02", btORBTo)
	if err != nil {
		return fmt.Errorf("invalid --to date %q: %w", btORBTo, err)
	}
	if to.Before(from) {
		return fmt.Errorf("--to must not be before --from")
	}
	to = to.Add(24*time.Hour - time.Second)
	session, err := normalizeStrategySession(btFadeSession)
	if err != nil {
		return fmt.Errorf("--session %q unsupported for orb-fade: %w", btFadeSession, err)
	}

	ranges, err := parsePositiveIntCSV(btFadeRanges)
	if err != nil {
		return fmt.Errorf("invalid --ranges %q: %w", btFadeRanges, err)
	}
	rrs, err := parsePositiveFloatLevels(btFadeRiskRewards)
	if err != nil {
		return fmt.Errorf("invalid --risk-rewards %q: %w", btFadeRiskRewards, err)
	}
	holds, err := parsePositiveIntCSV(btFadeMaxHolds)
	if err != nil {
		return fmt.Errorf("invalid --max-holds %q: %w", btFadeMaxHolds, err)
	}
	depths, err := parsePositiveFloatLevels(btFadeSweepDepths)
	if err != nil {
		return fmt.Errorf("invalid --sweep-depths %q: %w", btFadeSweepDepths, err)
	}
	targetModes, err := parseFadeTargetModes(btFadeTargetModes)
	if err != nil {
		return fmt.Errorf("invalid --target-modes %q: %w", btFadeTargetModes, err)
	}
	if btFadeEntryWindow <= 0 {
		return fmt.Errorf("--entry-window must be positive")
	}

	uni, err := buildBacktestUniverse(cfg)
	if err != nil {
		return err
	}

	var params []fadeParam
	for _, rm := range ranges {
		for _, mode := range targetModes {
			modeRRs := rrs
			if mode != "r" {
				modeRRs = []float64{0}
			}
			for _, rr := range modeRRs {
				for _, mh := range holds {
					for _, depth := range depths {
						params = append(params, fadeParam{
							Session: session, RangeMinutes: rm, RiskReward: rr, MaxHold: mh, SweepDepthMul: depth, TargetMode: mode,
						})
					}
				}
			}
		}
	}
	fmt.Printf("orb-fade · %s · %s -> %s · %d sets · %d instruments\n\n",
		session, btORBFrom, btORBTo, len(params), len(uni.instruments))

	results := make([]fadeCandidateResult, 0, len(params))
	for _, p := range params {
		trades, err := runFadeParam(ctx, uni.base, uni.provider, uni.costs, uni.instruments, p, from, to, btORBCapital)
		if err != nil {
			return err
		}
		res := orbreplay.Summarize(p.label(), trades)
		score := sweep.RobustnessScore(res.Overall, sweep.MinTradesFloor)
		gate := sweep.PassesAcceptanceGates(res.Overall, sweep.MinTradesFloor)
		results = append(results, fadeCandidateResult{Param: p, Result: res, Score: score, Gate: gate})
	}
	sort.Slice(results, func(i, j int) bool {
		if results[i].Score == results[j].Score {
			return results[i].Result.Overall.ProfitFactor > results[j].Result.Overall.ProfitFactor
		}
		return results[i].Score > results[j].Score
	})

	fmt.Printf("%-3s %-7s %-8s %-5s %-6s %-5s %7s %6s %6s %8s %11s %8s %5s\n",
		"#", "RANGE", "TARGET", "DEPTH", "RR", "HOLD", "TRADES", "WIN%", "PF", "EXP_R", "NET_PNL", "SCORE", "GATE")
	printed := 0
	for _, r := range results {
		m := r.Result.Overall
		if m.Trades < btFadePrintMinimum {
			continue
		}
		printed++
		gate := ""
		if r.Gate {
			gate = "PASS"
		}
		fmt.Printf("%-3d %-7d %-8s %-5.2f %-6.2f %-5d %7d %5.1f%% %6s %+8.3f %11.2f %8.3f %5s\n",
			printed, r.Param.RangeMinutes, r.Param.TargetMode, r.Param.SweepDepthMul, r.Param.RiskReward, r.Param.MaxHold,
			m.Trades, m.WinRate*100, fmtPF(m.ProfitFactor), m.ExpectancyR, m.NetPnL, r.Score, gate)
		if printed >= btFadeTop {
			break
		}
	}
	if printed == 0 {
		fmt.Printf("No candidates had at least %d trades.\n", btFadePrintMinimum)
	}
	return nil
}

func runFadeParam(ctx context.Context, base config.ForexORBConfig, provider data.MarketDataProvider, costs map[string]orbreplay.CostModel, instruments []config.Instrument, p fadeParam, from, to time.Time, capital float64) ([]orbreplay.SimTrade, error) {
	runCfg := base
	runCfg.Session = p.Session
	runCfg.RangeMinutes = p.RangeMinutes
	runCfg.RiskReward = p.RiskReward

	var trades []orbreplay.SimTrade
	for day := fadeTruncateDay(from); !day.After(to); day = day.AddDate(0, 0, 1) {
		sessionOpen, ok := fadeSessionOpenAt(day, p.Session)
		if !ok || sessionOpen.Before(from) || sessionOpen.After(to) {
			continue
		}
		fetchEnd := sessionOpen.Add(time.Duration(p.RangeMinutes+btFadeEntryWindow+p.MaxHold+60) * time.Minute)
		for _, inst := range instruments {
			t, err := simulateFadeSession(ctx, runCfg, provider, costs, inst, p, sessionOpen, fetchEnd, capital)
			if err != nil {
				return nil, err
			}
			if t != nil {
				trades = append(trades, *t)
			}
		}
	}
	return trades, nil
}

func simulateFadeSession(ctx context.Context, cfg config.ForexORBConfig, provider data.MarketDataProvider, costs map[string]orbreplay.CostModel, inst config.Instrument, p fadeParam, sessionOpen, fetchEnd time.Time, capital float64) (*orbreplay.SimTrade, error) {
	candles, err := provider.FetchCandles(ctx, inst.Symbol, data.Timeframe1Min, sessionOpen, fetchEnd)
	if err != nil {
		return nil, fmt.Errorf("orb-fade: fetch %s @ %s: %w", inst.Symbol, sessionOpen.Format(time.RFC3339), err)
	}
	sort.Slice(candles, func(i, j int) bool { return candles[i].Timestamp.Before(candles[j].Timestamp) })

	rangeEnd := sessionOpen.Add(time.Duration(p.RangeMinutes) * time.Minute)
	rb := orb.NewRangeBuffer(inst.Symbol, p.RangeMinutes)
	var watch []data.Candle
	for _, c := range candles {
		if c.Timestamp.Before(rangeEnd) {
			rb.Add(c)
		} else {
			watch = append(watch, c)
		}
	}
	or := rb.Range()
	if or == nil || len(watch) < 2 {
		return nil, nil
	}
	if inst.PipBuffer > 0 && (or.High-or.Low) < inst.PipBuffer*2 {
		return nil, nil
	}

	entryDeadline := sessionOpen.Add(time.Duration(btFadeEntryWindow) * time.Minute)
	depth := p.SweepDepthMul * inst.PipBuffer
	if depth <= 0 {
		depth = inst.PipBuffer
	}
	for i := 0; i+1 < len(watch); i++ {
		bar := watch[i]
		if !bar.Timestamp.Before(entryDeadline) {
			break
		}
		upFail := bar.High >= or.High+depth && bar.Close < or.High
		downFail := bar.Low <= or.Low-depth && bar.Close > or.Low
		if upFail && downFail {
			continue
		}
		switch {
		case upFail:
			entry := watch[i+1].Open
			stop := bar.High + inst.PipBuffer
			if stop <= entry {
				continue
			}
			br := fadeBracket(inst, "sell", entry, stop, capital, cfg, p, or)
			return resolveFadeTrade(inst, br, costs[inst.Symbol], p.MaxHold, watch, i+1), nil
		case downFail:
			entry := watch[i+1].Open
			stop := bar.Low - inst.PipBuffer
			if stop >= entry {
				continue
			}
			br := fadeBracket(inst, "buy", entry, stop, capital, cfg, p, or)
			return resolveFadeTrade(inst, br, costs[inst.Symbol], p.MaxHold, watch, i+1), nil
		}
	}
	return nil, nil
}

func fadeBracket(inst config.Instrument, side string, entry, stop float64, capital float64, cfg config.ForexORBConfig, p fadeParam, or *orb.OpeningRange) orb.BracketParams {
	br := orb.ComputeRetestBracket(inst.Symbol, side, entry, stop, capital, cfg, inst)
	switch p.TargetMode {
	case "mid":
		br.TakeProfit = (or.High + or.Low) / 2
	case "opposite":
		if side == "buy" {
			br.TakeProfit = or.High
		} else {
			br.TakeProfit = or.Low
		}
	}
	return br
}

func resolveFadeTrade(inst config.Instrument, p orb.BracketParams, cost orbreplay.CostModel, maxHoldMin int, watch []data.Candle, entryIdx int) *orbreplay.SimTrade {
	if p.Qty <= 0 || entryIdx >= len(watch) {
		return nil
	}
	half := cost.Spread / 2
	pv := fadePointValue(inst)
	entryTime := watch[entryIdx].Timestamp
	entryFill := p.EntryStop
	if p.Side == "buy" {
		entryFill += half
	} else {
		entryFill -= half
	}

	deadline := entryTime.Add(time.Duration(maxHoldMin) * time.Minute)
	var exitType string
	var exitMid float64
	var exitTime time.Time
	for _, c := range watch[entryIdx:] {
		if !c.Timestamp.Before(deadline) {
			exitType, exitMid, exitTime = "time_exit", c.Open, c.Timestamp
			break
		}
		if et, ep, ts, found := orb.ResolveExitFromCandles(p.Side, p.StopLoss, p.TakeProfit, []data.Candle{c}); found {
			exitType, exitMid, exitTime = et, ep, ts
			break
		}
	}
	if exitType == "" {
		last := watch[len(watch)-1]
		exitType, exitMid, exitTime = "end_of_data", last.Close, last.Timestamp
	}
	exitFill := exitMid
	if p.Side == "buy" {
		exitFill -= half
	} else {
		exitFill += half
	}
	gross := fadePnL(p.Side, entryFill, exitFill, p.Qty, pv)
	commission := cost.PerFillCommission(p.Qty, entryFill, pv) + cost.PerFillCommission(p.Qty, exitFill, pv)
	risk := math.Abs(p.EntryStop-p.StopLoss) * float64(p.Qty) * pv
	var r float64
	if risk > 0 {
		r = gross / risk
	}
	return &orbreplay.SimTrade{
		Ticker: inst.Symbol, Side: p.Side, EntryTime: entryTime, ExitTime: exitTime,
		EntryPrice: entryFill, ExitPrice: exitFill, Qty: p.Qty, ExitType: exitType,
		GrossPnL: gross, Commission: commission, NetPnL: gross - commission, R: r,
		HoldDur: exitTime.Sub(entryTime),
	}
}

func parsePositiveFloatLevels(s string) ([]float64, error) {
	parts := splitCSV(s)
	out := make([]float64, 0, len(parts))
	for _, p := range parts {
		v, err := strconv.ParseFloat(p, 64)
		if err != nil {
			return nil, fmt.Errorf("%q is not a number", p)
		}
		if v <= 0 {
			return nil, fmt.Errorf("value %v must be positive", v)
		}
		out = append(out, v)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no values")
	}
	return out, nil
}

func parsePositiveIntCSV(s string) ([]int, error) {
	parts := splitCSV(s)
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		v, err := strconv.Atoi(p)
		if err != nil {
			return nil, fmt.Errorf("%q is not an integer", p)
		}
		if v <= 0 {
			return nil, fmt.Errorf("value %v must be positive", v)
		}
		out = append(out, v)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no values")
	}
	return out, nil
}

func parseFadeTargetModes(s string) ([]string, error) {
	parts := splitCSV(s)
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		switch p {
		case "r", "mid", "opposite":
			out = append(out, p)
		default:
			return nil, fmt.Errorf("%q must be one of r,mid,opposite", p)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no values")
	}
	return out, nil
}

func fadeSessionOpenAt(day time.Time, session string) (time.Time, bool) {
	switch session {
	case "london":
		return day.Add(8 * time.Hour), true
	case "newyork":
		return day.Add(13*time.Hour + 30*time.Minute), true
	case "newyork_et":
		loc, err := time.LoadLocation("America/New_York")
		if err != nil {
			return time.Time{}, false
		}
		local := time.Date(day.Year(), day.Month(), day.Day(), 9, 30, 0, 0, loc)
		return local.UTC(), true
	default:
		return time.Time{}, false
	}
}

func normalizeStrategySession(session string) (string, error) {
	if btORBAssetClass == "futures" && session == "newyork" {
		session = "newyork_et"
	}
	switch session {
	case "london", "newyork", "newyork_et":
		return session, nil
	default:
		return "", fmt.Errorf("use london|newyork|newyork_et")
	}
}

func fadeTruncateDay(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

func fadePointValue(inst config.Instrument) float64 {
	if inst.PointValue > 0 {
		return inst.PointValue
	}
	return 1
}

func fadePnL(side string, entry, exit float64, qty int, pv float64) float64 {
	if side == "buy" {
		return (exit - entry) * float64(qty) * pv
	}
	return (entry - exit) * float64(qty) * pv
}
