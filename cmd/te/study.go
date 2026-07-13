package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/data"
	"github.com/romeo-folie/trade-engine/internal/logger"
	"github.com/romeo-folie/trade-engine/internal/store"
	"github.com/romeo-folie/trade-engine/internal/study"
	"github.com/spf13/cobra"
)

var (
	studySince     string
	studyCSVPath   string
	studyLimitATRs string
	studyTrailMult float64
)

func init() {
	execStudyCmd.Flags().StringVar(&studySince, "since", "", "Only trades opened on/after this date, YYYY-MM-DD (default: 90 days ago)")
	execStudyCmd.Flags().StringVar(&studyCSVPath, "csv", "", "Also write per-trade rows to this CSV file")
	execStudyCmd.Flags().StringVar(&studyLimitATRs, "limit-atrs", "0.25,0.5,1.0", "Patient-limit depths in daily ATRs below signal price")
	execStudyCmd.Flags().Float64Var(&studyTrailMult, "trail-mult", 1.5, "ATR multiple for the trailing-stop counterfactual")
	studyCmd.AddCommand(execStudyCmd)
	rootCmd.AddCommand(studyCmd)
}

var studyCmd = &cobra.Command{
	Use:   "study",
	Short: "Offline audits over recorded trading activity",
}

var execStudyCmd = &cobra.Command{
	Use:   "execution",
	Short: "Replay recorded stock trades against 1m bars to price the execution layer",
	Long: `Replays every closed stock-path trade against Alpaca historical 1-minute bars
and reports, per the execution audit:

  1. Entry chasing cost — what a live market order pays vs signal_price
     (paper mode fills at signal_price, so recorded P&L hides this).
  2. Exit fidelity — an intrabar broker-bracket sim vs the recorded exit
     (the engine checks stops hourly against candle closes), with each
     stop breach classified intraday (fixable) vs overnight gap (not).
  3. Entry tactic sweep — market-at-signal vs patient limits at ATR depths,
     scored by fill-rate-adjusted expectancy (unfilled signals count as 0).
  4. MAE/MFE — whether winners offered better entries before working.
  5. Trailing counterfactual — a ratcheting ATR trail vs what happened.

Purely read-only: no orders, no engine state. Requires DATABASE_URL and
Alpaca data credentials (IEX feed).`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runExecutionStudy(cmd)
	},
}

func runExecutionStudy(cmd *cobra.Command) error {
	ctx := cmd.Context()
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if err := logger.Init(cfg.Logging.Level, cfg.Logging.Format); err != nil {
		return err
	}

	since := time.Now().AddDate(0, 0, -90)
	if studySince != "" {
		since, err = time.Parse("2006-01-02", studySince)
		if err != nil {
			return fmt.Errorf("invalid --since date %q: %w", studySince, err)
		}
	}

	scfg := study.DefaultConfig()
	scfg.TrailingMult = studyTrailMult
	if studyLimitATRs != "" {
		scfg.LimitATRs = nil
		for part := range strings.SplitSeq(studyLimitATRs, ",") {
			k, err := strconv.ParseFloat(strings.TrimSpace(part), 64)
			if err != nil {
				return fmt.Errorf("invalid --limit-atrs entry %q: %w", part, err)
			}
			scfg.LimitATRs = append(scfg.LimitATRs, k)
		}
	}

	stocks := cfg.Markets.Stocks
	apiKey, secret := os.Getenv(stocks.APIKeyEnv), os.Getenv(stocks.SecretEnv)
	if apiKey == "" || secret == "" {
		return fmt.Errorf("Alpaca credentials unset: export %s and %s", stocks.APIKeyEnv, stocks.SecretEnv)
	}
	client := data.NewAlpacaClient(apiKey, secret, stocks.DataURL)

	if err := store.Connect(ctx); err != nil {
		return fmt.Errorf("connect DB: %w", err)
	}
	defer store.Close()

	cases, err := store.GetStudyTrades(ctx, since)
	if err != nil {
		return err
	}
	if len(cases) == 0 {
		fmt.Fprintf(cmd.OutOrStdout(), "no closed stock trades opened since %s\n", since.Format("2006-01-02"))
		return nil
	}
	fmt.Fprintf(cmd.OutOrStdout(), "replaying %d trades since %s (fetching 1m bars from Alpaca)...\n",
		len(cases), since.Format("2006-01-02"))

	runner, err := study.NewRunner(client, scfg)
	if err != nil {
		return err
	}
	results := runner.Run(ctx, cases)
	fmt.Fprint(cmd.OutOrStdout(), study.Report(results))

	if studyCSVPath != "" {
		if err := os.WriteFile(studyCSVPath, []byte(study.CSV(results)), 0o644); err != nil {
			return fmt.Errorf("write csv: %w", err)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "\nper-trade rows written to %s\n", studyCSVPath)
	}
	return nil
}
