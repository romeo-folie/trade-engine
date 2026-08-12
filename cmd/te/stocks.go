package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/execution"
	"github.com/romeo-folie/trade-engine/internal/store"
	"github.com/spf13/cobra"
)

var stocksPreflightJSON bool

var stocksCmd = &cobra.Command{
	Use:   "stocks",
	Short: "Stock-broker commands",
}

var stocksPreflightCmd = &cobra.Command{
	Use:   "preflight",
	Short: "Read and verify the configured Alpaca stock account",
	Long: `Fetch the configured Alpaca account using one read-only request.

It never submits, cancels, or replaces an order. It is useful after changing
markets.stocks.paper or credentials, before starting a paper or live engine.`,
	RunE: runStocksPreflight,
}

type stockPreflightResult struct {
	Environment              string    `json:"environment"`
	AccountNumber            string    `json:"account_number"`
	Status                   string    `json:"status"`
	Cash                     float64   `json:"cash"`
	Equity                   float64   `json:"equity"`
	BuyingPower              float64   `json:"buying_power"`
	RegTBuyingPower          float64   `json:"regt_buying_power"`
	DayTradingBuyingPower    float64   `json:"daytrading_buying_power"`
	NonMarginableBuyingPower float64   `json:"non_marginable_buying_power"`
	Multiplier               float64   `json:"multiplier"`
	TradingBlocked           bool      `json:"trading_blocked"`
	TransfersBlocked         bool      `json:"transfers_blocked"`
	AccountBlocked           bool      `json:"account_blocked"`
	TradeSuspendedByUser     bool      `json:"trade_suspended_by_user"`
	ShortingEnabled          bool      `json:"shorting_enabled"`
	FetchedAt                time.Time `json:"fetched_at"`
}

func runStocksPreflight(_ *cobra.Command, _ []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	// DB availability must not consume the account API's timeout budget. Connect
	// is globally synchronized and cleans up a failed candidate itself, so use a
	// bounded independent context before creating the broker request context.
	dbCtx, cancelDB := context.WithTimeout(context.Background(), 5*time.Second)
	var lookup runtimeOverrideLookup
	if err := store.Connect(dbCtx); err == nil {
		defer store.Close()
		lookup = store.GetOverride
	}
	cancelDB()
	mode, err := resolveRunMode(context.Background(), cfg.Engine.Mode, "", lookup)
	if err != nil {
		return err
	}
	if err := config.ValidateStockBrokerConfig(cfg.Markets.Stocks); err != nil {
		return err
	}
	if err := config.ValidateStockWorkflowConfig(mode, cfg.Markets.Stocks); err != nil {
		return err
	}
	if !cfg.Markets.Stocks.Enabled {
		return fmt.Errorf("markets.stocks.enabled=false")
	}
	if cfg.Markets.Stocks.APIKey == "" || cfg.Markets.Stocks.Secret == "" {
		return fmt.Errorf("Alpaca credentials are required; set %s and %s", cfg.Markets.Stocks.APIKeyEnv, cfg.Markets.Stocks.SecretEnv)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	result, err := preflightStockAccount(ctx, cfg.Markets.Stocks, execution.NewAlpacaBroker(cfg.Markets.Stocks))
	if err != nil {
		return err
	}
	if stocksPreflightJSON {
		return json.NewEncoder(os.Stdout).Encode(result)
	}
	printStockPreflight(result)
	return nil
}

type stockAccountSnapshotter interface {
	AccountSnapshot(context.Context) (execution.BrokerAccount, error)
}

func preflightStockAccount(ctx context.Context, stocks config.StocksConfig, broker stockAccountSnapshotter) (stockPreflightResult, error) {
	account, err := broker.AccountSnapshot(ctx)
	if err != nil {
		return stockPreflightResult{}, fmt.Errorf("read Alpaca account: %w", err)
	}
	if strings.TrimSpace(account.AccountNumber) == "" {
		return stockPreflightResult{}, fmt.Errorf("Alpaca account response did not include an account number")
	}
	if expected := strings.TrimSpace(stocks.ExpectedAccountNumber); expected != "" && account.AccountNumber != expected {
		return stockPreflightResult{}, fmt.Errorf("Alpaca account mismatch: configured expected account number does not match the connected account")
	}
	if !strings.EqualFold(strings.TrimSpace(account.Status), "ACTIVE") {
		return stockPreflightResult{}, fmt.Errorf("Alpaca account is not active (status %q)", account.Status)
	}
	if account.TradingBlocked || account.AccountBlocked || account.TradeSuspendedByUser {
		return stockPreflightResult{}, fmt.Errorf("Alpaca account cannot trade (trading_blocked=%t account_blocked=%t trade_suspended_by_user=%t)", account.TradingBlocked, account.AccountBlocked, account.TradeSuspendedByUser)
	}

	environment := "live"
	if stocks.Paper {
		environment = "paper"
	}
	return stockPreflightResult{
		Environment:              environment,
		AccountNumber:            account.AccountNumber,
		Status:                   account.Status,
		Cash:                     account.Cash,
		Equity:                   account.Equity,
		BuyingPower:              account.BuyingPower,
		RegTBuyingPower:          account.RegTBuyingPower,
		DayTradingBuyingPower:    account.DayTradingBuyingPower,
		NonMarginableBuyingPower: account.NonMarginableBuyingPower,
		Multiplier:               account.Multiplier,
		TradingBlocked:           account.TradingBlocked,
		TransfersBlocked:         account.TransfersBlocked,
		AccountBlocked:           account.AccountBlocked,
		TradeSuspendedByUser:     account.TradeSuspendedByUser,
		ShortingEnabled:          account.ShortingEnabled,
		FetchedAt:                account.FetchedAt,
	}, nil
}

func printStockPreflight(result stockPreflightResult) {
	fmt.Printf("Alpaca stock preflight: OK\n")
	fmt.Printf("  Environment: %s\n", result.Environment)
	fmt.Printf("  Account:     %s (%s)\n", result.AccountNumber, result.Status)
	fmt.Printf("  Cash:        %.2f\n", result.Cash)
	fmt.Printf("  Equity:      %.2f\n", result.Equity)
	fmt.Printf("  Buying power: %.2f (Reg-T %.2f, day-trading %.2f, non-marginable %.2f)\n",
		result.BuyingPower, result.RegTBuyingPower, result.DayTradingBuyingPower, result.NonMarginableBuyingPower)
}

func init() {
	stocksPreflightCmd.Flags().BoolVar(&stocksPreflightJSON, "json", false, "Print the explicit local preflight result as JSON")
	stocksCmd.AddCommand(stocksPreflightCmd)
	rootCmd.AddCommand(stocksCmd)
}
