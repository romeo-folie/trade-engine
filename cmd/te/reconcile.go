package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"strings"
	"time"

	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/engine"
	"github.com/romeo-folie/trade-engine/internal/execution"
	"github.com/romeo-folie/trade-engine/internal/store"
	"github.com/spf13/cobra"
)

var (
	reconcileBrokerName string
	reconcileTicker     string
	reconcileJSON       bool
)

type brokerReconcileReport struct {
	CheckedAt       time.Time                  `json:"checked_at"`
	Broker          string                     `json:"broker"`
	Ticker          string                     `json:"ticker,omitempty"`
	SnapshotMode    string                     `json:"snapshot_mode"`
	Clean           bool                       `json:"clean"`
	Drifts          []brokerReconcileDrift     `json:"drifts"`
	BrokerPositions []execution.BrokerPosition `json:"broker_positions,omitempty"`
	DBTrades        []brokerReconcileTrade     `json:"db_trades,omitempty"`
}

type brokerReconcileDrift struct {
	Ticker         string  `json:"ticker"`
	BrokerQty      float64 `json:"broker_qty"`
	DBQty          float64 `json:"db_qty"`
	Delta          float64 `json:"delta"`
	DBRowCount     int     `json:"db_row_count"`
	Source         string  `json:"source"`
	Recommendation string  `json:"recommendation"`
}

type brokerReconcileTrade struct {
	ID         string    `json:"id"`
	Ticker     string    `json:"ticker"`
	Direction  string    `json:"direction"`
	Quantity   float64   `json:"quantity"`
	Status     string    `json:"status"`
	Strategy   string    `json:"strategy"`
	EntryPrice float64   `json:"entry_price"`
	OpenedAt   time.Time `json:"opened_at"`
}

var reconcileCmd = &cobra.Command{
	Use:   "reconcile",
	Short: "Inspect broker and database consistency",
}

var reconcileBrokerCmd = &cobra.Command{
	Use:   "broker",
	Short: "Print a read-only broker/DB position reconciliation report",
	Long: `Fetches one broker position snapshot and one database open-trade
snapshot, compares them using the same rules as engine startup, and prints
recommended next actions. This command never writes the database, places an
order, adopts a position, or sends a notification.

This report is intentionally limited to the Alpaca equities account and the
legacy trades ledger used by the standard engine path. Forex, futures,
options, and ORB positions have separate ledgers and require their own
domain-aware reconciliation. Use --broker=alpaca to make the account choice
explicit.`,
	RunE: runBrokerReconcileReport,
}

func runBrokerReconcileReport(cmd *cobra.Command, args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	if ticker := strings.TrimSpace(reconcileTicker); ticker != "" && execution.IsOCCSymbol(strings.ToUpper(ticker)) {
		return fmt.Errorf("ticker %q is an OCC option symbol; use the options reconciler instead", reconcileTicker)
	}

	broker, brokerName, err := selectReconcileBroker(cfg, reconcileBrokerName)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	restoreDBEnv, err := useConfiguredDatabaseEnv(cfg)
	if err != nil {
		return err
	}
	defer restoreDBEnv()
	if err := store.Connect(ctx); err != nil {
		return fmt.Errorf("connect to database: %w", err)
	}
	defer store.Close()

	positions, err := broker.PositionsSnapshot(ctx)
	if err != nil {
		return fmt.Errorf("fetch %s positions: %w", brokerName, err)
	}
	trades, err := store.GetOwnedPositionTrades(ctx)
	if err != nil {
		return fmt.Errorf("fetch database positions: %w", err)
	}

	positions = filterBrokerPositions(positions, reconcileTicker)
	trades = filterReconcileTrades(trades, reconcileTicker)
	// The report uses the exact snapshots already fetched above. The snapshot
	// API is intentionally silent so --json remains valid machine-readable JSON.
	drifts := engine.CompareBrokerVsDBSnapshots(positions, trades, 0)
	report := buildBrokerReconcileReport(brokerName, reconcileTicker, positions, trades, drifts, time.Now().UTC())

	if reconcileJSON {
		blob, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			return fmt.Errorf("encode reconciliation report: %w", err)
		}
		fmt.Println(string(blob))
		return nil
	}
	printBrokerReconcileReport(report)
	return nil
}

