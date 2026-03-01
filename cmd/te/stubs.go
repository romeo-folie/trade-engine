package main

// stubs.go — all commands that are registered but not yet implemented.
// Each stub prints "not implemented yet" and exits 0.
// They will be replaced with real implementations in their respective phases.

import (
	"fmt"

	"github.com/spf13/cobra"
)

func stub(name string) func(cmd *cobra.Command, args []string) error {
	return func(cmd *cobra.Command, args []string) error {
		fmt.Printf("%s: not implemented yet (coming in a later phase)\n", name)
		return nil
	}
}

// ── Watchlist ─────────────────────────────────────────────────────────────────
// Watchlist commands are implemented in watchlist.go (Phase 2).
// watchlistCmd is populated by init() in watchlist.go.

var watchlistCmd = &cobra.Command{
	Use:   "watchlist",
	Short: "Manage the trading watchlist",
}

var _ = func() bool {
	// Real subcommands: add, remove, show (in watchlist.go).
	// Remaining stubs for commands coming in later phases:
	watchlistCmd.AddCommand(
		&cobra.Command{Use: "enable <ticker>", Short: "Enable a watchlist item", RunE: stub("te watchlist enable")},
		&cobra.Command{Use: "disable <ticker>", Short: "Disable a watchlist item", RunE: stub("te watchlist disable")},
		&cobra.Command{Use: "sync", Short: "Sync watchlist from config file", RunE: stub("te watchlist sync")},
	)
	return true
}()

// ── Trades & signals ──────────────────────────────────────────────────────────
// Implemented in trades.go (Phase 4). tradesCmd is defined here; subcommands are
// registered by trades.go init().

var tradesCmd = &cobra.Command{
	Use:   "trades",
	Short: "View and manage trades and signals",
}

// ── Dashboard ─────────────────────────────────────────────────────────────────

var dashboardCmd = &cobra.Command{
	Use:     "dashboard",
	Aliases: []string{"ui", "tui"},
	Short:   "Open the terminal dashboard",
	RunE:    runDashboard,
}

// ── Notifications ─────────────────────────────────────────────────────────────
// notifyCmd is defined in notify.go (Phase 6 implementation).

var _ = func() bool {
	notifyCmd.AddCommand(
		&cobra.Command{Use: "history", Short: "View notification history", RunE: stub("te notify history")},
	)
	return true
}()

// ── Knowledge graph ───────────────────────────────────────────────────────────

var graphCmd = &cobra.Command{
	Use:   "graph",
	Short: "Manage the knowledge graph (Phase 8)",
}

var _ = func() bool {
	graphCmd.AddCommand(
		&cobra.Command{Use: "enrich", Short: "Run knowledge graph enrichment", RunE: stub("te graph enrich")},
		&cobra.Command{Use: "show <entity>", Short: "Show entity graph context", RunE: stub("te graph show")},
		&cobra.Command{Use: "list", Short: "List entities in the graph", RunE: stub("te graph list")},
	)
	return true
}()

// ── Embeddings ────────────────────────────────────────────────────────────────

var embeddingsCmd = &cobra.Command{
	Use:   "embeddings",
	Short: "Manage embedding generation (Phase 8)",
}

var _ = func() bool {
	embeddingsCmd.AddCommand(
		&cobra.Command{Use: "sync", Short: "Sync pending embeddings", RunE: stub("te embeddings sync")},
		&cobra.Command{Use: "status", Short: "Show embedding pipeline status", RunE: stub("te embeddings status")},
	)
	return true
}()

// ── Web visualisation ─────────────────────────────────────────────────────────

var webCmd = &cobra.Command{
	Use:   "web",
	Short: "Start the web data-flow visualisation server (Phase 14)",
	RunE:  stub("te web"),
}
