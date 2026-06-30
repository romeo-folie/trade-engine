// Command orb-backfill recovers exit/PnL outcomes for ORB trades that are stuck in
// status='filled' — entry filled, exit never recorded. It replays each trade's price
// path from market-data bars (the same logic the live scheduler now uses as its exit
// backstop) and, where the path touches a take-profit or stop-loss leg, closes the
// trade with exit_source='replay'.
//
// WHY THIS EXISTS: production orb-forex ran for months recording zero exits because
// the bar-replay reconciler was gated to paper-only executors, the IBKR bracket child
// IDs were never captured, and IBKR's trades feed only retains 7 days — so any trade
// un-reconciled within a week became permanently invisible to the broker reconcilers.
// This command recovers those historical trades from market-data history, which reaches
// much further back (daily bars ~6 months) than the trades feed.
//
// SAFETY: does NOTHING destructive without --apply. Without it, prints the exit it WOULD
// record for each stuck trade and exits 0. CloseORBTradeAt guards on status='filled', so
// running this is idempotent and safe alongside a live engine: a trade the engine already
// closed is simply skipped.
//
// USAGE:
//
//	go run ./cmd/orb-backfill                  # dry run — show resolved exits, write nothing
//	go run ./cmd/orb-backfill --apply          # record the resolved exits
//	go run ./cmd/orb-backfill --horizon-days=14 --apply
//
// Market data comes from the IBKR Client Portal gateway in config (markets.ibkr), so the
// gateway must be running and authenticated. Trades whose bars the gateway no longer
// retains are reported as "no data" and left untouched.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/joho/godotenv"

	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/data"
	"github.com/romeo-folie/trade-engine/internal/orb"
	"github.com/romeo-folie/trade-engine/internal/store"
)

func main() {
	var (
		horizonDays = flag.Int("horizon-days", 7, "how many days past each trade's fill to replay bars over")
		apply       = flag.Bool("apply", false, "actually record resolved exits; without this flag the command only prints what it would do")
	)
	flag.Parse()

	// Auto-load .env so DATABASE_URL is available without explicit export.
	_ = godotenv.Load()

	ctx := context.Background()

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "load config: %v\n", err)
		os.Exit(1)
	}
	if !cfg.Markets.IBKR.Enabled {
		fmt.Fprintln(os.Stderr, "markets.ibkr.enabled must be true — bar replay needs IBKR market data")
		os.Exit(1)
	}

	if err := store.Connect(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "connect db: %v\n", err)
		os.Exit(1)
	}
	defer store.Close()
	db := store.GetDB()

	md := data.NewIBKRClient(cfg.Markets.IBKR.GatewayURL, cfg.Markets.IBKR.InsecureTLS)

	trades, err := store.GetOpenFilledORBTrades(ctx, db)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load filled trades: %v\n", err)
		os.Exit(1)
	}

	if len(trades) == 0 {
		fmt.Println("No stuck (status='filled') ORB trades to backfill. Nothing to do.")
		return
	}

	mode := "DRY RUN — no changes will be written (pass --apply to record)"
	if *apply {
		mode = "APPLY — recording resolved exits"
	}
	fmt.Printf("orb-backfill: %d stuck trade(s) | %s\n\n", len(trades), mode)
	fmt.Printf("%-9s %-5s %5s %12s %10s %8s %5s %s\n",
		"TICKER", "SIDE", "QTY", "EXIT", "PRICE", "PNL", "TF", "WHEN")

	now := time.Now().UTC()
	horizon := time.Duration(*horizonDays) * 24 * time.Hour

	var resolved, stillOpen, noData int
	var totalPnL float64

	for i := range trades {
		t := trades[i]

		// A zero entry fill price would make exitPnL compute against a 0 entry and write a
		// wildly wrong, permanent pnl. Skip rather than poison the row (matches the engine).
		if t.FillPrice == 0 {
			noData++
			fmt.Printf("%-9s %-5s %5d %12s %10s %8s %5s  zero fill_price — skipped\n",
				t.Ticker, t.Side, t.Qty, "no-fill", "-", "-", "-")
			continue
		}

		from := t.CreatedAt
		if t.FilledAt != nil {
			from = *t.FilledAt
		}
		to := from.Add(horizon)
		if to.After(now) {
			to = now
		}

		// If adaptive stop management moved the stop, t.StopLoss is no longer the level that
		// was live across the whole post-fill window — replaying it would fabricate a stop-out
		// at the moved level over a pre-move pullback (see migration 035). Disable the stop leg
		// (0 ⇒ ResolveExitViaBarReplay skips it) and replay only the immutable take-profit;
		// the stop side is owned by the live broker order. Mirrors reconcileBarReplayExits.
		replayStop := t.StopLoss
		if t.StopMovedAt != nil {
			replayStop = 0
			fmt.Printf("%-9s %-5s  stop_moved_at=%s — stop leg disabled, replaying TP only\n",
				t.Ticker, t.Side, t.StopMovedAt.Format(time.RFC3339))
		}
		res, rErr := orb.ResolveExitViaBarReplay(ctx, md, t.Ticker, t.Side, replayStop, t.TakeProfit, from, to, now)
		if rErr != nil {
			noData++
			fmt.Printf("%-9s %-5s %5d %12s %10s %8s %5s  %s\n",
				t.Ticker, t.Side, t.Qty, "no-data", "-", "-", "-", rErr)
			continue
		}
		if !res.Found {
			stillOpen++
			fmt.Printf("%-9s %-5s %5d %12s %10s %8s %5d  never touched a leg in %dd window\n",
				t.Ticker, t.Side, t.Qty, "still-open", "-", "-", res.Bars, *horizonDays)
			continue
		}

		pnl := orb.ExitPnL(t.Side, t.FillPrice, res.ExitPrice, t.Qty)
		resolved++
		totalPnL += pnl
		fmt.Printf("%-9s %-5s %5d %12s %10.5f %+8.2f %5s  %s\n",
			t.Ticker, t.Side, t.Qty, res.ExitType, res.ExitPrice, pnl,
			string(res.Timeframe), res.ExitTime.Format("2006-01-02 15:04"))

		if *apply {
			if dbErr := store.CloseORBTradeAt(ctx, db, t.ID, res.ExitType, "replay", res.ExitPrice, pnl, res.ExitTime); dbErr != nil {
				fmt.Fprintf(os.Stderr, "  ! close %s (%s) failed: %v\n", t.Ticker, t.ID, dbErr)
			}
		}
	}

	fmt.Printf("\nSummary: %d resolved (PnL %+.2f), %d still-open, %d no-data, of %d stuck.\n",
		resolved, totalPnL, stillOpen, noData, len(trades))
	if !*apply && resolved > 0 {
		fmt.Println("Dry run only — re-run with --apply to record these exits.")
	}
}
