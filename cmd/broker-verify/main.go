// Command broker-verify dumps the broker's current open positions, using the
// same broker config the engine reads. It exists so the operator can confirm
// the live JMIA quantity BEFORE running cmd/jmia-consolidate (issue #311).
//
// Usage:
//
//	go run ./cmd/broker-verify
//	go run ./cmd/broker-verify --ticker=JMIA
//
// Broker selection follows the same logic as `te run`:
//   - If markets.ibkr.enabled, use IBKR (read-only — no order placement).
//   - Otherwise, if markets.stocks.api_key is set, use Alpaca.
//   - Otherwise, refuse — paper mode has no real positions to verify.
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
	flag.Parse()

	_ = godotenv.Load()

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	bv, brokerName, err := selectBroker(cfg)
	if err != nil {
		return err
	}
	fmt.Printf("broker: %s\n", brokerName)

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

func selectBroker(cfg *config.Config) (execution.BrokerView, string, error) {
	// Mirror cmd/te/run.go broker selection but exclude paper mode (which
	// would return ErrBrokerUnavailable and serve no purpose here).
	if cfg.Markets.IBKR.Enabled {
		ibkr := execution.NewIBKRBroker(cfg.Markets.IBKR)
		// CheckAuth surfaces gateway-auth issues with a clearer error.
		if err := ibkr.CheckAuth(context.Background()); err != nil {
			return nil, "", fmt.Errorf("IBKR gateway not authenticated: %w", err)
		}
		return ibkr, "ibkr", nil
	}
	if cfg.Markets.Stocks.APIKey != "" {
		return execution.NewAlpacaBroker(cfg.Markets.Stocks), "alpaca", nil
	}
	return nil, "", fmt.Errorf("no live broker configured (set markets.stocks.api_key or markets.ibkr.enabled)")
}
