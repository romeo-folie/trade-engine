package main

import (
	"context"
	"fmt"
	"math"
	"sort"
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
	btShockSession     string
	btShockWindows     string
	btShockLookbacks   string
	btShockPercentiles string
	btShockRiskRewards string
	btShockMaxHolds    string
	btShockStopMults   string
	btShockEntryDelays string
	btShockSideMode    string
	btShockTop         int
	btShockMinTrades   int
	btShockAnalysis    bool
	btShockSlipMults   string
)

func init() {
	openingShockCmd.Flags().StringVar(&btORBFrom, "from", "", "Start date YYYY-MM-DD (required)")
	openingShockCmd.Flags().StringVar(&btORBTo, "to", "", "End date YYYY-MM-DD (required)")
	openingShockCmd.Flags().StringVar(&btShockSession, "session", "newyork", "Session: london | newyork | newyork_et (default newyork; futures maps newyork to newyork_et)")
	openingShockCmd.Flags().StringVar(&btORBInstruments, "instruments", "", "Comma-separated symbols (default: all configured)")
	openingShockCmd.Flags().StringVar(&btORBProvider, "provider", "", "Data provider: histdata (default) | dukascopy | exness | oanda")
	openingShockCmd.Flags().StringVar(&btORBAssetClass, "asset-class", "forex", "Instrument universe: forex (forex_orb.instruments) | futures (futures_orb/default micro-index instruments via index-CFD proxy data)")
	openingShockCmd.Flags().Float64Var(&btORBCapital, "capital", 100000, "Initial capital for position sizing")
	openingShockCmd.Flags().StringVar(&btShockWindows, "windows", "5,15,30", "Opening shock window minute levels to test")
	openingShockCmd.Flags().StringVar(&btShockLookbacks, "lookbacks", "20,60,120", "Prior-session lookback counts for percentile ranking")
	openingShockCmd.Flags().StringVar(&btShockPercentiles, "percentiles", "80,90,95", "Minimum historical percentile of absolute opening move")
	openingShockCmd.Flags().StringVar(&btShockRiskRewards, "risk-rewards", "1,1.5,2,3", "Target R multiples to test")
	openingShockCmd.Flags().StringVar(&btShockMaxHolds, "max-holds", "30,60,90,120", "Max-hold minute levels to test")
	openingShockCmd.Flags().StringVar(&btShockStopMults, "stop-mults", "0.5,1,1.5", "Stop buffer beyond shock extreme in pip-buffer multiples")
	openingShockCmd.Flags().StringVar(&btShockEntryDelays, "entry-delays", "0,5,15", "Minutes to wait after the shock window before entering")
	openingShockCmd.Flags().StringVar(&btShockSideMode, "side-mode", "both", "Allowed fade side: both | long | short")
	openingShockCmd.Flags().IntVar(&btShockTop, "top", 30, "How many ranked candidates to print")
	openingShockCmd.Flags().IntVar(&btShockMinTrades, "min-trades", 100, "Minimum trades for printed candidates")
	openingShockCmd.Flags().BoolVar(&btShockAnalysis, "analysis", false, "Print robustness analysis for the top candidate")
	openingShockCmd.Flags().StringVar(&btShockSlipMults, "slippage-mults", "1,1.5,2,3", "Spread/slippage multipliers for --analysis")
	_ = openingShockCmd.MarkFlagRequired("from")
	_ = openingShockCmd.MarkFlagRequired("to")
	backtestCmd.AddCommand(openingShockCmd)
}

