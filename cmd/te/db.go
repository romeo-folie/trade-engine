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

// resetSQL clears all transactional/market data while preserving:
//   - watchlist       (user-defined tickers)
//   - entities        (knowledge graph nodes)
//   - relationships   (knowledge graph edges)
//   - entity_mentions (news ↔ entity links)
const resetSQL = `TRUNCATE
  signals,
  trades,
  orders,
  analyses,
  analysis_cache,
  portfolio_snapshots,
  candles,
  news_items,
  backtest_runs,
  backtest_trades,
  orb_sessions,
  orb_trades,
  daily_summaries,
  prompt_cache,
  prediction_snapshots,
  config_history
CASCADE;`

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
		if _, err := store.GetDB().Exec(context.Background(), resetSQL); err != nil {
			return fmt.Errorf("reset failed: %w", err)
		}
		fmt.Println("done.")
		fmt.Println("Preserved: watchlist, entities, relationships, entity_mentions")
		return nil
	},
}

func init() {
	dbResetCmd.Flags().BoolVarP(&dbResetForce, "force", "f", false, "Skip confirmation prompt")
	dbCmd.AddCommand(dbResetCmd)
	rootCmd.AddCommand(dbCmd)
}
