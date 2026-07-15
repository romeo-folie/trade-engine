package main

import (
	"context"
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
	btORBAssetClass  string
	btORBDukaPaceMs  int
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
		c.Flags().StringVar(&btORBSession, "session", "", "Session: london | newyork | newyork_et (DST-aware US RTH; default: config forex_orb.session, or newyork_et for --asset-class futures)")
		c.Flags().StringVar(&btORBInstruments, "instruments", "", "Comma-separated symbols (default: all configured)")
		c.Flags().Float64Var(&btORBCapital, "capital", 100000, "Initial capital for position sizing")
		c.Flags().IntVar(&btORBMaxHold, "max-hold", 240, "Max-hold minutes for time-exit/range-filter variants")
		c.Flags().Float64Var(&btORBMinRangeATR, "min-range-atr", 0, "Override min range_atr_ratio for all instruments (range-filter)")
		c.Flags().Float64Var(&btORBMaxRangeATR, "max-range-atr", 0, "Override max range_atr_ratio for all instruments (range-filter)")
		c.Flags().StringVar(&btORBProvider, "provider", "", "Data provider: histdata (default) | dukascopy | oanda (default: config markets.backtest_provider)")
		c.Flags().StringVar(&btORBAssetClass, "asset-class", "forex", "Instrument universe: forex (forex_orb.instruments) | futures (futures_orb.instruments via free index-CFD proxy data)")
		c.Flags().IntVar(&btORBDukaPaceMs, "duka-pace-ms", 0, "Dukascopy politeness: >0 forces 1 in-flight fetch spaced this many ms apart (overrides config). Use 3000-5000 for a throttled cold IP; the disk cache makes the fill resumable, so re-run until it completes.")
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

	uni, err := buildBacktestUniverse(cfg)
	if err != nil {
		return nil, err
	}
	runCfg := uni.base
	if btORBSession != "" {
		runCfg.Session = btORBSession
	}
	if runCfg.Session == "" {
		runCfg.Session = "london"
	}

	engine := orbreplay.NewEngine(runCfg, uni.provider, uni.costs, 0)

	return &orbBacktestSetup{engine: engine, cfg: runCfg, from: from, to: to, instruments: uni.instruments}, nil
}

// backtestUniverse bundles everything one --asset-class contributes to a backtest or
// sweep: the base strategy config, the resolved instrument set, per-symbol cost models,
// the (possibly proxy-wrapped) data provider, and the sessions a sweep may vary over.
type backtestUniverse struct {
	base        config.ForexORBConfig
	instruments []config.Instrument
	costs       map[string]orbreplay.CostModel
	provider    data.MarketDataProvider
	sessions    []string
}

// buildBacktestUniverse resolves the --asset-class into a concrete backtest universe.
// The CLI range_atr_ratio overrides apply to every asset class identically.
func buildBacktestUniverse(cfg *config.Config) (*backtestUniverse, error) {
	provider, err := buildBacktestProvider(cfg)
	if err != nil {
		return nil, err
	}
	var uni *backtestUniverse
	switch btORBAssetClass {
	case "", "forex":
		instruments, err := resolveInstruments(cfg.ForexORB, btORBInstruments)
		if err != nil {
			return nil, err
		}
		uni = &backtestUniverse{
			base: cfg.ForexORB, instruments: instruments, costs: buildCostModels(instruments),
			provider: provider, sessions: []string{"london", "newyork"},
		}
	case "futures":
		uni, err = buildFuturesUniverse(cfg.FuturesORB, provider, resolveBacktestProviderName(cfg))
		if err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("unknown --asset-class %q (use forex|futures)", btORBAssetClass)
	}
	// CLI overrides apply the same range_atr_ratio bounds to every instrument, so the
	// range-filter variant can be swept without editing per-instrument config.
	for i := range uni.instruments {
		if btORBMinRangeATR > 0 {
			uni.instruments[i].MinRangeATR = btORBMinRangeATR
		}
		if btORBMaxRangeATR > 0 {
			uni.instruments[i].MaxRangeATR = btORBMaxRangeATR
		}
	}
	return uni, nil
}

