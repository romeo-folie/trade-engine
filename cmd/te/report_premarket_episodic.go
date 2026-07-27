package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/store"
	"github.com/spf13/cobra"
)

func runReportPremarketEpisodic(cmd *cobra.Command, _ []string) error {
	if _, err := config.Load(); err != nil {
		return err
	}
	ctx := cmd.Context()
	if err := store.Connect(ctx); err != nil {
		return fmt.Errorf("database: %w", err)
	}
	defer store.Close()
	fromValue, _ := cmd.Flags().GetString("from")
	toValue, _ := cmd.Flags().GetString("to")
	qualifiedOnly, _ := cmd.Flags().GetBool("qualified-only")
	from, to, err := weeklyLeaderReportDateRange(time.Now(), fromValue, toValue)
	if err != nil {
		return err
	}
	runs, err := store.ListCompletedPremarketEpisodicRunsBySessionDate(ctx, from, to)
	if err != nil {
		return err
	}
	runIDs := make([]string, 0, len(runs))
	for _, run := range runs {
		runIDs = append(runIDs, run.ID)
	}
	candidates, err := store.ListPremarketEpisodicCandidates(ctx, runIDs)
	if err != nil {
		return err
	}
	return renderPremarketEpisodicReport(
		os.Stdout, from, to, qualifiedOnly, runs, candidates,
	)
}

func renderPremarketEpisodicReport(
	out io.Writer, from, to time.Time, qualifiedOnly bool,
	runs []store.PremarketEpisodicRun,
	candidates []store.PremarketEpisodicCandidate,
) error {
	fmt.Fprintf(out, "Prospective premarket-episodic study  %s → %s\n",
		from.Format("2006-01-02"), to.Format("2006-01-02"))
	if len(runs) == 0 {
		fmt.Fprintln(out, "No completed premarket-episodic runs.")
		return nil
	}
	exchangeLocation, err := time.LoadLocation("America/New_York")
	if err != nil {
		return err
	}
	byRun := make(map[string][]store.PremarketEpisodicCandidate)
	for _, candidate := range candidates {
		if qualifiedOnly && candidate.Outcome != "qualified" {
			continue
		}
		byRun[candidate.RunID] = append(byRun[candidate.RunID], candidate)
	}
	for _, run := range runs {
		fmt.Fprintf(out,
			"\n%s %s ET  eligible=%d shortlisted=%d evaluated=%d qualified=%d failed=%d feed=%s entitlement=%s\n",
			run.SessionDate.Format("2006-01-02"),
			run.CheckpointAt.In(exchangeLocation).Format("15:04"),
			run.EligibleUniverseCount,
			run.ShortlistedCount, run.EvaluatedCount, run.QualifiedCount,
			run.FailureCount, run.Feed, run.EntitlementStatus,
		)
		for _, candidate := range byRun[run.ID] {
			catalyst := struct {
				Headline string `json:"headline"`
			}{}
			_ = json.Unmarshal(candidate.CatalystReference, &catalyst)
			fmt.Fprintf(out,
				"  %-7s %-13s rank=%s score=%s gap=%s rvol=%s volume=%s $vol=%s trades=%s spread=%s catalyst=%s promotion=%s\n",
				candidate.Ticker, candidate.Outcome,
				reportOptionalInt(candidate.Rank),
				reportOptionalFloat(candidate.Score, "%.3f"),
				reportOptionalFloat(candidate.GapPct, "%+.2f%%"),
				reportOptionalFloat(candidate.TimeAdjustedRVOL, "%.2fx"),
				reportOptionalInt64(candidate.PremarketVolume),
				reportOptionalFloat(candidate.DollarVolume, "$%.0f"),
				reportOptionalInt64(candidate.TradeCount),
				reportOptionalFloat(candidate.SpreadBPS, "%.1fbps"),
				candidate.CatalystStatus, candidate.PromotionStatus,
			)
			if catalyst.Headline != "" {
				fmt.Fprintf(out, "    catalyst: %s\n", catalyst.Headline)
			}
			if candidate.RemovalStage != "" {
				fmt.Fprintf(out, "    removed: %s — %s\n",
					candidate.RemovalStage, candidate.FailureReason)
			}
		}
	}
	return nil
}

func reportOptionalFloat(value *float64, format string) string {
	if value == nil {
		return "—"
	}
	return fmt.Sprintf(format, *value)
}

func reportOptionalInt(value *int) string {
	if value == nil {
		return "—"
	}
	return fmt.Sprintf("%d", *value)
}

func reportOptionalInt64(value *int64) string {
	if value == nil {
		return "—"
	}
	return fmt.Sprintf("%d", *value)
}
