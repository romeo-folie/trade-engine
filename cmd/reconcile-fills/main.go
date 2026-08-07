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
//	DATABASE_URL=… go run ./cmd/reconcile-fills --all
//
// Both sides are windowed by the same bounds, and the default window is "all of
// it". Narrowing the window is supported but lossy in a way no filter can fix:
// a position opened before the window has no acquisition inside it, so the walk
// opens a phantom short and inverts the sign of everything after. Symbols where
// that happened are flagged SOLD SHARES NEVER BOUGHT and excluded from the
// totals rather than quietly corrupting them.
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
		since     = flag.String("since", "2015-01-01", "reconcile fills and trades on/after this date (YYYY-MM-DD)")
		until     = flag.String("until", "", "reconcile before this date (YYYY-MM-DD); default tomorrow")
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
	if !before.After(after) {
		return fmt.Errorf("--until (%s) must be after --since (%s)", before.Format("2006-01-02"), since)
	}

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	stocks := cfg.Markets.Stocks
	if stocks.APIKey == "" || stocks.Secret == "" {
		return fmt.Errorf("markets.stocks api_key/secret not configured")
	}
	if stocks.BaseURL == "" {
		return fmt.Errorf("markets.stocks.base_url not configured")
	}

	if err := store.Connect(ctx); err != nil {
		return fmt.Errorf("connect db: %w", err)
	}
	defer store.Close()

	allFills, err := recon.ActivityFetcher{
		BaseURL: stocks.BaseURL, APIKey: stocks.APIKey, Secret: stocks.Secret,
	}.FetchFills(ctx, after, before)
	if err != nil {
		return fmt.Errorf("fetch fills: %w", err)
	}
	// Option fills share this Alpaca account but are recorded in orb_trades, not
	// `trades`. Reconciling them here would flag every contract as off-ledger and
	// would misprice them 100x (qty is contracts, price is per share).
	fills, options := recon.PartitionFills(allFills)
	broker := recon.RealizedBySymbol(fills)

	ledger, err := store.ClosedPnLByTicker(ctx, after, before)
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
	drift := func(s string) float64 { return ledger[s].PnL - broker[s].Realized }
	comparable := func(s string) bool { return broker[s].Flat() && broker[s].Trustworthy() }
	sort.Slice(sorted, func(i, j int) bool {
		a, b := sorted[i], sorted[j]
		// Comparable rows first, biggest drift on top; incomparable rows sort by
		// name because their "drift" is not a number anyone should rank on.
		if comparable(a) != comparable(b) {
			return comparable(a)
		}
		if comparable(a) {
			if da, db := absf(drift(a)), absf(drift(b)); da != db {
				return da > db
			}
		}
		return a < b
	})

	fmt.Printf("Alpaca fills: %d equity across %d symbols (%d option fills skipped — those live in orb_trades)\n",
		len(fills), len(broker), len(options))
	fmt.Printf("window %s → %s, applied to BOTH sides\n\n", after.Format("2006-01-02"), before.Format("2006-01-02"))
	fmt.Printf("%-8s %12s %12s %12s %5s  %s\n", "SYMBOL", "LEDGER", "BROKER", "DRIFT", "n", "note")
	fmt.Println("---------------------------------------------------------------------------")

	var flatLedger, flatBroker float64
	var comparableCount, reported int
	var excluded []string
	for _, s := range sorted {
		b, l := broker[s], ledger[s]
		note := b.Note(l.Trades)
		driftStr := "     —"
		if comparable(s) {
			flatLedger += l.PnL
			flatBroker += b.Realized
			comparableCount++
			driftStr = fmt.Sprintf("%+12.2f", drift(s))
		} else {
			excluded = append(excluded, s)
		}
		if showAll || (comparable(s) && absf(drift(s)) > threshold) || note != "" {
			fmt.Printf("%-8s %12.2f %12.2f %12s %5d  %s\n", s, l.PnL, b.Realized, driftStr, l.Trades, note)
			reported++
		}
	}

	fmt.Println("---------------------------------------------------------------------------")
	fmt.Printf("COMPARABLE symbols only (%d of %d) — flat position, complete history:\n", comparableCount, len(sorted))
	fmt.Printf("  ledger %12.2f\n  broker %12.2f\n  drift  %+12.2f\n",
		flatLedger, flatBroker, flatLedger-flatBroker)
	if len(excluded) > 0 {
		fmt.Printf("\nExcluded from the total (%d): %v\n", len(excluded), excluded)
	}
	fmt.Printf("\n%d symbol(s) reported, %d suppressed as clean.\n", reported, len(sorted)-reported)
	fmt.Println("\nRead-only: nothing was written. Broker fills are truth; drift means the ledger is wrong.")
	return nil
}

func absf(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}
