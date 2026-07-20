package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/romeo-folie/trade-engine/internal/backtest/orbreplay"
	"github.com/romeo-folie/trade-engine/internal/backtest/sweep"
	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/logger"
	"github.com/romeo-folie/trade-engine/internal/store"
	"github.com/spf13/cobra"
)

var (
	btSweepMode        string
	btSweepConcurrency int
	btSweepTop         int
	btSweepMaxRuns     int
	btSweepFromRun     string
	btSweepValFrom     string
	btSweepValTo       string
	btSweepFwdFrom     string
	btSweepFwdTo       string
	btSweepCloseFrac   string
	btSweepMaxHolds    string
)

func init() {
	orbSweepCmd.Flags().StringVar(&btORBFrom, "from", "", "Coarse: start date YYYY-MM-DD (the train window)")
	orbSweepCmd.Flags().StringVar(&btORBTo, "to", "", "Coarse: end date YYYY-MM-DD")
	orbSweepCmd.Flags().StringVar(&btORBSession, "session", "", "Coarse: restrict to one session (london | newyork | newyork_et)")
	orbSweepCmd.Flags().StringVar(&btORBInstruments, "instruments", "", "Comma-separated symbols (default: all configured)")
	orbSweepCmd.Flags().StringVar(&btORBProvider, "provider", "", "Data provider: histdata (default) | dukascopy | exness | oanda")
	orbSweepCmd.Flags().StringVar(&btORBAssetClass, "asset-class", "forex", "Instrument universe: forex (forex_orb.instruments) | futures (futures_orb.instruments via free index-CFD proxy data)")
	orbSweepCmd.Flags().IntVar(&btORBDukaPaceMs, "duka-pace-ms", 0, "Dukascopy politeness: >0 forces 1 in-flight fetch spaced this many ms apart (overrides config). Warm the cache first (resumable) with a paced single-variant backtest, then sweep off disk.")
	orbSweepCmd.Flags().Float64Var(&btORBCapital, "capital", 100000, "Initial capital for position sizing")
	orbSweepCmd.Flags().StringVar(&btSweepMode, "mode", "coarse", "Sweep mode: coarse | validate")
	orbSweepCmd.Flags().IntVar(&btSweepConcurrency, "concurrency", 8, "Parameter sets to evaluate in parallel")
	orbSweepCmd.Flags().IntVar(&btSweepTop, "top", 25, "How many ranked candidates to print")
	orbSweepCmd.Flags().IntVar(&btSweepMaxRuns, "max-runs", 0, "Cap the number of parameter sets (0 = full grid)")
	orbSweepCmd.Flags().StringVar(&btSweepMaxHolds, "max-holds", "", "Coarse: max-hold minute levels to sweep, comma-separated (e.g. 60,90,120,180)")
	orbSweepCmd.Flags().StringVar(&btSweepCloseFrac, "close-frac", "", "Coarse: close-position guard threshold levels to sweep, comma-separated (e.g. 0,0.55,0.6,0.65,0.7). Default: 0 (guard off)")
	// validate mode
	orbSweepCmd.Flags().StringVar(&btSweepFromRun, "from-run", "", "validate: parent coarse sweep_id whose gate-passers to walk-forward")
	orbSweepCmd.Flags().StringVar(&btSweepValFrom, "validate-from", "2025-01-01", "validate: validation window start")
	orbSweepCmd.Flags().StringVar(&btSweepValTo, "validate-to", "2025-12-31", "validate: validation window end")
	orbSweepCmd.Flags().StringVar(&btSweepFwdFrom, "forward-from", "2026-01-01", "validate: forward/holdout window start")
	orbSweepCmd.Flags().StringVar(&btSweepFwdTo, "forward-to", "", "validate: forward window end (default: today)")
	backtestCmd.AddCommand(orbSweepCmd)
}

// ── te backtest orb-sweep ─────────────────────────────────────────────────────