func selectReconcileBroker(cfg *config.Config, override string) (execution.BrokerView, string, error) {
	switch strings.ToLower(strings.TrimSpace(override)) {
	case "alpaca":
		if cfg.Markets.Stocks.APIKey == "" {
			return nil, "", fmt.Errorf("--broker=alpaca but markets.stocks.api_key is not configured")
		}
		return execution.NewAlpacaBroker(cfg.Markets.Stocks), "alpaca", nil
	case "ibkr":
		return nil, "", fmt.Errorf("IBKR reconciliation is not supported by this legacy-equity report; use the domain-specific forex/futures/ORB reconciler")
	case "":
		if cfg.Markets.Stocks.APIKey != "" {
			return execution.NewAlpacaBroker(cfg.Markets.Stocks), "alpaca", nil
		}
		return nil, "", fmt.Errorf("no Alpaca equities broker configured; this report does not reconcile IBKR domain ledgers")
	default:
		return nil, "", fmt.Errorf("invalid --broker=%q; valid value is alpaca", override)
	}
}

func filterBrokerPositions(positions []execution.BrokerPosition, ticker string) []execution.BrokerPosition {
	ticker = strings.TrimSpace(ticker)
	if ticker == "" {
		return positions
	}
	out := make([]execution.BrokerPosition, 0, 1)
	for _, p := range positions {
		if strings.EqualFold(p.Symbol, ticker) {
			out = append(out, p)
		}
	}
	return out
}

func filterReconcileTrades(trades []store.Trade, ticker string) []store.Trade {
	ticker = strings.TrimSpace(ticker)
	if ticker == "" {
		return trades
	}
	out := make([]store.Trade, 0, 1)
	for _, t := range trades {
		if strings.EqualFold(t.Ticker, ticker) {
			out = append(out, t)
		}
	}
	return out
}

func buildBrokerReconcileReport(brokerName, ticker string, positions []execution.BrokerPosition, trades []store.Trade, drifts []engine.BrokerDrift, checkedAt time.Time) brokerReconcileReport {
	report := brokerReconcileReport{
		CheckedAt:    checkedAt,
		Broker:       brokerName,
		Ticker:       strings.ToUpper(strings.TrimSpace(ticker)),
		SnapshotMode: "sequential_non_atomic",
		Clean:        len(drifts) == 0,
		Drifts:       make([]brokerReconcileDrift, 0, len(drifts)),
	}
	for _, d := range drifts {
		report.Drifts = append(report.Drifts, brokerReconcileDrift{
			Ticker:         d.Ticker,
			BrokerQty:      d.BrokerQty,
			DBQty:          d.DBQty,
			Delta:          d.BrokerQty - d.DBQty,
			DBRowCount:     d.DBRowCount,
			Source:         d.Source,
			Recommendation: reconcileRecommendation(d),
		})
	}
	for _, p := range positions {
		if reportContainsTicker(report.Drifts, p.Symbol) {
			report.BrokerPositions = append(report.BrokerPositions, p)
		}
	}
	for _, t := range trades {
		if reportContainsTicker(report.Drifts, t.Ticker) {
			report.DBTrades = append(report.DBTrades, brokerReconcileTrade{
				ID: t.ID, Ticker: t.Ticker, Direction: t.Direction,
				Quantity: t.Quantity, Status: t.Status, Strategy: t.Strategy,
				EntryPrice: t.EntryPrice, OpenedAt: t.OpenedAt,
			})
		}
	}
	return report
}

func reportContainsTicker(drifts []brokerReconcileDrift, ticker string) bool {
	for _, d := range drifts {
		if strings.EqualFold(d.Ticker, ticker) {
			return true
		}
	}
	return false
}

