package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/romeo-folie/trade-engine/internal/store"
	"github.com/spf13/cobra"
)

func init() {
	// Replace stub subcommands with real implementations.
	tradesCmd.ResetCommands()
	tradesCmd.AddCommand(tradesListCmd)
	tradesCmd.AddCommand(tradesShowCmd)
	tradesCmd.AddCommand(tradesPendingCmd)
	tradesCmd.AddCommand(tradesApproveCmd)
	tradesCmd.AddCommand(tradesRejectCmd)
}

var tradesListCmd = &cobra.Command{
	Use:   "list",
	Short: "List recent trades",
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := store.Connect(ctx); err != nil {
			return fmt.Errorf("database: %w", err)
		}
		defer store.Close()

		trades, err := store.GetRecentTrades(ctx, 20)
		if err != nil {
			return fmt.Errorf("get trades: %w", err)
		}
		if len(trades) == 0 {
			fmt.Println("No trades yet.")
			return nil
		}

		fmt.Printf("%-8s  %-6s  %-5s  %-12s  %-12s  %-12s  %-10s  %s\n",
			"TICKER", "DIR", "STATUS", "ENTRY", "EXIT", "P&L", "P&L %", "OPENED")
		for _, t := range trades {
			exitStr := "—"
			pnlStr := "—"
			pnlPctStr := "—"
			if t.Status == "closed" {
				exitStr = fmt.Sprintf("%.4f", t.ExitPrice)
				pnlStr = fmt.Sprintf("%.2f", t.PnL)
				pnlPctStr = fmt.Sprintf("%.2f%%", t.PnLPct)
			}
			fmt.Printf("%-8s  %-6s  %-5s  %-12.4f  %-12s  %-12s  %-10s  %s\n",
				t.Ticker, strings.ToUpper(t.Direction), t.Status,
				t.EntryPrice, exitStr, pnlStr, pnlPctStr,
				t.OpenedAt.Format("2006-01-02 15:04"))
		}
		return nil
	},
}

var tradesShowCmd = &cobra.Command{
	Use:   "show <trade-id>",
	Short: "Show full trade details",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := store.Connect(ctx); err != nil {
			return fmt.Errorf("database: %w", err)
		}
		defer store.Close()

		t, err := store.GetTradeByID(ctx, args[0])
		if err != nil {
			return fmt.Errorf("get trade: %w", err)
		}
		if t == nil {
			return fmt.Errorf("trade %s not found", args[0])
		}

		fmt.Printf("Trade ID:      %s\n", t.ID)
		fmt.Printf("Signal ID:     %s\n", t.SignalID)
		fmt.Printf("Ticker:        %s\n", t.Ticker)
		fmt.Printf("Direction:     %s\n", strings.ToUpper(t.Direction))
		fmt.Printf("Strategy:      %s\n", t.Strategy)
		fmt.Printf("Status:        %s\n", t.Status)
		fmt.Printf("Entry Price:   %.4f\n", t.EntryPrice)
		fmt.Printf("Quantity:      %.6f\n", t.Quantity)
		if t.StopLoss > 0 {
			fmt.Printf("Stop Loss:     %.4f\n", t.StopLoss)
		}
		if t.TakeProfit > 0 {
			fmt.Printf("Take Profit:   %.4f\n", t.TakeProfit)
		}
		if t.Status == "closed" {
			fmt.Printf("Exit Price:    %.4f\n", t.ExitPrice)
			fmt.Printf("P&L:           %.2f (%.2f%%)\n", t.PnL, t.PnLPct)
			fmt.Printf("Closed At:     %s\n", t.ClosedAt.Format(time.RFC3339))
		}
		fmt.Printf("Opened At:     %s\n", t.OpenedAt.Format(time.RFC3339))
		if t.ModelProvider != "" {
			fmt.Printf("Model:         %s/%s\n", t.ModelProvider, t.ModelName)
		}

		// Show linked orders.
		orders, err := store.GetOrdersByTrade(ctx, t.ID)
		if err == nil && len(orders) > 0 {
			fmt.Printf("\nOrders (%d):\n", len(orders))
			for _, o := range orders {
				tag := ""
				if o.IsStopLoss {
					tag = " [SL]"
				}
				if o.IsTakeProfit {
					tag = " [TP]"
				}
				fmt.Printf("  %s  %s %s  qty=%.6f  fill=%.4f  status=%s%s\n",
					o.ID[:8], strings.ToUpper(o.Side), o.OrderType,
					o.Quantity, o.FilledPrice, o.Status, tag)
			}
		}

		return nil
	},
}

var tradesPendingCmd = &cobra.Command{
	Use:     "pending",
	Aliases: []string{"signals"},
	Short:   "List pending signals awaiting approval",
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := store.Connect(ctx); err != nil {
			return fmt.Errorf("database: %w", err)
		}
		defer store.Close()

		signals, err := store.GetPendingSignals(ctx)
		if err != nil {
			return fmt.Errorf("get pending signals: %w", err)
		}
		if len(signals) == 0 {
			fmt.Println("No pending signals.")
			return nil
		}

		fmt.Printf("%-8s  %-6s  %-10s  %-8s  %-10s  %s\n",
			"TICKER", "ACTION", "PRICE", "CONF", "EXPIRES", "ID")
		for _, s := range signals {
			remaining := time.Until(s.ExpiresAt).Round(time.Second)
			expiryStr := remaining.String()
			if remaining <= 0 {
				expiryStr = "expiring"
			}
			fmt.Printf("%-8s  %-6s  %-10.4f  %-8.2f  %-10s  %s\n",
				s.Ticker, s.Action, s.SignalPrice,
				s.Confidence, expiryStr, s.ID[:8])
		}
		return nil
	},
}

var tradesApproveCmd = &cobra.Command{
	Use:   "approve <signal-id>",
	Short: "Approve a pending signal for execution",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := store.Connect(ctx); err != nil {
			return fmt.Errorf("database: %w", err)
		}
		defer store.Close()

		if err := store.UpdateSignalStatus(ctx, args[0], "approved", ""); err != nil {
			return fmt.Errorf("approve signal: %w", err)
		}
		fmt.Printf("Signal %s approved. Engine will execute on next tick.\n", args[0])
		return nil
	},
}

var tradesRejectCmd = &cobra.Command{
	Use:   "reject <signal-id>",
	Short: "Reject a pending signal",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := store.Connect(ctx); err != nil {
			return fmt.Errorf("database: %w", err)
		}
		defer store.Close()

		if err := store.UpdateSignalStatus(ctx, args[0], "rejected", "manually rejected"); err != nil {
			return fmt.Errorf("reject signal: %w", err)
		}
		fmt.Printf("Signal %s rejected.\n", args[0])
		return nil
	},
}