var orbSweepCmd = &cobra.Command{
	Use:   "orb-sweep",
	Short: "Search the ORB parameter space (coarse-grid-then-refine) and rank by robustness",
	Long: `Generate a parameter grid, backtest every combination over the window, persist the
results to Postgres (sweep_runs / sweep_results), and rank candidates by a robustness score
(expectancy per trade, gated on a trade-count floor — NOT net profit).

Results are stored so a later refine/validate run can seed from this run's survivors.`,
	Example: `  te backtest orb-sweep --mode coarse --from 2023-01-01 --to 2024-12-31
  te backtest orb-sweep --mode validate --from-run <coarse_sweep_id>`,
	RunE: func(cmd *cobra.Command, args []string) error {
		switch btSweepMode {
		case "coarse":
			if btORBFrom == "" || btORBTo == "" {
				return fmt.Errorf("coarse mode requires --from and --to")
			}
			return runCoarseSweep(cmd.Context())
		case "validate":
			if btSweepFromRun == "" {
				return fmt.Errorf("validate mode requires --from-run <coarse sweep_id>")
			}
			return runValidateSweep(cmd.Context())
		default:
			return fmt.Errorf("unsupported --mode %q (coarse | validate)", btSweepMode)
		}
	},
}

// parseFloatCSV parses a comma-separated list of floats (e.g. "0,0.55,0.6") for the
// --close-frac sweep flag. Each threshold must be in [0, 1) — a value >= 1 would require a
// close exactly at the range high to arm the buy side, which never fills.
func parseFloatCSV(s string) ([]float64, error) {
	parts := splitCSV(s)
	out := make([]float64, 0, len(parts))
	for _, p := range parts {
		v, err := strconv.ParseFloat(p, 64)
		if err != nil {
			return nil, fmt.Errorf("%q is not a number", p)
		}
		if v < 0 || v >= 1 {
			return nil, fmt.Errorf("threshold %v out of range [0, 1)", v)
		}
		out = append(out, v)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no values")
	}
	return out, nil
}

func parseIntCSV(s string) ([]int, error) {
	parts := splitCSV(s)
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		v, err := strconv.Atoi(p)
		if err != nil {
			return nil, fmt.Errorf("%q is not an integer", p)
		}
		if v < 0 {
			return nil, fmt.Errorf("value %v out of range [0, infinity)", v)
		}
		out = append(out, v)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no values")
	}
	return out, nil
}

func runCoarseSweep(ctx context.Context) error {
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

	// Base config: the coarse grid overrides session/range/risk-reward per set, so the base only
	// supplies the fields the sweep does not vary (timezone, ATR/trend settings, sizing caps).
	// The universe (instruments, costs, provider, sweepable sessions) comes from --asset-class.
	uni, err := buildBacktestUniverse(cfg)
	if err != nil {
		return err
	}

	grid := sweep.DefaultCoarseGrid()
	grid.Sessions = uni.sessions // e.g. futures sweeps only the DST-aware US RTH open
	if btORBSession != "" {
		grid.Sessions = []string{btORBSession}
	}
	if btSweepMaxHolds != "" {
		levels, err := parseIntCSV(btSweepMaxHolds)
		if err != nil {
			return fmt.Errorf("invalid --max-holds %q: %w", btSweepMaxHolds, err)
		}
		grid.MaxHoldMinutes = levels
	}
	if btSweepCloseFrac != "" {
		levels, err := parseFloatCSV(btSweepCloseFrac)
		if err != nil {
			return fmt.Errorf("invalid --close-frac %q: %w", btSweepCloseFrac, err)
		}
		grid.CloseFrac = levels
		fmt.Printf("NOTE: sweeping close-position guard levels %v (×%d the base grid)\n", levels, len(levels))
	}
	sets := grid.GenerateCoarse()
	if btSweepMaxRuns > 0 && btSweepMaxRuns < len(sets) {
		fmt.Printf("NOTE: capping %d parameter sets to --max-runs %d\n", len(sets), btSweepMaxRuns)
		sets = sets[:btSweepMaxRuns]
	}

	if err := store.Connect(ctx); err != nil {
		return fmt.Errorf("connect DB: %w", err)
	}
	db := store.GetDB()
	sweepID, err := store.CreateSweepRun(ctx, db, "coarse", from, to, nil, assetClassNote(""))
	if err != nil {
		return fmt.Errorf("create sweep run: %w", err)
	}

	fmt.Printf("orb-sweep coarse · %s -> %s · %d sets · %d instruments · concurrency %d\nsweep_id %s\n\n",
		btORBFrom, btORBTo, len(sets), len(uni.instruments), btSweepConcurrency, sweepID)

	runner := sweep.NewRunner(uni.provider, uni.base, uni.instruments, uni.costs, btORBCapital, btSweepConcurrency)
	results := runner.RunAll(ctx, sets, from, to, func(done, total int) {
		if done%25 == 0 || done == total {
			fmt.Printf("\r  evaluated %d/%d", done, total)
		}
	})
	fmt.Println()

	rows, failed := sweepResultRows(sweepID, "full", results)
	if len(results) > 0 && failed == len(results) {
		// Every set errored — a provider outage, an unsupported instrument, or an empty window.
		// This is a failed run, not a valid empty one; fail loudly rather than record success.
		return fmt.Errorf("sweep failed: all %d parameter sets errored (check provider/instruments/date range); sweep_id %s", failed, sweepID)
	}
	if err := store.InsertSweepResults(ctx, db, rows); err != nil {
		return fmt.Errorf("persist results: %w", err)
	}
	if err := store.SetSweepRunParamCount(ctx, db, sweepID, len(sets)); err != nil {
		return err
	}
	if failed > 0 {
		fmt.Printf("WARNING: %d/%d parameter sets errored and were skipped\n", failed, len(sets))
	}

	return printTopCandidates(ctx, sweepID)
}

