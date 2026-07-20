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
	btReclaimSession     string
	btReclaimLevels      string
	btReclaimRiskRewards string
	btReclaimMaxHolds    string
	btReclaimDepths      string
	btReclaimEntryWindow int
	btReclaimTop         int
	btReclaimMinTrades   int
)

func init() {
	sweepReclaimCmd.Flags().StringVar(&btORBFrom, "from", "", "Start date YYYY-MM-DD (required)")
	sweepReclaimCmd.Flags().StringVar(&btORBTo, "to", "", "End date YYYY-MM-DD (required)")
	sweepReclaimCmd.Flags().StringVar(&btReclaimSession, "session", "newyork", "Session: london | newyork")
	sweepReclaimCmd.Flags().StringVar(&btORBInstruments, "instruments", "", "Comma-separated symbols (default: all configured)")
	sweepReclaimCmd.Flags().StringVar(&btORBProvider, "provider", "", "Data provider: histdata (default) | dukascopy | exness | oanda")
	sweepReclaimCmd.Flags().Float64Var(&btORBCapital, "capital", 100000, "Initial capital for position sizing")
	sweepReclaimCmd.Flags().StringVar(&btReclaimLevels, "levels", "prior_day,overnight", "Level sets to test: prior_day, overnight")
	sweepReclaimCmd.Flags().StringVar(&btReclaimRiskRewards, "risk-rewards", "0.75,1,1.25,1.5,2,2.5,3", "Target R multiples to test")
	sweepReclaimCmd.Flags().StringVar(&btReclaimMaxHolds, "max-holds", "60,90,120,180,240", "Max-hold minute levels to test")
	sweepReclaimCmd.Flags().StringVar(&btReclaimDepths, "sweep-depths", "1,2,3,5,8", "Minimum sweep depth in pip-buffer multiples")
	sweepReclaimCmd.Flags().IntVar(&btReclaimEntryWindow, "entry-window", 240, "Minutes after session open to watch for a sweep/reclaim")
	sweepReclaimCmd.Flags().IntVar(&btReclaimTop, "top", 30, "How many ranked candidates to print")
	sweepReclaimCmd.Flags().IntVar(&btReclaimMinTrades, "min-trades", 100, "Minimum trades for printed candidates")
	_ = sweepReclaimCmd.MarkFlagRequired("from")
	_ = sweepReclaimCmd.MarkFlagRequired("to")
	backtestCmd.AddCommand(sweepReclaimCmd)
}

