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

	// prospective scanner-universe conversion study
	scannerFunnelCmd := &cobra.Command{
		Use:   "scanner-funnel",
		Short: "Report prospective scanner candidate conversion and realized outcomes",
		RunE:  runReportScannerFunnel,
	}
	scannerFunnelCmd.Flags().String("from", "", "Start date (YYYY-MM-DD); defaults to 30 days ago")
	scannerFunnelCmd.Flags().String("to", "", "End date (YYYY-MM-DD, inclusive); defaults to today")
	reportCmd.AddCommand(scannerFunnelCmd)

	weeklyLeadersCmd := &cobra.Command{
		Use:   "weekly-leaders",
		Short: "Report prospective full-universe leadership breadth and turnover",
		RunE:  runReportWeeklyLeaders,
	}
	weeklyLeadersCmd.Flags().String("from", "", "Start date (YYYY-MM-DD); defaults to 30 days ago")
	weeklyLeadersCmd.Flags().String("to", "", "End date (YYYY-MM-DD, inclusive); defaults to today")
	reportCmd.AddCommand(weeklyLeadersCmd)

	potentIndustriesCmd := &cobra.Command{
		Use:   "potent-industries",
		Short: "Report prospective industry potency, breadth, and member leadership",
		RunE:  runReportPotentIndustries,
	}
	potentIndustriesCmd.Flags().String("from", "", "Start date (YYYY-MM-DD); defaults to 30 days ago")
	potentIndustriesCmd.Flags().String("to", "", "End date (YYYY-MM-DD, inclusive); defaults to today")
	potentIndustriesCmd.Flags().String("industry", "", "Optional exact industry label")
	potentIndustriesCmd.Flags().Int("top-members", 3, "Qualified members shown per industry")
	reportCmd.AddCommand(potentIndustriesCmd)

	premarketEpisodicCmd := &cobra.Command{
		Use:   "premarket-episodic",
		Short: "Report prospective premarket gap and catalyst observations",
		RunE:  runReportPremarketEpisodic,
	}
	premarketEpisodicCmd.Flags().String("from", "", "Start date (YYYY-MM-DD); defaults to 30 days ago")
	premarketEpisodicCmd.Flags().String("to", "", "End date (YYYY-MM-DD, inclusive); defaults to today")
	premarketEpisodicCmd.Flags().Bool("qualified-only", false, "Show only qualified candidates")
	reportCmd.AddCommand(premarketEpisodicCmd)
}

func runReportWeeklyLeaders(cmd *cobra.Command, _ []string) error {
	if _, err := config.Load(); err != nil {
		return err
	}
	ctx := cmd.Context()
	if err := store.Connect(ctx); err != nil {
		return fmt.Errorf("database: %w", err)
	}
	defer store.Close()

	fromValue, _ := cmd.Flags().GetString("from")
	toValue, _ := cmd.Flags().GetString("to")
	from, to, err := weeklyLeaderReportDateRange(time.Now(), fromValue, toValue)
	if err != nil {
		return err
	}
	runs, err := store.ListCompletedWeeklyLeaderRunsBySessionDate(ctx, from, to)
	if err != nil {
		return err
	}
	fmt.Printf("Prospective weekly-leader study  %s → %s\n",
		from.Format("2006-01-02"), to.Format("2006-01-02"))
	if len(runs) == 0 {
		fmt.Println("No completed weekly-leader runs.")
		return nil
	}
	fmt.Printf("%-12s %9s %9s %9s %9s %9s %9s %10s %10s\n",
		"SESSION", "ELIGIBLE", "SCORED", "LEADERS", "ADDED", "REMOVED",
		"PERSIST", "BREADTH", "COVERAGE")
	fmt.Println(strings.Repeat("─", 102))
	var eligible, evaluated, qualified, failures, added, removed, persistent int
	var breadth, coverage float64
	for _, run := range runs {
		fmt.Printf("%-12s %9d %9d %9d %9d %9d %9d %9.2f%% %9.2f%%\n",
			run.SessionDate.Format("2006-01-02"), run.EligibleUniverseCount,
			run.EvaluatedCount, run.QualifiedCount, run.AddedCount,
			run.RemovedCount, run.PersistentCount, run.LeaderBreadth*100,
			run.EvaluationCoverage*100)
		eligible += run.EligibleUniverseCount
		evaluated += run.EvaluatedCount
		qualified += run.QualifiedCount
		failures += run.FailureCount
		added += run.AddedCount
		removed += run.RemovedCount
		persistent += run.PersistentCount
		breadth += run.LeaderBreadth
		coverage += run.EvaluationCoverage
	}
	fmt.Printf("\nRuns: %d · observations: %d · evaluated: %d · leaders: %d · failures: %d\n",
		len(runs), eligible, evaluated, qualified, failures)
	fmt.Printf("Transitions: +%d / −%d / %d persistent · average breadth %.2f%% · average coverage %.2f%%\n",
		added, removed, persistent, breadth/float64(len(runs))*100,
		coverage/float64(len(runs))*100)
	return nil
}