// sweepResultRows flattens each successful ParamResult into DB rows: one "ALL" portfolio row plus
// one per instrument, each carrying its metrics JSON and robustness score. Errored sets are
// counted (failed) and skipped.
func sweepResultRows(sweepID, split string, results []sweep.ParamResult) (rows []store.SweepResult, failed int) {
	for _, pr := range results {
		if pr.Err != nil {
			failed++
			continue
		}
		params := pr.Params.JSON()
		hash := pr.Params.Hash()
		add := func(scope string, m orbreplay.Metrics) {
			rows = append(rows, store.SweepResult{
				SweepID:            sweepID,
				ParameterSetHash:   hash,
				ResolvedParameters: params,
				InstrumentScope:    scope,
				PeriodSplit:        split,
				Metrics:            metricsJSON(m),
				Trades:             m.Trades,
				RankScore:          sweep.RobustnessScore(m, sweep.MinTradesFloor),
			})
		}
		add("ALL", pr.Result.Overall)
		for sym, m := range pr.Result.ByInstrument {
			add(sym, m)
		}
	}
	return rows, failed
}

// pfSentinel is the finite stand-in for an all-winners profit factor. computeMetrics uses
// +Inf when a run has net winners and zero net losers, but encoding/json rejects ±Inf/NaN, so
// storing it verbatim would marshal to nil → a NULL that violates metrics JSONB NOT NULL and
// aborts the sweep. The sentinel is large enough to clear every acceptance gate (>1.15) and
// RobustnessScore/PassesAcceptanceGates read the real Metrics, not this JSON, so ranking is
// unaffected — this only makes the stored/displayed value representable.
const pfSentinel = 999.0

// metricsJSON serialises the metrics fields worth querying later. Non-finite floats are mapped
// to finite stand-ins so the JSONB column is never nil.
func metricsJSON(m orbreplay.Metrics) json.RawMessage {
	pf := m.ProfitFactor
	if math.IsInf(pf, 0) || math.IsNaN(pf) {
		pf = pfSentinel
	}
	b, _ := json.Marshal(map[string]any{
		"trades":         m.Trades,
		"win_rate":       m.WinRate,
		"net_pnl":        m.NetPnL,
		"profit_factor":  pf,
		"expectancy_r":   m.ExpectancyR,
		"max_loss_r":     m.MaxLossR,
		"avg_winner_r":   m.AvgWinnerR,
		"avg_loser_r":    m.AvgLoserR,
		"overnight_hold": m.OvernightHold,
		"commission":     m.Commission,
	})
	return b
}

