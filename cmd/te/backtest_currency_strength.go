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
	"github.com/romeo-folie/trade-engine/internal/strategy/currencystrength"
	"github.com/spf13/cobra"
)

var (
	btStrengthStarts        string
	btStrengthRankEnds      string
	btStrengthExits         string
	btStrengthGaps          string
	btStrengthBroadMin      int
	btStrengthBroadMins     string
	btStrengthConfirms      string
	btStrengthTradeSyms     string
	btStrengthUnits         int
	btStrengthTop           int
	btStrengthMinTrades     int
	btStrengthAnalysis      bool
	btStrengthSlipMults     string
	btStrengthScoreMode     string
	btStrengthCurrencyModel string
	btStrengthAllowPartial  bool
	btStrengthTrainFrom     string
	btStrengthTrainTo       string
	btStrengthValFrom       string
	btStrengthValTo         string
	btStrengthFwdFrom       string
	btStrengthFwdTo         string
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
	currencyStrengthCmd.Flags().StringVar(&btStrengthScoreMode, "score-mode", "overall", "Candidate scoring mode: overall | walk-forward (train+validate selection; forward report only)")
	currencyStrengthCmd.Flags().StringVar(&btStrengthCurrencyModel, "currency-model", "sum", "Currency score model: sum | least_squares")
	currencyStrengthCmd.Flags().BoolVar(&btStrengthAllowPartial, "allow-partial-basket", false, "Allow connected degraded baskets (least_squares only)")
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
	Start         string
	RankEnd       string
	Exit          string
	MinGap        float64
	BroadMin      int
	Confirm       float64
	Units         int
	CurrencyModel string
	AllowPartial  bool
}

func (p strengthParam) label() string {
	return fmt.Sprintf("strength/%s/%s-%s/exit%s/gap%.4f/broad%d/confirm%.4f/units%d/partial%t", p.CurrencyModel, p.Start, p.RankEnd, p.Exit, p.MinGap, p.BroadMin, p.Confirm, p.Units, p.AllowPartial)
}

type strengthResult struct {
	Param           strengthParam
	Result          orbreplay.Result
	Score           float64
	SelectionTrades int
	SelectionNetPnL float64
	Gate            bool
}

type strengthSnapshots map[time.Time]map[string]map[string]float64

type strengthEvalWindow struct {
	Label string
	From  time.Time
	To    time.Time
}

const strengthSnapshotMaxLag = 2 * time.Minute

type strengthDayOutcome struct {
	Trade  *orbreplay.SimTrade
	Reason string
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
	if btStrengthCurrencyModel != "sum" && btStrengthCurrencyModel != "least_squares" {
		return fmt.Errorf("--currency-model must be sum or least_squares")
	}
	if btStrengthAllowPartial && btStrengthCurrencyModel != "least_squares" {
		return fmt.Errorf("--allow-partial-basket requires --currency-model least_squares")
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
	scoreWindows, err := maybeStrengthScoreWindows(btStrengthScoreMode, btStrengthAnalysis)
	if err != nil {
		return err
	}

	researchCfg := *cfg
	researchCfg.ForexORB = cfg.ForexORB
	researchCfg.ForexORB.Instruments = currencyStrengthResearchInstruments(cfg.ForexORB.Instruments)
	uni, err := buildBacktestUniverse(&researchCfg)
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

	params := buildStrengthParams(starts, rankEnds, exits, gaps, broadMins, confirms, btStrengthUnits, btStrengthCurrencyModel, btStrengthAllowPartial)
	if len(params) == 0 {
		return fmt.Errorf("no valid start/rank-end/exit combinations")
	}

	results := make([]strengthResult, 0, len(params))
	for _, p := range params {
		trades := runStrengthParam(snaps, pairs, uni.costs, p, from, to, tradeSymbols)
		res := orbreplay.Summarize(p.label(), trades)
		results = append(results, strengthResult{
			Param: p, Result: res,
			Score:           strengthScoreResult(res, btStrengthMinTrades, btStrengthScoreMode, scoreWindows),
			SelectionTrades: strengthSelectionTradeCount(res, btStrengthScoreMode, scoreWindows),
			SelectionNetPnL: strengthSelectionNetPnL(res, btStrengthScoreMode, scoreWindows),
			Gate:            strengthGateResult(res, btStrengthMinTrades, btStrengthScoreMode, scoreWindows),
		})
	}
	sortStrengthResults(results)

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

func sortStrengthResults(results []strengthResult) {
	sort.Slice(results, func(i, j int) bool {
		if results[i].Score == results[j].Score {
			if results[i].SelectionNetPnL == results[j].SelectionNetPnL {
				return results[i].Param.label() < results[j].Param.label()
			}
			return results[i].SelectionNetPnL > results[j].SelectionNetPnL
		}
		return results[i].Score > results[j].Score
	})
}

func buildStrengthParams(starts, rankEnds, exits []string, gaps []float64, broadMins []int, confirms []float64, units int, model string, allowPartial bool) []strengthParam {
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
								MinGap: gap, BroadMin: broadMin, Confirm: confirm, Units: units,
								CurrencyModel: model,
								AllowPartial:  allowPartial,
							})
						}
					}
				}
			}
		}
	}
	return params
}

