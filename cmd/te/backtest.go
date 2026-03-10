package main

import (
	"context"
	"fmt"
	"math"
	"os"
	"strings"
	"time"

	"github.com/romeo-folie/trade-engine/internal/analysis"
	"github.com/romeo-folie/trade-engine/internal/backtest"
	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/data"
	"github.com/romeo-folie/trade-engine/internal/logger"
	"github.com/romeo-folie/trade-engine/internal/store"
	"github.com/spf13/cobra"
)

// ── Flags ─────────────────────────────────────────────────────────────────────

var (
	btTicker    string
	btStrategy  string
	btModel     string
	btFrom      string
	btTo        string
	btNoCache   bool
	btCompareID []string
	btLimit     int
)

// ── Command tree ──────────────────────────────────────────────────────────────

var backtestCmd = &cobra.Command{
	Use:   "backtest",
	Short: "Run and manage backtests",
}

func init() {
	// run
	runBTCmd.Flags().StringVar(&btTicker, "ticker", "", "Ticker symbol (required)")
	runBTCmd.Flags().StringVar(&btStrategy, "strategy", "trend_following", "Strategy name")
	runBTCmd.Flags().StringVar(&btModel, "model", "", "Model override (default: config primary)")
	runBTCmd.Flags().StringVar(&btFrom, "from", "", "Start date YYYY-MM-DD (required)")
	runBTCmd.Flags().StringVar(&btTo, "to", "", "End date YYYY-MM-DD (required)")
	runBTCmd.Flags().BoolVar(&btNoCache, "no-cache", false, "Bypass prompt cache")
	_ = runBTCmd.MarkFlagRequired("ticker")
	_ = runBTCmd.MarkFlagRequired("from")
	_ = runBTCmd.MarkFlagRequired("to")

	// list
	listBTCmd.Flags().IntVar(&btLimit, "limit", 20, "Maximum number of runs to display")

	// compare
	compareBTCmd.Flags().StringArrayVar(&btCompareID, "id", nil, "Run label to compare (repeat for each run)")
	_ = compareBTCmd.MarkFlagRequired("id")

	backtestCmd.AddCommand(runBTCmd, listBTCmd, compareBTCmd)
}

// ── te backtest run ───────────────────────────────────────────────────────────

var runBTCmd = &cobra.Command{
	Use:   "run",
	Short: "Run a new backtest",
	Example: `  te backtest run --ticker AAPL --strategy momentum --from 2024-01-01 --to 2024-12-31
  te backtest run --ticker TSLA --model claude-opus-4-5 --from 2023-06-01 --to 2024-06-01 --no-cache`,
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		if err := logger.Init(cfg.Logging.Level, cfg.Logging.Format); err != nil {
			return err
		}

		from, err := time.Parse("2006-01-02", btFrom)
		if err != nil {
			return fmt.Errorf("invalid --from date %q: %w", btFrom, err)
		}
		to, err := time.Parse("2006-01-02", btTo)
		if err != nil {
			return fmt.Errorf("invalid --to date %q: %w", btTo, err)
		}
		if !to.After(from) {
			return fmt.Errorf("--to must be after --from")
		}

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		if err := store.Connect(ctx); err != nil {
			return fmt.Errorf("database: %w", err)
		}

		// Wire dependencies.
		alpaca := data.NewAlpacaClient(
			os.Getenv(cfg.Markets.Stocks.APIKeyEnv),
			os.Getenv(cfg.Markets.Stocks.SecretEnv),
			cfg.Markets.Stocks.DataURL,
		)
		az := analysis.NewAnalyzer(cfg.Models)
		runner := backtest.NewRunner(cfg, alpaca, az)

		opts := backtest.RunOptions{
			Ticker:    strings.ToUpper(btTicker),
			Strategy:  btStrategy,
			Timeframe: data.Timeframe1Day,
			From:      from,
			To:        to,
			Model:     btModel,
			NoCache:   btNoCache,
		}

		fmt.Printf("Backtesting %s · %s · %s → %s\n",
			opts.Ticker, opts.Strategy, from.Format("2006-01-02"), to.Format("2006-01-02"))

		// Progress bar.
		var lastPct int
		progress := func(cur, total int) {
			pct := cur * 100 / total
			if pct/5 > lastPct/5 { // update every 5%
				bar := strings.Repeat("█", pct/5) + strings.Repeat("░", 20-pct/5)
				fmt.Printf("\r[%s] %3d%% (%d/%d candles)", bar, pct, cur, total)
				lastPct = pct
			}
		}

		result, err := runner.Run(ctx, opts, progress)
		fmt.Println() // newline after progress bar
		if err != nil {
			return err
		}

		printMetrics(result)
		return nil
	},
}

