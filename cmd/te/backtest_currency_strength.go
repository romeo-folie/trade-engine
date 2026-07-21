package main

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/romeo-folie/trade-engine/internal/backtest/orbreplay"
	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/data"
	"github.com/romeo-folie/trade-engine/internal/logger"
	"github.com/spf13/cobra"
)

var (
	btStrengthStarts    string
	btStrengthRankEnds  string
	btStrengthExits     string
	btStrengthGaps      string
	btStrengthBroadMin  int
	btStrengthBroadMins string
	btStrengthConfirms  string
	btStrengthTradeSyms string
	btStrengthUnits     int
	btStrengthTop       int
	btStrengthMinTrades int
	btStrengthAnalysis  bool
	btStrengthSlipMults string
	btStrengthScoreMode string
	btStrengthTrainFrom string
	btStrengthTrainTo   string
	btStrengthValFrom   string
	btStrengthValTo     string
	btStrengthFwdFrom   string
	btStrengthFwdTo     string
)

func init() {
	currencyStrengthCmd.Flags().StringVar(&btORBFrom, "from", "", "Start date YYYY-MM-DD (required)")
	currencyStrengthCmd.Flags().StringVar(&btORBTo, "to", "", "End date YYYY-MM-DD (required)")
	currencyStrengthCmd.Flags().StringVar(&btORBInstruments, "instruments", "", "Comma-separated FX pairs (default: Exness FX research universe)")
	currencyStrengthCmd.Flags().StringVar(&btORBProvider, "provider", "", "Data provider: histdata (default) | dukascopy | exness | oanda")
	currencyStrengthCmd.Flags().StringVar(&btORBAssetClass, "asset-class", "forex", "Instrument universe: forex only")
	currencyStrengthCmd.Flags().StringVar(&btStrengthStarts, "starts", "00:00,02:00,06:00", "UTC strength-window start times, HH:MM")
	currencyStrengthCmd.Flags().StringVar(&btStrengthRankEnds, "rank-ends", "07:00,08:00,09:00,10:00", "UTC strength-window end / entry times, HH:MM")
	currencyStrengthCmd.Flags().StringVar(&btStrengthExits, "exits", "12:00,14:00,15:00,16:00,17:00,18:00,20:00", "UTC fixed exit times, HH:MM")
	currencyStrengthCmd.Flags().StringVar(&btStrengthGaps, "min-gaps", "0.0002,0.0004,0.0006,0.001,0.0015,0.002", "Minimum strongest-minus-weakest currency-score gaps")
	currencyStrengthCmd.Flags().IntVar(&btStrengthBroadMin, "broad-min", 2, "Minimum pair-vote breadth for strongest and weakest currencies")
	currencyStrengthCmd.Flags().StringVar(&btStrengthBroadMins, "broad-mins", "", "Comma-separated breadth thresholds to sweep (default: --broad-min)")
	currencyStrengthCmd.Flags().StringVar(&btStrengthConfirms, "confirm-min-moves", "0", "Comma-separated minimum routed-pair signed log moves before entry")
	currencyStrengthCmd.Flags().StringVar(&btStrengthTradeSyms, "trade-instruments", "", "Comma-separated routed pairs allowed for execution (default: any ranked pair)")
	currencyStrengthCmd.Flags().IntVar(&btStrengthUnits, "units", 100000, "Fixed trade size in base units")
	currencyStrengthCmd.Flags().IntVar(&btStrengthTop, "top", 20, "How many ranked candidates to print")
	currencyStrengthCmd.Flags().IntVar(&btStrengthMinTrades, "min-trades", 100, "Minimum trades for printed candidates")
	currencyStrengthCmd.Flags().BoolVar(&btStrengthAnalysis, "analysis", false, "Print robustness detail for the top candidate")
	currencyStrengthCmd.Flags().StringVar(&btStrengthSlipMults, "slippage-mults", "1,1.5,2,3", "Spread/slippage multipliers for --analysis")
	currencyStrengthCmd.Flags().StringVar(&btStrengthScoreMode, "score-mode", "overall", "Candidate scoring mode: overall | walk-forward")
	currencyStrengthCmd.Flags().StringVar(&btStrengthTrainFrom, "train-from", "2022-01-01", "Analysis train window start")
	currencyStrengthCmd.Flags().StringVar(&btStrengthTrainTo, "train-to", "2024-12-31", "Analysis train window end")
	currencyStrengthCmd.Flags().StringVar(&btStrengthValFrom, "validate-from", "2025-01-01", "Analysis validation window start")
	currencyStrengthCmd.Flags().StringVar(&btStrengthValTo, "validate-to", "2025-12-31", "Analysis validation window end")
	currencyStrengthCmd.Flags().StringVar(&btStrengthFwdFrom, "forward-from", "2026-01-01", "Analysis forward window start")
	currencyStrengthCmd.Flags().StringVar(&btStrengthFwdTo, "forward-to", "", "Analysis forward window end (default: --to)")
	_ = currencyStrengthCmd.MarkFlagRequired("from")
	_ = currencyStrengthCmd.MarkFlagRequired("to")
	backtestCmd.AddCommand(currencyStrengthCmd)
}

