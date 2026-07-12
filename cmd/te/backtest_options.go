package main

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/romeo-folie/trade-engine/internal/backtest/optionsreplay"
	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/data"
	"github.com/romeo-folie/trade-engine/internal/logger"
	"github.com/romeo-folie/trade-engine/internal/options"
	"github.com/spf13/cobra"
)

var (
	btOptFrom        string
	btOptTo          string
	btOptUnderlyings string
)

func init() {
	orbOptionsCmd.Flags().StringVar(&btOptFrom, "from", "", "Start date YYYY-MM-DD (required)")
	orbOptionsCmd.Flags().StringVar(&btOptTo, "to", "", "End date YYYY-MM-DD (required)")
	orbOptionsCmd.Flags().StringVar(&btOptUnderlyings, "underlyings", "", "Comma-separated underlyings (default: options_orb.underlyings from config)")
	_ = orbOptionsCmd.MarkFlagRequired("from")
	_ = orbOptionsCmd.MarkFlagRequired("to")
	backtestCmd.AddCommand(orbOptionsCmd)
}

// ── te backtest orb-options ───────────────────────────────────────────────────

var orbOptionsCmd = &cobra.Command{
	Use:   "orb-options",
	Short: "Backtest options-ORB against Alpaca historical option data (parity-by-reuse)",
	Long: `Replay historical ORB breakouts on the configured underlyings, executing each as a
long single-leg option, through the SAME live functions production uses — the
fail-closed strike gates, contract sizing, app-managed exit, and multiplier-correct
P&L. The backtest supplies only as-of historical data; every trading decision is the
production code's, so results describe the code that would ship.

DATA REQUIREMENTS: Alpaca historical option bars/quotes require an OPRA-entitled data
subscription (Algo Trader Plus) and only reach back to ~Feb 2024. A missing
entitlement or an out-of-range window surfaces as HTTP errors from the data calls,
not fabricated fills — the run reports what it could and no more.`,
	Example: `  te backtest orb-options --from 2024-06-01 --to 2024-08-31
  te backtest orb-options --from 2024-06-01 --to 2024-06-30 --underlyings SPY,QQQ`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runOptionsBacktest(cmd.Context())
	},
}

func runOptionsBacktest(ctx context.Context) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if err := logger.Init(cfg.Logging.Level, cfg.Logging.Format); err != nil {
		return err
	}
	from, err := time.Parse("2006-01-02", btOptFrom)
	if err != nil {
		return fmt.Errorf("invalid --from date %q: %w", btOptFrom, err)
	}
	to, err := time.Parse("2006-01-02", btOptTo)
	if err != nil {
		return fmt.Errorf("invalid --to date %q: %w", btOptTo, err)
	}
	if to.Before(from) {
		return fmt.Errorf("--to must not be before --from")
	}
	to = to.Add(24*time.Hour - time.Second)

	underlyings := cfg.OptionsORB.Underlyings
	if btOptUnderlyings != "" {
		underlyings = nil
		for _, s := range splitCSV(btOptUnderlyings) {
			underlyings = append(underlyings, strings.ToUpper(s))
		}
	}
	if len(underlyings) == 0 {
		return fmt.Errorf("no underlyings: set options_orb.underlyings or pass --underlyings")
	}

	stocks := cfg.Markets.Stocks
	apiKey, secret := os.Getenv(stocks.APIKeyEnv), os.Getenv(stocks.SecretEnv)
	if apiKey == "" || secret == "" {
		return fmt.Errorf("Alpaca credentials unset: export %s and %s", stocks.APIKeyEnv, stocks.SecretEnv)
	}
	hd := data.NewAlpacaOptionsHist(apiKey, secret, stocks.DataURL, stocks.BaseURL)

	rcfg := optionsBacktestConfig(cfg)

	fmt.Printf("options-ORB backtest · %s → %s · %s\n\n",
		btOptFrom, btOptTo, strings.Join(underlyings, ","))
	fmt.Println("Seeding IV history and assembling breakouts (this fetches historical option data)…")

	runner, err := optionsreplay.NewRunner(ctx, hd, underlyings, from, to, rcfg, nil)
	if err != nil {
		return fmt.Errorf("build runner: %w", err)
	}

	overall := make([]optionsreplay.SimTrade, 0)
	for _, u := range underlyings {
		trades, err := runner.RunUnderlying(ctx, u, from, to)
		if err != nil {
			return fmt.Errorf("run %s: %w", u, err)
		}
		printOptionsReport(u, optionsreplay.Aggregate(trades))
		overall = append(overall, trades...)
	}
	if len(underlyings) > 1 {
		printOptionsReport("ALL", optionsreplay.Aggregate(overall))
	}
	return nil
}

// optionsBacktestConfig maps the live options_orb + equities ORB config onto the
// backtest runner config, so a backtest is driven by the SAME knobs production is.
func optionsBacktestConfig(cfg *config.Config) optionsreplay.RunnerConfig {
	o := cfg.OptionsORB
	return optionsreplay.RunnerConfig{
		Gate: options.GateConfig{
			IVRankCeiling:        o.IVRankCeiling,
			MaxDTE:               o.MaxDTE,
			DeltaMin:             o.DeltaMin,
			DeltaMax:             o.DeltaMax,
			SpreadPctCeiling:     o.SpreadPctCeiling,
			EarningsBlackoutDays: o.EarningsBlackoutDays,
			ETFUnderlyings:       o.ETFUnderlyings,
		},
		Replay: optionsreplay.Config{
			PremiumBudget:   o.PremiumBudget,
			MaxLossPerTrade: o.MaxLossPerTrade,
			TimeExitMinutes: o.TimeExitMinutes,
		},
		Assemble: optionsreplay.AssembleConfig{
			RangeMinutes:    cfg.ORB.RangeMinutes,
			EntryWindowMins: 60,
			SessionOpenET:   "09:30",
			SessionEndET:    "15:50", // matches the live options flatten cutoff
			RiskReward:      cfg.ORB.RiskReward,
		},
		IVLookbackDays: o.IVLookbackDays,
		IVMinSamples:   o.IVMinSamples,
	}
}

func printOptionsReport(scope string, r optionsreplay.Report) {
	fmt.Printf("\n── %s ──────────────────────────────────\n", scope)
	fmt.Printf("  Signals:    %d\n", r.Total)
	fmt.Printf("  Accepted:   %d\n", r.Accepted)
	fmt.Printf("  Wins/Loss:  %d / %d  (scratch %d)\n", r.Wins, r.Losses, r.Scratches)
	fmt.Printf("  Win rate:   %.1f%%  (priced, resolved trades only)\n", r.WinRate()*100)
	fmt.Printf("  Net P&L:    $%.2f\n", r.TotalPnL)
	if r.Unpriced > 0 || r.Unresolved > 0 {
		fmt.Printf("  Excluded:   %d unpriced (missing quote), %d unresolved (data ran out) — NOT in win rate/P&L\n",
			r.Unpriced, r.Unresolved)
	}
	if len(r.ExitsByReason) > 0 {
		fmt.Printf("  Exits:      %s\n", sortedCounts(r.ExitsByReason))
	}
	if len(r.AbortsByGate) > 0 {
		fmt.Printf("  Aborts:     %s\n", sortedCounts(r.AbortsByGate))
	}
}

// sortedCounts renders a count map as "key=n key=n" in deterministic key order.
func sortedCounts(m map[string]int) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", k, m[k]))
	}
	return strings.Join(parts, "  ")
}