// ── te backtest list ──────────────────────────────────────────────────────────

var listBTCmd = &cobra.Command{
	Use:   "list",
	Short: "List past backtest runs",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		_ = logger.Init(cfg.Logging.Level, cfg.Logging.Format)

		ctx := context.Background()
		if err := store.Connect(ctx); err != nil {
			return fmt.Errorf("database: %w", err)
		}

		runs, err := store.ListRuns(ctx, btLimit)
		if err != nil {
			return err
		}
		if len(runs) == 0 {
			fmt.Println("No backtest runs found.")
			return nil
		}

		fmt.Printf("%-10s %-8s %-14s %-10s %-10s %8s %8s %8s\n",
			"Label", "Ticker", "Strategy", "From", "To", "Return%", "Sharpe", "MaxDD%")
		fmt.Println(strings.Repeat("─", 80))
		for _, r := range runs {
			ret := fmtPct(r.TotalReturnPct)
			sharpe := fmtF(r.SharpeRatio)
			dd := fmtPct(r.MaxDrawdownPct)
			fmt.Printf("%-10s %-8s %-14s %-10s %-10s %8s %8s %8s\n",
				r.Label, r.Ticker, truncate(r.Strategy, 14),
				r.FromDate.Format("2006-01-02"), r.ToDate.Format("2006-01-02"),
				ret, sharpe, dd,
			)
		}
		return nil
	},
}

// ── te backtest compare ───────────────────────────────────────────────────────

var compareBTCmd = &cobra.Command{
	Use:   "compare",
	Short: "Compare two or more backtest runs side-by-side",
	Example: `  te backtest compare --id BT-00001 --id BT-00002`,
	RunE: func(cmd *cobra.Command, args []string) error {
		if len(btCompareID) < 2 {
			return fmt.Errorf("provide at least two --id flags to compare")
		}

		cfg, err := config.Load()
		if err != nil {
			return err
		}
		_ = logger.Init(cfg.Logging.Level, cfg.Logging.Format)

		ctx := context.Background()
		if err := store.Connect(ctx); err != nil {
			return fmt.Errorf("database: %w", err)
		}

		runs := make([]*store.BacktestRun, 0, len(btCompareID))
		for _, id := range btCompareID {
			r, err := store.GetRunByLabel(ctx, strings.ToUpper(id))
			if err != nil {
				return fmt.Errorf("fetch run %s: %w", id, err)
			}
			if r == nil {
				return fmt.Errorf("run %s not found", id)
			}
			runs = append(runs, r)
		}

		printComparison(runs)
		return nil
	},
}

// ── Output formatting ─────────────────────────────────────────────────────────

func printMetrics(r *backtest.RunResult) {
	m := r.Metrics
	hits, misses := r.Hits, r.Misses
	total := hits + misses
	hitPct := 0.0
	if total > 0 {
		hitPct = float64(hits) / float64(total) * 100
	}

	fmt.Printf("\n╔═══════════════════════════════════╗\n")
	fmt.Printf("║  Backtest Complete: %-14s ║\n", r.Label)
	fmt.Printf("╚═══════════════════════════════════╝\n\n")

	fmt.Printf("  Ticker:     %s\n", r.Options.Ticker)
	fmt.Printf("  Strategy:   %s\n", r.Options.Strategy)
	fmt.Printf("  Period:     %s → %s\n",
		r.Options.From.Format("2006-01-02"), r.Options.To.Format("2006-01-02"))
	fmt.Printf("  Cache:      %d hits / %d misses (%.0f%%)\n\n", hits, misses, hitPct)

	fmt.Println("  Performance")
	fmt.Println("  ───────────────────────────────────")
	printRow("Total Return", fmt.Sprintf("%.2f%%", m.TotalReturnPct))
	printRow("Annualised Return", fmt.Sprintf("%.2f%%", m.AnnualizedReturnPct))
	printRow("Max Drawdown", fmt.Sprintf("%.2f%%", m.MaxDrawdownPct))
	printRow("Sharpe Ratio", fmt.Sprintf("%.3f", m.SharpeRatio))
	printRow("Sortino Ratio", fmt.Sprintf("%.3f", m.SortinoRatio))
	printRow("Calmar Ratio", fmt.Sprintf("%.3f", m.CalmarRatio))
	fmt.Println()
	fmt.Println("  Trade Statistics")
	fmt.Println("  ───────────────────────────────────")
	printRow("Total Trades", fmt.Sprintf("%d", m.TotalTrades))
	printRow("Win Rate", fmt.Sprintf("%.1f%%", m.WinRatePct))
	printRow("Profit Factor", fmt.Sprintf("%.2f", m.ProfitFactor))
	printRow("Expectancy", fmt.Sprintf("$%.2f", m.Expectancy))
	printRow("Avg Hold Time", fmt.Sprintf("%.1fh", m.AvgHoldHours))
	printRow("Best Trade", fmt.Sprintf("%.2f%%", m.BestTradePct))
	printRow("Worst Trade", fmt.Sprintf("%.2f%%", m.WorstTradePct))
	fmt.Println()
}

