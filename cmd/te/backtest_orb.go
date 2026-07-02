package main

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/romeo-folie/trade-engine/internal/backtest/orbreplay"
	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/data"
	"github.com/romeo-folie/trade-engine/internal/logger"
	"github.com/spf13/cobra"
)

// ── Flags ─────────────────────────────────────────────────────────────────────

var (
	btORBFrom        string
	btORBTo          string
	btORBSession     string
	btORBInstruments string
	btORBVariant     string
	btORBVariants    []string
	btORBCapital     float64
	btORBMaxHold     int
	btORBMinRangeATR float64
	btORBMaxRangeATR float64
	btORBProvider    string
)

// defaultSpreads are conservative per-instrument bid/ask spreads (price units) used by
// the backtest cost model when nothing more specific is configured. Commissions and
// spread dominate ORB economics (PR #359), so these must never be zero.
var defaultSpreads = map[string]float64{
	"EUR/USD": 0.00008,
	"GBP/USD": 0.00010,
	"XAU/USD": 0.30,
	"XAG/USD": 0.02,
}

const orbCommissionPerSide = 2.0 // $ per fill, ≈ the broker-statement round-trip of ~$4

func init() {
	for _, c := range []*cobra.Command{orbBTCmd, orbCompareBTCmd} {
		c.Flags().StringVar(&btORBFrom, "from", "", "Start date YYYY-MM-DD (required)")
		c.Flags().StringVar(&btORBTo, "to", "", "End date YYYY-MM-DD (required)")
		c.Flags().StringVar(&btORBSession, "session", "", "Session: london | newyork (default: config forex_orb.session)")
		c.Flags().StringVar(&btORBInstruments, "instruments", "", "Comma-separated symbols (default: all configured)")
		c.Flags().Float64Var(&btORBCapital, "capital", 100000, "Initial capital for position sizing")
		c.Flags().IntVar(&btORBMaxHold, "max-hold", 240, "Max-hold minutes for time-exit/range-filter variants")
		c.Flags().Float64Var(&btORBMinRangeATR, "min-range-atr", 0, "Override min range_atr_ratio for all instruments (range-filter)")
		c.Flags().Float64Var(&btORBMaxRangeATR, "max-range-atr", 0, "Override max range_atr_ratio for all instruments (range-filter)")
		c.Flags().StringVar(&btORBProvider, "provider", "", "Data provider: histdata (default) | dukascopy | oanda (default: config markets.backtest_provider)")
		_ = c.MarkFlagRequired("from")
		_ = c.MarkFlagRequired("to")
	}
	orbBTCmd.Flags().StringVar(&btORBVariant, "variant", "baseline", "Rule variant: baseline | time-exit | range-filter | confirmed | adaptive | retest")
	orbCompareBTCmd.Flags().StringArrayVar(&btORBVariants, "variants", []string{"baseline", "time-exit"}, "Variants to compare (repeat or comma-separate)")

	backtestCmd.AddCommand(orbBTCmd, orbCompareBTCmd)
}

// ── te backtest orb ───────────────────────────────────────────────────────────

var orbBTCmd = &cobra.Command{
	Use:   "orb",
	Short: "Replay the forex/CFD ORB strategy over historical intraday data",
	Example: `  te backtest orb --from 2026-04-01 --to 2026-06-30 --variant baseline
  te backtest orb --from 2026-04-01 --to 2026-06-30 --variant time-exit --max-hold 90`,
	RunE: func(cmd *cobra.Command, args []string) error {
		setup, err := loadORBBacktest()
		if err != nil {
			return err
		}
		v, err := resolveVariant(btORBVariant, btORBMaxHold)
		if err != nil {
			return err
		}
		trades, err := setup.engine.Run(cmd.Context(), v, setup.from, setup.to, setup.instruments, btORBCapital)
		if err != nil {
			return err
		}
		printORBResult(orbreplay.Summarize(v.Name, trades), setup)
		return nil
	},
}

// ── te backtest orb-compare ───────────────────────────────────────────────────

var orbCompareBTCmd = &cobra.Command{
	Use:     "orb-compare",
	Short:   "Compare ORB rule variants over the same window",
	Example: `  te backtest orb-compare --from 2026-04-01 --to 2026-06-30 --variants baseline,time-exit`,
	RunE: func(cmd *cobra.Command, args []string) error {
		setup, err := loadORBBacktest()
		if err != nil {
			return err
		}
		names := splitCSV(strings.Join(btORBVariants, ","))
		fmt.Printf("ORB backtest comparison · %s · %s -> %s\n\n",
			setup.cfg.Session, btORBFrom, btORBTo)
		fmt.Printf("%-12s %7s %6s %6s %8s %11s %10s\n",
			"VARIANT", "TRADES", "WIN%", "PF", "EXP_R", "NET_PNL", "OVERNIGHT")
		for _, name := range names {
			v, err := resolveVariant(name, btORBMaxHold)
			if err != nil {
				return err
			}
			trades, err := setup.engine.Run(cmd.Context(), v, setup.from, setup.to, setup.instruments, btORBCapital)
			if err != nil {
				return err
			}
			m := orbreplay.Summarize(v.Name, trades).Overall
			fmt.Printf("%-12s %7d %5.1f%% %6s %+8.2f %11.2f %10d\n",
				v.Name, m.Trades, m.WinRate*100, fmtPF(m.ProfitFactor), m.ExpectancyR, m.NetPnL, m.OvernightHold)
		}
		return nil
	},
}