// printTopCandidates prints the run's best portfolio-level ("ALL") candidates by rank score.
func printTopCandidates(ctx context.Context, sweepID string) error {
	top, err := store.TopSweepResults(ctx, store.GetDB(), sweepID, "ALL", "full", btSweepTop)
	if err != nil {
		return fmt.Errorf("query top results: %w", err)
	}
	fmt.Printf("\nTop %d candidates by robustness (portfolio, %d shown):\n\n", btSweepTop, len(top))
	fmt.Printf("%-4s %-16s %7s %6s %6s %8s %11s %8s %5s\n",
		"#", "HASH", "TRADES", "WIN%", "PF", "EXP_R", "NET_PNL", "SCORE", "GATE")
	for i, r := range top {
		var m struct {
			Trades       int     `json:"trades"`
			WinRate      float64 `json:"win_rate"`
			ProfitFactor float64 `json:"profit_factor"`
			ExpectancyR  float64 `json:"expectancy_r"`
			NetPnL       float64 `json:"net_pnl"`
			MaxLossR     float64 `json:"max_loss_r"`
		}
		_ = json.Unmarshal(r.Metrics, &m)
		gate := ""
		if sweep.PassesAcceptanceGates(orbreplay.Metrics{
			Trades: m.Trades, ExpectancyR: m.ExpectancyR, ProfitFactor: m.ProfitFactor,
		}, sweep.MinTradesFloor) {
			gate = "PASS"
		}
		fmt.Printf("%-4d %-16s %7d %5.1f%% %6.2f %8.3f %11.2f %8.3f %5s\n",
			i+1, r.ParameterSetHash, m.Trades, m.WinRate*100, m.ProfitFactor, m.ExpectancyR, m.NetPnL, r.RankScore, gate)
	}
	return nil
}

// ── validate mode (walk-forward) ──────────────────────────────────────────────