// ── futures universe (index micros over free CFD proxy data) ─────────────────

// futuresProxiesByProvider maps each micro contract to the free index-CFD price series
// used as its backtest proxy, PER DATA PROVIDER (vendors name the same index differently
// and carry different subsets). HistData's full tick catalogue has exactly two US index
// CFDs — SPXUSD and NSXUSD (verified 2026-07: no Dow, no Russell) — while Dukascopy
// carries all four. A contract without a proxy on the selected provider is skipped with a
// note. A cash-index level tracks the future closely enough for RULE research — basis
// shifts the price LEVEL, not the size of a point — but fills are approximate;
// contract-accurate research needs an exchange-data source. Running the same sweep on
// both providers is also a cheap cross-vendor check against data artifacts.
var futuresProxiesByProvider = map[string]map[string]string{
	"histdata": {
		"MES": "SPXUSD", // S&P 500
		"MNQ": "NSXUSD", // Nasdaq 100
	},
	"dukascopy": {
		"MES": "USA500IDXUSD",   // S&P 500
		"MNQ": "USATECHIDXUSD",  // Nasdaq 100
		"MYM": "USA30IDXUSD",    // Dow Jones Industrial Average
		"M2K": "USSC2000IDXUSD", // Russell 2000
	},
}

// Futures cost assumptions. Commission is IBKR's all-in micro E-mini rate per contract
// per side (≈ $0.62–0.85 with exchange fees; the conservative end is used). Spread is
// modeled at 2 ticks — the touch is usually 1 tick, but the proxy series is a mid-price
// index level, so the extra tick absorbs proxy-vs-contract slippage.
const (
	futuresCommissionPerContractSide = 0.85
	futuresSpreadTicks               = 2.0
)

func buildFuturesUniverse(fcfg config.FuturesORBConfig, provider data.MarketDataProvider, providerName string) (*backtestUniverse, error) {
	if len(fcfg.Instruments) == 0 {
		return nil, fmt.Errorf("no futures_orb instruments configured")
	}
	providerProxies, ok := futuresProxiesByProvider[providerName]
	if !ok {
		return nil, fmt.Errorf("--asset-class futures has no index-CFD proxy mapping for provider %q (use histdata or dukascopy)", providerName)
	}
	available := make([]string, 0, len(providerProxies))
	for _, fi := range fcfg.Instruments {
		if _, ok := providerProxies[fi.Symbol]; ok {
			available = append(available, fi.Symbol)
		}
	}
	filter := splitCSV(btORBInstruments)
	want := make(map[string]bool, len(filter))
	for _, sym := range filter {
		want[sym] = true
	}
	matched := make(map[string]bool, len(want))

	var instruments []config.Instrument
	costs := make(map[string]orbreplay.CostModel)
	proxies := make(map[string]string)
	for _, fi := range fcfg.Instruments {
		if len(want) > 0 && !want[fi.Symbol] {
			continue
		}
		matched[fi.Symbol] = true
		proxy, ok := providerProxies[fi.Symbol]
		if !ok {
			fmt.Printf("NOTE: skipping %s — no index-CFD proxy series on %s (available: %s)\n",
				fi.Symbol, providerName, strings.Join(available, ", "))
			continue
		}
		if fi.TickSize <= 0 || fi.TickValue <= 0 {
			return nil, fmt.Errorf("futures instrument %s needs tick_size and tick_value", fi.Symbol)
		}
		proxies[fi.Symbol] = proxy
		instruments = append(instruments, config.Instrument{
			Symbol:     fi.Symbol,
			SecType:    "FUT",
			MinLotSize: 1, // whole contracts
			PipBuffer:  float64(fi.TickBuffer) * fi.TickSize,
			MarginRate: fi.MarginRate,
			PointValue: fi.TickValue / fi.TickSize, // $/point/contract: MES 5, MNQ 2, MYM 0.5
		})
		costs[fi.Symbol] = orbreplay.CostModel{
			Spread:            futuresSpreadTicks * fi.TickSize,
			CommissionPerUnit: futuresCommissionPerContractSide,
		}
	}
	for sym := range want {
		if !matched[sym] {
			return nil, fmt.Errorf("instrument %q is not configured under futures_orb.instruments", sym)
		}
	}
	if len(instruments) == 0 {
		return nil, fmt.Errorf("no futures instrument has a proxy data series on %s (available: %s)", providerName, strings.Join(available, ", "))
	}

	// Base strategy knobs come from futures_orb; ATR/trend timeframes fall back to the
	// engine's 1h defaults, same as forex. The session is the DST-aware US RTH open —
	// index futures ORB is an RTH-open strategy, so it is the only session swept.
	base := config.ForexORBConfig{
		Session:        "newyork_et",
		RangeMinutes:   fcfg.RangeMinutes,
		RiskReward:     fcfg.RiskReward,
		MaxRiskPct:     fcfg.MaxRiskPct,
		MaxPositionPct: fcfg.MaxPositionPct,
	}
	return &backtestUniverse{
		base: base, instruments: instruments, costs: costs,
		provider: proxySymbolProvider{MarketDataProvider: provider, proxies: proxies},
		sessions: []string{"newyork_et"},
	}, nil
}

