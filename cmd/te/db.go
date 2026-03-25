package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/store"
	"github.com/spf13/cobra"
)

// resetTables is the ordered list of tables to truncate.
// Each is truncated only if it exists — safe across different migration states.
var resetTables = []string{
	"signals",
	"trades",
	"orders",
	"analyses",
	"analysis_cache",
	"portfolio_snapshots",
	"candles",
	"news_items",
	"backtest_runs",
	"backtest_trades",
	"orb_sessions",
	"orb_trades",
	"daily_summaries",
	"prompt_cache",
	"prediction_snapshots",
	"config_history",
}

var dbResetForce bool

var dbCmd = &cobra.Command{
	Use:   "db",
	Short: "Database management commands",
}

var dbResetCmd = &cobra.Command{
	Use:   "reset",
	Short: "Reset trading data while preserving watchlist and knowledge graph",
	Long: `Truncates all transactional and market data tables.

Preserved tables:
  - watchlist        (your configured tickers)
  - entities         (knowledge graph nodes)
  - relationships    (knowledge graph edges)
  - entity_mentions  (news <-> entity links)

Cleared tables:
  signals, trades, orders, analyses, analysis_cache,
  portfolio_snapshots, candles, news_items, backtest_runs,
  backtest_trades, orb_sessions, orb_trades, daily_summaries,
  prompt_cache, prediction_snapshots, config_history`,
	RunE: func(cmd *cobra.Command, args []string) error {
		if !dbResetForce {
			fmt.Print("This will permanently delete all trading, signal, and market data.\nType 'yes' to confirm: ")
			reader := bufio.NewReader(os.Stdin)
			answer, _ := reader.ReadString('\n')
			if strings.TrimSpace(strings.ToLower(answer)) != "yes" {
				fmt.Println("Aborted.")
				return nil
			}
		}

		cfg, err := config.Load()
		if err != nil {
			return err
		}
		_ = cfg

		if err := store.Connect(context.Background()); err != nil {
			return fmt.Errorf("connect to database: %w", err)
		}
		defer store.Close()

		fmt.Print("Resetting database... ")
		ctx := context.Background()
		cleared := 0
		for _, tbl := range resetTables {
			// Check existence before truncating — safe across different migration states.
			var exists bool
			err := store.GetDB().QueryRow(ctx,
				`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema='public' AND table_name=$1)`,
				tbl,
			).Scan(&exists)
			if err != nil {
				return fmt.Errorf("check table %s exists: %w", tbl, err)
			}
			if !exists {
				continue
			}
			if _, err := store.GetDB().Exec(ctx, "TRUNCATE "+tbl+" CASCADE"); err != nil {
				return fmt.Errorf("truncate %s: %w", tbl, err)
			}
			cleared++
		}
		fmt.Printf("done. (%d tables cleared)\n", cleared)
		fmt.Println("Preserved: watchlist, entities, relationships, entity_mentions")
		return nil
	},
}

func init() {
	dbResetCmd.Flags().BoolVarP(&dbResetForce, "force", "f", false, "Skip confirmation prompt")
	dbCmd.AddCommand(dbResetCmd)
	rootCmd.AddCommand(dbCmd)
}
