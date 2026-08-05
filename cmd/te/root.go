package main

import (
	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/data"
	"github.com/spf13/cobra"
)

var rootCmd = &cobra.Command{
	Use:     "te",
	Short:   "TradeEngine — LLM-powered algorithmic trading system",
	Version: "0.1.0",
	Long: `TradeEngine (te) is an LLM-powered algorithmic trading engine.

It monitors markets, runs AI analysis via Claude and other models,
manages a paper or live trading portfolio, and provides a full-featured
terminal dashboard and Telegram integration.`,
	SilenceUsage: true,
	// config.Load() is called from ~20 subcommands, so the candle-sanity
	// thresholds (issue #519) are installed once here instead of at every call
	// site — a missed site would silently run a different guard than the
	// operator configured. The guard's built-in defaults already apply without
	// this, so a config-load failure downgrades to defaults rather than
	// disabling protection.
	PersistentPreRun: func(*cobra.Command, []string) {
		applyCandleSanity()
	},
}

// applyCandleSanity converts the operator's YAML into the data package's
// process-wide guard configuration. The config package deliberately stays a
// leaf (stdlib + yaml only), so the two structs are mirrored and translated
// here rather than shared by import.
func applyCandleSanity() {
	cfg, err := config.Load()
	if err != nil {
		return // keep internal/data's calibrated defaults
	}
	cs := cfg.Markets.CandleSanity
	data.SetCandleSanity(data.CandleSanityConfig{
		Enabled:                  cs.CandleSanityEnabled(),
		MaxRangeRatio:            cs.MaxRangeRatio,
		MaxRangePct:              cs.MaxRangePct,
		ScaleWindow:              cs.ScaleWindow,
		MaxConsecutiveRejections: cs.MaxConsecutiveRejections,
	})
}

func init() {
	// Engine lifecycle
	rootCmd.AddCommand(runCmd)
	rootCmd.AddCommand(stopCmd)
	rootCmd.AddCommand(statusCmd)
	rootCmd.AddCommand(modeCmd)

	// Config
	rootCmd.AddCommand(configCmd)

	// Database
	rootCmd.AddCommand(migrateCmd)

	// Logs
	rootCmd.AddCommand(logsCmd)

	// Watchlist
	rootCmd.AddCommand(watchlistCmd)

	// Trades and signals
	rootCmd.AddCommand(tradesCmd)

	// Models
	rootCmd.AddCommand(modelsCmd)

	// Backtesting
	rootCmd.AddCommand(backtestCmd)

	// Reports
	rootCmd.AddCommand(reportCmd)

	// Dashboard TUI
	rootCmd.AddCommand(dashboardCmd)

	// Notifications
	rootCmd.AddCommand(notifyCmd)

	// Knowledge graph
	rootCmd.AddCommand(graphCmd)

	// Point-in-time swing universe
	rootCmd.AddCommand(universeCmd)

	// Embeddings
	rootCmd.AddCommand(embeddingsCmd)

	// Web visualisation
	rootCmd.AddCommand(webCmd)
}