var sweepReclaimCmd = &cobra.Command{
	Use:   "sweep-reclaim",
	Short: "Replay liquidity sweep/reclaim entries around prior/overnight levels",
	Long: `Replay a mean-reversion liquidity-sweep strategy:
build prior-day or overnight high/low levels, wait for price to sweep a level and
close back through it, then enter next bar in the reclaim direction.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runSweepReclaim(cmd.Context())
	},
}

type reclaimParam struct {
	Session       string
	LevelSet      string
	RiskReward    float64
	MaxHold       int
	SweepDepthMul float64
}

func (p reclaimParam) label() string {
	return fmt.Sprintf("%s/%s/reclaim/depth%.2f/rr%.2f/mh%d", p.Session, p.LevelSet, p.SweepDepthMul, p.RiskReward, p.MaxHold)
}

type reclaimResult struct {
	Param  reclaimParam
	Result orbreplay.Result
	Score  float64
	Gate   bool
}

func runSweepReclaim(ctx context.Context) error {
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
	if btReclaimSession != "london" && btReclaimSession != "newyork" {
		return fmt.Errorf("--session %q unsupported for sweep-reclaim (use london|newyork)", btReclaimSession)
	}
	levelSets, err := parseReclaimLevelSets(btReclaimLevels)
	if err != nil {
		return fmt.Errorf("invalid --levels %q: %w", btReclaimLevels, err)
	}
	rrs, err := parsePositiveFloatLevels(btReclaimRiskRewards)
	if err != nil {
		return fmt.Errorf("invalid --risk-rewards %q: %w", btReclaimRiskRewards, err)
	}
	holds, err := parsePositiveIntCSV(btReclaimMaxHolds)
	if err != nil {
		return fmt.Errorf("invalid --max-holds %q: %w", btReclaimMaxHolds, err)
	}
	depths, err := parsePositiveFloatLevels(btReclaimDepths)
	if err != nil {
		return fmt.Errorf("invalid --sweep-depths %q: %w", btReclaimDepths, err)
	}
	if btReclaimEntryWindow <= 0 {
		return fmt.Errorf("--entry-window must be positive")
	}

	provider, err := buildBacktestProvider(cfg)
	if err != nil {
		return err
	}
	instruments, err := resolveInstruments(cfg.ForexORB, btORBInstruments)
	if err != nil {
		return err
	}
	costs := buildCostModels(instruments)

	var params []reclaimParam
	for _, levelSet := range levelSets {
		for _, rr := range rrs {
			for _, mh := range holds {
				for _, depth := range depths {
					params = append(params, reclaimParam{
						Session: btReclaimSession, LevelSet: levelSet, RiskReward: rr, MaxHold: mh, SweepDepthMul: depth,
					})
				}
			}
		}
	}
	fmt.Printf("sweep-reclaim · %s · %s -> %s · %d sets · %d instruments\n\n",
		btReclaimSession, btORBFrom, btORBTo, len(params), len(instruments))

	results := make([]reclaimResult, 0, len(params))
	for _, p := range params {
		trades, err := runReclaimParam(ctx, cfg.ForexORB, provider, costs, instruments, p, from, to, btORBCapital)
		if err != nil {
			return err
		}
		res := orbreplay.Summarize(p.label(), trades)
		score := sweep.RobustnessScore(res.Overall, sweep.MinTradesFloor)
		gate := sweep.PassesAcceptanceGates(res.Overall, sweep.MinTradesFloor)
		results = append(results, reclaimResult{Param: p, Result: res, Score: score, Gate: gate})
	}
	sort.Slice(results, func(i, j int) bool {
		if results[i].Score == results[j].Score {
			return results[i].Result.Overall.ProfitFactor > results[j].Result.Overall.ProfitFactor
		}
		return results[i].Score > results[j].Score
	})

	fmt.Printf("%-3s %-10s %-5s %-6s %-5s %7s %6s %6s %8s %11s %8s %5s\n",
		"#", "LEVELS", "DEPTH", "RR", "HOLD", "TRADES", "WIN%", "PF", "EXP_R", "NET_PNL", "SCORE", "GATE")
	printed := 0
	for _, r := range results {
		m := r.Result.Overall
		if m.Trades < btReclaimMinTrades {
			continue
		}
		printed++
		gate := ""
		if r.Gate {
			gate = "PASS"
		}
		fmt.Printf("%-3d %-10s %-5.2f %-6.2f %-5d %7d %5.1f%% %6s %+8.3f %11.2f %8.3f %5s\n",
			printed, r.Param.LevelSet, r.Param.SweepDepthMul, r.Param.RiskReward, r.Param.MaxHold,
			m.Trades, m.WinRate*100, fmtPF(m.ProfitFactor), m.ExpectancyR, m.NetPnL, r.Score, gate)
		if printed >= btReclaimTop {
			break
		}
	}
	if printed == 0 {
		fmt.Printf("No candidates had at least %d trades.\n", btReclaimMinTrades)
	}
	return nil
}

func runReclaimParam(ctx context.Context, base config.ForexORBConfig, provider data.MarketDataProvider, costs map[string]orbreplay.CostModel, instruments []config.Instrument, p reclaimParam, from, to time.Time, capital float64) ([]orbreplay.SimTrade, error) {
	runCfg := base
	runCfg.Session = p.Session
	runCfg.RiskReward = p.RiskReward

	var trades []orbreplay.SimTrade
	for day := fadeTruncateDay(from); !day.After(to); day = day.AddDate(0, 0, 1) {
		sessionOpen, ok := fadeSessionOpenAt(day, p.Session)
		if !ok || sessionOpen.Before(from) || sessionOpen.After(to) {
			continue
		}
		fetchStart, levelEnd := reclaimLookbackWindow(day, sessionOpen, p.LevelSet)
		fetchEnd := sessionOpen.Add(time.Duration(btReclaimEntryWindow+p.MaxHold+60) * time.Minute)
		for _, inst := range instruments {
			t, err := simulateReclaimSession(ctx, runCfg, provider, costs, inst, p, fetchStart, levelEnd, sessionOpen, fetchEnd, capital)
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

func simulateReclaimSession(ctx context.Context, cfg config.ForexORBConfig, provider data.MarketDataProvider, costs map[string]orbreplay.CostModel, inst config.Instrument, p reclaimParam, fetchStart, levelEnd, sessionOpen, fetchEnd time.Time, capital float64) (*orbreplay.SimTrade, error) {
	candles, err := provider.FetchCandles(ctx, inst.Symbol, data.Timeframe1Min, fetchStart, fetchEnd)
	if err != nil {
		return nil, fmt.Errorf("sweep-reclaim: fetch %s @ %s: %w", inst.Symbol, sessionOpen.Format(time.RFC3339), err)
	}
	sort.Slice(candles, func(i, j int) bool { return candles[i].Timestamp.Before(candles[j].Timestamp) })

	levelBars, watch := splitReclaimBars(candles, fetchStart, levelEnd, sessionOpen)
	if len(levelBars) == 0 || len(watch) < 2 {
		return nil, nil
	}
	hi, lo := candleHighLow(levelBars)
	levels := []reclaimLevel{
		{Name: "high", Price: hi, SweepSide: "up"},
		{Name: "low", Price: lo, SweepSide: "down"},
	}
	depth := p.SweepDepthMul * inst.PipBuffer
	if depth <= 0 {
		depth = inst.PipBuffer
	}
	entryDeadline := sessionOpen.Add(time.Duration(btReclaimEntryWindow) * time.Minute)

	for i := 0; i+1 < len(watch); i++ {
		bar := watch[i]
		if !bar.Timestamp.Before(entryDeadline) {
			break
		}
		for _, lvl := range levels {
			switch lvl.SweepSide {
			case "up":
				if bar.High >= lvl.Price+depth && bar.Close < lvl.Price {
					entry := watch[i+1].Open
					stop := bar.High + inst.PipBuffer
					if stop <= entry {
						continue
					}
					br := orb.ComputeRetestBracket(inst.Symbol, "sell", entry, stop, capital, cfg, inst)
					return resolveFadeTrade(inst, br, costs[inst.Symbol], p.MaxHold, watch, i+1), nil
				}
			case "down":
				if bar.Low <= lvl.Price-depth && bar.Close > lvl.Price {
					entry := watch[i+1].Open
					stop := bar.Low - inst.PipBuffer
					if stop >= entry {
						continue
					}
					br := orb.ComputeRetestBracket(inst.Symbol, "buy", entry, stop, capital, cfg, inst)
					return resolveFadeTrade(inst, br, costs[inst.Symbol], p.MaxHold, watch, i+1), nil
				}
			}
		}
	}
	return nil, nil
}

type reclaimLevel struct {
	Name      string
	Price     float64
	SweepSide string
}

func splitReclaimBars(candles []data.Candle, fetchStart, levelEnd, sessionOpen time.Time) (levelBars, watch []data.Candle) {
	for _, c := range candles {
		switch {
		case !c.Timestamp.Before(fetchStart) && c.Timestamp.Before(levelEnd):
			levelBars = append(levelBars, c)
		case !c.Timestamp.Before(sessionOpen):
			watch = append(watch, c)
		}
	}
	return levelBars, watch
}

func candleHighLow(bars []data.Candle) (float64, float64) {
	hi, lo := bars[0].High, bars[0].Low
	for _, b := range bars[1:] {
		hi = math.Max(hi, b.High)
		lo = math.Min(lo, b.Low)
	}
	return hi, lo
}

func reclaimLookbackWindow(day, sessionOpen time.Time, levelSet string) (time.Time, time.Time) {
	switch levelSet {
	case "prior_day":
		return day.AddDate(0, 0, -1), day
	case "overnight":
		switch {
		case sessionOpen.Hour() < 12:
			return day, sessionOpen // Asia/overnight lead-in for London.
		default:
			return day.Add(22*time.Hour).AddDate(0, 0, -1), sessionOpen // prior 22:00 UTC into NY.
		}
	default:
		return sessionOpen.Add(-24 * time.Hour), sessionOpen
	}
}

func parseReclaimLevelSets(s string) ([]string, error) {
	parts := splitCSV(s)
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		switch p {
		case "prior_day", "overnight":
			out = append(out, p)
		default:
			return nil, fmt.Errorf("%q must be one of prior_day,overnight", p)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no values")
	}
	return out, nil
}