func weeklyLeaderReportDateRange(
	now time.Time, fromValue, toValue string,
) (time.Time, time.Time, error) {
	exchangeLocation, err := time.LoadLocation("America/New_York")
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("load exchange timezone: %w", err)
	}
	exchangeToday := now.In(exchangeLocation)
	to := time.Date(
		exchangeToday.Year(), exchangeToday.Month(), exchangeToday.Day(),
		0, 0, 0, 0, time.UTC,
	)
	from := to.AddDate(0, 0, -30)
	if fromValue != "" {
		from, err = time.Parse("2006-01-02", fromValue)
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("invalid --from date: %w", err)
		}
	}
	if toValue != "" {
		to, err = time.Parse("2006-01-02", toValue)
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("invalid --to date: %w", err)
		}
	}
	if from.After(to) {
		return time.Time{}, time.Time{}, fmt.Errorf("--from must be on or before --to")
	}
	return from, to, nil
}

func runReportScannerFunnel(cmd *cobra.Command, _ []string) error {
	if _, err := config.Load(); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := store.Connect(ctx); err != nil {
		return fmt.Errorf("database: %w", err)
	}

	now := time.Now()
	from := now.AddDate(0, 0, -30)
	to := now
	if value, _ := cmd.Flags().GetString("from"); value != "" {
		parsed, err := time.ParseInLocation("2006-01-02", value, time.Local)
		if err != nil {
			return fmt.Errorf("invalid --from date: %w", err)
		}
		from = parsed
	}
	if value, _ := cmd.Flags().GetString("to"); value != "" {
		parsed, err := time.ParseInLocation("2006-01-02", value, time.Local)
		if err != nil {
			return fmt.Errorf("invalid --to date: %w", err)
		}
		to = parsed.AddDate(0, 0, 1)
	}
	if !from.Before(to) {
		return fmt.Errorf("--from must be before --to")
	}

	funnel, err := store.GetScannerFunnel(ctx, from, to)
	if err != nil {
		return fmt.Errorf("load scanner funnel: %w", err)
	}
	printScannerFunnel(funnel, from, to)
	return nil
}

func printScannerFunnel(f store.ScannerFunnel, from, to time.Time) {
	rate := func(numerator, denominator int) string {
		if denominator == 0 {
			return "—"
		}
		return fmt.Sprintf("%.1f%%", float64(numerator)/float64(denominator)*100)
	}

	fmt.Printf("Prospective scanner funnel  %s → %s\n",
		from.Format("2006-01-02"), to.Add(-time.Nanosecond).Format("2006-01-02"))
	fmt.Printf("Scan runs: %d\n\n", f.Runs)
	fmt.Printf("%-26s %8s %13s\n", "STAGE", "COUNT", "CONVERSION")
	fmt.Println(strings.Repeat("─", 51))
	fmt.Printf("%-26s %8d %13s\n", "Candidates sourced", f.Sourced, "—")
	fmt.Printf("%-26s %8d %13s\n", "Qualification known", f.QualificationKnown,
		rate(f.QualificationKnown, f.Sourced))
	fmt.Printf("%-26s %8d %13s\n", "Pullback qualified", f.PullbackQualified,
		rate(f.PullbackQualified, f.QualificationKnown))
	fmt.Printf("%-26s %8d %13s\n", "Eligible for scoring", f.ScoringEligible,
		rate(f.ScoringEligible, f.Sourced))
	fmt.Printf("%-26s %8d %13s\n", "LLM scored", f.Scored,
		rate(f.Scored, f.ScoringEligible))
	fmt.Printf("%-26s %8d %13s\n", "LLM recommended", f.Recommended,
		rate(f.Recommended, f.Scored))
	fmt.Printf("%-26s %8d %13s\n", "Passed conviction", f.PassedConviction,
		rate(f.PassedConviction, f.Scored))
	fmt.Printf("%-26s %8d %13s\n", "Added to watchlist", f.WatchlistAdded,
		rate(f.WatchlistAdded, f.PassedConviction))
	fmt.Printf("%-26s %8d %13s\n", "Candidates analyzed", f.AnalyzedCandidates,
		rate(f.AnalyzedCandidates, f.WatchlistAdded))
	fmt.Printf("%-26s %8d %13s\n", "HOLD/WAIT candidates", f.HoldWaitCandidates,
		rate(f.HoldWaitCandidates, f.AnalyzedCandidates))
	fmt.Printf("%-26s %8d %13s\n", "Pre-analysis outcomes", f.EngineOutcomeCandidates,
		rate(f.EngineOutcomeCandidates, f.WatchlistAdded))
	fmt.Printf("%-26s %8d %13s\n", "Candidates with signals", f.SignalCandidates,
		rate(f.SignalCandidates, f.WatchlistAdded))
	fmt.Printf("%-26s %8d %13s\n", "Candidates with trades", f.ExecutedTradeCandidates,
		rate(f.ExecutedTradeCandidates, f.WatchlistAdded))

	winRate := rate(f.Wins, f.ClosedTrades)
	fmt.Printf("\nAnalyses: %d · Engine outcomes: %d · Signals: %d · Trades: %d · Closed: %d · Win rate: %s · Realized P&L: $%+.2f\n",
		f.Analyses, f.EngineOutcomes, f.Signals, f.Trades, f.ClosedTrades, winRate, f.RealizedPnL)
	printScannerStages("Qualification outcomes", f.QualificationStages)
	printScannerStages("Terminal outcomes", f.FinalStages)
	printScannerStages("Pre-analysis engine outcomes", f.EngineStages)
}

func printScannerStages(title string, stages []store.ScannerStageCount) {
	if len(stages) == 0 {
		return
	}
	fmt.Printf("\n%s:\n", title)
	for _, stage := range stages {
		fmt.Printf("  %-30s %d\n", strings.ReplaceAll(stage.Stage, "_", " "), stage.Count)
	}
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