var currencyStrengthCmd = &cobra.Command{
	Use:   "currency-strength",
	Short: "Replay currency-strength rotation over FX pairs",
	Long: `Rank individual currencies over an Asia/early-London window, trade the
strongest available currency against the weakest available currency, require broad
confirmation across multiple pairs, then exit at a fixed UTC time.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runCurrencyStrength(cmd.Context())
	},
}

type strengthPair struct {
	Instrument config.Instrument
	Base       string
	Quote      string
}

type strengthParam struct {
	Start    string
	RankEnd  string
	Exit     string
	MinGap   float64
	BroadMin int
	Confirm  float64
	Units    int
}

func (p strengthParam) label() string {
	return fmt.Sprintf("strength/%s-%s/exit%s/gap%.4f/broad%d/confirm%.4f/units%d", p.Start, p.RankEnd, p.Exit, p.MinGap, p.BroadMin, p.Confirm, p.Units)
}

type strengthResult struct {
	Param  strengthParam
	Result orbreplay.Result
	Score  float64
	Gate   bool
}

type strengthSnapshots map[time.Time]map[string]map[string]float64

type strengthEvalWindow struct {
	Label string
	From  time.Time
	To    time.Time
}

func runCurrencyStrength(ctx context.Context) error {
	if btORBAssetClass != "" && btORBAssetClass != "forex" {
		return fmt.Errorf("currency-strength supports --asset-class forex only")
	}
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
	if btStrengthBroadMin <= 0 {
		return fmt.Errorf("--broad-min must be positive")
	}
	if btStrengthUnits <= 0 {
		return fmt.Errorf("--units must be positive")
	}

	starts, err := parseClockList(btStrengthStarts)
	if err != nil {
		return fmt.Errorf("invalid --starts %q: %w", btStrengthStarts, err)
	}
	rankEnds, err := parseClockList(btStrengthRankEnds)
	if err != nil {
		return fmt.Errorf("invalid --rank-ends %q: %w", btStrengthRankEnds, err)
	}
	exits, err := parseClockList(btStrengthExits)
	if err != nil {
		return fmt.Errorf("invalid --exits %q: %w", btStrengthExits, err)
	}
	gaps, err := parsePositiveFloatLevels(btStrengthGaps)
	if err != nil {
		return fmt.Errorf("invalid --min-gaps %q: %w", btStrengthGaps, err)
	}
	broadMins, err := parseStrengthBroadMins(btStrengthBroadMins, btStrengthBroadMin)
	if err != nil {
		return err
	}
	confirms, err := parseNonNegativeFloatLevels(btStrengthConfirms)
	if err != nil {
		return fmt.Errorf("invalid --confirm-min-moves %q: %w", btStrengthConfirms, err)
	}
	scoreWindows, err := strengthScoreWindows()
	if err != nil {
		return err
	}

	uni, err := buildBacktestUniverse(cfg)
	if err != nil {
		return err
	}
	pairs, err := strengthPairs(uni.instruments)
	if err != nil {
		return err
	}
	tradeSymbols, err := parseStrengthTradeSymbols(btStrengthTradeSyms, pairs)
	if err != nil {
		return err
	}
	times := uniqueClockTimes(starts, rankEnds, exits)
	snaps, err := loadStrengthSnapshots(ctx, uni.provider, pairs, times, from, to)
	if err != nil {
		return err
	}

	var params []strengthParam
	for _, start := range starts {
		for _, rankEnd := range rankEnds {
			if !clockAfter(rankEnd, start) {
				continue
			}
			for _, exit := range exits {
				if !clockAfter(exit, rankEnd) {
					continue
				}
				for _, gap := range gaps {
					for _, broadMin := range broadMins {
						for _, confirm := range confirms {
							params = append(params, strengthParam{
								Start: start, RankEnd: rankEnd, Exit: exit,
								MinGap: gap, BroadMin: broadMin, Confirm: confirm, Units: btStrengthUnits,
							})
						}
					}
				}
			}
		}
	}
	if len(params) == 0 {
		return fmt.Errorf("no valid start/rank-end/exit combinations")
	}

	results := make([]strengthResult, 0, len(params))
	for _, p := range params {
		trades := runStrengthParam(snaps, pairs, uni.costs, p, from, to, tradeSymbols)
		res := orbreplay.Summarize(p.label(), trades)
		results = append(results, strengthResult{
			Param:  p,
			Result: res,
			Score:  strengthScoreResult(res, btStrengthMinTrades, btStrengthScoreMode, scoreWindows),
			Gate:   strengthGateResult(res, btStrengthMinTrades, btStrengthScoreMode, scoreWindows),
		})
	}
	sort.Slice(results, func(i, j int) bool {
		if results[i].Score == results[j].Score {
			return results[i].Result.Overall.NetPnL > results[j].Result.Overall.NetPnL
		}
		return results[i].Score > results[j].Score
	})

	fmt.Printf("currency-strength · %s -> %s · %d sets · %d pairs · units=%d\n\n",
		btORBFrom, btORBTo, len(params), len(pairs), btStrengthUnits)
	printStrengthResults(results, btStrengthTop, btStrengthMinTrades)
	if btStrengthAnalysis {
		slips, err := parsePositiveFloatLevels(btStrengthSlipMults)
		if err != nil {
			return fmt.Errorf("invalid --slippage-mults %q: %w", btStrengthSlipMults, err)
		}
		if err := printStrengthAnalysis(snaps, pairs, uni.costs, results, from, to, slips, tradeSymbols, scoreWindows); err != nil {
			return err
		}
	}
	return nil
}

func strengthPairs(instruments []config.Instrument) ([]strengthPair, error) {
	var pairs []strengthPair
	for _, inst := range instruments {
		base, quote, ok := splitFXPair(inst.Symbol)
		if !ok {
			continue
		}
		if inst.SecType != "" && inst.SecType != "CASH" {
			continue
		}
		pairs = append(pairs, strengthPair{Instrument: inst, Base: base, Quote: quote})
	}
	if len(pairs) < 3 {
		return nil, fmt.Errorf("currency-strength needs at least three CASH FX pairs; got %d", len(pairs))
	}
	return pairs, nil
}

func splitFXPair(symbol string) (string, string, bool) {
	parts := strings.Split(strings.ToUpper(strings.TrimSpace(symbol)), "/")
	if len(parts) != 2 || len(parts[0]) != 3 || len(parts[1]) != 3 {
		return "", "", false
	}
	return parts[0], parts[1], true
}

func parseClockList(csv string) ([]string, error) {
	parts := splitCSV(csv)
	if len(parts) == 0 {
		return nil, fmt.Errorf("no values")
	}
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		t, err := time.Parse("15:04", p)
		if err != nil {
			return nil, err
		}
		out = append(out, t.Format("15:04"))
	}
	return out, nil
}

func parseStrengthBroadMins(csv string, fallback int) ([]int, error) {
	if strings.TrimSpace(csv) == "" {
		return []int{fallback}, nil
	}
	out, err := parsePositiveIntCSV(csv)
	if err != nil {
		return nil, fmt.Errorf("invalid --broad-mins %q: %w", csv, err)
	}
	return out, nil
}

func parseNonNegativeFloatLevels(csv string) ([]float64, error) {
	parts := splitCSV(csv)
	out := make([]float64, 0, len(parts))
	for _, p := range parts {
		v, err := strconv.ParseFloat(p, 64)
		if err != nil {
			return nil, fmt.Errorf("%q is not a number", p)
		}
		if v < 0 {
			return nil, fmt.Errorf("value %v must not be negative", v)
		}
		out = append(out, v)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no values")
	}
	return out, nil
}

func parseStrengthTradeSymbols(csv string, pairs []strengthPair) (map[string]bool, error) {
	parts := splitCSV(csv)
	if len(parts) == 0 {
		return nil, nil
	}
	available := make(map[string]bool, len(pairs))
	for _, pair := range pairs {
		available[pair.Instrument.Symbol] = true
	}
	out := make(map[string]bool, len(parts))
	for _, raw := range parts {
		sym := strings.ToUpper(strings.TrimSpace(raw))
		if !available[sym] {
			return nil, fmt.Errorf("--trade-instruments symbol %q is not in the FX backtest universe", raw)
		}
		out[sym] = true
	}
	return out, nil
}

func strengthScoreWindows() ([]strengthEvalWindow, error) {
	if btStrengthScoreMode != "overall" && btStrengthScoreMode != "walk-forward" {
		return nil, fmt.Errorf("--score-mode must be overall or walk-forward")
	}
	windows := []struct {
		Label string
		From  string
		To    string
	}{
		{"train", btStrengthTrainFrom, btStrengthTrainTo},
		{"validate", btStrengthValFrom, btStrengthValTo},
		{"forward", btStrengthFwdFrom, cmpOr(btStrengthFwdTo, btORBTo)},
	}
	out := make([]strengthEvalWindow, 0, len(windows))
	for _, w := range windows {
		from, to, err := parseWindow(w.From, w.To)
		if err != nil {
			return nil, fmt.Errorf("%s window: %w", w.Label, err)
		}
		out = append(out, strengthEvalWindow{Label: w.Label, From: from, To: to})
	}
	return out, nil
}

func uniqueClockTimes(groups ...[]string) []string {
	seen := make(map[string]bool)
	var out []string
	for _, group := range groups {
		for _, t := range group {
			if !seen[t] {
				seen[t] = true
				out = append(out, t)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return clockAfter(out[j], out[i]) })
	return out
}

func clockAfter(a, b string) bool {
	at, _ := time.Parse("15:04", a)
	bt, _ := time.Parse("15:04", b)
	return at.After(bt)
}

func loadStrengthSnapshots(ctx context.Context, provider data.MarketDataProvider, pairs []strengthPair, clockTimes []string, from, to time.Time) (strengthSnapshots, error) {
	out := make(strengthSnapshots)
	for day := fadeTruncateDay(from); !day.After(to); day = day.AddDate(0, 0, 1) {
		if day.Before(fadeTruncateDay(from)) || day.After(to) {
			continue
		}
		dayMap := make(map[string]map[string]float64)
		for _, pair := range pairs {
			values, err := strengthDayCloses(ctx, provider, pair.Instrument.Symbol, day, clockTimes)
			if err != nil {
				return nil, err
			}
			if len(values) == len(clockTimes) {
				dayMap[pair.Instrument.Symbol] = values
			}
		}
		if len(dayMap) == len(pairs) {
			out[day] = dayMap
		}
	}
	return out, nil
}

func strengthDayCloses(ctx context.Context, provider data.MarketDataProvider, symbol string, day time.Time, clockTimes []string) (map[string]float64, error) {
	fetchStart := day
	fetchEnd := day.Add(24 * time.Hour)
	candles, err := provider.FetchCandles(ctx, symbol, data.Timeframe1Min, fetchStart, fetchEnd)
	if err != nil {
		return nil, fmt.Errorf("currency-strength: fetch %s @ %s: %w", symbol, day.Format("2006-01-02"), err)
	}
	sort.Slice(candles, func(i, j int) bool { return candles[i].Timestamp.Before(candles[j].Timestamp) })
	out := make(map[string]float64, len(clockTimes))
	targets := make(map[string]time.Time, len(clockTimes))
	for _, clock := range clockTimes {
		t, _ := time.Parse("15:04", clock)
		targets[clock] = day.Add(time.Duration(t.Hour())*time.Hour + time.Duration(t.Minute())*time.Minute)
	}
	for _, c := range candles {
		for clock, target := range targets {
			if !c.Timestamp.After(target) {
				out[clock] = c.Close
			}
		}
	}
	return out, nil
}

func runStrengthParam(snaps strengthSnapshots, pairs []strengthPair, costs map[string]orbreplay.CostModel, p strengthParam, from, to time.Time, tradeSymbols map[string]bool) []orbreplay.SimTrade {
	days := make([]time.Time, 0, len(snaps))
	for day := range snaps {
		if !day.Before(fadeTruncateDay(from)) && !day.After(to) {
			days = append(days, day)
		}
	}
	sort.Slice(days, func(i, j int) bool { return days[i].Before(days[j]) })
	var trades []orbreplay.SimTrade
	for _, day := range days {
		if trade := simulateStrengthDay(snaps[day], pairs, costs, p, day, tradeSymbols); trade != nil {
			trades = append(trades, *trade)
		}
	}
	return trades
}

func simulateStrengthDay(day map[string]map[string]float64, pairs []strengthPair, costs map[string]orbreplay.CostModel, p strengthParam, date time.Time, tradeSymbols map[string]bool) *orbreplay.SimTrade {
	scores := make(map[string]float64)
	votes := make(map[string]int)
	pairReturns := make(map[string]float64, len(pairs))
	for _, pair := range pairs {
		values := day[pair.Instrument.Symbol]
		start, okStart := values[p.Start]
		end, okEnd := values[p.RankEnd]
		exit, okExit := values[p.Exit]
		if !okStart || !okEnd || !okExit || start <= 0 || end <= 0 {
			return nil
		}
		_ = exit
		ret := math.Log(end / start)
		pairReturns[pair.Instrument.Symbol] = ret
		scores[pair.Base] += ret
		scores[pair.Quote] -= ret
		switch {
		case ret > 0:
			votes[pair.Base]++
			votes[pair.Quote]--
		case ret < 0:
			votes[pair.Base]--
			votes[pair.Quote]++
		}
	}
	strong, weak, gap, ok := strongestWeakest(scores)
	if !ok || gap < p.MinGap || votes[strong] < p.BroadMin || votes[weak] > -p.BroadMin {
		return nil
	}
	pair, side, ok := routeStrengthPair(pairs, strong, weak)
	if !ok {
		return nil
	}
	if len(tradeSymbols) > 0 && !tradeSymbols[pair.Instrument.Symbol] {
		return nil
	}
	confirm := pairReturns[pair.Instrument.Symbol]
	if side == "sell" {
		confirm = -confirm
	}
	if confirm < p.Confirm {
		return nil
	}
	entryMid := day[pair.Instrument.Symbol][p.RankEnd]
	exitMid := day[pair.Instrument.Symbol][p.Exit]
	entryTime := strengthTimestamp(date, p.RankEnd)
	exitTime := strengthTimestamp(date, p.Exit)
	return resolveStrengthTrade(pair.Instrument, side, entryMid, exitMid, entryTime, exitTime, costs[pair.Instrument.Symbol], p.Units)
}

func strongestWeakest(scores map[string]float64) (strong, weak string, gap float64, ok bool) {
	if len(scores) == 0 {
		return "", "", 0, false
	}
	first := true
	var strongScore, weakScore float64
	for cur, score := range scores {
		if first || score > strongScore {
			strong, strongScore = cur, score
		}
		if first || score < weakScore {
			weak, weakScore = cur, score
		}
		first = false
	}
	return strong, weak, strongScore - weakScore, strong != "" && weak != "" && strong != weak
}

func routeStrengthPair(pairs []strengthPair, strong, weak string) (strengthPair, string, bool) {
	for _, pair := range pairs {
		if pair.Base == strong && pair.Quote == weak {
			return pair, "buy", true
		}
		if pair.Base == weak && pair.Quote == strong {
			return pair, "sell", true
		}
	}
	return strengthPair{}, "", false
}

func strengthTimestamp(day time.Time, clock string) time.Time {
	t, _ := time.Parse("15:04", clock)
	return day.Add(time.Duration(t.Hour())*time.Hour + time.Duration(t.Minute())*time.Minute)
}

func resolveStrengthTrade(inst config.Instrument, side string, entryMid, exitMid float64, entryTime, exitTime time.Time, cost orbreplay.CostModel, qty int) *orbreplay.SimTrade {
	half := cost.Spread / 2
	pv := strengthPointValue(inst)
	entryFill := entryMid
	exitFill := exitMid
	if side == "buy" {
		entryFill += half
		exitFill -= half
	} else {
		entryFill -= half
		exitFill += half
	}
	gross := fadePnL(side, entryFill, exitFill, qty, pv)
	commission := cost.PerFillCommission(qty, entryFill, pv) + cost.PerFillCommission(qty, exitFill, pv)
	return &orbreplay.SimTrade{
		Ticker: inst.Symbol, Side: side, EntryTime: entryTime, ExitTime: exitTime,
		EntryPrice: entryFill, ExitPrice: exitFill, Qty: qty, ExitType: "time_exit",
		GrossPnL: gross, Commission: commission, NetPnL: gross - commission,
		HoldDur: exitTime.Sub(entryTime),
	}
}

func strengthPointValue(inst config.Instrument) float64 {
	if inst.PointValue > 0 {
		return inst.PointValue
	}
	return 1
}

func strengthScore(m orbreplay.Metrics, minTrades int) float64 {
	if m.Trades == 0 {
		return math.Inf(-1)
	}
	avg := m.NetPnL / float64(m.Trades)
	score := avg/100.0 + (m.ProfitFactor-1.0)*0.8 + (m.WinRate-0.5)*0.4
	if m.Trades < minTrades {
		score -= 1000.0 + float64(minTrades-m.Trades)/float64(minTrades)
	}
	return score
}

func strengthGate(m orbreplay.Metrics, minTrades int) bool {
	if m.Trades < minTrades {
		return false
	}
	avg := m.NetPnL / float64(m.Trades)
	return m.ProfitFactor >= 1.15 && avg > 0 && m.WinRate >= 0.50
}

func strengthScoreResult(res orbreplay.Result, minTrades int, mode string, windows []strengthEvalWindow) float64 {
	if mode != "walk-forward" {
		return strengthScore(res.Overall, minTrades)
	}
	windowMinTrades := strengthWindowMinTrades(minTrades)
	worst := math.Inf(1)
	for _, window := range windows {
		trades := filterStrengthTrades(res.Trades, window.From, window.To)
		score := strengthScore(orbreplay.Summarize(window.Label, trades).Overall, windowMinTrades)
		if score < worst {
			worst = score
		}
	}
	if math.IsInf(worst, 1) {
		return math.Inf(-1)
	}
	return worst + strengthScore(res.Overall, minTrades)*0.25
}

func strengthGateResult(res orbreplay.Result, minTrades int, mode string, windows []strengthEvalWindow) bool {
	if mode != "walk-forward" {
		return strengthGate(res.Overall, minTrades)
	}
	if res.Overall.Trades < minTrades {
		return false
	}
	windowMinTrades := strengthWindowMinTrades(minTrades)
	for _, window := range windows {
		trades := filterStrengthTrades(res.Trades, window.From, window.To)
		if !strengthGate(orbreplay.Summarize(window.Label, trades).Overall, windowMinTrades) {
			return false
		}
	}
	return true
}

func strengthWindowMinTrades(minTrades int) int {
	windowMinTrades := minTrades / 4
	if windowMinTrades < 1 {
		windowMinTrades = 1
	}
	return windowMinTrades
}

func filterStrengthTrades(trades []orbreplay.SimTrade, from, to time.Time) []orbreplay.SimTrade {
	out := make([]orbreplay.SimTrade, 0, len(trades))
	for _, trade := range trades {
		if !trade.EntryTime.Before(from) && !trade.EntryTime.After(to) {
			out = append(out, trade)
		}
	}
	return out
}

func printStrengthResults(results []strengthResult, top, minTrades int) {
	fmt.Printf("%-3s %-5s %-5s %-5s %-7s %5s %7s %7s %6s %6s %11s %9s %8s %5s\n",
		"#", "START", "RANK", "EXIT", "GAP", "BRD", "CONF", "TRADES", "WIN%", "PF", "NET_PNL", "AVG_PNL", "SCORE", "GATE")
	printed := 0
	for _, r := range results {
		m := r.Result.Overall
		if m.Trades < minTrades {
			continue
		}
		printed++
		gate := ""
		if r.Gate {
			gate = "PASS"
		}
		avg := 0.0
		if m.Trades > 0 {
			avg = m.NetPnL / float64(m.Trades)
		}
		fmt.Printf("%-3d %-5s %-5s %-5s %-7.4f %5d %7.4f %7d %5.1f%% %6s %11.2f %9.2f %8.3f %5s\n",
			printed, r.Param.Start, r.Param.RankEnd, r.Param.Exit, r.Param.MinGap,
			r.Param.BroadMin, r.Param.Confirm, m.Trades, m.WinRate*100, fmtPF(m.ProfitFactor), m.NetPnL, avg, r.Score, gate)
		if printed >= top {
			break
		}
	}
	if printed == 0 {
		fmt.Printf("No candidates had at least %d trades.\n", minTrades)
	}
}

func printStrengthAnalysis(snaps strengthSnapshots, pairs []strengthPair, costs map[string]orbreplay.CostModel, results []strengthResult, from, to time.Time, slipMults []float64, tradeSymbols map[string]bool, scoreWindows []strengthEvalWindow) error {
	bestIdx := -1
	for i, r := range results {
		if r.Result.Overall.Trades >= btStrengthMinTrades {
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
	printStrengthMetricLine("overall", best.Result.Overall)
	printStrengthGrouped("Yearly", groupContTrades(trades, func(t orbreplay.SimTrade) string {
		return t.EntryTime.UTC().Format("2006")
	}))
	printStrengthGrouped("Pair split", groupContTrades(trades, func(t orbreplay.SimTrade) string {
		return t.Ticker
	}))
	printStrengthMonthlyDrawdown(trades)
	if err := printStrengthWalkForward(snaps, pairs, costs, best.Param, tradeSymbols); err != nil {
		return err
	}

	fmt.Printf("\nSpread/slippage stress\n")
	fmt.Printf("%-8s %7s %6s %6s %11s %9s %8s %5s\n", "MULT", "TRADES", "WIN%", "PF", "NET_PNL", "AVG_PNL", "SCORE", "GATE")
	for _, mult := range slipMults {
		stressCosts := multiplyContSpreads(costs, mult)
		trades := runStrengthParam(snaps, pairs, stressCosts, best.Param, from, to, tradeSymbols)
		res := orbreplay.Summarize(best.Param.label(), trades)
		m := res.Overall
		avg := 0.0
		if m.Trades > 0 {
			avg = m.NetPnL / float64(m.Trades)
		}
		gate := ""
		if strengthGateResult(res, btStrengthMinTrades, btStrengthScoreMode, scoreWindows) {
			gate = "PASS"
		}
		fmt.Printf("%-8.2f %7d %5.1f%% %6s %11.2f %9.2f %8.3f %5s\n",
			mult, m.Trades, m.WinRate*100, fmtPF(m.ProfitFactor), m.NetPnL, avg,
			strengthScoreResult(res, btStrengthMinTrades, btStrengthScoreMode, scoreWindows), gate)
	}
	return nil
}

func printStrengthMetricLine(label string, m orbreplay.Metrics) {
	avg := 0.0
	if m.Trades > 0 {
		avg = m.NetPnL / float64(m.Trades)
	}
	fmt.Printf("%-12s trades %4d  win %5.1f%%  PF %6s  net %+10.2f  avg %+8.2f\n",
		label, m.Trades, m.WinRate*100, fmtPF(m.ProfitFactor), m.NetPnL, avg)
}

func printStrengthGrouped(title string, groups map[string][]orbreplay.SimTrade) {
	keys := make([]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	fmt.Printf("\n%s\n", title)
	fmt.Printf("%-12s %7s %6s %6s %11s %9s\n", "BUCKET", "TRADES", "WIN%", "PF", "NET_PNL", "AVG_PNL")
	for _, k := range keys {
		m := orbreplay.Summarize(k, groups[k]).Overall
		avg := 0.0
		if m.Trades > 0 {
			avg = m.NetPnL / float64(m.Trades)
		}
		fmt.Printf("%-12s %7d %5.1f%% %6s %11.2f %9.2f\n",
			k, m.Trades, m.WinRate*100, fmtPF(m.ProfitFactor), m.NetPnL, avg)
	}
}

func printStrengthMonthlyDrawdown(trades []orbreplay.SimTrade) {
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
	fmt.Printf("%-8s %7s %6s %6s %11s %9s %11s %11s\n", "MONTH", "TRADES", "WIN%", "PF", "NET_PNL", "AVG_PNL", "CUM_NET", "DD")
	for i := 0; i < limit; i++ {
		row := rows[i]
		m := row.Metrics
		avg := 0.0
		if m.Trades > 0 {
			avg = m.NetPnL / float64(m.Trades)
		}
		fmt.Printf("%-8s %7d %5.1f%% %6s %11.2f %9.2f %11.2f %11.2f\n",
			row.Month, m.Trades, m.WinRate*100, fmtPF(m.ProfitFactor), m.NetPnL, avg, row.CumNet, row.Drawdown)
	}
}

func printStrengthWalkForward(snaps strengthSnapshots, pairs []strengthPair, costs map[string]orbreplay.CostModel, p strengthParam, tradeSymbols map[string]bool) error {
	windows := []struct {
		Label string
		From  string
		To    string
	}{
		{"train", btStrengthTrainFrom, btStrengthTrainTo},
		{"validate", btStrengthValFrom, btStrengthValTo},
		{"forward", btStrengthFwdFrom, cmpOr(btStrengthFwdTo, btORBTo)},
	}
	fmt.Printf("\nWalk-forward\n")
	fmt.Printf("%-10s %7s %6s %6s %11s %9s\n", "WINDOW", "TRADES", "WIN%", "PF", "NET_PNL", "AVG_PNL")
	for _, w := range windows {
		from, to, err := parseWindow(w.From, w.To)
		if err != nil {
			return fmt.Errorf("%s window: %w", w.Label, err)
		}
		trades := runStrengthParam(snaps, pairs, costs, p, from, to, tradeSymbols)
		m := orbreplay.Summarize(p.label(), trades).Overall
		avg := 0.0
		if m.Trades > 0 {
			avg = m.NetPnL / float64(m.Trades)
		}
		fmt.Printf("%-10s %7d %5.1f%% %6s %11.2f %9.2f\n",
			w.Label, m.Trades, m.WinRate*100, fmtPF(m.ProfitFactor), m.NetPnL, avg)
	}
	return nil
}
