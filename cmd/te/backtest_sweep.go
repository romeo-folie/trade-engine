package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
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
)

func init() {
	orbSweepCmd.Flags().StringVar(&btORBFrom, "from", "", "Start date YYYY-MM-DD (required)")
	orbSweepCmd.Flags().StringVar(&btORBTo, "to", "", "End date YYYY-MM-DD (required)")
	orbSweepCmd.Flags().StringVar(&btORBInstruments, "instruments", "", "Comma-separated symbols (default: all configured)")
	orbSweepCmd.Flags().StringVar(&btORBProvider, "provider", "", "Data provider: histdata (default) | dukascopy | oanda")
	orbSweepCmd.Flags().Float64Var(&btORBCapital, "capital", 100000, "Initial capital for position sizing")
	orbSweepCmd.Flags().StringVar(&btSweepMode, "mode", "coarse", "Sweep mode: coarse (broad low-resolution grid)")
	orbSweepCmd.Flags().IntVar(&btSweepConcurrency, "concurrency", 8, "Parameter sets to evaluate in parallel")
	orbSweepCmd.Flags().IntVar(&btSweepTop, "top", 25, "How many ranked candidates to print")
	orbSweepCmd.Flags().IntVar(&btSweepMaxRuns, "max-runs", 0, "Cap the number of parameter sets (0 = full grid)")
	_ = orbSweepCmd.MarkFlagRequired("from")
	_ = orbSweepCmd.MarkFlagRequired("to")
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
	Example: `  te backtest orb-sweep --mode coarse --from 2023-01-01 --to 2024-12-31`,
	RunE: func(cmd *cobra.Command, args []string) error {
		if btSweepMode != "coarse" {
			return fmt.Errorf("unsupported --mode %q (only 'coarse' is implemented; refine/validate land next)", btSweepMode)
		}
		return runCoarseSweep(cmd.Context())
	},
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
	base := cfg.ForexORB
	instruments, err := resolveInstruments(base, btORBInstruments)
	if err != nil {
		return err
	}
	provider, err := buildBacktestProvider(cfg)
	if err != nil {
		return err
	}
	costs := buildCostModels(instruments)

	sets := sweep.DefaultCoarseGrid().GenerateCoarse()
	if btSweepMaxRuns > 0 && btSweepMaxRuns < len(sets) {
		fmt.Printf("NOTE: capping %d parameter sets to --max-runs %d\n", len(sets), btSweepMaxRuns)
		sets = sets[:btSweepMaxRuns]
	}

	if err := store.Connect(ctx); err != nil {
		return fmt.Errorf("connect DB: %w", err)
	}
	db := store.GetDB()
	sweepID, err := store.CreateSweepRun(ctx, db, "coarse", from, to, nil, "")
	if err != nil {
		return fmt.Errorf("create sweep run: %w", err)
	}

	fmt.Printf("orb-sweep coarse · %s -> %s · %d sets · %d instruments · concurrency %d\nsweep_id %s\n\n",
		btORBFrom, btORBTo, len(sets), len(instruments), btSweepConcurrency, sweepID)

	runner := sweep.NewRunner(provider, base, instruments, costs, btORBCapital, btSweepConcurrency)
	results := runner.RunAll(ctx, sets, from, to, func(done, total int) {
		if done%25 == 0 || done == total {
			fmt.Printf("\r  evaluated %d/%d", done, total)
		}
	})
	fmt.Println()

	rows, failed := sweepResultRows(sweepID, results)
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
func sweepResultRows(sweepID string, results []sweep.ParamResult) (rows []store.SweepResult, failed int) {
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
				PeriodSplit:        "full", // coarse runs one window; train/validate/forward arrive with walk-forward
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
