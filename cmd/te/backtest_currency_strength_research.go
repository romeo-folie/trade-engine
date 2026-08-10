package main

import (
	"context"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	csresearch "github.com/romeo-folie/trade-engine/internal/backtest/currencystrengthresearch"
	"github.com/romeo-folie/trade-engine/internal/backtest/orbreplay"
	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/logger"
	"github.com/spf13/cobra"
)

type strengthResearchOptions struct {
	From, To, Provider, Instruments string
	Starts, RankEnds, Exits         string
	Gaps, BroadMins, Confirms       string
	CurrencyModel, CostMultipliers  string
	Units, MinTradesPerYear         int
	MinImprovementPerAffectedDay    float64
	MinAggregateImprovement         float64
	AllowPartial                    bool
	JSONOutput                      string
}

var btStrengthResearch strengthResearchOptions

func init() {
	f := currencyStrengthResearchCmd.Flags()
	f.StringVar(&btStrengthResearch.From, "from", "", "Research data start, YYYY-MM-DD (required)")
	f.StringVar(&btStrengthResearch.To, "to", "", "Research data end, YYYY-MM-DD (required)")
	f.StringVar(&btStrengthResearch.Provider, "provider", "", "Data provider: histdata | dukascopy | exness | oanda")
	f.StringVar(&btStrengthResearch.Instruments, "instruments", "", "Comma-separated subset of the default 15-pair FX research universe")
	f.StringVar(&btStrengthResearch.Starts, "starts", "06:00", "UTC strength-window starts")
	f.StringVar(&btStrengthResearch.RankEnds, "rank-ends", "07:00,08:00,09:00,10:00", "UTC rank/entry times")
	f.StringVar(&btStrengthResearch.Exits, "exits", "12:00,14:00,16:00,18:00,20:00", "UTC exits")
	f.StringVar(&btStrengthResearch.Gaps, "min-gaps", "0.0002,0.0006,0.001,0.002,0.003,0.005", "Minimum latent strength gaps")
	f.StringVar(&btStrengthResearch.BroadMins, "broad-mins", "1,2,3,4", "Breadth thresholds")
	f.StringVar(&btStrengthResearch.Confirms, "confirm-min-moves", "0", "Routed-pair confirmation thresholds")
	f.StringVar(&btStrengthResearch.CurrencyModel, "currency-model", "least_squares", "Currency model: sum | least_squares")
	f.BoolVar(&btStrengthResearch.AllowPartial, "allow-partial-basket", true, "Allow a degraded basket while its scoring graph remains connected")
	f.IntVar(&btStrengthResearch.Units, "units", 100000, "Fixed base-currency units")
	f.IntVar(&btStrengthResearch.MinTradesPerYear, "min-selection-trades-per-year", 75, "Candidate eligibility floor in every selection year")
	f.Float64Var(&btStrengthResearch.MinImprovementPerAffectedDay, "min-improvement-per-affected-day", 5, "Minimum USD P&L improvement per affected day in each improved fold")
	f.Float64Var(&btStrengthResearch.MinAggregateImprovement, "min-aggregate-improvement", 250, "Minimum aggregate USD P&L improvement at both 1x and 2x costs")
	f.StringVar(&btStrengthResearch.CostMultipliers, "cost-multipliers", "1,2", "Cost levels used for unseen comparisons")
	f.StringVar(&btStrengthResearch.JSONOutput, "json-output", "", "Optional path for the complete JSON report")
	_ = currencyStrengthResearchCmd.MarkFlagRequired("from")
	_ = currencyStrengthResearchCmd.MarkFlagRequired("to")
	backtestCmd.AddCommand(currencyStrengthResearchCmd)
}

var currencyStrengthResearchCmd = &cobra.Command{
	Use:   "currency-strength-research",
	Short: "Run rolling-origin selection and pair-ablation research",
	Long:  "Run version 1 of the currency-strength exclusion protocol. It requires exactly four expanding annual test folds, 75 selection trades per year by default, and material improvement at both 1x and 2x costs.",
	RunE: func(cmd *cobra.Command, _ []string) error {
		return runCurrencyStrengthResearch(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(), btStrengthResearch)
	},
}