var openingShockCmd = &cobra.Command{
	Use:   "opening-shock",
	Short: "Replay percentile-gated opening shock reversal entries",
	Long: `Replay an opening shock reversal model:
measure the first N minutes after the session open, require that absolute move to
rank above a historical percentile for the same instrument/window, then fade the
shock with a stop beyond the shock extreme and an R-multiple target.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runOpeningShock(cmd.Context())
	},
}

type shockParam struct {
	Session    string
	Window     int
	Lookback   int
	Percentile float64
	RiskReward float64
	MaxHold    int
	StopMult   float64
	EntryDelay int
	SideMode   string
}

func (p shockParam) label() string {
	return fmt.Sprintf("%s/shock%d/lb%d/p%.0f/stop%.2f/rr%.2f/d%d/mh%d/%s",
		p.Session, p.Window, p.Lookback, p.Percentile, p.StopMult, p.RiskReward, p.EntryDelay, p.MaxHold, p.SideMode)
}

type shockResult struct {
	Param  shockParam
	Result orbreplay.Result
	Score  float64
	Gate   bool
}

func runOpeningShock(ctx context.Context) error {
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
	session, err := normalizeStrategySession(btShockSession)
	if err != nil {
		return fmt.Errorf("--session %q unsupported for opening-shock: %w", btShockSession, err)
	}
	if btShockSideMode != "both" && btShockSideMode != "long" && btShockSideMode != "short" {
		return fmt.Errorf("--side-mode must be both, long, or short")
	}

	windows, err := parsePositiveIntCSV(btShockWindows)
	if err != nil {
		return fmt.Errorf("invalid --windows %q: %w", btShockWindows, err)
	}
	lookbacks, err := parsePositiveIntCSV(btShockLookbacks)
	if err != nil {
		return fmt.Errorf("invalid --lookbacks %q: %w", btShockLookbacks, err)
	}
	percentiles, err := parsePercentileLevels(btShockPercentiles)
	if err != nil {
		return fmt.Errorf("invalid --percentiles %q: %w", btShockPercentiles, err)
	}
	rrs, err := parsePositiveFloatLevels(btShockRiskRewards)
	if err != nil {
		return fmt.Errorf("invalid --risk-rewards %q: %w", btShockRiskRewards, err)
	}
	holds, err := parsePositiveIntCSV(btShockMaxHolds)
	if err != nil {
		return fmt.Errorf("invalid --max-holds %q: %w", btShockMaxHolds, err)
	}
	stopMults, err := parsePositiveFloatLevels(btShockStopMults)
	if err != nil {
		return fmt.Errorf("invalid --stop-mults %q: %w", btShockStopMults, err)
	}
	delays, err := parseNonNegativeIntCSV(btShockEntryDelays)
	if err != nil {
		return fmt.Errorf("invalid --entry-delays %q: %w", btShockEntryDelays, err)
	}

	uni, err := buildBacktestUniverse(cfg)
	if err != nil {
		return err
	}

	var params []shockParam
	for _, w := range windows {
		for _, lb := range lookbacks {
			for _, pct := range percentiles {
				for _, stop := range stopMults {
					for _, rr := range rrs {
						for _, delay := range delays {
							for _, hold := range holds {
								params = append(params, shockParam{
									Session: session, Window: w, Lookback: lb, Percentile: pct,
									StopMult: stop, RiskReward: rr, EntryDelay: delay, MaxHold: hold,
									SideMode: btShockSideMode,
								})
							}
						}
					}
				}
			}
		}
	}

	fmt.Printf("opening-shock · %s · %s -> %s · %d sets · %d instruments\n\n",
		session, btORBFrom, btORBTo, len(params), len(uni.instruments))

	results := make([]shockResult, 0, len(params))
	for _, p := range params {
		trades, err := runShockParam(ctx, uni.base, uni.provider, uni.costs, uni.instruments, p, from, to, btORBCapital)
		if err != nil {
			return err
		}
		res := orbreplay.Summarize(p.label(), trades)
		score := sweep.RobustnessScore(res.Overall, sweep.MinTradesFloor)
		gate := sweep.PassesAcceptanceGates(res.Overall, sweep.MinTradesFloor)
		results = append(results, shockResult{Param: p, Result: res, Score: score, Gate: gate})
	}
	sort.Slice(results, func(i, j int) bool {
		if results[i].Score == results[j].Score {
			return results[i].Result.Overall.ProfitFactor > results[j].Result.Overall.ProfitFactor
		}
		return results[i].Score > results[j].Score
	})

	fmt.Printf("%-3s %-4s %-4s %-4s %-5s %-6s %-5s %-5s %7s %6s %6s %8s %11s %8s %5s\n",
		"#", "WIN", "LB", "PCT", "STOP", "RR", "DLY", "HOLD", "TRADES", "WIN%", "PF", "EXP_R", "NET_PNL", "SCORE", "GATE")
	printed := 0
	for _, r := range results {
		m := r.Result.Overall
		if m.Trades < btShockMinTrades {
			continue
		}
		printed++
		gate := ""
		if r.Gate {
			gate = "PASS"
		}
		fmt.Printf("%-3d %-4d %-4d %-4.0f %-5.2f %-6.2f %-5d %-5d %7d %5.1f%% %6s %+8.3f %11.2f %8.3f %5s\n",
			printed, r.Param.Window, r.Param.Lookback, r.Param.Percentile, r.Param.StopMult,
			r.Param.RiskReward, r.Param.EntryDelay, r.Param.MaxHold, m.Trades, m.WinRate*100,
			fmtPF(m.ProfitFactor), m.ExpectancyR, m.NetPnL, r.Score, gate)
		if printed >= btShockTop {
			break
		}
	}
	if printed == 0 {
		fmt.Printf("No candidates had at least %d trades.\n", btShockMinTrades)
	}
	if btShockAnalysis {
		slips, err := parsePositiveFloatLevels(btShockSlipMults)
		if err != nil {
			return fmt.Errorf("invalid --slippage-mults %q: %w", btShockSlipMults, err)
		}
		if err := printShockRobustness(ctx, uni.base, uni.provider, uni.costs, uni.instruments, results, from, to, btORBCapital, slips); err != nil {
			return err
		}
	}
	return nil
}

func runShockParam(ctx context.Context, base config.ForexORBConfig, provider data.MarketDataProvider, costs map[string]orbreplay.CostModel, instruments []config.Instrument, p shockParam, from, to time.Time, capital float64) ([]orbreplay.SimTrade, error) {
	runCfg := base
	runCfg.Session = p.Session
	runCfg.RangeMinutes = p.Window
	runCfg.RiskReward = p.RiskReward

	history := make(map[string][]float64)
	var trades []orbreplay.SimTrade
	for day := fadeTruncateDay(from).AddDate(0, 0, -p.Lookback-10); !day.After(to); day = day.AddDate(0, 0, 1) {
		sessionOpen, ok := fadeSessionOpenAt(day, p.Session)
		if !ok {
			continue
		}
		fetchEnd := sessionOpen.Add(time.Duration(p.Window+p.EntryDelay+p.MaxHold+60) * time.Minute)
		for _, inst := range instruments {
			shock, trade, err := simulateShockSession(ctx, runCfg, provider, costs, inst, p, history[inst.Symbol], sessionOpen, fetchEnd, capital)
			if err != nil {
				return nil, err
			}
			if shock.ok {
				history[inst.Symbol] = append(history[inst.Symbol], shock.absMove)
				if len(history[inst.Symbol]) > p.Lookback {
					history[inst.Symbol] = history[inst.Symbol][len(history[inst.Symbol])-p.Lookback:]
				}
			}
			if sessionOpen.Before(from) || sessionOpen.After(to) {
				continue
			}
			if trade != nil {
				trades = append(trades, *trade)
			}
		}
	}
	return trades, nil
}

type shockMove struct {
	ok      bool
	absMove float64
}

func simulateShockSession(ctx context.Context, cfg config.ForexORBConfig, provider data.MarketDataProvider, costs map[string]orbreplay.CostModel, inst config.Instrument, p shockParam, history []float64, sessionOpen, fetchEnd time.Time, capital float64) (shockMove, *orbreplay.SimTrade, error) {
	candles, err := provider.FetchCandles(ctx, inst.Symbol, data.Timeframe1Min, sessionOpen, fetchEnd)
	if err != nil {
		return shockMove{}, nil, fmt.Errorf("opening-shock: fetch %s @ %s: %w", inst.Symbol, sessionOpen.Format(time.RFC3339), err)
	}
	sort.Slice(candles, func(i, j int) bool { return candles[i].Timestamp.Before(candles[j].Timestamp) })
	if len(candles) < p.Window+1 {
		return shockMove{}, nil, nil
	}

	shockEnd := sessionOpen.Add(time.Duration(p.Window) * time.Minute)
	var shockBars, watch []data.Candle
	for _, c := range candles {
		if c.Timestamp.Before(shockEnd) {
			shockBars = append(shockBars, c)
		} else {
			watch = append(watch, c)
		}
	}
	if len(shockBars) == 0 || len(watch) < 2 {
		return shockMove{}, nil, nil
	}
	open := shockBars[0].Open
	close := shockBars[len(shockBars)-1].Close
	move := close - open
	absMove := math.Abs(move)
	if absMove <= 0 {
		return shockMove{ok: true, absMove: absMove}, nil, nil
	}
	if len(history) < p.Lookback {
		return shockMove{ok: true, absMove: absMove}, nil, nil
	}
	threshold := percentileNearestRank(history, p.Percentile)
	if absMove < threshold {
		return shockMove{ok: true, absMove: absMove}, nil, nil
	}

	side := "sell"
	if move < 0 {
		side = "buy"
	}
	if !shockSideAllowed(p.SideMode, side) {
		return shockMove{ok: true, absMove: absMove}, nil, nil
	}
	entryIdx := 0
	if p.EntryDelay > 0 {
		entryTime := shockEnd.Add(time.Duration(p.EntryDelay) * time.Minute)
		entryIdx = -1
		for i, c := range watch {
			if !c.Timestamp.Before(entryTime) {
				entryIdx = i
				break
			}
		}
		if entryIdx < 0 {
			return shockMove{ok: true, absMove: absMove}, nil, nil
		}
	}
	hi, lo := candleHighLow(shockBars)
	entry := watch[entryIdx].Open
	stopBuffer := p.StopMult * inst.PipBuffer
	if stopBuffer <= 0 {
		stopBuffer = inst.PipBuffer
	}
	stop := hi + stopBuffer
	if side == "buy" {
		stop = lo - stopBuffer
	}
	if (side == "sell" && stop <= entry) || (side == "buy" && stop >= entry) {
		return shockMove{ok: true, absMove: absMove}, nil, nil
	}
	br := orb.ComputeRetestBracket(inst.Symbol, side, entry, stop, capital, cfg, inst)
	return shockMove{ok: true, absMove: absMove}, resolveFadeTrade(inst, br, costs[inst.Symbol], p.MaxHold, watch, entryIdx), nil
}

func parsePercentileLevels(s string) ([]float64, error) {
	levels, err := parsePositiveFloatLevels(s)
	if err != nil {
		return nil, err
	}
	for _, v := range levels {
		if v <= 0 || v >= 100 {
			return nil, fmt.Errorf("percentile %.2f out of range (0,100)", v)
		}
	}
	return levels, nil
}

func parseNonNegativeIntCSV(s string) ([]int, error) {
	levels, err := parseIntCSV(s)
	if err != nil {
		return nil, err
	}
	return levels, nil
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

func shockSideAllowed(mode, side string) bool {
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

func printShockRobustness(ctx context.Context, base config.ForexORBConfig, provider data.MarketDataProvider, costs map[string]orbreplay.CostModel, instruments []config.Instrument, results []shockResult, from, to time.Time, capital float64, slipMults []float64) error {
	if len(results) == 0 {
		return nil
	}
	bestIdx := -1
	for i, r := range results {
		if r.Result.Overall.Trades >= btShockMinTrades {
			bestIdx = i
			break
		}
	}
	if bestIdx < 0 {
		return nil
	}
	best := results[bestIdx]
	trades := best.Result.Trades
	fmt.Printf("\nRobustness detail for %s\n", best.Param.label())
	printContMetricLine("overall", best.Result.Overall)
	printContGrouped("Yearly", groupContTrades(trades, func(t orbreplay.SimTrade) string {
		return t.EntryTime.UTC().Format("2006")
	}))
	printContGrouped("Side split", groupContTrades(trades, func(t orbreplay.SimTrade) string {
		if t.Side == "buy" {
			return "long"
		}
		if t.Side == "sell" {
			return "short"
		}
		return t.Side
	}))
	printContMonthlyDrawdown(trades)

	fmt.Printf("\nSpread/slippage stress\n")
	fmt.Printf("%-8s %7s %6s %6s %8s %11s %8s %5s\n", "MULT", "TRADES", "WIN%", "PF", "EXP_R", "NET_PNL", "SCORE", "GATE")
	for _, mult := range slipMults {
		stressCosts := multiplyContSpreads(costs, mult)
		stressTrades, err := runShockParam(ctx, base, provider, stressCosts, instruments, best.Param, from, to, capital)
		if err != nil {
			return fmt.Errorf("opening-shock slippage %.2fx: %w", mult, err)
		}
		res := orbreplay.Summarize(best.Param.label(), stressTrades)
		score := sweep.RobustnessScore(res.Overall, sweep.MinTradesFloor)
		gate := ""
		if sweep.PassesAcceptanceGates(res.Overall, sweep.MinTradesFloor) {
			gate = "PASS"
		}
		m := res.Overall
		fmt.Printf("%-8.2f %7d %5.1f%% %6s %+8.3f %11.2f %8.3f %5s\n",
			mult, m.Trades, m.WinRate*100, fmtPF(m.ProfitFactor), m.ExpectancyR, m.NetPnL, score, gate)
	}
	printShockNeighborStability(results)
	return nil
}

func printShockNeighborStability(results []shockResult) {
	var eligible []shockResult
	for _, r := range results {
		if r.Result.Overall.Trades >= btShockMinTrades {
			eligible = append(eligible, r)
		}
	}
	if len(eligible) == 0 {
		return
	}
	pass := 0
	pfs := make([]float64, 0, len(eligible))
	exps := make([]float64, 0, len(eligible))
	nets := make([]float64, 0, len(eligible))
	for _, r := range eligible {
		if r.Gate {
			pass++
		}
		m := r.Result.Overall
		pfs = append(pfs, m.ProfitFactor)
		exps = append(exps, m.ExpectancyR)
		nets = append(nets, m.NetPnL)
	}
	fmt.Printf("\nParameter-neighbor stability\n")
	fmt.Printf("eligible_sets=%d  gate_pass=%d  median_pf=%s  median_exp=%+.3fR  median_net=%+.2f\n",
		len(eligible), pass, fmtPF(medianContFloat(pfs)), medianContFloat(exps), medianContFloat(nets))
}