// ── shared setup ──────────────────────────────────────────────────────────────

type orbBacktestSetup struct {
	engine      *orbreplay.Engine
	cfg         config.ForexORBConfig
	from, to    time.Time
	instruments []config.Instrument
}

func loadORBBacktest() (*orbBacktestSetup, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	if err := logger.Init(cfg.Logging.Level, cfg.Logging.Format); err != nil {
		return nil, err
	}

	from, err := time.Parse("2006-01-02", btORBFrom)
	if err != nil {
		return nil, fmt.Errorf("invalid --from date %q: %w", btORBFrom, err)
	}
	to, err := time.Parse("2006-01-02", btORBTo)
	if err != nil {
		return nil, fmt.Errorf("invalid --to date %q: %w", btORBTo, err)
	}
	if to.Before(from) {
		return nil, fmt.Errorf("--to must not be before --from")
	}
	to = to.Add(24*time.Hour - time.Second) // include the last day's session

	forexCfg := cfg.ForexORB
	if btORBSession != "" {
		forexCfg.Session = btORBSession
	}
	if forexCfg.Session == "" {
		forexCfg.Session = "london"
	}

	instruments, err := resolveInstruments(forexCfg, btORBInstruments)
	if err != nil {
		return nil, err
	}

	provider, err := buildBacktestProvider(cfg)
	if err != nil {
		return nil, err
	}

	costs := buildCostModels(instruments)
	engine := orbreplay.NewEngine(forexCfg, provider, costs, 0)

	return &orbBacktestSetup{engine: engine, cfg: forexCfg, from: from, to: to, instruments: instruments}, nil
}

// buildBacktestProvider selects the ORB backtest data source. HistData is the default: free
// bulk tick ZIPs with no account, API key, or rate limit. The --provider flag overrides config.
func buildBacktestProvider(cfg *config.Config) (data.MarketDataProvider, error) {
	prov := btORBProvider
	if prov == "" {
		prov = cfg.Markets.BacktestProvider
	}
	switch strings.ToLower(cmpOr(prov, "histdata")) {
	case "histdata":
		return data.NewHistDataClient(cfg.Markets.HistData.BaseURL, resolveBacktestCacheDir(cfg.Markets.HistData.CacheDir, "histdata-cache"))
	case "dukascopy":
		return data.NewDukascopyClient(cfg.Markets.Dukascopy.BaseURL, resolveBacktestCacheDir(cfg.Markets.Dukascopy.CacheDir, "dukascopy-cache"), cfg.Markets.Dukascopy.PointScale), nil
	case "oanda":
		return buildOANDAProvider(cfg.Markets.OANDA)
	default:
		return nil, fmt.Errorf("unknown backtest data provider %q (use histdata|dukascopy|oanda)", prov)
	}
}

// resolveBacktestCacheDir maps a config cache_dir value to an actual path. An unset value
// defaults to ~/.tradeengine/<defaultSubdir> so the cache is on by default (fetch-once, then
// serve from disk). "off"/"none"/"disabled" (case-insensitive) turns the cache off.
func resolveBacktestCacheDir(configured, defaultSubdir string) string {
	switch strings.ToLower(strings.TrimSpace(configured)) {
	case "off", "none", "disabled":
		return ""
	case "":
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			return "" // no home → cache disabled rather than writing to an unknown location
		}
		return filepath.Join(home, ".tradeengine", defaultSubdir)
	default:
		return configured
	}
}

func buildOANDAProvider(oa config.OANDAConfig) (data.MarketDataProvider, error) {
	token := oa.Token
	if token == "" {
		token = os.Getenv("OANDA_API_TOKEN")
	}
	if token == "" {
		return nil, fmt.Errorf("OANDA API token not set — create a free OANDA practice account, then set %s (or markets.oanda.token_env)",
			cmpOr(oa.TokenEnv, "OANDA_API_TOKEN"))
	}
	base := oa.BaseURL
	if base == "" {
		base = data.OANDAPracticeURL
		if !oa.Practice {
			base = data.OANDALiveURL
		}
	}
	return data.NewOANDAClient(token, oa.AccountID, base), nil
}

