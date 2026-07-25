package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/data"
	"github.com/romeo-folie/trade-engine/internal/logger"
	"github.com/romeo-folie/trade-engine/internal/store"
	"github.com/romeo-folie/trade-engine/internal/strategy/pullback"
	"github.com/romeo-folie/trade-engine/internal/study/pullbackfreq"
	"github.com/spf13/cobra"
)

var (
	pullbackFreqFrom              string
	pullbackFreqTo                string
	pullbackFreqTickers           string
	pullbackFreqActiveOnly        bool
	pullbackFreqRespectMembership bool
	pullbackFreqMinDollarVolume   float64
	pullbackFreqTopFraction       float64
	pullbackFreqEpisodeGap        int
	pullbackFreqCSV               string
)

func init() {
	pullbackFrequencyCmd.Flags().StringVar(&pullbackFreqFrom, "from", "", "Study start date YYYY-MM-DD (default: two years before --to)")
	pullbackFrequencyCmd.Flags().StringVar(&pullbackFreqTo, "to", "", "Study end date YYYY-MM-DD (default: today)")
	pullbackFrequencyCmd.Flags().StringVar(&pullbackFreqTickers, "tickers", "", "Comma-separated fixed cohort (default: recorded stock watchlist)")
	pullbackFrequencyCmd.Flags().BoolVar(&pullbackFreqActiveOnly, "active-only", false, "Use only currently active watchlist tickers")
	pullbackFrequencyCmd.Flags().BoolVar(&pullbackFreqRespectMembership, "respect-membership-dates", false, "Restrict watchlist tickers to their stored created/expiry dates")
	pullbackFrequencyCmd.Flags().Float64Var(&pullbackFreqMinDollarVolume, "min-dollar-volume", 1_000_000, "Minimum daily close×volume")
	pullbackFrequencyCmd.Flags().Float64Var(&pullbackFreqTopFraction, "top-fraction", 0.25, "Retain this fraction of momentum-qualified cohort each day")
	pullbackFrequencyCmd.Flags().IntVar(&pullbackFreqEpisodeGap, "episode-gap-sessions", 5, "Group eligible days separated by at most this many sessions into one setup episode")
	pullbackFrequencyCmd.Flags().StringVar(&pullbackFreqCSV, "csv", "", "Write distinct setup episodes to this CSV file")
	studyCmd.AddCommand(pullbackFrequencyCmd)
}

var pullbackFrequencyCmd = &cobra.Command{
	Use:   "pullback-frequency",
	Short: "Measure deterministic pullback setup frequency without LLM calls",
	Long: `Builds a daily momentum/liquidity cohort from supplied or recorded
watchlist tickers, ranks the qualifying names point-in-time within that cohort,
and applies the complete pullback-in-strength evaluator using candle prefixes
only. No LLM analysis is invoked and no trades or engine state are changed.

The default recorded-watchlist cohort is exploratory: the database does not
retain every historical scanner candidate considered, so it is not a
survivorship-free reconstruction of the live scanner universe.`,
	RunE: runPullbackFrequencyStudy,
}

type refreshingDailyLoader struct {
	provider data.MarketDataProvider
}

func (l refreshingDailyLoader) FetchCandles(
	ctx context.Context, ticker string, tf data.Timeframe, from, to time.Time,
) ([]data.Candle, error) {
	candles, err := l.provider.FetchCandles(ctx, ticker, tf, from, to)
	if err != nil {
		return nil, err
	}
	if len(candles) == 0 {
		return nil, nil
	}
	if err := store.UpsertCandles(ctx, candles); err != nil {
		return nil, fmt.Errorf("cache refreshed candles: %w", err)
	}
	return candles, nil
}