func currencyStrengthResearchInstruments(configured []config.Instrument) []config.Instrument {
	defaults := []config.Instrument{
		{Symbol: "AUD/CHF", SecType: "CASH", MinLotSize: 5000, PipBuffer: 0.0002, MaxHoldMinutes: 180},
		{Symbol: "AUD/JPY", SecType: "CASH", MinLotSize: 5000, PipBuffer: 0.02, MaxHoldMinutes: 180},
		{Symbol: "AUD/USD", SecType: "CASH", MinLotSize: 5000, PipBuffer: 0.0002, MaxHoldMinutes: 180},
		{Symbol: "EUR/AUD", SecType: "CASH", MinLotSize: 5000, PipBuffer: 0.0002, MaxHoldMinutes: 180},
		{Symbol: "EUR/CHF", SecType: "CASH", MinLotSize: 5000, PipBuffer: 0.0002, MaxHoldMinutes: 180},
		{Symbol: "EUR/GBP", SecType: "CASH", MinLotSize: 5000, PipBuffer: 0.0002, MaxHoldMinutes: 180},
		{Symbol: "EUR/JPY", SecType: "CASH", MinLotSize: 5000, PipBuffer: 0.02, MaxHoldMinutes: 180},
		{Symbol: "EUR/USD", SecType: "CASH", MinLotSize: 5000, PipBuffer: 0.0002, MaxHoldMinutes: 240},
		{Symbol: "GBP/AUD", SecType: "CASH", MinLotSize: 5000, PipBuffer: 0.0002, MaxHoldMinutes: 180},
		{Symbol: "GBP/CHF", SecType: "CASH", MinLotSize: 5000, PipBuffer: 0.0002, MaxHoldMinutes: 180},
		{Symbol: "GBP/JPY", SecType: "CASH", MinLotSize: 5000, PipBuffer: 0.02, MaxHoldMinutes: 180},
		{Symbol: "GBP/USD", SecType: "CASH", MinLotSize: 5000, PipBuffer: 0.0002, MaxHoldMinutes: 180},
		{Symbol: "CHF/JPY", SecType: "CASH", MinLotSize: 5000, PipBuffer: 0.02, MaxHoldMinutes: 180},
		{Symbol: "USD/CHF", SecType: "CASH", MinLotSize: 5000, PipBuffer: 0.0002, MaxHoldMinutes: 180},
		{Symbol: "USD/JPY", SecType: "CASH", MinLotSize: 5000, PipBuffer: 0.02, MaxHoldMinutes: 180},
	}
	configuredBySymbol := make(map[string]config.Instrument, len(configured))
	for _, inst := range configured {
		configuredBySymbol[inst.Symbol] = inst
	}
	out := make([]config.Instrument, 0, len(defaults))
	for _, fallback := range defaults {
		inst := fallback
		if configured, ok := configuredBySymbol[fallback.Symbol]; ok {
			inst = configured
		}
		out = append(out, withDefaultSpotFXCommission(inst))
	}
	return out
}

