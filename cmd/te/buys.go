package main

import (
	"context"
	"fmt"

	"github.com/romeo-folie/trade-engine/internal/store"
	"github.com/spf13/cobra"
)

var buyControlSource string

var buysCmd = &cobra.Command{
	Use:   "buys",
	Short: "Pause or resume new stock buys without affecting exits",
}

func setBuyControlCommand(enabled bool, verb string) *cobra.Command {
	return &cobra.Command{
		Use:   verb,
		Short: map[bool]string{true: "Resume new stock buys", false: "Pause new stock buys"}[enabled],
		RunE: func(*cobra.Command, []string) error {
			ctx := context.Background()
			if err := store.Connect(ctx); err != nil {
				return fmt.Errorf("connect to database: %w", err)
			}
			defer store.Close()
			if err := store.SetBuyControl(ctx, store.BuyControlStocks, enabled, buyControlSource); err != nil {
				return fmt.Errorf("set buy control: %w", err)
			}
			state := "resumed"
			if !enabled {
				state = "paused"
			}
			fmt.Printf("New stock buys %s. Exits and protective stops remain active.\n", state)
			return nil
		},
	}
}

var buysStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show the persisted new-buy control",
	RunE: func(*cobra.Command, []string) error {
		ctx := context.Background()
		if err := store.Connect(ctx); err != nil {
			return fmt.Errorf("connect to database: %w", err)
		}
		defer store.Close()
		control, err := store.GetBuyControl(ctx, store.BuyControlStocks)
		if err != nil {
			return fmt.Errorf("read buy control: %w", err)
		}
		state := "enabled"
		if !control.Enabled {
			state = "paused"
		}
		if control.UpdatedAt.IsZero() {
			fmt.Printf("New stock buys: %s (default; no operator override)\n", state)
			return nil
		}
		fmt.Printf("New stock buys: %s (by %s at %s)\n", state, control.UpdatedBy,
			control.UpdatedAt.UTC().Format("2006-01-02 15:04:05Z"))
		return nil
	},
}

func init() {
	buysCmd.AddCommand(setBuyControlCommand(false, "pause"), setBuyControlCommand(true, "resume"), buysStatusCmd)
	buysCmd.PersistentFlags().StringVar(&buyControlSource, "source", "cli", "Audit source for this change")
	rootCmd.AddCommand(buysCmd)
}
