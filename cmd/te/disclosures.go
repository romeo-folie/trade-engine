package main

import (
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/disclosures"
	"github.com/romeo-folie/trade-engine/internal/store"
	"github.com/spf13/cobra"
)

var disclosuresCmd = &cobra.Command{
	Use:   "disclosures",
	Short: "Inspect congressional periodic transaction disclosures",
}

var (
	disclosureListFrom              string
	disclosureListTo                string
	disclosureListSource            string
	disclosureListFiler             string
	disclosureListTicker            string
	disclosureListStatus            string
	disclosureListLimit             int
	disclosureSyncFrom              string
	disclosureSyncTo                string
	disclosureSyncSource            string
	disclosureSyncLimit             int
	disclosureSyncAck               bool
	disclosureSyncSenateSessionFile string
	disclosureSyncSenateUserAgent   string
)

var disclosureSyncCmd = &cobra.Command{
	Use:   "sync",
	Short: "Ingest congressional PTR filings for an explicit date range",
	Long: `Ingests official House Clerk and/or Senate eFD periodic transaction
reports. Review the current official-site terms and use restrictions before
passing --acknowledge-use-restrictions. Senate sync automatically uses the
persistent Chrome profile created by 'te disclosures senate authorize'.`,
	RunE: func(cmd *cobra.Command, _ []string) error {
		from, to, err := disclosureDateRange(time.Now().UTC(), disclosureSyncFrom, disclosureSyncTo)
		if err != nil {
			return err
		}
		if disclosureSyncLimit <= 0 {
			return fmt.Errorf("--max-reports must be > 0")
		}
		source := strings.ToLower(strings.TrimSpace(disclosureSyncSource))
		if source != "all" && source != "house" && source != "senate" && source != "oge" {
			return fmt.Errorf("--source must be all, house, senate, or oge")
		}
		if !disclosureSyncAck {
			return fmt.Errorf("review the official-site restrictions, then pass --acknowledge-use-restrictions")
		}
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		disclosureCfg := cfg.Disclosures
		disclosureCfg.Enabled = true
		disclosureCfg.AcknowledgeUseRestrictions = true
		disclosureCfg.House.Enabled = source == "all" || source == "house"
		disclosureCfg.Senate.Enabled = source == "all" || source == "senate"
		disclosureCfg.OGE.Enabled = source == "all" || source == "oge"
		disclosureCfg.Senate.AcknowledgeUseRestrictions = disclosureCfg.Senate.Enabled
		if validation := config.ValidateDisclosuresConfig(disclosureCfg); len(validation) > 0 {
			return fmt.Errorf("invalid disclosure config: %s", strings.Join(validation, "; "))
		}
		if err := store.Connect(cmd.Context()); err != nil {
			return fmt.Errorf("database: %w", err)
		}
		defer store.Close()
		runner, err := disclosures.NewRunner(disclosureCfg)
		if err != nil {
			return err
		}
		defer runner.Close() //nolint:errcheck
		if runner.Senate != nil {
			if strings.TrimSpace(disclosureSyncSenateSessionFile) != "" {
				cookieData, err := os.ReadFile(disclosureSyncSenateSessionFile)
				if err != nil {
					return fmt.Errorf("read Senate browser session file: %w", err)
				}
				session, err := disclosures.ParseSenateBrowserSession(cookieData)
				if err != nil {
					return err
				}
				userAgent := strings.TrimSpace(disclosureSyncSenateUserAgent)
				if userAgent == "" {
					userAgent = session.UserAgent
				}
				if err := runner.Senate.ImportBrowserSession(
					session.Cookies, userAgent,
				); err != nil {
					return err
				}
			} else if strings.TrimSpace(disclosureSyncSenateUserAgent) != "" {
				runner.Senate.UserAgent = strings.TrimSpace(disclosureSyncSenateUserAgent)
			}
		}
		result, syncErr := runner.SyncRange(
			cmd.Context(), from, to, disclosureSyncLimit,
		)
		fmt.Printf("Disclosure sync %s through %s: House %d, Senate %d, OGE %d reports processed.\n",
			from.Format("2006-01-02"), to.Format("2006-01-02"),
			result.HouseProcessed, result.SenateProcessed, result.OGEProcessed)
		return syncErr
	},
}