func resolveInstruments(forexCfg config.ForexORBConfig, csv string) ([]config.Instrument, error) {
	if len(forexCfg.Instruments) == 0 {
		return nil, fmt.Errorf("no forex_orb instruments configured")
	}
	var out []config.Instrument
	if csv == "" {
		out = append(out, forexCfg.Instruments...)
	} else {
		bySymbol := make(map[string]config.Instrument, len(forexCfg.Instruments))
		for _, inst := range forexCfg.Instruments {
			bySymbol[inst.Symbol] = inst
		}
		for _, sym := range splitCSV(csv) {
			inst, ok := bySymbol[sym]
			if !ok {
				return nil, fmt.Errorf("instrument %q is not configured under forex_orb.instruments", sym)
			}
			out = append(out, inst)
		}
	}
	// CLI overrides apply the same range_atr_ratio bounds to every instrument, so the
	// range-filter variant can be swept without editing per-instrument config.
	for i := range out {
		if btORBMinRangeATR > 0 {
			out[i].MinRangeATR = btORBMinRangeATR
		}
		if btORBMaxRangeATR > 0 {
			out[i].MaxRangeATR = btORBMaxRangeATR
		}
	}
	return out, nil
}

func buildCostModels(instruments []config.Instrument) map[string]orbreplay.CostModel {
	costs := make(map[string]orbreplay.CostModel, len(instruments))
	for _, inst := range instruments {
		spread, ok := defaultSpreads[inst.Symbol]
		if !ok {
			spread = inst.PipBuffer // sensible fallback for an unlisted instrument
		}
		// Per-instrument commission_per_side overrides the flat default so the replay charges
		// (and its commission/risk gate measures) the same estimate the live gate uses.
		commission := orbCommissionPerSide
		if inst.CommissionPerSide > 0 {
			commission = inst.CommissionPerSide
		}
		costs[inst.Symbol] = orbreplay.CostModel{Spread: spread, CommissionPerSide: commission}
	}
	return costs
}

func resolveVariant(name string, maxHold int) (orbreplay.Variant, error) {
	switch name {
	case "baseline":
		return orbreplay.Baseline(), nil
	case "time-exit":
		return orbreplay.TimeExit(maxHold), nil
	case "range-filter":
		return orbreplay.RangeFilter(maxHold), nil
	case "confirmed":
		return orbreplay.Confirmed(maxHold), nil
	case "adaptive":
		return orbreplay.Adaptive(maxHold), nil
	case "retest":
		return orbreplay.Retest(maxHold), nil
	default:
		return orbreplay.Variant{}, fmt.Errorf("unknown variant %q (known: baseline, time-exit, range-filter, confirmed, adaptive, retest)", name)
	}
}

func printORBResult(r orbreplay.Result, setup *orbBacktestSetup) {
	m := r.Overall
	fmt.Printf("ORB backtest: %s · %s · %s -> %s\n\n", r.Variant, setup.cfg.Session, btORBFrom, btORBTo)
	if m.Trades == 0 {
		fmt.Println("No trades simulated (no breakouts, or no candle data for the window).")
		return
	}
	fmt.Printf("Trades:          %d\n", m.Trades)
	fmt.Printf("Win rate:        %.1f%% (%d W / %d L)\n", m.WinRate*100, m.Wins, m.Losses)
	fmt.Printf("Profit factor:   %s\n", fmtPF(m.ProfitFactor))
	fmt.Printf("Expectancy:      %+.2fR/trade\n", m.ExpectancyR)
	fmt.Printf("Avg winner:      %+.2fR\n", m.AvgWinnerR)
	fmt.Printf("Avg loser:       %+.2fR\n", m.AvgLoserR)
	fmt.Printf("Max loss:        %+.2fR\n", m.MaxLossR)
	fmt.Printf("Avg hold:        %s\n", m.AvgHold.Round(time.Minute))
	fmt.Printf("Overnight holds: %d\n", m.OvernightHold)
	fmt.Printf("Net PnL:         %+.2f  (gross %+.2f, commission %.2f)\n", m.NetPnL, m.GrossPnL, m.Commission)
	fmt.Printf("Exits:           %s\n", exitTypeSummary(m.ByExitType))

	fmt.Println("\nBy instrument:")
	for _, sym := range r.SortedInstruments() {
		im := r.ByInstrument[sym]
		fmt.Printf("  %-8s trades=%-3d win=%4.1f%% pf=%s exp=%+.2fR net=%+.2f\n",
			sym, im.Trades, im.WinRate*100, fmtPF(im.ProfitFactor), im.ExpectancyR, im.NetPnL)
	}
}

func exitTypeSummary(byType map[string]int) string {
	var parts []string
	for _, k := range []string{"tp", "stop", "time_exit", "end_of_data"} {
		if n := byType[k]; n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, k))
		}
	}
	if len(parts) == 0 {
		return "—"
	}
	return strings.Join(parts, ", ")
}

// fmtPF renders a profit factor, showing ∞ for the all-winners sentinel and — when
// there were no decided trades at all (PF == 0 with no data).
func fmtPF(pf float64) string {
	switch {
	case math.IsInf(pf, 1):
		return "∞"
	case pf == 0:
		return "—"
	default:
		return fmt.Sprintf("%.2f", pf)
	}
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
