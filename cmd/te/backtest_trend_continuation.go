package main

import (
	"context"
	"fmt"
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
	btContSession      string
	btContRanges       string
	btContDelays       string
	btContConfirmBars  string
	btContDepths       string
	btContRiskRewards  string
	btContMaxHolds     string
	btContTop          int
	btContMinTrades    int
	btContAnalysis     bool
	btContSlipMults    string
	btContSideMode     string
	btContTrendFilter  string
	btContTrendLookbk  int
	btContMinRangePip  float64
	btContMaxRangePip  float64
	btContExcludeRange string
	btContMonthlyStop  float64
)

func init() {
	trendContCmd.Flags().StringVar(&btORBFrom, "from", "", "Start date YYYY-MM-DD (required)")
	trendContCmd.Flags().StringVar(&btORBTo, "to", "", "End date YYYY-MM-DD (required)")
	trendContCmd.Flags().StringVar(&btContSession, "session", "newyork", "Session: london | newyork")
	trendContCmd.Flags().StringVar(&btORBInstruments, "instruments", "", "Comma-separated symbols (default: all configured)")
	trendContCmd.Flags().StringVar(&btORBProvider, "provider", "", "Data provider: histdata (default) | dukascopy | exness | oanda")
	trendContCmd.Flags().Float64Var(&btORBCapital, "capital", 100000, "Initial capital for position sizing")
	trendContCmd.Flags().StringVar(&btContRanges, "ranges", "15,30,60", "Opening-range minute levels to test")
	trendContCmd.Flags().StringVar(&btContDelays, "delays", "15,30,60,90", "Minutes after range lock before continuation may enter")
	trendContCmd.Flags().StringVar(&btContConfirmBars, "confirm-bars", "1,3,5", "Consecutive closes beyond range required at decision time")
	trendContCmd.Flags().StringVar(&btContDepths, "depths", "1,2,3,5", "Minimum close extension in pip-buffer multiples")
	trendContCmd.Flags().StringVar(&btContRiskRewards, "risk-rewards", "1,1.5,2,2.5,3", "Target R multiples to test")
	trendContCmd.Flags().StringVar(&btContMaxHolds, "max-holds", "60,90,120,180,240", "Max-hold minute levels to test")
	trendContCmd.Flags().IntVar(&btContTop, "top", 30, "How many ranked candidates to print")
	trendContCmd.Flags().IntVar(&btContMinTrades, "min-trades", 100, "Minimum trades for printed candidates")
	trendContCmd.Flags().BoolVar(&btContAnalysis, "analysis", false, "Print robustness analysis for the top candidate")
	trendContCmd.Flags().StringVar(&btContSlipMults, "slippage-mults", "1,1.5,2,3", "Spread/slippage multipliers for --analysis")
	trendContCmd.Flags().StringVar(&btContSideMode, "side-mode", "both", "Allowed trade side: both | long | short")
	trendContCmd.Flags().StringVar(&btContTrendFilter, "trend-filter", "none", "Pre-open trend filter: none | aligned | contrarian")
	trendContCmd.Flags().IntVar(&btContTrendLookbk, "trend-lookback", 240, "Minutes before session open used by --trend-filter")
	trendContCmd.Flags().Float64Var(&btContMinRangePip, "min-range-pips", 0, "Minimum opening range width in pip-buffer units (0 disables)")
	trendContCmd.Flags().Float64Var(&btContMaxRangePip, "max-range-pips", 0, "Maximum opening range width in pip-buffer units (0 disables)")
	trendContCmd.Flags().StringVar(&btContExcludeRange, "exclude-range-pips", "", "Comma-separated opening range-width bands to skip, min:max in pip-buffer units, max exclusive (e.g. 32:60)")
	trendContCmd.Flags().Float64Var(&btContMonthlyStop, "monthly-stop-loss", 0, "Stop taking new entries for the rest of a UTC month after realized net PnL falls below -value (0 disables)")
	_ = trendContCmd.MarkFlagRequired("from")
	_ = trendContCmd.MarkFlagRequired("to")
	backtestCmd.AddCommand(trendContCmd)
}