var disclosureListCmd = &cobra.Command{
	Use:   "list",
	Short: "List ingested House Clerk and Senate eFD reports",
	RunE: func(cmd *cobra.Command, _ []string) error {
		from, to, err := disclosureDateRange(
			time.Now().UTC(), disclosureListFrom, disclosureListTo,
		)
		if err != nil {
			return err
		}
		if disclosureListLimit < 0 {
			return fmt.Errorf("--limit must be >= 0")
		}
		if disclosureListSource != "" && disclosureListSource != "house" && disclosureListSource != "senate" && disclosureListSource != "oge" {
			return fmt.Errorf("--source must be house, senate, or oge")
		}
		if disclosureListStatus != "" && !validDisclosureStatus(disclosureListStatus) {
			return fmt.Errorf("--status must be pending, completed, failed, or no_text")
		}
		if err := store.Connect(cmd.Context()); err != nil {
			return fmt.Errorf("database: %w", err)
		}
		defer store.Close()
		limit := disclosureListLimit
		if limit == 0 {
			limit = 1000000
		}
		items, total, err := store.ListPoliticalDisclosureReports(
			cmd.Context(), store.PoliticalDisclosureReportFilter{
				From: from, To: to, Source: disclosureListSource,
				Filer: disclosureListFiler, Ticker: disclosureListTicker,
				Status: disclosureListStatus, Limit: limit,
			},
		)
		if err != nil {
			return err
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "FILED\tSOURCE\tFILER\tOFFICE\tSTATUS\tTXNS\tTICKERS\tREPORT ID")
		for _, item := range items {
			tickers := strings.Join(item.Tickers, ",")
			if tickers == "" {
				tickers = "—"
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%d\t%s\t%s\n",
				item.FilingDate.Format("2006-01-02"), item.Source, item.FilerName,
				optionalDisclosureText(item.Office), disclosureStatusLabel(item),
				item.TransactionCount, tickers, item.ExternalID,
			)
		}
		_ = w.Flush()
		fmt.Printf("\nShowing %d of %d reports from %s through %s.\n",
			len(items), total, from.Format("2006-01-02"), to.Format("2006-01-02"))
		return nil
	},
}

var disclosureShowCmd = &cobra.Command{
	Use:   "show <house|senate> <report-id>",
	Short: "Show one disclosure and all extracted transaction rows",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		source := strings.ToLower(strings.TrimSpace(args[0]))
		if source != "house" && source != "senate" && source != "oge" {
			return fmt.Errorf("source must be house, senate, or oge")
		}
		if err := store.Connect(cmd.Context()); err != nil {
			return fmt.Errorf("database: %w", err)
		}
		defer store.Close()
		report, err := store.GetPoliticalDisclosureReport(cmd.Context(), source, args[1])
		if err != nil {
			return err
		}
		if report == nil {
			return fmt.Errorf("%s disclosure %q was not found", source, args[1])
		}
		fmt.Printf("%s disclosure %s\n", strings.ToUpper(report.Source), report.ExternalID)
		fmt.Printf("Filer: %s", report.FilerName)
		if report.Office != "" {
			fmt.Printf(" (%s)", report.Office)
		}
		fmt.Println()
		fmt.Printf("Filed: %s · Status: %s · Transactions: %d\n",
			report.FilingDate.Format("2006-01-02"), report.ExtractionStatus,
			len(report.Transactions))
		if report.ExtractionStatus == "failed" {
			if report.RetryableFailure && report.NextRetryAt != nil {
				fmt.Printf("Automatic retry: attempt %d/%d after %s\n", report.RetryAttemptCount,
					store.MaxPoliticalDisclosureRetryAttempts,
					report.NextRetryAt.UTC().Format(time.RFC3339))
			} else {
				fmt.Printf("Automatic retry: exhausted; run 'te disclosures retry %s %s' to re-queue it.\n",
					report.Source, report.ExternalID)
			}
		}
		fmt.Printf("Document: %s\n", report.DocumentURL)
		if report.ExtractionError != "" {
			fmt.Printf("Extraction note: %s\n", report.ExtractionError)
		}
		if len(report.Transactions) == 0 {
			return nil
		}
		fmt.Println()
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "#\tDATE\tOWNER\tTYPE\tTICKER\tAMOUNT\tASSET")
		for _, item := range report.Transactions {
			fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%s\t%s\n",
				item.Ordinal, item.TransactionDate.Format("2006-01-02"),
				optionalDisclosureText(item.Owner), item.TransactionType,
				optionalDisclosureText(item.Ticker), optionalDisclosureText(item.AmountText),
				item.AssetName,
			)
		}
		return w.Flush()
	},
}