type commandStrengthResearchSimulator struct {
	snaps strengthSnapshots
	pairs []strengthPair
	costs map[string]orbreplay.CostModel
	from  time.Time
	to    time.Time
}

func (s commandStrengthResearchSimulator) Run(_ context.Context, req csresearch.Request) ([]orbreplay.SimTrade, error) {
	param := strengthParam{
		Start: req.Param.Start, RankEnd: req.Param.RankEnd, Exit: req.Param.Exit,
		MinGap: req.Param.MinGap, BroadMin: req.Param.BroadMin, Confirm: req.Param.Confirm,
		Units: req.Param.Units, CurrencyModel: req.Param.CurrencyModel, AllowPartial: req.Param.AllowPartial,
	}
	return runStrengthParamScoped(s.snaps, s.pairs, multiplyStrengthResearchCosts(s.costs, req.CostMultiplier), param, s.from, s.to, strengthEvalScope{
		ScoreSymbols: symbolSetFromSlice(req.ScoreSymbols), TradeSymbols: symbolSetFromSlice(req.TradeSymbols),
	}), nil
}

func multiplyStrengthResearchCosts(costs map[string]orbreplay.CostModel, multiplier float64) map[string]orbreplay.CostModel {
	out := make(map[string]orbreplay.CostModel, len(costs))
	for symbol, cost := range costs {
		cost.Spread *= multiplier
		cost.CommissionPerSide *= multiplier
		cost.CommissionBpsPerSide *= multiplier
		cost.CommissionMinPerSide *= multiplier
		cost.CommissionPerUnit *= multiplier
		out[symbol] = cost
	}
	return out
}