func reconcileRecommendation(d engine.BrokerDrift) string {
	switch d.Source {
	case "broker_only":
		return "Confirm the broker position is intentional; adoption is an explicit operator decision."
	case "db_only":
		return "Confirm the broker is flat, then close/archive the stale DB position through the approved workflow."
	case "side_mismatch":
		return "Direction conflict: stop automated correction and investigate broker orders and DB trade history immediately."
	case "mismatch":
		if d.BrokerQty != 0 && d.DBQty != 0 && (d.BrokerQty < 0) != (d.DBQty < 0) {
			return "Direction conflict: stop automated correction and investigate broker orders and DB trade history immediately."
		}
		if (d.BrokerQty < 0) == (d.DBQty < 0) && math.Abs(d.BrokerQty) < math.Abs(d.DBQty) {
			return "DB overstates the broker position; inspect partial fills or missed exits before correcting the ledger."
		}
		return "Inspect broker fills and DB orders; only the untracked surplus may be eligible for explicit recovery adoption."
	default:
		return "Investigate the broker and DB records before taking corrective action."
	}
}

// useConfiguredDatabaseEnv bridges config.database.url_env to the store
// package's historical DATABASE_URL contract without changing process-global
// state beyond this command invocation. The returned function restores the
// caller's environment before the command exits.
func useConfiguredDatabaseEnv(cfg *config.Config) (func(), error) {
	envName := strings.TrimSpace(cfg.Database.URLEnv)
	if envName == "" || envName == "DATABASE_URL" {
		return func() {}, nil
	}
	dsn := os.Getenv(envName)
	if dsn == "" {
		return nil, fmt.Errorf("configured database environment variable %s is not set", envName)
	}
	previous, hadPrevious := os.LookupEnv("DATABASE_URL")
	if err := os.Setenv("DATABASE_URL", dsn); err != nil {
		return nil, fmt.Errorf("set DATABASE_URL from %s: %w", envName, err)
	}
	return func() {
		if hadPrevious {
			_ = os.Setenv("DATABASE_URL", previous)
		} else {
			_ = os.Unsetenv("DATABASE_URL")
		}
	}, nil
}

func printBrokerReconcileReport(report brokerReconcileReport) {
	if report.Clean {
		fmt.Printf("broker/DB reconciled: clean (broker=%s, checked=%s, snapshots=%s)\n", report.Broker, report.CheckedAt.Format(time.RFC3339), report.SnapshotMode)
		return
	}
	fmt.Printf("broker/DB drift report (read-only; broker=%s, checked=%s, snapshots=%s)\n", report.Broker, report.CheckedAt.Format(time.RFC3339), report.SnapshotMode)
	for _, d := range report.Drifts {
		fmt.Printf("\n%s [%s] broker=%.6f db=%.6f delta=%.6f rows=%d\n", d.Ticker, d.Source, d.BrokerQty, d.DBQty, d.Delta, d.DBRowCount)
		for _, p := range report.BrokerPositions {
			if strings.EqualFold(p.Symbol, d.Ticker) {
				fmt.Printf("  broker: %s %.6f avg=%.6f current=%.6f\n", p.Side, p.Quantity, p.AvgEntryPrice, p.CurrentPrice)
			}
		}
		for _, t := range report.DBTrades {
			if strings.EqualFold(t.Ticker, d.Ticker) {
				fmt.Printf("  db:     %s %.6f status=%s strategy=%s trade=%s\n", t.Direction, t.Quantity, t.Status, t.Strategy, t.ID)
			}
		}
		fmt.Printf("  action: %s\n", d.Recommendation)
	}
	fmt.Println("\nNo database or broker changes were made.")
}

func init() {
	reconcileBrokerCmd.Flags().StringVar(&reconcileBrokerName, "broker", "", "broker to inspect: alpaca (default: Alpaca when configured)")
	reconcileBrokerCmd.Flags().StringVar(&reconcileTicker, "ticker", "", "limit the report to one ticker")
	reconcileBrokerCmd.Flags().BoolVar(&reconcileJSON, "json", false, "emit machine-readable JSON")
	reconcileCmd.AddCommand(reconcileBrokerCmd)
	rootCmd.AddCommand(reconcileCmd)
}