var disclosureRetryCmd = &cobra.Command{
	Use:   "retry <house|senate|oge> <report-id>",
	Short: "Re-queue a failed disclosure for the next sync",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		source := strings.ToLower(strings.TrimSpace(args[0]))
		if source != "house" && source != "senate" && source != "oge" {
			return fmt.Errorf("source must be house, senate, or oge")
		}
		if err := store.Connect(cmd.Context()); err != nil {
			return fmt.Errorf("database: %w", err)
		}
		defer store.Close()
		found, err := store.RetryPoliticalDisclosureReport(cmd.Context(), source, args[1])
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("%s disclosure %q was not found", source, args[1])
		}
		fmt.Printf("Re-queued %s disclosure %s for the next sync.\n", source, args[1])
		return nil
	},
}

func disclosureStatusLabel(item store.PoliticalDisclosureReportSummary) string {
	if item.ExtractionStatus != "failed" {
		return item.ExtractionStatus
	}
	if item.RetryableFailure && item.NextRetryAt != nil {
		return fmt.Sprintf("failed (retry %d/%d after %s)", item.RetryAttemptCount,
			store.MaxPoliticalDisclosureRetryAttempts,
			item.NextRetryAt.UTC().Format(time.RFC3339))
	}
	if item.RetryAttemptCount > 0 {
		return fmt.Sprintf("failed (manual retry; %d attempts)", item.RetryAttemptCount)
	}
	return "failed (manual retry)"
}

func disclosureDateRange(now time.Time, fromValue, toValue string) (time.Time, time.Time, error) {
	to := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	from := to.AddDate(0, 0, -90)
	var err error
	if strings.TrimSpace(fromValue) != "" {
		from, err = time.Parse("2006-01-02", fromValue)
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("invalid --from date: %w", err)
		}
	}
	if strings.TrimSpace(toValue) != "" {
		to, err = time.Parse("2006-01-02", toValue)
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("invalid --to date: %w", err)
		}
	}
	if from.After(to) {
		return time.Time{}, time.Time{}, fmt.Errorf("--from must be on or before --to")
	}
	return from, to, nil
}

func validDisclosureStatus(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "pending", "completed", "failed", "no_text":
		return true
	default:
		return false
	}
}

func optionalDisclosureText(value string) string {
	if strings.TrimSpace(value) == "" {
		return "—"
	}
	return value
}

func init() {
	disclosuresCmd.AddCommand(
		disclosureSyncCmd, disclosureListCmd, disclosureShowCmd, disclosureRetryCmd, disclosureSenateCmd,
	)
	disclosureSyncCmd.Flags().StringVar(&disclosureSyncFrom, "from", "", "Start filing date (YYYY-MM-DD; defaults to 90 days ago)")
	disclosureSyncCmd.Flags().StringVar(&disclosureSyncTo, "to", "", "End filing date (YYYY-MM-DD; defaults to today)")
	disclosureSyncCmd.Flags().StringVar(&disclosureSyncSource, "source", "all", "Source to ingest: all, house, or senate")
	disclosureSyncCmd.Flags().IntVar(&disclosureSyncLimit, "max-reports", 5000, "Maximum reports to process across enabled sources")
	disclosureSyncCmd.Flags().BoolVar(&disclosureSyncAck, "acknowledge-use-restrictions", false, "Confirm current official-site terms and use restrictions were reviewed")
	disclosureSyncCmd.Flags().StringVar(&disclosureSyncSenateSessionFile, "senate-session-file", "", "Local Senate cookies or copied request headers (values are never logged)")
	disclosureSyncCmd.Flags().StringVar(&disclosureSyncSenateUserAgent, "senate-user-agent", "", "Exact User-Agent from the browser that exported the Senate session")
	_ = disclosureSyncCmd.Flags().MarkHidden("senate-session-file")
	_ = disclosureSyncCmd.Flags().MarkHidden("senate-user-agent")
	disclosureListCmd.Flags().StringVar(&disclosureListFrom, "from", "", "Start filing date (YYYY-MM-DD; defaults to 90 days ago)")
	disclosureListCmd.Flags().StringVar(&disclosureListTo, "to", "", "End filing date (YYYY-MM-DD; defaults to today)")
	disclosureListCmd.Flags().StringVar(&disclosureListSource, "source", "", "Filter by house or senate")
	disclosureListCmd.Flags().StringVar(&disclosureListFiler, "filer", "", "Filter by filer name substring")
	disclosureListCmd.Flags().StringVar(&disclosureListTicker, "ticker", "", "Filter by exact ticker")
	disclosureListCmd.Flags().StringVar(&disclosureListStatus, "status", "", "Filter by extraction status")
	disclosureListCmd.Flags().IntVar(&disclosureListLimit, "limit", 100, "Maximum reports to print (0 prints all)")
}
