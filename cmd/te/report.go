package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/reports"
	"github.com/romeo-folie/trade-engine/internal/store"
)

// ── Root report command ───────────────────────────────────────────────────────

var reportCmd = &cobra.Command{
	Use:   "report",
	Short: "Generate performance reports",
}

func init() {
	// performance
	perfCmd := &cobra.Command{
		Use:   "performance",
		Short: "Generate a portfolio performance report",
		RunE:  runReportPerformance,
	}
	perfCmd.Flags().String("from", "", "Start date (YYYY-MM-DD); defaults to 30 days ago")
	perfCmd.Flags().String("to", "", "End date (YYYY-MM-DD); defaults to today")
	reportCmd.AddCommand(perfCmd)

	// daily
	dailyCmd := &cobra.Command{
		Use:   "daily",
		Short: "Generate a daily summary report",
		RunE:  runReportDaily,
	}
	dailyCmd.Flags().String("date", "", "Date (YYYY-MM-DD); defaults to today")
	reportCmd.AddCommand(dailyCmd)

	// weekly
	weeklyCmd := &cobra.Command{
		Use:   "weekly",
		Short: "Generate a weekly summary report",
		RunE:  runReportWeekly,
	}
	weeklyCmd.Flags().String("from", "", "Week start date (YYYY-MM-DD); defaults to 7 days ago")
	reportCmd.AddCommand(weeklyCmd)

	// backtest report
	btRptCmd := &cobra.Command{
		Use:   "backtest <id>",
		Short: "Generate a report for a single backtest run",
		Args:  cobra.ExactArgs(1),
		RunE:  runReportBacktest,
	}
	reportCmd.AddCommand(btRptCmd)

	// backtest compare
	btCompareCmd := &cobra.Command{
		Use:   "compare <id1> <id2>",
		Short: "Generate a side-by-side comparison of two backtest runs",
		Args:  cobra.ExactArgs(2),
		RunE:  runReportBacktestCompare,
	}
	reportCmd.AddCommand(btCompareCmd)
}

// ── Performance report ────────────────────────────────────────────────────────

func runReportPerformance(cmd *cobra.Command, _ []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := store.Connect(ctx); err != nil {
		return fmt.Errorf("database: %w", err)
	}

	to := time.Now()
	from := to.AddDate(0, 0, -30)

	if s, _ := cmd.Flags().GetString("from"); s != "" {
		if from, err = time.Parse("2006-01-02", s); err != nil {
			return fmt.Errorf("invalid --from date: %w", err)
		}
	}
	if s, _ := cmd.Flags().GetString("to"); s != "" {
		if to, err = time.Parse("2006-01-02", s); err != nil {
			return fmt.Errorf("invalid --to date: %w", err)
		}
	}

	snaps, err := store.GetPortfolioSnapshots(ctx, from, to)
	if err != nil {
		return fmt.Errorf("load snapshots: %w", err)
	}
	summaries, err := store.GetDailySummaries(ctx, from, to)
	if err != nil {
		return fmt.Errorf("load summaries: %w", err)
	}
	trades, err := store.GetClosedTradesByDateRange(ctx, from, to)
	if err != nil {
		return fmt.Errorf("load trades: %w", err)
	}

	gen, err := reports.New(cfg.Reports)
	if err != nil {
		return err
	}

	bodyPrefix, page, err := reports.BuildPerformancePage(reports.PerformanceData{
		From:      from,
		To:        to,
		Snapshots: snaps,
		Summaries: summaries,
		Trades:    trades,
	})
	if err != nil {
		return err
	}

	path, err := gen.WritePage(reports.ReportFilename("performance"), page, "", bodyPrefix)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stdout, "Report written: %s\n", path)

	// Phase 12.6: per-model accuracy breakdown (printed to stdout when data exists).
	modelRows, err := store.GetModelPerformance(ctx, 1)
	if err == nil && len(modelRows) > 0 {
		fmt.Println("\n── Per-Model Accuracy (Ensemble) ─────────────────────────────────────────────")
		fmt.Printf("%-14s  %-28s  %8s  %10s  %8s\n", "PROVIDER", "MODEL", "TRADES", "AVG P&L %", "WIN RATE")
		fmt.Println(strings.Repeat("─", 75))
		for _, r := range modelRows {
			pnl, wr := "─", "─"
			if r.AvgPnLPct != nil { pnl = fmt.Sprintf("%+.2f%%", *r.AvgPnLPct) }
			if r.WinRate != nil    { wr  = fmt.Sprintf("%.1f%%", *r.WinRate*100) }
			provider := "unknown"; if r.Provider != nil { provider = *r.Provider }
			model := "unknown"; if r.Model != nil { model = *r.Model }
			fmt.Printf("%-14s  %-28s  %8d  %10s  %8s\n", provider, model, r.TradeCount, pnl, wr)
		}
	}
	return nil
}

// ── Daily report ──────────────────────────────────────────────────────────────

