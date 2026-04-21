package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"
	"time"

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
			if err != nil || !exists {
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

var dbBackfillCmd = &cobra.Command{
	Use:   "backfill-summaries",
	Short: "Recompute starting_value, pnl, and pnl_pct for past daily_summaries rows",
	Long: `Corrects historical daily_summaries rows whose starting_value was set from
a mid-session snapshot (the old bug) instead of the previous-day closing snapshot.

For each past day it:
  1. Finds the last portfolio_snapshot written before midnight of that day.
  2. Uses its total_value as the correct starting_value.
  3. Recomputes pnl = ending_value - starting_value and pnl_pct accordingly.

Today's row is skipped — the running engine writes it correctly in real time.
Safe to run multiple times (idempotent).`,
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		_ = cfg

		if err := store.Connect(context.Background()); err != nil {
			return fmt.Errorf("connect to database: %w", err)
		}
		defer store.Close()

		ctx := context.Background()
		todayStr := time.Now().UTC().Format("2006-01-02")

		rows, err := store.GetDB().Query(ctx, `
			SELECT id, date, ending_value
			FROM daily_summaries
			WHERE date < $1::date
			ORDER BY date ASC`, todayStr)
		if err != nil {
			return fmt.Errorf("query daily_summaries: %w", err)
		}
		defer rows.Close()

		type summaryRow struct {
			id          string
			date        time.Time
			endingValue float64
		}
		var past []summaryRow
		for rows.Next() {
			var r summaryRow
			if err := rows.Scan(&r.id, &r.date, &r.endingValue); err != nil {
				return fmt.Errorf("scan: %w", err)
			}
			past = append(past, r)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("iterate daily_summaries: %w", err)
		}

		if len(past) == 0 {
			fmt.Println("No past daily_summaries rows found — nothing to backfill.")
			return nil
		}

		updated := 0
		skipped := 0
		for _, r := range past {
			dayMidnight := r.date.UTC().Truncate(24 * time.Hour)

			var prevTotal float64
			err := store.GetDB().QueryRow(ctx, `
				SELECT total_value FROM portfolio_snapshots
				WHERE created_at < $1
				ORDER BY created_at DESC LIMIT 1`, dayMidnight).Scan(&prevTotal)
			if err != nil {
				if !store.IsNoRows(err) {
					return fmt.Errorf("query prior snapshot for %s: %w", r.date.Format("2006-01-02"), err)
				}
				fmt.Printf("  skip  %s — no prior snapshot\n", r.date.Format("2006-01-02"))
				skipped++
				continue
			}

			pnl := r.endingValue - prevTotal
			pnlPct := 0.0
			if prevTotal > 0 {
				pnlPct = (pnl / prevTotal) * 100
			}

			if _, err := store.GetDB().Exec(ctx, `
				UPDATE daily_summaries
				SET starting_value=$1, pnl=$2, pnl_pct=$3
				WHERE id=$4`, prevTotal, pnl, pnlPct, r.id); err != nil {
				return fmt.Errorf("update %s: %w", r.date.Format("2006-01-02"), err)
			}
			fmt.Printf("  fixed %s  starting=%.2f  pnl=%.2f (%.2f%%)\n",
				r.date.Format("2006-01-02"), prevTotal, pnl, pnlPct)
			updated++
		}

		fmt.Printf("\nDone: %d updated, %d skipped (no prior snapshot).\n", updated, skipped)
		return nil
	},
}

func init() {
	dbResetCmd.Flags().BoolVarP(&dbResetForce, "force", "f", false, "Skip confirmation prompt")
	dbCmd.AddCommand(dbResetCmd)
	dbCmd.AddCommand(dbBackfillCmd)
	rootCmd.AddCommand(dbCmd)
}