func strengthPairs(instruments []config.Instrument) ([]strengthPair, error) {
	var pairs []strengthPair
	for _, inst := range instruments {
		base, quote, ok := currencystrength.SplitPair(inst.Symbol)
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
		if v < 0 || math.IsNaN(v) || math.IsInf(v, 0) {
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

func maybeStrengthScoreWindows(mode string, analysis bool) ([]strengthEvalWindow, error) {
	if mode != "overall" && mode != "walk-forward" {
		return nil, fmt.Errorf("--score-mode must be overall or walk-forward")
	}
	if mode != "walk-forward" && !analysis {
		return nil, nil
	}
	return strengthScoreWindows()
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
	// Load one symbol across the complete date range before moving to the next.
	// This lets file-backed providers parse and cache a consolidated symbol once.
	for _, pair := range pairs {
		for day := fadeTruncateDay(from); !day.After(to); day = day.AddDate(0, 0, 1) {
			values, err := strengthDayCloses(ctx, provider, pair.Instrument.Symbol, day, clockTimes)
			if err != nil {
				return nil, err
			}
			if len(values) > 0 {
				if out[day] == nil {
					out[day] = make(map[string]map[string]float64)
				}
				out[day][pair.Instrument.Symbol] = values
			}
		}
	}
	return out, nil
}

func strengthDayCloses(ctx context.Context, provider data.MarketDataProvider, symbol string, day time.Time, clockTimes []string) (map[string]float64, error) {
	// A midnight decision needs the final completed minute from the prior day.
	// Keep the lookback bounded by the same freshness rule used below.
	fetchStart := day.Add(-strengthSnapshotMaxLag)
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
			lag := target.Sub(c.Timestamp)
			// Candle timestamps identify the minute's open. The close is only
			// knowable one minute later, so the target-minute bar is future data.
			if lag > 0 && lag <= strengthSnapshotMaxLag {
				out[clock] = c.Close
			}
		}
	}
	return out, nil
}

func runStrengthParam(snaps strengthSnapshots, pairs []strengthPair, costs map[string]orbreplay.CostModel, p strengthParam, from, to time.Time, tradeSymbols map[string]bool) []orbreplay.SimTrade {
	return runStrengthParamScoped(snaps, pairs, costs, p, from, to, strengthEvalScope{TradeSymbols: tradeSymbols})
}

type strengthEvalScope struct {
	TradeSymbols map[string]bool
	ScoreSymbols map[string]bool
}

func runStrengthParamScoped(snaps strengthSnapshots, pairs []strengthPair, costs map[string]orbreplay.CostModel, p strengthParam, from, to time.Time, scope strengthEvalScope) []orbreplay.SimTrade {
	days := make([]time.Time, 0, len(snaps))
	for day := range snaps {
		if !day.Before(fadeTruncateDay(from)) && !day.After(to) {
			days = append(days, day)
		}
	}
	sort.Slice(days, func(i, j int) bool { return days[i].Before(days[j]) })
	var trades []orbreplay.SimTrade
	for _, day := range days {
		if trade := evaluateStrengthDayScoped(snaps[day], pairs, costs, p, day, scope).Trade; trade != nil {
			trades = append(trades, *trade)
		}
	}
	return trades
}

func simulateStrengthDay(day map[string]map[string]float64, pairs []strengthPair, costs map[string]orbreplay.CostModel, p strengthParam, date time.Time, tradeSymbols map[string]bool) *orbreplay.SimTrade {
	return evaluateStrengthDay(day, pairs, costs, p, date, tradeSymbols).Trade
}

func evaluateStrengthDay(day map[string]map[string]float64, pairs []strengthPair, costs map[string]orbreplay.CostModel, p strengthParam, date time.Time, tradeSymbols map[string]bool) strengthDayOutcome {
	return evaluateStrengthDayScoped(day, pairs, costs, p, date, strengthEvalScope{TradeSymbols: tradeSymbols})
}

func evaluateStrengthDayScoped(day map[string]map[string]float64, pairs []strengthPair, costs map[string]orbreplay.CostModel, p strengthParam, date time.Time, scope strengthEvalScope) strengthDayOutcome {
	evalPairs := make([]currencystrength.Pair, 0, len(pairs))
	windows := make(map[string]currencystrength.Window, len(pairs))
	bySymbol := make(map[string]config.Instrument, len(pairs))
	for _, pair := range pairs {
		evalPairs = append(evalPairs, currencystrength.Pair{Symbol: pair.Instrument.Symbol, Base: pair.Base, Quote: pair.Quote})
		bySymbol[pair.Instrument.Symbol] = pair.Instrument
		values := day[pair.Instrument.Symbol]
		start, okStart := values[p.Start]
		end, okEnd := values[p.RankEnd]
		if !okStart || !okEnd || start <= 0 || end <= 0 {
			continue
		}
		windows[pair.Instrument.Symbol] = currencystrength.Window{Start: start, End: end}
	}
	decision := currencystrength.Evaluate(evalPairs, windows, currencystrength.Config{
		MinGap:             p.MinGap,
		BroadMin:           p.BroadMin,
		ConfirmMinMove:     p.Confirm,
		TradeInstruments:   scope.TradeSymbols,
		ScoreInstruments:   scope.ScoreSymbols,
		ScoreMethod:        p.CurrencyModel,
		AllowPartialBasket: p.AllowPartial,
	})
	if decision.Signal == nil {
		return strengthDayOutcome{Reason: decision.SkipReason}
	}
	inst := bySymbol[decision.Signal.Symbol]
	routed := day[decision.Signal.Symbol]
	entryMid, entryOK := routed[p.RankEnd]
	exitMid, exitOK := routed[p.Exit]
	if !entryOK || !exitOK || entryMid <= 0 || exitMid <= 0 {
		return strengthDayOutcome{Reason: "missing_routed_price"}
	}
	_, quote, ok := currencystrength.SplitPair(decision.Signal.Symbol)
	if !ok {
		return strengthDayOutcome{Reason: "invalid_routed_pair"}
	}
	entryQuoteUSD, entryConversionOK := strengthFXRate(day, pairs, p.RankEnd, quote, "USD")
	exitQuoteUSD, exitConversionOK := strengthFXRate(day, pairs, p.Exit, quote, "USD")
	if !entryConversionOK || !exitConversionOK {
		return strengthDayOutcome{Reason: "missing_usd_conversion"}
	}
	entryTime := strengthTimestamp(date, p.RankEnd)
	exitTime := strengthTimestamp(date, p.Exit)
	return strengthDayOutcome{
		Trade:  resolveStrengthTrade(inst, decision.Signal.Side, entryMid, exitMid, entryQuoteUSD, exitQuoteUSD, entryTime, exitTime, costs[decision.Signal.Symbol], p.Units),
		Reason: "signal",
	}
}

func strengthTimestamp(day time.Time, clock string) time.Time {
	t, _ := time.Parse("15:04", clock)
	return day.Add(time.Duration(t.Hour())*time.Hour + time.Duration(t.Minute())*time.Minute)
}

func resolveStrengthTrade(inst config.Instrument, side string, entryMid, exitMid, entryQuoteUSD, exitQuoteUSD float64, entryTime, exitTime time.Time, cost orbreplay.CostModel, qty int) *orbreplay.SimTrade {
	half := cost.Spread / 2
	entryFill := entryMid
	exitFill := exitMid
	if side == "buy" {
		entryFill += half
		exitFill -= half
	} else {
		entryFill -= half
		exitFill += half
	}
	// Price PnL is denominated in the pair's quote currency. Convert it at
	// the exit snapshot; use each fill's contemporaneous rate for commission
	// notional so all reported results are in USD.
	gross := fadePnL(side, entryFill, exitFill, qty, exitQuoteUSD)
	commission := cost.PerFillCommission(qty, entryFill, entryQuoteUSD) + cost.PerFillCommission(qty, exitFill, exitQuoteUSD)
	return &orbreplay.SimTrade{
		Ticker: inst.Symbol, Side: side, EntryTime: entryTime, ExitTime: exitTime,
		EntryPrice: entryFill, ExitPrice: exitFill, Qty: qty, ExitType: "time_exit",
		GrossPnL: gross, Commission: commission, NetPnL: gross - commission,
		HoldDur: exitTime.Sub(entryTime),
	}
}

func strengthFXRate(day map[string]map[string]float64, pairs []strengthPair, clock, from, to string) (float64, bool) {
	if from == to {
		return 1, true
	}
	type edge struct {
		to     string
		rate   float64
		symbol string
	}
	graph := make(map[string][]edge)
	for _, pair := range pairs {
		price, ok := day[pair.Instrument.Symbol][clock]
		if !ok || price <= 0 || math.IsNaN(price) || math.IsInf(price, 0) {
			continue
		}
		if pair.Base == from && pair.Quote == to {
			return price, true
		}
		if pair.Base == to && pair.Quote == from {
			return 1 / price, true
		}
		graph[pair.Base] = append(graph[pair.Base], edge{to: pair.Quote, rate: price, symbol: pair.Instrument.Symbol})
		graph[pair.Quote] = append(graph[pair.Quote], edge{to: pair.Base, rate: 1 / price, symbol: pair.Instrument.Symbol})
	}
	for currency := range graph {
		sort.Slice(graph[currency], func(i, j int) bool {
			if graph[currency][i].to == graph[currency][j].to {
				return graph[currency][i].symbol < graph[currency][j].symbol
			}
			return graph[currency][i].to < graph[currency][j].to
		})
	}
	type node struct {
		currency string
		rate     float64
	}
	queue := []node{{currency: from, rate: 1}}
	seen := map[string]bool{from: true}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		for _, next := range graph[current.currency] {
			rate := current.rate * next.rate
			if next.to == to {
				return rate, true
			}
			if !seen[next.to] {
				seen[next.to] = true
				queue = append(queue, node{currency: next.to, rate: rate})
			}
		}
	}
	return 0, false
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
	selectionWindows := strengthSelectionWindows(windows)
	for _, window := range selectionWindows {
		trades := filterStrengthTrades(res.Trades, window.From, window.To)
		score := strengthScore(orbreplay.Summarize(window.Label, trades).Overall, windowMinTrades)
		if score < worst {
			worst = score
		}
	}
	if math.IsInf(worst, 1) {
		return math.Inf(-1)
	}
	selectionTrades := filterStrengthTradesForWindows(res.Trades, selectionWindows)
	return worst + strengthScore(orbreplay.Summarize("selection", selectionTrades).Overall, minTrades)*0.25
}

