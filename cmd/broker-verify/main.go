// Command broker-verify dumps the broker's current open positions, using the
// same broker config the engine reads. It exists so the operator can confirm
// the live JMIA quantity BEFORE running cmd/jmia-consolidate (issue #311).
//
// Usage:
//
//	go run ./cmd/broker-verify
//	go run ./cmd/broker-verify --ticker=JMIA
//
// Broker selection mirrors `cmd/te/run.go`:
//   - For equities (the default for tickers like JMIA), `te run` uses Alpaca
//     whenever markets.stocks.api_key is set, regardless of whether IBKR is
//     also enabled (IBKR is wired separately for orb-forex mode).
//   - To explicitly pick a broker, pass --broker=alpaca or --broker=ibkr.
//   - If --broker is empty (auto), the precedence is:
//     1. markets.stocks.api_key set        → Alpaca   (matches `te run` for equities)
//     2. markets.ibkr.enabled              → IBKR     (forex / IBKR-only setups)
//     3. neither                           → refuse
//
// The selected broker name is logged before any positions are queried so the
// operator can confirm which account is being read.
//
// Output is one line per position, plus a single summary line for the
// requested ticker (if --ticker is given) so the value can be piped into
// jmia-consolidate.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	_ "time/tzdata"

	"github.com/joho/godotenv"
	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/execution"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "broker-verify: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	ticker := flag.String("ticker", "", "if set, additionally print a single-line summary for this ticker")
	brokerFlag := flag.String("broker", "", "force broker selection: alpaca | ibkr (empty = auto-select to match `te run` for equities)")
	flag.Parse()

	_ = godotenv.Load()

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	bv, brokerName, err := selectBroker(cfg, strings.ToLower(strings.TrimSpace(*brokerFlag)))
	if err != nil {
		return err
	}
	if *brokerFlag == "" {
		fmt.Printf("broker: %s (auto-selected; pass --broker to override)\n", brokerName)
	} else {
		fmt.Printf("broker: %s (forced via --broker)\n", brokerName)
	}

	ctx := context.Background()
	positions, err := bv.PositionsSnapshot(ctx)
	if err != nil {
		return fmt.Errorf("PositionsSnapshot: %w", err)
	}

	if len(positions) == 0 {
		fmt.Println("(no open positions)")
	} else {
		fmt.Printf("%-10s %-6s %-10s %-12s %-12s %-12s\n",
			"SYMBOL", "SIDE", "QTY", "AVG_ENTRY", "MARKET_VAL", "UNREAL_PL")
		for _, p := range positions {
			fmt.Printf("%-10s %-6s %-10g %-12.4f %-12.2f %-12.2f\n",
				p.Symbol, p.Side, p.Quantity, p.AvgEntryPrice, p.MarketValue, p.UnrealizedPL)
		}
	}

	if *ticker != "" {
		want := strings.ToUpper(*ticker)
		var found *execution.BrokerPosition
		for i := range positions {
			if strings.EqualFold(positions[i].Symbol, want) {
				found = &positions[i]
				break
			}
		}
		fmt.Println()
		if found == nil {
			fmt.Printf("summary: %s not found at broker (qty=0)\n", want)
		} else {
			fmt.Printf("summary: %s qty=%g avg_entry=%.6f side=%s\n",
				found.Symbol, found.Quantity, found.AvgEntryPrice, found.Side)
			fmt.Printf("hint:    --broker-qty=%g --broker-avg-entry=%g\n",
				found.Quantity, found.AvgEntryPrice)
		}
	}
	return nil
}

func selectBroker(cfg *config.Config, override string) (execution.BrokerView, string, error) {
	// Explicit override.
	switch override {
	case "alpaca":
		if cfg.Markets.Stocks.APIKey == "" {
			return nil, "", fmt.Errorf("--broker=alpaca but markets.stocks.api_key is not set")
		}
		return execution.NewAlpacaBroker(cfg.Markets.Stocks), "alpaca", nil
	case "ibkr":
		if !cfg.Markets.IBKR.Enabled {
			return nil, "", fmt.Errorf("--broker=ibkr but markets.ibkr.enabled is false")
		}
		ibkr := execution.NewIBKRBroker(cfg.Markets.IBKR)
		if err := ibkr.CheckAuth(context.Background()); err != nil {
			return nil, "", fmt.Errorf("IBKR gateway not authenticated: %w", err)
		}
		return ibkr, "ibkr", nil
	case "":
		// auto — fall through
	default:
		return nil, "", fmt.Errorf("invalid --broker=%q (valid: alpaca, ibkr)", override)
	}

	// Auto-select. Match `cmd/te/run.go`'s equities path: Alpaca whenever
	// markets.stocks.api_key is set. JMIA and other equities are read from
	// the Alpaca account in production, so verifying against IBKR (even if
	// IBKR is also enabled for forex) would read the wrong account.
	if cfg.Markets.Stocks.APIKey != "" {
		return execution.NewAlpacaBroker(cfg.Markets.Stocks), "alpaca", nil
	}
	if cfg.Markets.IBKR.Enabled {
		ibkr := execution.NewIBKRBroker(cfg.Markets.IBKR)
		if err := ibkr.CheckAuth(context.Background()); err != nil {
			return nil, "", fmt.Errorf("IBKR gateway not authenticated: %w", err)
		}
		return ibkr, "ibkr", nil
	}
	return nil, "", fmt.Errorf("no live broker configured (set markets.stocks.api_key or markets.ibkr.enabled, or pass --broker)")
}
