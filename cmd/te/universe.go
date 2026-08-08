package main

import (
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/data"
	"github.com/romeo-folie/trade-engine/internal/engine"
	"github.com/romeo-folie/trade-engine/internal/store"
	swinguniverse "github.com/romeo-folie/trade-engine/internal/universe"
	"github.com/spf13/cobra"
)

var universeCmd = &cobra.Command{
	Use:   "universe",
	Short: "Build and inspect immutable swing-trading universe snapshots",
}

var universeRefreshCmd = &cobra.Command{
	Use:   "refresh",
	Short: "Ensure the current completed session has an approved universe",
	Long: `Fetches the current Alpaca asset master and market evidence, applies the
configured objective eligibility filters, and stores one immutable session snapshot.
It reuses an existing approved attempt and intentionally refuses pre-close,
weekend, holiday, or historical backfills from current live APIs.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		if !cfg.Scanner.Universe.Enabled {
			return fmt.Errorf("scanner.universe.enabled is false")
		}
		if cfg.Scanner.Universe.Provider != "alpaca" {
			return fmt.Errorf("unsupported universe provider %q", cfg.Scanner.Universe.Provider)
		}
		if cfg.Markets.Stocks.APIKey == "" || cfg.Markets.Stocks.Secret == "" {
			return fmt.Errorf("Alpaca stock API credentials are required")
		}
		fundamentals, err := universeFundamentalsProvider(cfg.Markets.News)
		if err != nil {
			return err
		}
		if err := connectDB(); err != nil {
			return err
		}
		defer store.Close()

		alpaca := data.NewAlpacaClient(
			cfg.Markets.Stocks.APIKey, cfg.Markets.Stocks.Secret, cfg.Markets.Stocks.DataURL,
		)
		provider := data.NewAlpacaUniverseClient(alpaca, cfg.Markets.Stocks.BaseURL)
		universeCfg := cfg.Scanner.Universe
		// A manual refresh is an explicit one-shot opt-in even when the durable
		// scheduler remains disabled in configuration.
		universeCfg.Enabled = true
		runner := swinguniverse.SessionRunner{
			Config: universeCfg,
			Builder: swinguniverse.Builder{
				Config: universeCfg, Assets: provider, Snapshots: provider,
				Bars: provider, Fundamentals: fundamentals,
			},
		}
		now := time.Now()
		session := engine.MostRecentCompletedMarketSession(now, 0)
		version, err := runner.RunSession(cmd.Context(), session)
		if err != nil {
			return err
		}
		fmt.Printf(
			"Universe %s approved as of %s: %d/%d eligible, %.1f%% market-data coverage, %d partial failures.\n",
			version.ID, version.AsOf.Format(time.RFC3339), version.EligibleCount,
			version.MemberCount, version.Quality.MarketDataCoverage*100,
			version.Quality.PartialFailureCount,
		)
		return nil
	},
}

// universeFundamentalsProvider keeps provider identity and credentials coupled.
// The refresh currently needs Finnhub's company-profile endpoint for market-cap
// and taxonomy evidence. A news key belonging to Benzinga (or any future news
// provider) must never be sent to Finnhub. Cached-only refreshes are not an
// explicit operating mode yet, so missing Finnhub configuration fails before
// opening the database or starting provider work.
func universeFundamentalsProvider(news config.NewsAPIConfig) (swinguniverse.FundamentalsProvider, error) {
	if !strings.EqualFold(strings.TrimSpace(news.Provider), "finnhub") {
		return nil, fmt.Errorf(
			"universe refresh requires Finnhub fundamentals: markets.news.provider must be %q, got %q",
			"finnhub", news.Provider,
		)
	}
	if strings.TrimSpace(news.APIKey) == "" {
		apiKeyEnv := strings.TrimSpace(news.APIKeyEnv)
		if apiKeyEnv == "" {
			apiKeyEnv = "FINNHUB_API_KEY"
		}
		return nil, fmt.Errorf(
			"universe refresh requires Finnhub fundamentals credentials: export %s",
			apiKeyEnv,
		)
	}
	return data.NewFinnhubClient(news.APIKey), nil
}

var (
	universeListAsOf  string
	universeListAll   bool
	universeListLimit int
)

var universeListCmd = &cobra.Command{
	Use:   "list",
	Short: "Enumerate the latest completed universe available at a decision time",
	RunE: func(cmd *cobra.Command, args []string) error {
		if universeListLimit < 0 {
			return fmt.Errorf("--limit must be >= 0")
		}
		asOf := time.Now().UTC()
		if universeListAsOf != "" {
			parsed, err := time.Parse(time.RFC3339, universeListAsOf)
			if err != nil {
				return fmt.Errorf("parse --as-of (expected RFC3339): %w", err)
			}
			asOf = parsed
		}
		if err := connectDB(); err != nil {
			return err
		}
		defer store.Close()
		ctx := cmd.Context()
		version, err := store.GetLatestCompletedSwingUniverseVersionAsOf(ctx, asOf)
		if err != nil {
			return err
		}
		if version == nil {
			fmt.Printf("No completed universe was available as of %s.\n", asOf.Format(time.RFC3339))
			return nil
		}
		members, err := store.ListSwingUniverseMembers(ctx, version.ID, !universeListAll)
		if err != nil {
			return err
		}
		shown := len(members)
		if universeListLimit > 0 && shown > universeListLimit {
			shown = universeListLimit
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "TICKER\tELIGIBLE\tEXCHANGE\tPRICE\tMARKET CAP ($MM)\tMEDIAN $VOLUME\tINDUSTRY\tSTATUS\tREMOVAL/FAILURE")
		for _, member := range members[:shown] {
			fmt.Fprintf(w, "%s\t%t\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
				member.Ticker, member.Eligible, member.Exchange, optionalFloat(member.Price),
				optionalFloat(member.MarketCapMM), optionalFloat(member.DollarVolume),
				member.Industry, member.TaxonomyStatus, memberReason(member),
			)
		}
		_ = w.Flush()
		fmt.Printf(
			"\nVersion %s as of %s: showing %d of %d rows (%d eligible of %d members).\n",
			version.ID, version.AsOf.Format(time.RFC3339), shown, len(members),
			version.EligibleCount, version.MemberCount,
		)
		return nil
	},
}

func optionalFloat(value *float64) string {
	if value == nil {
		return "—"
	}
	return fmt.Sprintf("%.2f", *value)
}

func memberReason(member store.SwingUniverseMember) string {
	if member.FailureReason != "" {
		stage := member.RemovalStage
		if stage == "" {
			stage = member.FailureStage
		}
		if stage == "" {
			return member.FailureReason
		}
		return stage + ": " + member.FailureReason
	}
	return member.RemovalStage
}

func init() {
	universeCmd.AddCommand(universeRefreshCmd, universeListCmd)
	universeListCmd.Flags().StringVar(
		&universeListAsOf, "as-of", "", "Decision time in RFC3339 (defaults to now)",
	)
	universeListCmd.Flags().BoolVar(
		&universeListAll, "all", false, "Include ineligible and failed asset-master rows",
	)
	universeListCmd.Flags().IntVar(
		&universeListLimit, "limit", 100, "Maximum rows to print (0 prints all)",
	)
}
