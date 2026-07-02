// Command backfill-company-names is a one-shot tool that upserts a company
// entity for every active-watchlist / recent-analysis ticker that has no
// entities row, using Finnhub's /stock/profile2 endpoint for the name.
//
// The engine runs the same sweep automatically at boot (cmd/te/run.go); this
// tool exists to close the gap on demand without waiting for a restart.
//
// Usage:
//
//	DATABASE_URL=... FINNHUB_API_KEY=... go run ./cmd/backfill-company-names
//	DATABASE_URL=... go run ./cmd/backfill-company-names -dry-run
//
// The Finnhub key falls back to the key configured for markets.news when the
// env var is unset.
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
	"github.com/romeo-folie/trade-engine/internal/knowledge"
	"github.com/romeo-folie/trade-engine/internal/store"
)

const connectTimeout = 5 * time.Second

func main() {
	_ = godotenv.Load()

	dryRun := flag.Bool("dry-run", false, "list tickers missing an entity without fetching or writing anything")
	days := flag.Int("days", 30, "how many days of analyses to scan for tickers")
	flag.Parse()

	ctx := context.Background()

	connectCtx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	if err := store.Connect(connectCtx); err != nil {
		fmt.Fprintf(os.Stderr, "store.Connect: %v\n", err)
		os.Exit(1)
	}
	defer store.Close()

	missing, err := store.GetTickersMissingEntity(ctx, *days)
	if err != nil {
		fmt.Fprintf(os.Stderr, "GetTickersMissingEntity: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("tickers missing an entity: %d %v\n", len(missing), missing)
	if *dryRun || len(missing) == 0 {
		return
	}

	apiKey := os.Getenv("FINNHUB_API_KEY")
	if apiKey == "" {
		cfg, err := config.Load()
		if err != nil {
			fmt.Fprintf(os.Stderr, "FINNHUB_API_KEY unset and config.Load failed: %v\n", err)
			os.Exit(1)
		}
		apiKey = cfg.Markets.News.APIKey
	}
	if apiKey == "" {
		fmt.Fprintln(os.Stderr, "no Finnhub API key (set FINNHUB_API_KEY or configure markets.news)")
		os.Exit(1)
	}

	added, err := knowledge.BackfillCompanyNames(ctx, data.NewFinnhubClient(apiKey))
	if err != nil {
		fmt.Fprintf(os.Stderr, "backfill: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("entities created: %d\n", added)
}