// runValidateSweep takes a parent coarse run's gate-passing configs and re-runs each over the
// validation and forward windows, so a config that only shone in-sample is exposed. Results are
// stored under a new run (parent_id = the coarse run) with period_split validate/forward, and the
// report shows train/validate/forward side by side.
func runValidateSweep(ctx context.Context) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if err := logger.Init(cfg.Logging.Level, cfg.Logging.Format); err != nil {
		return err
	}
	valFrom, valTo, err := parseWindow(btSweepValFrom, btSweepValTo)
	if err != nil {
		return fmt.Errorf("validate window: %w", err)
	}
	fwdToStr := btSweepFwdTo
	if fwdToStr == "" {
		fwdToStr = time.Now().UTC().Format("2006-01-02")
	}
	fwdFrom, fwdTo, err := parseWindow(btSweepFwdFrom, fwdToStr)
	if err != nil {
		return fmt.Errorf("forward window: %w", err)
	}

	uni, err := buildBacktestUniverse(cfg)
	if err != nil {
		return err
	}

	if err := store.Connect(ctx); err != nil {
		return fmt.Errorf("connect DB: %w", err)
	}
	db := store.GetDB()
	parent, err := store.GetSweepRun(ctx, db, btSweepFromRun)
	if err != nil {
		return fmt.Errorf("parent run %s not found: %w", btSweepFromRun, err)
	}
	// The universe (instruments/costs/sessions) must match the one the parent was swept
	// with — otherwise futures survivors silently replay against forex instruments (or
	// vice versa) and the walk-forward table is plausible-looking nonsense. The parent's
	// asset class is stamped in its notes at coarse time.
	parentFutures := strings.Contains(parent.Notes, "asset_class=futures")
	if parentFutures != (btORBAssetClass == "futures") {
		parentClass, flagHint := "forex", "omit --asset-class (or pass forex)"
		if parentFutures {
			parentClass, flagHint = "futures", "re-run with --asset-class futures"
		}
		return fmt.Errorf("parent run %s was swept with asset_class=%s — %s", btSweepFromRun, parentClass, flagHint)
	}

	// Survivors: the parent's gate-passing configs (PF>1.15, EXP>0.05R, >=100 trades on any
	// instrument), deserialised back into parameter sets.
	survJSON, err := store.SurvivingParamSets(ctx, db, btSweepFromRun, 1.15, 0.05, sweep.MinTradesFloor)
	if err != nil {
		return fmt.Errorf("load survivors: %w", err)
	}
	if len(survJSON) == 0 {
		return fmt.Errorf("parent run %s has no gate-passing configs to validate", btSweepFromRun)
	}
	sets := make([]sweep.ParamSet, 0, len(survJSON))
	for _, j := range survJSON {
		var p sweep.ParamSet
		if err := json.Unmarshal(j, &p); err != nil {
			return fmt.Errorf("decode survivor params: %w", err)
		}
		sets = append(sets, p)
	}

	valRunID, err := store.CreateSweepRun(ctx, db, "validate", valFrom, valTo, &btSweepFromRun, assetClassNote("walk-forward of "+btSweepFromRun))
	if err != nil {
		return fmt.Errorf("create validate run: %w", err)
	}
	fmt.Printf("orb-sweep validate · from-run %s (train %s→%s)\n  %d survivors · validate %s→%s · forward %s→%s\n  validate_run %s\n\n",
		btSweepFromRun, parent.PeriodFrom.Format("2006-01-02"), parent.PeriodTo.Format("2006-01-02"),
		len(sets), btSweepValFrom, btSweepValTo, btSweepFwdFrom, fwdToStr, valRunID)

	runner := sweep.NewRunner(uni.provider, uni.base, uni.instruments, uni.costs, btORBCapital, btSweepConcurrency)
	prog := func(label string) func(done, total int) {
		return func(done, total int) {
			if done%10 == 0 || done == total {
				fmt.Printf("\r  %s %d/%d", label, done, total)
			}
		}
	}
	valResults := runner.RunAll(ctx, sets, valFrom, valTo, prog("validate"))
	fmt.Println()
	fwdResults := runner.RunAll(ctx, sets, fwdFrom, fwdTo, prog("forward "))
	fmt.Println()

	rows, valFailed := sweepResultRows(valRunID, "validate", valResults)
	fwdRows, fwdFailed := sweepResultRows(valRunID, "forward", fwdResults)
	// If EVERY config errored on a window, that's a data/provider failure — not "the edge died
	// out-of-sample". Fail loud rather than print a misleading 0-HOLDS report.
	if len(valResults) > 0 && valFailed == len(valResults) {
		return fmt.Errorf("validate failed: all %d configs errored on the validation window %s→%s (check data coverage)", valFailed, btSweepValFrom, btSweepValTo)
	}
	if len(fwdResults) > 0 && fwdFailed == len(fwdResults) {
		return fmt.Errorf("validate failed: all %d configs errored on the forward window %s→%s (check data coverage)", fwdFailed, btSweepFwdFrom, fwdToStr)
	}
	if valFailed > 0 || fwdFailed > 0 {
		fmt.Printf("WARNING: %d validate + %d forward config runs errored and were skipped (data gaps?)\n", valFailed, fwdFailed)
	}
	rows = append(rows, fwdRows...)
	if err := store.InsertSweepResults(ctx, db, rows); err != nil {
		return fmt.Errorf("persist validate results: %w", err)
	}
	if err := store.SetSweepRunParamCount(ctx, db, valRunID, len(sets)); err != nil {
		return err
	}
	return printWalkForward(ctx, btSweepFromRun, valRunID)
}

// assetClassNote stamps the sweep run's asset class into its notes so validate mode can
// verify a --from-run parent was swept over the same instrument universe. Forex (the
// default) is left unstamped for continuity with pre-futures runs.
func assetClassNote(base string) string {
	if btORBAssetClass != "futures" {
		return base
	}
	note := "asset_class=futures (index micros over CFD proxy data)"
	if base == "" {
		return note
	}
	return base + " · " + note
}