func runReportDaily(cmd *cobra.Command, _ []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := store.Connect(ctx); err != nil {
		return fmt.Errorf("database: %w", err)
	}

	date := time.Now().Truncate(24 * time.Hour)
	if s, _ := cmd.Flags().GetString("date"); s != "" {
		if date, err = time.Parse("2006-01-02", s); err != nil {
			return fmt.Errorf("invalid --date: %w", err)
		}
	}

	summary, err := store.GetDailySummaryByDate(ctx, date)
	if err != nil {
		return fmt.Errorf("load summary: %w", err)
	}
	// GetClosedTradesByDateRange uses a half-open [from, to) interval — pass
	// the start of the next day so a trade closed at the very last second of
	// `date` is still included.
	dayEnd := date.Add(24 * time.Hour)
	trades, err := store.GetClosedTradesByDateRange(ctx, date, dayEnd)
	if err != nil {
		return fmt.Errorf("load trades: %w", err)
	}
	snaps, err := store.GetPortfolioSnapshots(ctx, date, dayEnd)
	if err != nil {
		return fmt.Errorf("load snapshots: %w", err)
	}

	gen, err := reports.New(cfg.Reports)
	if err != nil {
		return err
	}

	bodyPrefix, page, err := reports.BuildDailyPage(reports.DailyData{
		Date:      date,
		Summary:   summary,
		Trades:    trades,
		Snapshots: snaps,
	})
	if err != nil {
		return err
	}

	path, err := gen.WritePage(reports.ReportFilename("daily"), page, "", bodyPrefix)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stdout, "Report written: %s\n", path)
	return nil
}

// ── Weekly report ─────────────────────────────────────────────────────────────

func runReportWeekly(cmd *cobra.Command, _ []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := store.Connect(ctx); err != nil {
		return fmt.Errorf("database: %w", err)
	}

	to := time.Now()
	from := to.AddDate(0, 0, -7)
	if s, _ := cmd.Flags().GetString("from"); s != "" {
		var parseErr error
		if from, parseErr = time.Parse("2006-01-02", s); parseErr != nil {
			return fmt.Errorf("invalid --from date: %w", parseErr)
		}
	}

	summaries, err := store.GetDailySummaries(ctx, from, to)
	if err != nil {
		return fmt.Errorf("load summaries: %w", err)
	}
	trades, err := store.GetClosedTradesByDateRange(ctx, from, to)
	if err != nil {
		return fmt.Errorf("load trades: %w", err)
	}
	snaps, err := store.GetPortfolioSnapshots(ctx, from, to)
	if err != nil {
		return fmt.Errorf("load snapshots: %w", err)
	}

	gen, err := reports.New(cfg.Reports)
	if err != nil {
		return err
	}

	bodyPrefix, page, err := reports.BuildWeeklyPage(reports.WeeklyData{
		From:      from,
		To:        to,
		Summaries: summaries,
		Trades:    trades,
		Snapshots: snaps,
	})
	if err != nil {
		return err
	}

	path, err := gen.WritePage(reports.ReportFilename("weekly"), page, "", bodyPrefix)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stdout, "Report written: %s\n", path)
	return nil
}

// ── Backtest report ───────────────────────────────────────────────────────────

func runReportBacktest(_ *cobra.Command, args []string) error {
	label := args[0]
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := store.Connect(ctx); err != nil {
		return fmt.Errorf("database: %w", err)
	}

	run, err := store.GetRunByLabel(ctx, label)
	if err != nil {
		return fmt.Errorf("load backtest %s: %w", label, err)
	}
	trades, err := store.ListTradesForRun(ctx, run.ID)
	if err != nil {
		return fmt.Errorf("load backtest trades: %w", err)
	}

	gen, err := reports.New(cfg.Reports)
	if err != nil {
		return err
	}

	bodyPrefix, page, err := reports.BuildBacktestPage(reports.BacktestData{Run: *run, Trades: trades})
	if err != nil {
		return err
	}

	filename := fmt.Sprintf("backtest-%s-%s.html", label, time.Now().Format("2006-01-02T150405"))
	path, err := gen.WritePage(filename, page, "", bodyPrefix)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stdout, "Report written: %s\n", path)
	return nil
}

// ── Backtest comparison report ────────────────────────────────────────────────

func runReportBacktestCompare(_ *cobra.Command, args []string) error {
	labelA, labelB := args[0], args[1]

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := store.Connect(ctx); err != nil {
		return fmt.Errorf("database: %w", err)
	}

	runA, err := store.GetRunByLabel(ctx, labelA)
	if err != nil {
		return fmt.Errorf("load backtest %s: %w", labelA, err)
	}
	tradesA, err := store.ListTradesForRun(ctx, runA.ID)
	if err != nil {
		return fmt.Errorf("load trades for %s: %w", labelA, err)
	}

	runB, err := store.GetRunByLabel(ctx, labelB)
	if err != nil {
		return fmt.Errorf("load backtest %s: %w", labelB, err)
	}
	tradesB, err := store.ListTradesForRun(ctx, runB.ID)
	if err != nil {
		return fmt.Errorf("load trades for %s: %w", labelB, err)
	}

	gen, err := reports.New(cfg.Reports)
	if err != nil {
		return err
	}

	bodyPrefix, page, err := reports.BuildBacktestComparePage(
		reports.BacktestData{Run: *runA, Trades: tradesA},
		reports.BacktestData{Run: *runB, Trades: tradesB},
	)
	if err != nil {
		return err
	}

	filename := fmt.Sprintf("compare-%s-%s-%s.html", labelA, labelB, time.Now().Format("2006-01-02T150405"))
	path, err := gen.WritePage(filename, page, "", bodyPrefix)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stdout, "Report written: %s\n", path)
	return nil
}
