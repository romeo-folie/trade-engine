package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/data"
	"github.com/romeo-folie/trade-engine/internal/earnings"
	"github.com/romeo-folie/trade-engine/internal/logger"
	"github.com/romeo-folie/trade-engine/internal/store"
	"github.com/spf13/cobra"
)

var (
	earningsSyncUnderlyings string
	earningsSyncFrom        string
	earningsSyncTo          string
	earningsListSymbol      string
)

// earningsCmd groups earnings-calendar operations that populate earnings_calendar
// (migration 045) — the shared table the options-ORB earnings-blackout gate reads.
var earningsCmd = &cobra.Command{
	Use:   "earnings",
	Short: "Earnings-calendar operations for the options-ORB earnings-blackout gate",
}

var earningsSyncCmd = &cobra.Command{
	Use:   "sync",
	Short: "Sync coming earnings dates from Finnhub into earnings_calendar",
	Long: `Fetch scheduled/recent earnings from Finnhub /calendar/earnings for the single-name
options-ORB underlyings and upsert them (source="finnhub").

WHY THIS EXISTS: the live earnings-blackout gate reads earnings_calendar and fails CLOSED
for a single name with no dates. Run this before enabling single-name underlyings and
daily thereafter (e.g. from te-switch) so the next report date is always known.

FREE-TIER LIMIT: Finnhub serves ~1 month of history plus all upcoming, so this populates
the coming reports the LIVE gate needs; historical (backtest) dates come from
'te earnings seed' instead. A 'seed' row is never overwritten by a sync.`,
	Example: `  te earnings sync --underlyings NVDA,TSLA
  te earnings sync   # single-name options_orb.underlyings, next ~4 months`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runEarningsSync(cmd.Context())
	},
}

var earningsSeedCmd = &cobra.Command{
	Use:   "seed",
	Short: "Seed curated historical earnings dates (for backtests the free tier can't reach)",
	Long: `Upsert the hand-curated historical earnings dates (earnings.HistoricalSeed) as
source="seed". These fill the backtest windows Finnhub's free tier (~1 month history)
returns empty for, so a backtest over 2024-2025 applies the same earnings blackout the
live gate would. Idempotent; VERIFY the dates against public record before trusting P&L.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runEarningsSeed(cmd.Context())
	},
}

var earningsListCmd = &cobra.Command{
	Use:   "list",
	Short: "List stored earnings dates for a symbol (verification)",
	RunE: func(cmd *cobra.Command, args []string) error {
		return runEarningsList(cmd.Context())
	},
}

func init() {
	earningsSyncCmd.Flags().StringVar(&earningsSyncUnderlyings, "underlyings", "", "Comma-separated symbols (default: single-name options_orb.underlyings)")
	earningsSyncCmd.Flags().StringVar(&earningsSyncFrom, "from", "", "From date YYYY-MM-DD (default: today−30d)")
	earningsSyncCmd.Flags().StringVar(&earningsSyncTo, "to", "", "To date YYYY-MM-DD (default: today+120d)")
	earningsListCmd.Flags().StringVar(&earningsListSymbol, "symbol", "", "Symbol to list (required)")
	earningsCmd.AddCommand(earningsSyncCmd, earningsSeedCmd, earningsListCmd)
	rootCmd.AddCommand(earningsCmd)
}

// singleNameUnderlyings returns the configured options-ORB underlyings that are NOT
// earnings-exempt ETFs — the only symbols the earnings gate (and thus a sync) applies to.
func singleNameUnderlyings(cfg *config.Config) []string {
	etf := make(map[string]bool, len(cfg.OptionsORB.ETFUnderlyings))
	for _, s := range cfg.OptionsORB.ETFUnderlyings {
		etf[strings.ToUpper(s)] = true
	}
	var out []string
	for _, u := range cfg.OptionsORB.Underlyings {
		if !etf[strings.ToUpper(u)] {
			out = append(out, strings.ToUpper(u))
		}
	}
	return out
}

func runEarningsSync(ctx context.Context) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if err := logger.Init(cfg.Logging.Level, cfg.Logging.Format); err != nil {
		return err
	}

	var symbols []string
	if earningsSyncUnderlyings != "" {
		for _, s := range splitCSV(earningsSyncUnderlyings) {
			symbols = append(symbols, strings.ToUpper(s))
		}
	} else {
		symbols = singleNameUnderlyings(cfg)
	}
	if len(symbols) == 0 {
		return fmt.Errorf("no single-name symbols to sync: pass --underlyings or add non-ETF options_orb.underlyings")
	}

	if cfg.Markets.News.APIKey == "" {
		return fmt.Errorf("Finnhub API key unset: export %s", cfg.Markets.News.APIKeyEnv)
	}

	now := time.Now()
	from, to := now.AddDate(0, 0, -30), now.AddDate(0, 0, 120)
	if earningsSyncFrom != "" {
		if from, err = time.Parse("2006-01-02", earningsSyncFrom); err != nil {
			return fmt.Errorf("invalid --from: %w", err)
		}
	}
	if earningsSyncTo != "" {
		if to, err = time.Parse("2006-01-02", earningsSyncTo); err != nil {
			return fmt.Errorf("invalid --to: %w", err)
		}
	}

	if err := store.Connect(ctx); err != nil {
		return fmt.Errorf("connect DB: %w", err)
	}
	client := data.NewFinnhubClient(cfg.Markets.News.APIKey)

	fmt.Printf("earnings sync · %s → %s · %s\n", from.Format("2006-01-02"), to.Format("2006-01-02"), strings.Join(symbols, ","))
	n, err := earnings.Sync(ctx, store.GetDB(), client, symbols, from, to)
	if err != nil {
		return err
	}
	fmt.Printf("done · %d earnings dates upserted\n", n)
	if n == 0 {
		fmt.Println("NOTE: 0 rows. Free tier serves only ~1 month history + upcoming — historical windows return empty; use `te earnings seed` for backtest dates.")
	}
	return nil
}

func runEarningsSeed(ctx context.Context) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if err := logger.Init(cfg.Logging.Level, cfg.Logging.Format); err != nil {
		return err
	}
	if err := store.Connect(ctx); err != nil {
		return fmt.Errorf("connect DB: %w", err)
	}
	n, err := earnings.SeedHistorical(ctx, store.GetDB())
	if err != nil {
		return err
	}
	fmt.Printf("done · %d curated historical earnings dates seeded (source=seed)\n", n)
	return nil
}

func runEarningsList(ctx context.Context) error {
	if earningsListSymbol == "" {
		return fmt.Errorf("--symbol is required")
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if err := logger.Init(cfg.Logging.Level, cfg.Logging.Format); err != nil {
		return err
	}
	if err := store.Connect(ctx); err != nil {
		return fmt.Errorf("connect DB: %w", err)
	}
	sym := strings.ToUpper(earningsListSymbol)
	from := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	to := time.Now().AddDate(1, 0, 0)
	events, err := store.GetEarnings(ctx, store.GetDB(), sym, from, to)
	if err != nil {
		return err
	}
	fmt.Printf("%s · %d earnings dates\n", sym, len(events))
	for _, e := range events {
		fmt.Printf("  %s  %-4s  (%s)\n", e.Date.Format("2006-01-02"), e.SessionHour, e.Source)
	}
	return nil
}