// parseWindow parses an inclusive [from, to] date window into UTC bounds (to extended to end-of-day).
func parseWindow(fromStr, toStr string) (time.Time, time.Time, error) {
	from, err := time.Parse("2006-01-02", fromStr)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("invalid from %q: %w", fromStr, err)
	}
	to, err := time.Parse("2006-01-02", toStr)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("invalid to %q: %w", toStr, err)
	}
	if to.Before(from) {
		return time.Time{}, time.Time{}, fmt.Errorf("to %q before from %q", toStr, fromStr)
	}
	return from, to.Add(24*time.Hour - time.Second), nil
}

// printWalkForward prints each surviving (config × instrument) with its train / validate / forward
// expectancy and trade counts, and flags configs whose edge HOLDS out-of-sample (positive
// expectancy in all three periods) — the only ones worth trusting.
func printWalkForward(ctx context.Context, parentID, valRunID string) error {
	// LEFT JOIN both OOS splits: a train-passer that produced ZERO trades in the validate or
	// forward window has no row for that split — the single clearest overfit signal — and must
	// show as a miss, not silently vanish. NULLS LAST keeps traded configs at the top.
	rows, err := store.GetDB().Query(ctx, `
		SELECT t.parameter_set_hash, t.instrument_scope,
		       round((t.metrics->>'expectancy_r')::numeric,3), t.trades,
		       round((v.metrics->>'expectancy_r')::numeric,3), v.trades,
		       round((f.metrics->>'expectancy_r')::numeric,3), f.trades
		FROM sweep_results t
		LEFT JOIN sweep_results v ON v.sweep_id=$2 AND v.period_split='validate'
		     AND v.parameter_set_hash=t.parameter_set_hash AND v.instrument_scope=t.instrument_scope
		LEFT JOIN sweep_results f ON f.sweep_id=$2 AND f.period_split='forward'
		     AND f.parameter_set_hash=t.parameter_set_hash AND f.instrument_scope=t.instrument_scope
		WHERE t.sweep_id=$1 AND t.period_split='full' AND t.instrument_scope<>'ALL'
		  AND t.trades>=100 AND (t.metrics->>'profit_factor')::numeric>1.15 AND (t.metrics->>'expectancy_r')::numeric>0.05
		ORDER BY t.instrument_scope, (v.metrics->>'expectancy_r')::numeric DESC NULLS LAST`,
		parentID, valRunID)
	if err != nil {
		return fmt.Errorf("walk-forward query: %w", err)
	}
	defer rows.Close()

	fmt.Printf("\nWALK-FORWARD (train-passers, expectancy_r per period; HOLDS = positive in all three, no-trade window = miss):\n\n")
	fmt.Printf("%-16s %-8s %8s %10s %10s   %s\n", "HASH", "INST", "TRAIN", "VALID", "FWD", "HOLDS")
	var held, total int
	col := func(e *float64, tr *int) string {
		if e == nil {
			return "  no-trade"
		}
		return fmt.Sprintf("%.3f(%d)", *e, *tr)
	}
	for rows.Next() {
		total++
		var hash, inst string
		var trainE float64
		var trainTr int
		var valE, fwdE *float64
		var valTr, fwdTr *int
		if err := rows.Scan(&hash, &inst, &trainE, &trainTr, &valE, &valTr, &fwdE, &fwdTr); err != nil {
			return err
		}
		// HOLDS: positive expectancy in ALL THREE periods. A no-trade OOS window is not a hold.
		holds := trainE > 0 && valE != nil && *valE > 0 && fwdE != nil && *fwdE > 0
		mark := ""
		if holds {
			mark = "✓ HOLDS"
			held++
		}
		fmt.Printf("%-16s %-8s %8.3f %10s %10s   %s\n", hash, inst, trainE, col(valE, valTr), col(fwdE, fwdTr), mark)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	fmt.Printf("\n%d of %d train-passing (config × instrument) HELD — positive expectancy in train, validate AND forward.\n", held, total)
	return nil
}