// proxySymbolProvider translates instrument symbols to their proxy data series on fetch,
// so results, sweep rows and logs stay keyed by the real contract (MES) while the candles
// come from the CFD series (SPXUSD). Unmapped symbols pass through untouched.
type proxySymbolProvider struct {
	data.MarketDataProvider
	proxies map[string]string
}

func (p proxySymbolProvider) FetchCandles(ctx context.Context, ticker string, tf data.Timeframe, from, to time.Time) ([]data.Candle, error) {
	if mapped, ok := p.proxies[ticker]; ok {
		ticker = mapped
	}
	return p.MarketDataProvider.FetchCandles(ctx, ticker, tf, from, to)
}

// resolveBacktestProviderName is the effective data-source name for this run: the
// --provider flag, else config markets.backtest_provider, else histdata.
func resolveBacktestProviderName(cfg *config.Config) string {
	prov := btORBProvider
	if prov == "" {
		prov = cfg.Markets.BacktestProvider
	}
	return strings.ToLower(cmpOr(prov, "histdata"))
}

// buildBacktestProvider selects the ORB backtest data source. HistData is the default: free
// bulk tick ZIPs with no account, API key, or rate limit. The --provider flag overrides config.
func buildBacktestProvider(cfg *config.Config) (data.MarketDataProvider, error) {
	switch resolveBacktestProviderName(cfg) {
	case "histdata":
		return data.NewHistDataClient(cfg.Markets.HistData.BaseURL, resolveBacktestCacheDir(cfg.Markets.HistData.CacheDir, "histdata-cache"))
	case "dukascopy":
		opts := data.DukascopyOptions{
			FetchConcurrency:   cfg.Markets.Dukascopy.FetchConcurrency,
			MinRequestInterval: time.Duration(cfg.Markets.Dukascopy.MinRequestIntervalMs) * time.Millisecond,
		}
		if btORBDukaPaceMs > 0 { // --duka-pace-ms: the polite, resumable-fill profile, overrides config
			opts.FetchConcurrency = 1
			opts.MinRequestInterval = time.Duration(btORBDukaPaceMs) * time.Millisecond
			opts.RequestTimeout = 60 * time.Second // the feed tarpits bulk fetchers to ~18s; be patient
		}
		return data.NewDukascopyClient(cfg.Markets.Dukascopy.BaseURL, resolveBacktestCacheDir(cfg.Markets.Dukascopy.CacheDir, "dukascopy-cache"), cfg.Markets.Dukascopy.PointScale, opts), nil
	case "oanda":
		return buildOANDAProvider(cfg.Markets.OANDA)
	default:
		return nil, fmt.Errorf("unknown backtest data provider %q (use histdata|dukascopy|oanda)", resolveBacktestProviderName(cfg))
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