func strengthSelectionNetPnL(res orbreplay.Result, mode string, windows []strengthEvalWindow) float64 {
	if mode != "walk-forward" {
		return res.Overall.NetPnL
	}
	trades := filterStrengthTradesForWindows(res.Trades, strengthSelectionWindows(windows))
	return orbreplay.Summarize("selection", trades).Overall.NetPnL
}

func strengthSelectionTradeCount(res orbreplay.Result, mode string, windows []strengthEvalWindow) int {
	if mode != "walk-forward" {
		return res.Overall.Trades
	}
	return len(filterStrengthTradesForWindows(res.Trades, strengthSelectionWindows(windows)))
}

func strengthGateResult(res orbreplay.Result, minTrades int, mode string, windows []strengthEvalWindow) bool {
	if mode != "walk-forward" {
		return strengthGate(res.Overall, minTrades)
	}
	selectionWindows := strengthSelectionWindows(windows)
	selectionTrades := filterStrengthTradesForWindows(res.Trades, selectionWindows)
	if len(selectionTrades) < minTrades {
		return false
	}
	windowMinTrades := strengthWindowMinTrades(minTrades)
	for _, window := range selectionWindows {
		trades := filterStrengthTrades(res.Trades, window.From, window.To)
		if !strengthGate(orbreplay.Summarize(window.Label, trades).Overall, windowMinTrades) {
			return false
		}
	}
	return true
}

