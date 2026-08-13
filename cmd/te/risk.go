package main

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/execution"
	"github.com/romeo-folie/trade-engine/internal/store"
	"github.com/spf13/cobra"
)

var (
	riskBaselineAccountID string
	riskBaselineEquity    float64
)

var riskCmd = &cobra.Command{
	Use:   "risk",
	Short: "Operate account-level risk controls",
}

var riskResetBaselineCmd = &cobra.Command{
	Use:   "reset-baseline",
	Short: "Reset an account's drawdown baseline after a capital change",
	Long: `Reset the account-scoped daily and drawdown baseline after a confirmed
paper reset, deposit, or withdrawal. This is deliberately manual: a balance
drop alone may be a real loss and must not silently clear the circuit breaker.`,
	RunE: func(*cobra.Command, []string) error {
		if strings.TrimSpace(riskBaselineAccountID) == "" {
			return fmt.Errorf("--account-id is required")
		}
		if !validRiskBaselineEquity(riskBaselineEquity) {
			return fmt.Errorf("--equity must be positive")
		}
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		if err := config.ValidateStockBrokerConfig(cfg.Markets.Stocks); err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		account, err := execution.NewAlpacaBroker(cfg.Markets.Stocks).AccountSnapshot(ctx)
		if err != nil {
			return fmt.Errorf("read Alpaca account: %w", err)
		}
		if err := execution.ValidateBrokerAccount(account, cfg.Markets.Stocks.ExpectedAccountNumber, time.Now().UTC()); err != nil {
			return fmt.Errorf("validate Alpaca account: %w", err)
		}
		if account.AccountID != riskBaselineAccountID {
			return fmt.Errorf("configured Alpaca account does not match --account-id")
		}
		if !riskBaselineEquityMatchesSnapshot(riskBaselineEquity, account.Equity) {
			return fmt.Errorf("--equity does not match the connected Alpaca account equity")
		}
		if err := store.Connect(ctx); err != nil {
			return fmt.Errorf("connect to database: %w", err)
		}
		defer store.Close()
		baseline, err := store.ResetAccountRiskBaseline(
			ctx, riskBaselineAccountID, account.Equity, account.LastEquity, time.Now(),
		)
		if err != nil {
			return fmt.Errorf("reset account risk baseline: %w", err)
		}
		fmt.Printf("Risk baseline reset for account %s: peak=%.2f daily_reference=%.2f\n", baseline.AccountID, baseline.PeakEquity, baseline.LastEquity)
		return nil
	},
}

func validRiskBaselineEquity(equity float64) bool {
	return equity > 0 && !math.IsNaN(equity) && !math.IsInf(equity, 0)
}

// Alpaca reports account equity in cents. Accept only that display precision
// from the operator-confirmation flag, then persist broker truth rather than a
// manually typed value.
func riskBaselineEquityMatchesSnapshot(requested, snapshot float64) bool {
	return validRiskBaselineEquity(snapshot) && math.Abs(requested-snapshot) <= 0.01
}

func init() {
	riskResetBaselineCmd.Flags().StringVar(&riskBaselineAccountID, "account-id", "", "Broker account ID from stocks preflight")
	riskResetBaselineCmd.Flags().Float64Var(&riskBaselineEquity, "equity", 0, "Confirmed current account equity")
	riskCmd.AddCommand(riskResetBaselineCmd)
	rootCmd.AddCommand(riskCmd)
}