func runPullbackFrequencyStudy(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if err := logger.Init(cfg.Logging.Level, cfg.Logging.Format); err != nil {
		return err
	}
	to := pullbackFrequencyDay(time.Now())
	if pullbackFreqTo != "" {
		to, err = time.Parse("2006-01-02", pullbackFreqTo)
		if err != nil {
			return fmt.Errorf("invalid --to date %q: %w", pullbackFreqTo, err)
		}
	}
	from := to.AddDate(-2, 0, 0)
	if pullbackFreqFrom != "" {
		from, err = time.Parse("2006-01-02", pullbackFreqFrom)
		if err != nil {
			return fmt.Errorf("invalid --from date %q: %w", pullbackFreqFrom, err)
		}
	}
	if to.Before(from) {
		return fmt.Errorf("--to must be on or after --from")
	}
	if pullbackFreqTopFraction <= 0 || pullbackFreqTopFraction > 1 {
		return fmt.Errorf("--top-fraction must be in (0,1]")
	}
	if err := store.Connect(ctx); err != nil {
		return fmt.Errorf("connect DB: %w", err)
	}
	defer store.Close()

	members, cohortLabel, err := pullbackFrequencyMembers(ctx)
	if err != nil {
		return err
	}
	if len(members) == 0 {
		return fmt.Errorf("pullback frequency: cohort is empty")
	}

	stocks := cfg.Markets.Stocks
	apiKey, secret := os.Getenv(stocks.APIKeyEnv), os.Getenv(stocks.SecretEnv)
	if apiKey == "" || secret == "" {
		return fmt.Errorf("Alpaca credentials unset: export %s and %s", stocks.APIKeyEnv, stocks.SecretEnv)
	}
	client := data.NewAlpacaClient(apiKey, secret, stocks.DataURL)

	fmt.Fprintf(cmd.OutOrStdout(), "Cohort: %s · %d tickers · %s → %s\n",
		cohortLabel, len(members), from.Format("2006-01-02"), to.Format("2006-01-02"))
	fmt.Fprintln(cmd.OutOrStdout(),
		"Historical market-cap snapshots are unavailable; the $100M scanner floor is not reconstructed.")
	result, err := pullbackfreq.Run(ctx, refreshingDailyLoader{provider: client}, members, pullbackfreq.Config{
		From: from, To: to,
		MinDollarVolume:    pullbackFreqMinDollarVolume,
		TopFraction:        pullbackFreqTopFraction,
		EpisodeGapSessions: pullbackFreqEpisodeGap,
		Pullback: pullback.Config{
			MinMomentumReturnPct:  cfg.Risk.EntryGates.PullbackMinMomentumReturnPct,
			ConsolidationSessions: cfg.Risk.EntryGates.PullbackConsolidationSessions,
			SupportTolerancePct:   cfg.Risk.EntryGates.PullbackSupportTolerancePct,
			MaxStopWidthPct:       cfg.Risk.EntryGates.PullbackMaxStopWidthPct,
		},
		Progress: func(current, total int, ticker string) {
			fmt.Fprintf(cmd.OutOrStdout(), "\rLoading daily candles: %d/%d (%s)", current, total, ticker)
		},
	})
	fmt.Fprintln(cmd.OutOrStdout())
	if err != nil {
		return err
	}
	fmt.Fprint(cmd.OutOrStdout(), pullbackfreq.Report(result))
	if pullbackFreqCSV != "" {
		if err := os.WriteFile(pullbackFreqCSV, []byte(pullbackfreq.CSV(result)), 0o644); err != nil {
			return fmt.Errorf("write frequency CSV: %w", err)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "\nEpisode rows written to %s\n", pullbackFreqCSV)
	}
	return nil
}

func pullbackFrequencyDay(t time.Time) time.Time {
	year, month, day := t.Date()
	return time.Date(year, month, day, 0, 0, 0, 0, t.Location())
}

func pullbackFrequencyMembers(ctx context.Context) ([]pullbackfreq.Member, string, error) {
	if pullbackFreqTickers != "" {
		var members []pullbackfreq.Member
		for part := range strings.SplitSeq(pullbackFreqTickers, ",") {
			members = append(members, pullbackfreq.Member{Ticker: strings.TrimSpace(part)})
		}
		return members, "explicit fixed cohort", nil
	}
	items, err := store.GetWatchlistItems(ctx, pullbackFreqActiveOnly)
	if err != nil {
		return nil, "", fmt.Errorf("load watchlist cohort: %w", err)
	}
	members := make([]pullbackfreq.Member, 0, len(items))
	for _, item := range items {
		if !strings.EqualFold(item.Platform, "stock") {
			continue
		}
		member := pullbackfreq.Member{Ticker: item.Ticker}
		if pullbackFreqRespectMembership {
			member.EligibleFrom = item.CreatedAt
			if item.ExpiresAt != nil {
				member.EligibleTo = *item.ExpiresAt
			} else if !item.Active {
				member.EligibleTo = item.UpdatedAt
			}
		}
		members = append(members, member)
	}
	label := "recorded watchlist fixed cohort (exploratory)"
	if pullbackFreqActiveOnly {
		label = "active watchlist fixed cohort (exploratory)"
	}
	if pullbackFreqRespectMembership {
		label = "recorded watchlist with stored membership dates"
	}
	return members, label, nil
}