func strengthSelectionWindows(windows []strengthEvalWindow) []strengthEvalWindow {
	out := make([]strengthEvalWindow, 0, len(windows))
	for _, window := range windows {
		if window.Label != "forward" {
			out = append(out, window)
		}
	}
	return out
}

func filterStrengthTradesForWindows(trades []orbreplay.SimTrade, windows []strengthEvalWindow) []orbreplay.SimTrade {
	out := make([]orbreplay.SimTrade, 0, len(trades))
	for _, trade := range trades {
		for _, window := range windows {
			if !trade.EntryTime.Before(window.From) && !trade.EntryTime.After(window.To) {
				out = append(out, trade)
				break
			}
		}
	}
	return out
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
		if r.SelectionTrades < minTrades {
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
		if r.SelectionTrades >= btStrengthMinTrades {
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
	printStrengthDecisionOutcomes(snaps, pairs, costs, best.Param, from, to, tradeSymbols)
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

func printStrengthDecisionOutcomes(snaps strengthSnapshots, pairs []strengthPair, costs map[string]orbreplay.CostModel, p strengthParam, from, to time.Time, tradeSymbols map[string]bool) {
	counts := make(map[string]int)
	for day, snapshot := range snaps {
		if day.Before(fadeTruncateDay(from)) || day.After(to) {
			continue
		}
		outcome := evaluateStrengthDay(snapshot, pairs, costs, p, day, tradeSymbols)
		counts[cmpOr(outcome.Reason, "unknown")]++
	}
	type row struct {
		reason string
		count  int
	}
	rows := make([]row, 0, len(counts))
	for reason, count := range counts {
		rows = append(rows, row{reason: reason, count: count})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].count == rows[j].count {
			return rows[i].reason < rows[j].reason
		}
		return rows[i].count > rows[j].count
	})
	fmt.Printf("\nDecision outcomes\n")
	fmt.Printf("%-28s %7s\n", "OUTCOME", "DAYS")
	for _, item := range rows {
		fmt.Printf("%-28s %7d\n", item.reason, item.count)
	}
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
