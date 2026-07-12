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
	"github.com/romeo-folie/trade-engine/internal/store"
	"github.com/spf13/cobra"
)

var (
	btOptFrom        string
	btOptTo          string
	btOptUnderlyings string
	btOptSpreadPct   float64
	btOptETFs        string
	btOptPremium     float64
	btOptMaxLoss     float64
)

func init() {
	orbOptionsCmd.Flags().StringVar(&btOptFrom, "from", "", "Start date YYYY-MM-DD (required)")
	orbOptionsCmd.Flags().StringVar(&btOptTo, "to", "", "End date YYYY-MM-DD (required)")
	orbOptionsCmd.Flags().StringVar(&btOptUnderlyings, "underlyings", "", "Comma-separated underlyings (default: options_orb.underlyings from config)")
	orbOptionsCmd.Flags().Float64Var(&btOptSpreadPct, "modeled-spread-pct", 0.03, "Modeled bid/ask width as a fraction of mid (free bars-data has no real NBBO). Brackets the last-trade price for the spread gate and costs the exit (market sell → bid). 0.03 = 3%, conservative")
	orbOptionsCmd.Flags().StringVar(&btOptETFs, "etf-underlyings", "SPY,QQQ", "Comma-separated earnings-exempt ETFs. Single names hit the earnings gate, which fails closed with no earnings feed, so the backtest is ETF-only")
	orbOptionsCmd.Flags().Float64Var(&btOptPremium, "premium-budget", 1000, "Dollars of premium to deploy per trade (sizing). options_orb.premium_budget if 0")
	orbOptionsCmd.Flags().Float64Var(&btOptMaxLoss, "max-loss-per-trade", 0, "Hard cap on premium at risk per trade (0 = no extra cap beyond the budget)")
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

DATA: runs on Alpaca's FREE historical option BARS (no OPRA/Algo Trader Plus needed).
Since the basic plan has no historical NBBO quotes, the bid/ask is reconstructed from
the last-trade price (1-minute bar close) bracketed by --modeled-spread-pct, and the
exit models a market-sell receiving the bid. The spread gate therefore runs on a
MODELED spread, not real NBBO — a deliberate free-data trade-off (see
docs/options-orb-backtest.md). Alpaca option history only reaches back to ~Feb 2024;
earlier windows return empty and HTTP errors are logged, never fabricated.`,
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
	hd := data.NewAlpacaOptionsHist(apiKey, secret, stocks.DataURL, stocks.BaseURL, btOptSpreadPct)

	// Persist the IV seed to option_iv_history (write-through): the first run seeds the
	// window, later runs read it back and fetch nothing. Shared with the live gate.
	if err := store.Connect(ctx); err != nil {
		return fmt.Errorf("connect DB (for the persisted IV seed): %w", err)
	}
	ivstore := options.NewStoreIVStore(store.GetDB())

	rcfg := optionsBacktestConfig(cfg)
	rcfg.Replay.ModeledSpreadPct = btOptSpreadPct // exit models a market-sell receiving the bid
	// ETF underlyings are earnings-exempt. Single names hit the earnings gate, which —
	// with no earnings provider wired for the backtest — fails CLOSED, so the backtest
	// is effectively ETF-only. Mark the configured/flagged ETFs so SPY/QQQ aren't
	// rejected as unknown single names. (Union with any config value.)
	etfs := append([]string(nil), cfg.OptionsORB.ETFUnderlyings...)
	for _, e := range splitCSV(btOptETFs) {
		etfs = append(etfs, strings.ToUpper(e))
	}
	rcfg.Gate.ETFUnderlyings = etfs

	// Sizing budget: config is often unset for the backtest, and a 0 budget aborts
	// every trade at sizing. Fall back to the flag default.
	if btOptPremium > 0 {
		rcfg.Replay.PremiumBudget = btOptPremium
	}
	if btOptMaxLoss > 0 {
		rcfg.Replay.MaxLossPerTrade = btOptMaxLoss
	}

	// Warn on the modeled-spread footgun: the spread gate aborts when spread% ≥ ceiling,
	// so a modeled spread at/above the ceiling rejects every contract.
	ceiling := rcfg.Gate.SpreadPctCeiling
	if ceiling <= 0 {
		ceiling = 5 // options.DefaultGateConfig ceiling, applied in NewEngine
	}
	if btOptSpreadPct*100 >= ceiling {
		fmt.Printf("WARNING: --modeled-spread-pct %.3f (%.1f%%) ≥ the %.1f%% spread-gate ceiling — every contract will abort at 'spread'. Use a smaller value.\n",
			btOptSpreadPct, btOptSpreadPct*100, ceiling)
	}

	fmt.Printf("options-ORB backtest · %s → %s · %s\n\n",
		btOptFrom, btOptTo, strings.Join(underlyings, ","))
	fmt.Println("Seeding IV history and assembling breakouts (this fetches historical option data)…")

	runner, err := optionsreplay.NewRunner(ctx, hd, ivstore, underlyings, from, to, rcfg, nil)
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
