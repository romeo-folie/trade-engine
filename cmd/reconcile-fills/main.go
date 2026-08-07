// Command reconcile-fills compares the trades ledger against Alpaca fill
// history and reports where they disagree.
//
// READ-ONLY. It never writes to the database. Correcting the drift it finds is
// a separate, deliberate step — the JMIA incident (#329) is a standing reminder
// that overwriting ledger rows destroys the evidence needed to check the work.
//
// Usage:
//
//	DATABASE_URL=… go run ./cmd/reconcile-fills
//	DATABASE_URL=… go run ./cmd/reconcile-fills --since 2026-06-01 --all
//
// Only symbols whose position is FLAT compare exactly. An open position carries
// unrealized P&L that neither side books, so those rows are reported separately
// and excluded from the totals rather than silently mixed in.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/recon"
	"github.com/romeo-folie/trade-engine/internal/store"
)

func main() {
	var (
		since     = flag.String("since", "2015-01-01", "fetch fills on/after this date (YYYY-MM-DD)")
		until     = flag.String("until", "", "fetch fills before this date (YYYY-MM-DD); default now+1d")
		threshold = flag.Float64("threshold", 1.0, "only report symbols drifting by more than this many dollars")
		showAll   = flag.Bool("all", false, "report every symbol, not just those over --threshold")
	)
	flag.Parse()

	if err := run(*since, *until, *threshold, *showAll); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(since, until string, threshold float64, showAll bool) error {
	ctx := context.Background()

	after, err := time.Parse("2006-01-02", since)
	if err != nil {
		return fmt.Errorf("--since: %w", err)
	}
	before := time.Now().AddDate(0, 0, 1)
	if until != "" {
		if before, err = time.Parse("2006-01-02", until); err != nil {
			return fmt.Errorf("--until: %w", err)
		}
	}

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	stocks := cfg.Markets.Stocks
	if stocks.APIKey == "" || stocks.Secret == "" {
		return fmt.Errorf("markets.stocks api_key/secret not configured")
	}

	if err := store.Connect(ctx); err != nil {
		return fmt.Errorf("connect db: %w", err)
	}
	defer store.Close()

	fills, err := recon.ActivityFetcher{
		BaseURL: stocks.BaseURL, APIKey: stocks.APIKey, Secret: stocks.Secret,
	}.FetchFills(ctx, after, before)
	if err != nil {
		return fmt.Errorf("fetch fills: %w", err)
	}
	broker := recon.RealizedBySymbol(fills)

	ledger, err := store.ClosedPnLByTicker(ctx)
	if err != nil {
		return fmt.Errorf("ledger totals: %w", err)
	}

	symbols := map[string]struct{}{}
	for s := range broker {
		symbols[s] = struct{}{}
	}
	for s := range ledger {
		symbols[s] = struct{}{}
	}
	sorted := make([]string, 0, len(symbols))
	for s := range symbols {
		sorted = append(sorted, s)
	}
	sort.Slice(sorted, func(i, j int) bool {
		di := ledger[sorted[i]].PnL - broker[sorted[i]].Realized
		dj := ledger[sorted[j]].PnL - broker[sorted[j]].Realized
		return absf(di) > absf(dj)
	})

	fmt.Printf("Alpaca fills: %d across %d symbols   window %s → %s\n\n",
		len(fills), len(broker), after.Format("2006-01-02"), before.Format("2006-01-02"))
	fmt.Printf("%-8s %12s %12s %12s %5s  %s\n", "SYMBOL", "LEDGER", "BROKER", "DRIFT", "n", "note")
	fmt.Println("--------------------------------------------------------------------")

	var flatLedger, flatBroker float64
	var flatCount, reported int
	var openSyms []string
	for _, s := range sorted {
		b, l := broker[s], ledger[s]
		drift := l.PnL - b.Realized
		note := ""
		if b.Shorted {
			note = "had short position"
		}
		if !b.Flat() {
			note = fmt.Sprintf("OPEN %.4f — not comparable", b.Position)
			openSyms = append(openSyms, s)
		} else {
			flatLedger += l.PnL
			flatBroker += b.Realized
			flatCount++
		}
		if l.Trades == 0 && (b.Realized != 0 || !b.Flat()) {
			note = "NO LEDGER ROWS — traded entirely off-ledger"
		}
		if showAll || absf(drift) > threshold || note != "" {
			fmt.Printf("%-8s %12.2f %12.2f %+12.2f %5d  %s\n", s, l.PnL, b.Realized, drift, l.Trades, note)
			reported++
		}
	}

	fmt.Println("--------------------------------------------------------------------")
	fmt.Printf("FLAT symbols only (%d of %d) — the exact comparison:\n", flatCount, len(sorted))
	fmt.Printf("  ledger %12.2f\n  broker %12.2f\n  drift  %+12.2f\n",
		flatLedger, flatBroker, flatLedger-flatBroker)
	if len(openSyms) > 0 {
		fmt.Printf("\nExcluded (open positions): %v\n", openSyms)
	}
	fmt.Printf("\n%d symbol(s) reported; %d within $%.2f.\n", reported, len(sorted)-reported, threshold)
	fmt.Println("\nRead-only: nothing was written. Broker fills are truth; drift means the ledger is wrong.")
	return nil
}

func absf(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}