func runCurrencyStrengthResearch(ctx context.Context, stdout, stderr io.Writer, opts strengthResearchOptions) error {
	from, to, err := parseWindow(opts.From, opts.To)
	if err != nil {
		return err
	}
	folds, err := csresearch.ExpandingAnnualFolds(from, to)
	if err != nil {
		return err
	}
	if len(folds) != 4 {
		return fmt.Errorf("exclusion protocol requires exactly four folds; %s through %s produced %d", opts.From, opts.To, len(folds))
	}
	if opts.MinTradesPerYear <= 0 || opts.Units <= 0 {
		return fmt.Errorf("units and min-selection-trades-per-year must be positive")
	}
	if opts.MinImprovementPerAffectedDay <= 0 || math.IsNaN(opts.MinImprovementPerAffectedDay) || math.IsInf(opts.MinImprovementPerAffectedDay, 0) || opts.MinAggregateImprovement <= 0 || math.IsNaN(opts.MinAggregateImprovement) || math.IsInf(opts.MinAggregateImprovement, 0) {
		return fmt.Errorf("exclusion improvement thresholds must be finite and positive")
	}
	if opts.CurrencyModel != "sum" && opts.CurrencyModel != "least_squares" {
		return fmt.Errorf("currency-model must be sum or least_squares")
	}
	if opts.AllowPartial && opts.CurrencyModel != "least_squares" {
		return fmt.Errorf("allow-partial-basket requires least_squares")
	}
	starts, err := parseClockList(opts.Starts)
	if err != nil {
		return fmt.Errorf("starts: %w", err)
	}
	rankEnds, err := parseClockList(opts.RankEnds)
	if err != nil {
		return fmt.Errorf("rank-ends: %w", err)
	}
	exits, err := parseClockList(opts.Exits)
	if err != nil {
		return fmt.Errorf("exits: %w", err)
	}
	gaps, err := parsePositiveFloatLevels(opts.Gaps)
	if err != nil {
		return fmt.Errorf("min-gaps: %w", err)
	}
	broadMins, err := parsePositiveIntCSV(opts.BroadMins)
	if err != nil {
		return fmt.Errorf("broad-mins: %w", err)
	}
	confirms, err := parseNonNegativeFloatLevels(opts.Confirms)
	if err != nil {
		return fmt.Errorf("confirm-min-moves: %w", err)
	}
	costs, err := parsePositiveFloatLevels(opts.CostMultipliers)
	if err != nil {
		return fmt.Errorf("cost-multipliers: %w", err)
	}
	if !containsFloat(costs, 1) || !containsFloat(costs, 2) {
		return fmt.Errorf("cost-multipliers must include 1 and 2 for the exclusion rule")
	}
	if duplicate, ok := duplicateFloat(costs); ok {
		return fmt.Errorf("cost-multipliers contains duplicate %v", duplicate)
	}
	params := buildStrengthParams(starts, rankEnds, exits, gaps, broadMins, confirms, opts.Units, opts.CurrencyModel, opts.AllowPartial)
	if len(params) == 0 {
		return fmt.Errorf("parameter grid has no valid time combinations")
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if err := logger.Init(cfg.Logging.Level, cfg.Logging.Format); err != nil {
		return err
	}
	providerName := strings.ToLower(cmpOr(opts.Provider, cmpOr(cfg.Markets.BacktestProvider, "histdata")))
	provider, err := buildBacktestProviderNamed(cfg, providerName, 0)
	if err != nil {
		return err
	}
	forexCfg := cfg.ForexORB
	forexCfg.Instruments = currencyStrengthResearchInstruments(cfg.ForexORB.Instruments)
	instruments, err := resolveInstruments(forexCfg, opts.Instruments)
	if err != nil {
		return err
	}
	pairs, err := strengthPairs(instruments)
	if err != nil {
		return err
	}
	snaps, err := loadStrengthSnapshots(ctx, provider, pairs, uniqueClockTimes(starts, rankEnds, exits), from, to)
	if err != nil {
		return err
	}

	researchParams := make([]csresearch.Param, 0, len(params))
	for _, p := range params {
		researchParams = append(researchParams, csresearch.Param{Start: p.Start, RankEnd: p.RankEnd, Exit: p.Exit, MinGap: p.MinGap, BroadMin: p.BroadMin, Confirm: p.Confirm, Units: p.Units, CurrencyModel: p.CurrencyModel, AllowPartial: p.AllowPartial})
	}
	universe := make([]string, 0, len(pairs))
	for _, pair := range pairs {
		universe = append(universe, pair.Instrument.Symbol)
	}
	sort.Strings(universe)
	criteria := csresearch.DefaultCriteria()
	criteria.MinTradesPerSelectionYear = opts.MinTradesPerYear
	criteria.CostMultipliers = costs
	criteria.MinImprovementPerAffectedDayUSD = opts.MinImprovementPerAffectedDay
	criteria.MinAggregateImprovementUSD = opts.MinAggregateImprovement
	runner := csresearch.Runner{
		Simulator: commandStrengthResearchSimulator{snaps: snaps, pairs: pairs, costs: buildCostModels(instruments), from: from, to: to},
		Params:    researchParams, Universe: universe, Folds: folds, Criteria: criteria,
		Progress: func(message string) { fmt.Fprintf(stderr, "%s\n", message) },
	}
	report, err := runner.Run(ctx)
	if err != nil {
		return err
	}
	if err := csresearch.WriteText(stdout, report); err != nil {
		return err
	}
	if opts.JSONOutput != "" {
		if err := writeStrengthResearchJSONAtomic(opts.JSONOutput, report); err != nil {
			return err
		}
		fmt.Fprintf(stderr, "JSON report: %s\n", opts.JSONOutput)
	}
	return nil
}

func symbolSetFromSlice(symbols []string) map[string]bool {
	if symbols == nil {
		return nil
	}
	out := make(map[string]bool, len(symbols))
	for _, symbol := range symbols {
		out[symbol] = true
	}
	return out
}

func containsFloat(values []float64, target float64) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func duplicateFloat(values []float64) (float64, bool) {
	seen := make(map[float64]bool, len(values))
	for _, value := range values {
		if seen[value] {
			return value, true
		}
		seen[value] = true
	}
	return 0, false
}

func writeStrengthResearchJSONAtomic(path string, report csresearch.Report) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".currency-strength-research-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary JSON report: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := csresearch.WriteJSON(tmp, report); err != nil {
		tmp.Close()
		return fmt.Errorf("encode JSON report: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync JSON report: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close JSON report: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("publish JSON report: %w", err)
	}
	return nil
}
