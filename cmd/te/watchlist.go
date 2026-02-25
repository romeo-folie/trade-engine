package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/data"
	"github.com/romeo-folie/trade-engine/internal/engine"
	"github.com/romeo-folie/trade-engine/internal/store"
	"github.com/spf13/cobra"
)

// watchlist command variables.
var (
	watchlistStrategies string
	watchlistTimeframes string
)

// watchlistAddCmd adds a ticker to the watchlist and triggers a history backfill.
var watchlistAddCmd = &cobra.Command{
	Use:   "add <ticker>",
	Short: "Add a ticker to the watchlist (triggers historical backfill)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ticker := strings.ToUpper(args[0])

		strategies := splitCSV(watchlistStrategies)
		if len(strategies) == 0 {
			strategies = []string{"trend_following"}
		}
		timeframes := splitCSV(watchlistTimeframes)
		if len(timeframes) == 0 {
			timeframes = []string{"1d"}
		}

		ctx := context.Background()
		if err := store.Connect(ctx); err != nil {
			return fmt.Errorf("connect to database: %w", err)
		}
		defer store.Close()

		item := store.WatchlistItem{
			Ticker:     ticker,
			Platform:   "stock",
			Strategies: strategies,
			Timeframes: timeframes,
			Source:     "manual",
		}
		if err := store.AddWatchlistItem(ctx, item); err != nil {
			return fmt.Errorf("add to watchlist: %w", err)
		}
		fmt.Printf("✓ %s added to watchlist (strategies: %s, timeframes: %s)\n",
			ticker, strings.Join(strategies, ","), strings.Join(timeframes, ","))

		// Backfill history if Alpaca is configured.
		cfg, err := config.Load()
		if err != nil {
			fmt.Println("⚠  Config unavailable — skipping backfill.")
			return nil
		}
		if cfg.Markets.Stocks.APIKey == "" {
			fmt.Println("⚠  ALPACA_API_KEY not set — skipping backfill. Set it and re-run te watchlist add.")
			return nil
		}

		alpaca := data.NewAlpacaClient(cfg.Markets.Stocks.APIKey, cfg.Markets.Stocks.Secret, cfg.Markets.Stocks.DataURL)
		eng := engine.New(cfg, alpaca, nil, cfg.Engine.Mode)

		fmt.Printf("Backfilling history for %s (2y daily + 30d hourly)...\n", ticker)
		if err := eng.BackfillHistory(ctx, ticker); err != nil {
			fmt.Printf("⚠  Backfill incomplete: %v\n", err)
		} else {
			fmt.Printf("✓ Backfill complete for %s\n", ticker)
		}
		return nil
	},
}

// watchlistRemoveCmd soft-deletes a ticker from the watchlist.
var watchlistRemoveCmd = &cobra.Command{
	Use:   "remove <ticker>",
	Short: "Remove a ticker from the watchlist",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ticker := strings.ToUpper(args[0])

		ctx := context.Background()
		if err := store.Connect(ctx); err != nil {
			return fmt.Errorf("connect to database: %w", err)
		}
		defer store.Close()

		if err := store.RemoveWatchlistItem(ctx, ticker, "stock"); err != nil {
			return fmt.Errorf("remove from watchlist: %w", err)
		}
		fmt.Printf("✓ %s removed from watchlist\n", ticker)
		return nil
	},
}

// watchlistShowCmd prints the watchlist as a table.
var watchlistShowCmd = &cobra.Command{
	Use:   "show",
	Short: "Show all watchlist items",
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := context.Background()
		if err := store.Connect(ctx); err != nil {
			return fmt.Errorf("connect to database: %w", err)
		}
		defer store.Close()

		items, err := store.GetWatchlistItems(ctx, false)
		if err != nil {
			return fmt.Errorf("get watchlist: %w", err)
		}
		if len(items) == 0 {
			fmt.Println("Watchlist is empty. Use: te watchlist add <ticker>")
			return nil
		}

		fmt.Printf("%-8s %-10s %-30s %-15s %-6s\n", "TICKER", "PLATFORM", "STRATEGIES", "TIMEFRAMES", "ACTIVE")
		fmt.Println(strings.Repeat("-", 75))
		for _, item := range items {
			active := "yes"
			if !item.Active {
				active = "no"
			}
			fmt.Printf("%-8s %-10s %-30s %-15s %-6s\n",
				item.Ticker,
				item.Platform,
				strings.Join(item.Strategies, ","),
				strings.Join(item.Timeframes, ","),
				active,
			)
		}
		return nil
	},
}

func init() {
	watchlistAddCmd.Flags().StringVar(&watchlistStrategies, "strategies", "trend_following",
		"Comma-separated list of strategies (trend_following, momentum, mean_reversion, news_sentiment)")
	watchlistAddCmd.Flags().StringVar(&watchlistTimeframes, "timeframes", "1d",
		"Comma-separated list of timeframes (1m, 5m, 15m, 1h, 1d)")

	// Register real implementations onto the watchlistCmd declared in stubs.go.
	watchlistCmd.AddCommand(watchlistAddCmd)
	watchlistCmd.AddCommand(watchlistRemoveCmd)
	watchlistCmd.AddCommand(watchlistShowCmd)
}

// splitCSV splits a comma-separated string, trimming whitespace and empty elements.
func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