func printComparison(runs []*store.BacktestRun) {
	// Header row
	labels := make([]string, len(runs))
	for i, r := range runs {
		labels[i] = r.Label
	}
	colW := 14
	fmt.Printf("  %-26s", "Metric")
	for _, l := range labels {
		fmt.Printf("  %-*s", colW, l)
	}
	fmt.Println()
	fmt.Println("  " + strings.Repeat("─", 26+len(runs)*(colW+2)))

	type metricRow struct {
		name string
		vals []*float64
	}

	rows := []metricRow{
		{"Ticker", nil},
		{"Period", nil},
		{"Strategy", nil},
		{"Total Return %", ptrSlice(runs, func(r *store.BacktestRun) *float64 { return r.TotalReturnPct })},
		{"Annualised Return %", ptrSlice(runs, func(r *store.BacktestRun) *float64 { return r.AnnualizedReturnPct })},
		{"Max Drawdown %", ptrSlice(runs, func(r *store.BacktestRun) *float64 { return r.MaxDrawdownPct })},
		{"Sharpe Ratio", ptrSlice(runs, func(r *store.BacktestRun) *float64 { return r.SharpeRatio })},
		{"Sortino Ratio", ptrSlice(runs, func(r *store.BacktestRun) *float64 { return r.SortinoRatio })},
		{"Calmar Ratio", ptrSlice(runs, func(r *store.BacktestRun) *float64 { return r.CalmarRatio })},
		{"Win Rate %", ptrSlice(runs, func(r *store.BacktestRun) *float64 { return r.WinRatePct })},
		{"Profit Factor", ptrSlice(runs, func(r *store.BacktestRun) *float64 { return r.ProfitFactor })},
		{"Expectancy $", ptrSlice(runs, func(r *store.BacktestRun) *float64 { return r.Expectancy })},
		{"Total Trades", nil},
		{"Cache Hit Rate %", ptrSlice(runs, func(r *store.BacktestRun) *float64 { return r.CacheHitRate })},
	}

	for _, row := range rows {
		fmt.Printf("  %-26s", row.name)
		for i, r := range runs {
			var cell string
			switch row.name {
			case "Ticker":
				cell = r.Ticker
			case "Period":
				cell = r.FromDate.Format("2006-01-02") + "…"
			case "Strategy":
				cell = truncate(r.Strategy, colW)
			case "Total Trades":
				cell = fmt.Sprintf("%d", r.TotalTrades)
			default:
				if row.vals != nil && row.vals[i] != nil {
					cell = fmt.Sprintf("%.3f", *row.vals[i])
				} else {
					cell = "—"
				}
			}
			fmt.Printf("  %-*s", colW, truncate(cell, colW))
		}
		fmt.Println()
	}
	fmt.Println()
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func printRow(label, value string) {
	fmt.Printf("  %-22s %s\n", label, value)
}

func fmtPct(p *float64) string {
	if p == nil {
		return "—"
	}
	return fmt.Sprintf("%.2f%%", *p)
}

func fmtF(f *float64) string {
	if f == nil || math.IsInf(*f, 0) || math.IsNaN(*f) {
		return "—"
	}
	return fmt.Sprintf("%.3f", *f)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func ptrSlice(runs []*store.BacktestRun, fn func(*store.BacktestRun) *float64) []*float64 {
	out := make([]*float64, len(runs))
	for i, r := range runs {
		out[i] = fn(r)
	}
	return out
}
