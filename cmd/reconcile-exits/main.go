// Command reconcile-exits runs one sweep of the exit-fill reconciler: every
// exit order still marked 'pending' whose trade is already closed is checked
// against the broker, the order row is backfilled with the confirmed fill,
// and the trade's exit_price/pnl/pnl_pct are rewritten to broker truth when
// they differ from the trigger price recorded at close time.
//
// Exists so the historical backlog (81 unreconciled exits as of 2026-07-02,
// including a LAC trade recorded +$787 that actually closed −$19) can be
// healed immediately without waiting for an engine restart — the engine runs
// the same sweep periodically once it carries this code.
//
// Usage:
//
//	go run ./cmd/reconcile-exits            # dry summary of what would change
//	go run ./cmd/reconcile-exits --apply    # write corrections to the DB
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	_ "time/tzdata"

	"github.com/joho/godotenv"
	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/engine"
	"github.com/romeo-folie/trade-engine/internal/execution"
	"github.com/romeo-folie/trade-engine/internal/store"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "reconcile-exits: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	apply := flag.Bool("apply", false, "write corrections to the DB (default is a read-only report)")
	limit := flag.Int("limit", 500, "max pending exit orders to examine")
	flag.Parse()

	_ = godotenv.Load()

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	if cfg.Markets.Stocks.APIKey == "" {
		return fmt.Errorf("markets.stocks.api_key not configured — need Alpaca credentials to confirm fills")
	}
	broker := execution.NewAlpacaBroker(cfg.Markets.Stocks)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	if err := store.Connect(ctx); err != nil {
		return fmt.Errorf("store.Connect: %w", err)
	}
	defer store.Close()

	pending, err := store.GetPendingExitOrders(ctx, *limit)
	if err != nil {
		return fmt.Errorf("GetPendingExitOrders: %w", err)
	}
	fmt.Printf("pending exit orders on closed trades: %d\n", len(pending))

	if !*apply {
		// Read-only report: poll the broker, print what would change.
		for _, p := range pending {
			res, err := broker.GetOrderStatus(ctx, p.BrokerOrderID)
			if err != nil {
				fmt.Printf("%-6s trade=%s  broker lookup failed: %v\n", p.Ticker, p.TradeID, err)
				continue
			}
			status := execution.NormalizeOrderStatus(res.Status)
			if status == "filled" {
				fmt.Printf("%-6s trade=%s  recorded_exit=%.4f  broker_fill=%.4f  broker_status=%s\n",
					p.Ticker, p.TradeID, p.ExitPrice, res.FilledPrice, res.Status)
			} else {
				fmt.Printf("%-6s trade=%s  recorded_exit=%.4f  broker_status=%s (no fill)\n",
					p.Ticker, p.TradeID, p.ExitPrice, res.Status)
			}
		}
		fmt.Println("\nread-only report — re-run with --apply to write corrections")
		return nil
	}

	stats := engine.ReconcileExitOrderFillsWithStore(ctx, broker, *limit)
	fmt.Printf("scanned=%d corrected=%d confirmed=%d terminal=%d still_pending=%d not_found=%d errors=%d\n",
		stats.Scanned, stats.Corrected, stats.Confirmed, stats.Terminal,
		stats.StillPending, stats.NotFound, stats.Errors)
	if stats.Errors > 0 {
		return fmt.Errorf("%d rows failed — re-run to retry", stats.Errors)
	}
	return nil
}
