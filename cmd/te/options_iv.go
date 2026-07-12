package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/romeo-folie/trade-engine/internal/backtest/optionsreplay"
	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/data"
	"github.com/romeo-folie/trade-engine/internal/logger"
	"github.com/romeo-folie/trade-engine/internal/store"
	"github.com/spf13/cobra"
)

var (
	ivBackfillUnderlyings string
	ivBackfillLookback    int
	ivBackfillRefMinute   int
)

// optionsCmd groups options-ORB operational subcommands.
var optionsCmd = &cobra.Command{
	Use:   "options",
	Short: "Options-ORB operations",
}

var optionsBackfillIVCmd = &cobra.Command{
	Use:   "backfill-iv",
	Short: "Seed option_iv_history so the live options-ORB IV-Rank gate has data",
	Long: `Derive daily ATM implied volatility for the options-ORB underlyings from Alpaca
historical option quotes and upsert it into option_iv_history (source="derived").

WHY THIS EXISTS: the live IV-Rank gate reads option_iv_history and fails CLOSED below
iv_min_samples stored days — so without a writer, every breakout aborts at iv_rank and
options-ORB places zero trades. Run this before enabling/starting an options session
(e.g. from te-switch), daily, so the trailing IV window stays populated. Idempotent:
UpsertOptionIV never lets a "derived" row overwrite a live-recorded one.

Reuses the SAME derivation as the backtest (optionsreplay.BuildIVSeries), so the live
gate ranks against the identical IV series a backtest would.`,
	Example: `  te options backfill-iv --lookback-days 60
  te options backfill-iv --underlyings SPY,QQQ --lookback-days 252`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runBackfillIV(cmd.Context())
	},
}

func init() {
	optionsBackfillIVCmd.Flags().StringVar(&ivBackfillUnderlyings, "underlyings", "", "Comma-separated underlyings (default: options_orb.underlyings)")
	optionsBackfillIVCmd.Flags().IntVar(&ivBackfillLookback, "lookback-days", 0, "Calendar days back to seed (default: options_orb.iv_lookback_days or 252)")
	optionsBackfillIVCmd.Flags().IntVar(&ivBackfillRefMinute, "iv-ref-minute", 15, "Minutes after 09:30 ET at which the daily ATM IV is sampled")
	optionsCmd.AddCommand(optionsBackfillIVCmd)
	rootCmd.AddCommand(optionsCmd)
}

func runBackfillIV(ctx context.Context) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if err := logger.Init(cfg.Logging.Level, cfg.Logging.Format); err != nil {
		return err
	}

	underlyings := cfg.OptionsORB.Underlyings
	if ivBackfillUnderlyings != "" {
		underlyings = nil
		for _, s := range splitCSV(ivBackfillUnderlyings) {
			underlyings = append(underlyings, strings.ToUpper(s))
		}
	}
	if len(underlyings) == 0 {
		return fmt.Errorf("no underlyings: set options_orb.underlyings or pass --underlyings")
	}

	lookback := ivBackfillLookback
	if lookback <= 0 {
		lookback = cfg.OptionsORB.IVLookbackDays
	}
	if lookback <= 0 {
		lookback = 252
	}

	stocks := cfg.Markets.Stocks
	apiKey, secret := os.Getenv(stocks.APIKeyEnv), os.Getenv(stocks.SecretEnv)
	if apiKey == "" || secret == "" {
		return fmt.Errorf("Alpaca credentials unset: export %s and %s", stocks.APIKeyEnv, stocks.SecretEnv)
	}
	hd := data.NewAlpacaOptionsHist(apiKey, secret, stocks.DataURL, stocks.BaseURL)
	chain := optionsreplay.NewHistChainProvider(hd, 0, 0)

	if err := store.Connect(ctx); err != nil {
		return fmt.Errorf("connect DB: %w", err)
	}
	db := store.GetDB()

	// Seed [today-lookback, today]. Deriving up to "now" keeps the trailing window
	// fresh when this runs daily; UpsertOptionIV is idempotent per (underlying, date).
	to := time.Now()
	from := to.AddDate(0, 0, -lookback)

	fmt.Printf("options backfill-iv · %s → %s · %s\n", from.Format("2006-01-02"), to.Format("2006-01-02"), strings.Join(underlyings, ","))
	total := 0
	for _, u := range underlyings {
		series, err := chain.BuildIVSeries(ctx, u, from, to, ivBackfillRefMinute)
		if err != nil {
			return fmt.Errorf("derive IV series for %s: %w", u, err)
		}
		written := 0
		for _, d := range series {
			if err := store.UpsertOptionIV(ctx, db, u, d.Date, d.ATMIV, "derived"); err != nil {
				return fmt.Errorf("upsert IV %s %s: %w", u, d.Date.Format("2006-01-02"), err)
			}
			written++
		}
		fmt.Printf("  %-6s %d days seeded\n", u, written)
		total += written
	}
	fmt.Printf("done · %d IV rows upserted\n", total)
	if total == 0 {
		// No rows almost always means the OPRA entitlement/date window returned nothing —
		// fail loud so an operator doesn't think the gate is now populated.
		return fmt.Errorf("no IV rows derived (check OPRA entitlement + that the window is within Alpaca's ~Feb-2024 option-data coverage)")
	}
	return nil
}