var trendContCmd = &cobra.Command{
	Use:   "trend-continuation",
	Short: "Replay delayed continuation entries after the opening range",
	Long: `Replay a delayed continuation model:
define the opening range, wait a delay, require consecutive closes beyond the range,
then enter in the retained direction with a range-opposite stop.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runTrendContinuation(cmd.Context())
	},
}

type contParam struct {
	Session      string
	RangeMinutes int
	DelayMinutes int
	ConfirmBars  int
	DepthMul     float64
	RiskReward   float64
	MaxHold      int
	SideMode     string
	TrendFilter  string
	TrendLookbk  int
	MinRangePips float64
	MaxRangePips float64
	ExcludeRange []orb.RangeBand
	MonthlyStop  float64
}

type contResult struct {
	Param  contParam
	Result orbreplay.Result
	Score  float64
	Gate   bool
}

func (p contParam) label() string {
	label := fmt.Sprintf("%s/or%d/cont/d%d/c%d/depth%.2f/rr%.2f/mh%d", p.Session, p.RangeMinutes, p.DelayMinutes, p.ConfirmBars, p.DepthMul, p.RiskReward, p.MaxHold)
	if p.SideMode != "" && p.SideMode != "both" {
		label += "/" + p.SideMode
	}
	if p.TrendFilter != "" && p.TrendFilter != "none" {
		label += fmt.Sprintf("/trend_%s_%dm", p.TrendFilter, p.TrendLookbk)
	}
	if p.MinRangePips > 0 || p.MaxRangePips > 0 {
		label += fmt.Sprintf("/range%.1f-%.1fp", p.MinRangePips, p.MaxRangePips)
	}
	for _, band := range p.ExcludeRange {
		label += fmt.Sprintf("/skiprange%.1f-%.1fp", band.Min, band.Max)
	}
	if p.MonthlyStop > 0 {
		label += fmt.Sprintf("/monthlystop%.0f", p.MonthlyStop)
	}
	return label
}

func runTrendContinuation(ctx context.Context) error {
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
	if btContSession != "london" && btContSession != "newyork" {
		return fmt.Errorf("--session %q unsupported for trend-continuation (use london|newyork)", btContSession)
	}

	ranges, err := parsePositiveIntCSV(btContRanges)
	if err != nil {
		return fmt.Errorf("invalid --ranges %q: %w", btContRanges, err)
	}
	delays, err := parsePositiveIntCSV(btContDelays)
	if err != nil {
		return fmt.Errorf("invalid --delays %q: %w", btContDelays, err)
	}
	confirms, err := parsePositiveIntCSV(btContConfirmBars)
	if err != nil {
		return fmt.Errorf("invalid --confirm-bars %q: %w", btContConfirmBars, err)
	}
	depths, err := parsePositiveFloatLevels(btContDepths)
	if err != nil {
		return fmt.Errorf("invalid --depths %q: %w", btContDepths, err)
	}
	rrs, err := parsePositiveFloatLevels(btContRiskRewards)
	if err != nil {
		return fmt.Errorf("invalid --risk-rewards %q: %w", btContRiskRewards, err)
	}
	holds, err := parsePositiveIntCSV(btContMaxHolds)
	if err != nil {
		return fmt.Errorf("invalid --max-holds %q: %w", btContMaxHolds, err)
	}
	if btContSideMode != "both" && btContSideMode != "long" && btContSideMode != "short" {
		return fmt.Errorf("--side-mode must be both, long, or short")
	}
	if btContTrendFilter != "none" && btContTrendFilter != "aligned" && btContTrendFilter != "contrarian" {
		return fmt.Errorf("--trend-filter must be none, aligned, or contrarian")
	}
	if btContTrendLookbk <= 0 {
		return fmt.Errorf("--trend-lookback must be positive")
	}
	if btContMinRangePip < 0 || btContMaxRangePip < 0 {
		return fmt.Errorf("--min-range-pips/--max-range-pips must not be negative")
	}
	if btContMaxRangePip > 0 && btContMinRangePip > btContMaxRangePip {
		return fmt.Errorf("--min-range-pips must not exceed --max-range-pips")
	}
	if btContMonthlyStop < 0 {
		return fmt.Errorf("--monthly-stop-loss must not be negative")
	}
	excludeRanges, err := orb.ParseRangeBands(btContExcludeRange)
	if err != nil {
		return fmt.Errorf("invalid --exclude-range-pips %q: %w", btContExcludeRange, err)
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

	var params []contParam
	for _, rm := range ranges {
		for _, delay := range delays {
			for _, confirm := range confirms {
				for _, depth := range depths {
					for _, rr := range rrs {
						for _, hold := range holds {
							params = append(params, contParam{
								Session: btContSession, RangeMinutes: rm, DelayMinutes: delay, ConfirmBars: confirm,
								DepthMul: depth, RiskReward: rr, MaxHold: hold, SideMode: btContSideMode,
								TrendFilter: btContTrendFilter, TrendLookbk: btContTrendLookbk,
								MinRangePips: btContMinRangePip, MaxRangePips: btContMaxRangePip,
								ExcludeRange: append([]orb.RangeBand(nil), excludeRanges...),
								MonthlyStop:  btContMonthlyStop,
							})
						}
					}
				}
			}
		}
	}

	fmt.Printf("trend-continuation · %s · %s -> %s · %d sets · %d instruments\n\n",
		btContSession, btORBFrom, btORBTo, len(params), len(instruments))
	fmt.Printf("filters: side=%s trend=%s lookback=%dm range_pips=%.1f..%.1f exclude_range_pips=%s monthly_stop_loss=%.2f\n\n",
		btContSideMode, btContTrendFilter, btContTrendLookbk, btContMinRangePip, btContMaxRangePip, orb.FormatRangeBands(excludeRanges), btContMonthlyStop)

	results := make([]contResult, 0, len(params))
	for _, p := range params {
		trades, err := runContParam(ctx, cfg.ForexORB, provider, costs, instruments, p, from, to, btORBCapital)
		if err != nil {
			return err
		}
		res := orbreplay.Summarize(p.label(), trades)
		score := sweep.RobustnessScore(res.Overall, sweep.MinTradesFloor)
		gate := sweep.PassesAcceptanceGates(res.Overall, sweep.MinTradesFloor)
		results = append(results, contResult{Param: p, Result: res, Score: score, Gate: gate})
	}
	sort.Slice(results, func(i, j int) bool {
		if results[i].Score == results[j].Score {
			return results[i].Result.Overall.ProfitFactor > results[j].Result.Overall.ProfitFactor
		}
		return results[i].Score > results[j].Score
	})

	fmt.Printf("%-3s %-5s %-5s %-5s %-5s %-6s %-5s %7s %6s %6s %8s %11s %8s %5s\n",
		"#", "RNG", "DELAY", "CONF", "DEPTH", "RR", "HOLD", "TRADES", "WIN%", "PF", "EXP_R", "NET_PNL", "SCORE", "GATE")
	printed := 0
	for _, r := range results {
		m := r.Result.Overall
		if m.Trades < btContMinTrades {
			continue
		}
		printed++
		gate := ""
		if r.Gate {
			gate = "PASS"
		}
		fmt.Printf("%-3d %-5d %-5d %-5d %-5.2f %-6.2f %-5d %7d %5.1f%% %6s %+8.3f %11.2f %8.3f %5s\n",
			printed, r.Param.RangeMinutes, r.Param.DelayMinutes, r.Param.ConfirmBars, r.Param.DepthMul,
			r.Param.RiskReward, r.Param.MaxHold, m.Trades, m.WinRate*100, fmtPF(m.ProfitFactor),
			m.ExpectancyR, m.NetPnL, r.Score, gate)
		if printed >= btContTop {
			break
		}
	}
	if printed == 0 {
		fmt.Printf("No candidates had at least %d trades.\n", btContMinTrades)
	}
	if btContAnalysis {
		slips, err := parsePositiveFloatLevels(btContSlipMults)
		if err != nil {
			return fmt.Errorf("invalid --slippage-mults %q: %w", btContSlipMults, err)
		}
		if err := printContRobustness(ctx, cfg.ForexORB, provider, costs, instruments, results, from, to, btORBCapital, slips); err != nil {
			return err
		}
	}
	return nil
}

func runContParam(ctx context.Context, base config.ForexORBConfig, provider data.MarketDataProvider, costs map[string]orbreplay.CostModel, instruments []config.Instrument, p contParam, from, to time.Time, capital float64) ([]orbreplay.SimTrade, error) {
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
		fetchStart := sessionOpen
		if p.TrendFilter != "" && p.TrendFilter != "none" {
			fetchStart = sessionOpen.Add(-time.Duration(p.TrendLookbk) * time.Minute)
		}
		fetchEnd := sessionOpen.Add(time.Duration(p.RangeMinutes+p.DelayMinutes+p.MaxHold+90) * time.Minute)
		for _, inst := range instruments {
			t, err := simulateContSession(ctx, runCfg, provider, costs, inst, p, fetchStart, sessionOpen, fetchEnd, capital)
			if err != nil {
				return nil, err
			}
			if t != nil {
				trades = append(trades, *t)
			}
		}
	}
	return applyContMonthlyStopLoss(trades, p.MonthlyStop), nil
}

func simulateContSession(ctx context.Context, cfg config.ForexORBConfig, provider data.MarketDataProvider, costs map[string]orbreplay.CostModel, inst config.Instrument, p contParam, fetchStart, sessionOpen, fetchEnd time.Time, capital float64) (*orbreplay.SimTrade, error) {
	candles, err := provider.FetchCandles(ctx, inst.Symbol, data.Timeframe1Min, fetchStart, fetchEnd)
	if err != nil {
		return nil, fmt.Errorf("trend-continuation: fetch %s @ %s: %w", inst.Symbol, sessionOpen.Format(time.RFC3339), err)
	}
	sort.Slice(candles, func(i, j int) bool { return candles[i].Timestamp.Before(candles[j].Timestamp) })

	rangeEnd := sessionOpen.Add(time.Duration(p.RangeMinutes) * time.Minute)
	rb := orb.NewRangeBuffer(inst.Symbol, p.RangeMinutes)
	var watch []data.Candle
	for _, c := range candles {
		if c.Timestamp.Before(sessionOpen) {
			continue
		}
		if c.Timestamp.Before(rangeEnd) {
			rb.Add(c)
		} else {
			watch = append(watch, c)
		}
	}
	or := rb.Range()
	if or == nil {
		return nil, nil
	}
	evalInst := inst
	evalInst.RiskReward = 0
	signal, ok := orb.EvaluateTrendContinuation(candles, or, sessionOpen, fetchEnd, evalInst, cfg, orb.TrendContinuationParams{
		DelayMinutes:        p.DelayMinutes,
		ConfirmBars:         p.ConfirmBars,
		DepthMult:           p.DepthMul,
		TrendFilter:         p.TrendFilter,
		TrendLookbackMinute: p.TrendLookbk,
		MinRangePips:        p.MinRangePips,
		MaxRangePips:        p.MaxRangePips,
		ExcludeRange:        p.ExcludeRange,
	}, capital)
	if !ok {
		return nil, nil
	}
	sideMode := "long"
	if signal.Side == "sell" {
		sideMode = "short"
	}
	if !contSideAllowed(p.SideMode, sideMode) {
		return nil, nil
	}
	entryIdx := -1
	for i, c := range watch {
		if c.Timestamp.Equal(signal.EntryTime) {
			entryIdx = i
			break
		}
	}
	if entryIdx < 0 {
		return nil, nil
	}
	return resolveFadeTrade(inst, signal.Bracket, costs[inst.Symbol], p.MaxHold, watch, entryIdx), nil
}

func contSideAllowed(mode, side string) bool {
	switch mode {
	case "", "both":
		return true
	case "long":
		return side == "long"
	case "short":
		return side == "short"
	default:
		return false
	}
}

func applyContMonthlyStopLoss(trades []orbreplay.SimTrade, limit float64) []orbreplay.SimTrade {
	if limit <= 0 || len(trades) == 0 {
		return trades
	}
	ordered := append([]orbreplay.SimTrade(nil), trades...)
	sort.SliceStable(ordered, func(i, j int) bool {
		return ordered[i].EntryTime.Before(ordered[j].EntryTime)
	})
	instrumentMonthNet := make(map[string]float64)
	halted := make(map[string]bool)
	filtered := make([]orbreplay.SimTrade, 0, len(ordered))
	for _, trade := range ordered {
		key := trade.Ticker + "\x00" + trade.EntryTime.UTC().Format("2006-01")
		if halted[key] {
			continue
		}
		filtered = append(filtered, trade)
		instrumentMonthNet[key] += trade.NetPnL
		if instrumentMonthNet[key] <= -limit {
			halted[key] = true
		}
	}
	return filtered
}

func printContRobustness(ctx context.Context, base config.ForexORBConfig, provider data.MarketDataProvider, costs map[string]orbreplay.CostModel, instruments []config.Instrument, results []contResult, from, to time.Time, capital float64, slipMults []float64) error {
	if len(results) == 0 {
		return nil
	}
	bestIdx := -1
	for i, r := range results {
		if r.Result.Overall.Trades >= btContMinTrades {
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
		switch t.Side {
		case "buy":
			return "long"
		case "sell":
			return "short"
		default:
			return t.Side
		}
	}))
	printContMonthlyDrawdown(trades)
	printContIBKRSilverOverlay(trades)

	fmt.Printf("\nSpread/slippage stress\n")
	fmt.Printf("%-8s %7s %6s %6s %8s %11s %8s %5s\n", "MULT", "TRADES", "WIN%", "PF", "EXP_R", "NET_PNL", "SCORE", "GATE")
	for _, mult := range slipMults {
		stressCosts := multiplyContSpreads(costs, mult)
		stressTrades, err := runContParam(ctx, base, provider, stressCosts, instruments, best.Param, from, to, capital)
		if err != nil {
			return fmt.Errorf("trend-continuation slippage %.2fx: %w", mult, err)
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

	printContNeighborStability(results)
	return nil
}

func printContMetricLine(label string, m orbreplay.Metrics) {
	fmt.Printf("%-12s trades %4d  win %5.1f%%  PF %6s  exp %+7.3fR  net %+10.2f\n",
		label, m.Trades, m.WinRate*100, fmtPF(m.ProfitFactor), m.ExpectancyR, m.NetPnL)
}

func printContGrouped(title string, groups map[string][]orbreplay.SimTrade) {
	keys := make([]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	fmt.Printf("\n%s\n", title)
	fmt.Printf("%-12s %7s %6s %6s %8s %11s\n", "BUCKET", "TRADES", "WIN%", "PF", "EXP_R", "NET_PNL")
	for _, k := range keys {
		m := orbreplay.Summarize(k, groups[k]).Overall
		fmt.Printf("%-12s %7d %5.1f%% %6s %+8.3f %11.2f\n",
			k, m.Trades, m.WinRate*100, fmtPF(m.ProfitFactor), m.ExpectancyR, m.NetPnL)
	}
}

func groupContTrades(trades []orbreplay.SimTrade, keyFn func(orbreplay.SimTrade) string) map[string][]orbreplay.SimTrade {
	out := make(map[string][]orbreplay.SimTrade)
	for _, t := range trades {
		key := keyFn(t)
		out[key] = append(out[key], t)
	}
	return out
}

func printContMonthlyDrawdown(trades []orbreplay.SimTrade) {
	byMonth := groupContTrades(trades, func(t orbreplay.SimTrade) string {
		return t.EntryTime.UTC().Format("2006-01")
	})
	months := make([]string, 0, len(byMonth))
	for month := range byMonth {
		months = append(months, month)
	}
	sort.Strings(months)

	type monthRow struct {
		Month    string
		Metrics  orbreplay.Metrics
		CumNet   float64
		Drawdown float64
	}
	rows := make([]monthRow, 0, len(months))
	peak := 0.0
	cum := 0.0
	worstDD := monthRow{}
	for _, month := range months {
		m := orbreplay.Summarize(month, byMonth[month]).Overall
		cum += m.NetPnL
		if cum > peak {
			peak = cum
		}
		row := monthRow{Month: month, Metrics: m, CumNet: cum, Drawdown: cum - peak}
		rows = append(rows, row)
		if row.Drawdown < worstDD.Drawdown {
			worstDD = row
		}
	}

	posMonths := 0
	for _, row := range rows {
		if row.Metrics.NetPnL > 0 {
			posMonths++
		}
	}
	fmt.Printf("\nMonthly drawdown\n")
	fmt.Printf("positive_months=%d/%d  max_month_end_drawdown=%+.2f at %s\n", posMonths, len(rows), worstDD.Drawdown, worstDD.Month)
	sort.Slice(rows, func(i, j int) bool {
		return rows[i].Metrics.NetPnL < rows[j].Metrics.NetPnL
	})
	limit := 12
	if len(rows) < limit {
		limit = len(rows)
	}
	fmt.Printf("Worst %d months by net PnL\n", limit)
	fmt.Printf("%-8s %7s %6s %6s %8s %11s %11s %11s\n", "MONTH", "TRADES", "WIN%", "PF", "EXP_R", "NET_PNL", "CUM_NET", "DD")
	for i := 0; i < limit; i++ {
		row := rows[i]
		m := row.Metrics
		fmt.Printf("%-8s %7d %5.1f%% %6s %+8.3f %11.2f %11.2f %11.2f\n",
			row.Month, m.Trades, m.WinRate*100, fmtPF(m.ProfitFactor), m.ExpectancyR, m.NetPnL, row.CumNet, row.Drawdown)
	}
}

func printContIBKRSilverOverlay(trades []orbreplay.SimTrade) {
	var xag []orbreplay.SimTrade
	for _, t := range trades {
		if t.Ticker == "XAG/USD" {
			xag = append(xag, t)
		}
	}
	if len(xag) == 0 {
		return
	}
	var qtySum, notionalSum, simCommSum, ibkrCommSum float64
	adjusted := make([]orbreplay.SimTrade, 0, len(xag))
	for _, t := range xag {
		entryNotional := t.EntryPrice * float64(t.Qty)
		exitNotional := t.ExitPrice * float64(t.Qty)
		ibkrComm := ibkrLondonSilverCommission(entryNotional) + ibkrLondonSilverCommission(exitNotional)
		nt := t
		nt.Commission = ibkrComm
		nt.NetPnL = t.GrossPnL - ibkrComm
		adjusted = append(adjusted, nt)
		qtySum += float64(t.Qty)
		notionalSum += entryNotional
		simCommSum += t.Commission
		ibkrCommSum += ibkrComm
	}
	sim := orbreplay.Summarize("sim", xag).Overall
	ibkr := orbreplay.Summarize("ibkr-london-silver", adjusted).Overall
	n := float64(len(xag))
	fmt.Printf("\nIBKR London Silver commission overlay\n")
	fmt.Printf("avg_qty=%.0f oz  avg_entry_notional=$%.2f  avg_sim_commission=$%.2f/trade  avg_ibkr_commission=$%.2f/trade\n",
		qtySum/n, notionalSum/n, simCommSum/n, ibkrCommSum/n)
	fmt.Printf("%-12s %7s %6s %6s %8s %11s %11s\n", "MODEL", "TRADES", "WIN%", "PF", "EXP_R", "NET_PNL", "COMMISSION")
	fmt.Printf("%-12s %7d %5.1f%% %6s %+8.3f %11.2f %11.2f\n",
		"current", sim.Trades, sim.WinRate*100, fmtPF(sim.ProfitFactor), sim.ExpectancyR, sim.NetPnL, sim.Commission)
	fmt.Printf("%-12s %7d %5.1f%% %6s %+8.3f %11.2f %11.2f\n",
		"ibkr-bps", ibkr.Trades, ibkr.WinRate*100, fmtPF(ibkr.ProfitFactor), ibkr.ExpectancyR, ibkr.NetPnL, ibkr.Commission)
}

func ibkrLondonSilverCommission(notional float64) float64 {
	if notional < 0 {
		notional = -notional
	}
	fee := tieredBpsFee(notional, []bpsTier{
		{Limit: 250000, Rate: 0.0003},
		{Limit: 500000, Rate: 0.0002},
		{Limit: 0, Rate: 0.00015},
	})
	if fee < 2 {
		return 2
	}
	return fee
}

type bpsTier struct {
	Limit float64
	Rate  float64
}

func tieredBpsFee(notional float64, tiers []bpsTier) float64 {
	var fee, prev float64
	for _, tier := range tiers {
		if tier.Limit > 0 && notional > tier.Limit {
			fee += (tier.Limit - prev) * tier.Rate
			prev = tier.Limit
			continue
		}
		fee += (notional - prev) * tier.Rate
		return fee
	}
	return fee
}

func printContNeighborStability(results []contResult) {
	var eligible []contResult
	for _, r := range results {
		if r.Result.Overall.Trades >= btContMinTrades {
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

	worst := append([]contResult(nil), eligible...)
	sort.Slice(worst, func(i, j int) bool {
		return worst[i].Score < worst[j].Score
	})
	limit := 8
	if len(worst) < limit {
		limit = len(worst)
	}
	fmt.Printf("Weakest %d eligible neighbors\n", limit)
	fmt.Printf("%-3s %-5s %-5s %-5s %-5s %-6s %-5s %7s %6s %6s %8s %11s %8s %5s\n",
		"#", "RNG", "DELAY", "CONF", "DEPTH", "RR", "HOLD", "TRADES", "WIN%", "PF", "EXP_R", "NET_PNL", "SCORE", "GATE")
	for i := 0; i < limit; i++ {
		r := worst[i]
		m := r.Result.Overall
		gate := ""
		if r.Gate {
			gate = "PASS"
		}
		fmt.Printf("%-3d %-5d %-5d %-5d %-5.2f %-6.2f %-5d %7d %5.1f%% %6s %+8.3f %11.2f %8.3f %5s\n",
			i+1, r.Param.RangeMinutes, r.Param.DelayMinutes, r.Param.ConfirmBars, r.Param.DepthMul,
			r.Param.RiskReward, r.Param.MaxHold, m.Trades, m.WinRate*100, fmtPF(m.ProfitFactor),
			m.ExpectancyR, m.NetPnL, r.Score, gate)
	}
}

func medianContFloat(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	cp := append([]float64(nil), xs...)
	sort.Float64s(cp)
	mid := len(cp) / 2
	if len(cp)%2 == 1 {
		return cp[mid]
	}
	return (cp[mid-1] + cp[mid]) / 2
}

func multiplyContSpreads(costs map[string]orbreplay.CostModel, mult float64) map[string]orbreplay.CostModel {
	out := make(map[string]orbreplay.CostModel, len(costs))
	for sym, c := range costs {
		c.Spread *= mult
		out[sym] = c
	}
	return out
}
