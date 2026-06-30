package main

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/store"
)

var orbCmd = &cobra.Command{
	Use:   "orb",
	Short: "Inspect ORB (opening-range breakout) trade performance",
}

var orbStatsCmd = &cobra.Command{
	Use:   "stats",
	Short: "Summarize recorded ORB trade outcomes (win rate, PnL, by instrument)",
	Long: `Summarize closed ORB trades: win rate, realized PnL, and a breakdown by
instrument, exit type (take-profit vs stop), and exit source (broker-confirmed
fills vs bar-replay reconstructions). Open trades whose exit has not yet been
recorded are reported separately so a recording gap is visible.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		if _, err := config.Load(); err != nil {
			return fmt.Errorf("load config: %w", err)
		}
		ctx := context.Background()
		if err := store.Connect(ctx); err != nil {
			return fmt.Errorf("connect db: %w", err)
		}
		defer store.Close()

		rows, err := store.GetORBStats(ctx, store.GetDB())
		if err != nil {
			return fmt.Errorf("load orb stats: %w", err)
		}
		if len(rows) == 0 {
			fmt.Println("No ORB trades recorded yet.")
			return nil
		}

		// Per-instrument table.
		fmt.Printf("%-9s %6s %5s %6s %6s %6s %6s %10s %9s %s\n",
			"TICKER", "CLOSED", "WINS", "LOSS", "TP", "STOP", "TIME", "PNL", "WIN%", "SOURCE(br/rp)")

		var totClosed, totWins, totLosses, totScratch, totTP, totStop, totTime, totBroker, totReplay, totOpen int
		var totPnL float64
		for _, r := range rows {
			winPct := "—"
			decided := r.Wins + r.Losses
			if decided > 0 {
				winPct = fmt.Sprintf("%.0f%%", float64(r.Wins)/float64(decided)*100)
			}
			fmt.Printf("%-9s %6d %5d %6d %6d %6d %6d %10.2f %9s   %d/%d\n",
				r.Ticker, r.Closed, r.Wins, r.Losses, r.TPExits, r.StopExits, r.TimeExits,
				r.TotalPnL, winPct, r.BrokerExits, r.ReplayExits)

			totClosed += r.Closed
			totWins += r.Wins
			totLosses += r.Losses
			totScratch += r.Scratches
			totTP += r.TPExits
			totStop += r.StopExits
			totTime += r.TimeExits
			totBroker += r.BrokerExits
			totReplay += r.ReplayExits
			totOpen += r.OpenFilled
			totPnL += r.TotalPnL
		}

		fmt.Println()
		overallWin := "—"
		decided := totWins + totLosses
		if decided > 0 {
			overallWin = fmt.Sprintf("%.1f%%", float64(totWins)/float64(decided)*100)
		}
		fmt.Printf("Overall: %d closed | %d wins / %d losses", totClosed, totWins, totLosses)
		if totScratch > 0 {
			fmt.Printf(" / %d scratch", totScratch)
		}
		fmt.Printf(" | win rate %s | realized PnL %+.2f\n", overallWin, totPnL)
		fmt.Printf("Exits: %d take-profit, %d stop, %d time-exit  |  source: %d broker-confirmed, %d bar-replay\n",
			totTP, totStop, totTime, totBroker, totReplay)
		if totOpen > 0 {
			fmt.Printf("\n⚠  %d trade(s) still open (entry filled, exit not yet recorded). "+
				"Run `go run ./cmd/orb-backfill` to reconcile them.\n", totOpen)
		}
		return nil
	},
}

func init() {
	orbCmd.AddCommand(orbStatsCmd)
	rootCmd.AddCommand(orbCmd)
}
