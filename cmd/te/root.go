package main

import (
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

	// Embeddings
	rootCmd.AddCommand(embeddingsCmd)

	// Web visualisation
	rootCmd.AddCommand(webCmd)
}
