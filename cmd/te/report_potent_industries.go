package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/store"
)

func runReportPotentIndustries(cmd *cobra.Command, _ []string) error {
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
	industry, _ := cmd.Flags().GetString("industry")
	topMembers, _ := cmd.Flags().GetInt("top-members")
	if topMembers <= 0 {
		return fmt.Errorf("--top-members must be greater than zero")
	}
	from, to, err := weeklyLeaderReportDateRange(time.Now(), fromValue, toValue)
	if err != nil {
		return err
	}
	runs, err := store.ListCompletedIndustryRotationRunsBySessionDate(ctx, from, to)
	if err != nil {
		return err
	}
	runIDs := make([]string, 0, len(runs))
	for _, run := range runs {
		runIDs = append(runIDs, run.ID)
	}
	groups, err := store.ListIndustryRotationGroups(ctx, runIDs)
	if err != nil {
		return err
	}
	transitions, err := store.ListIndustryRotationTransitions(ctx, runIDs)
	if err != nil {
		return err
	}
	members, err := store.ListIndustryRotationTopMembers(ctx, runIDs, topMembers)
	if err != nil {
		return err
	}
	return renderPotentIndustryReport(
		os.Stdout, from, to, strings.TrimSpace(industry),
		runs, groups, transitions, members,
	)
}

func renderPotentIndustryReport(
	out io.Writer,
	from, to time.Time,
	industryFilter string,
	runs []store.IndustryRotationRun,
	groups []store.IndustryRotationGroup,
	transitions []store.IndustryRotationTransition,
	members []store.IndustryRotationMember,
) error {
	fmt.Fprintf(out, "Prospective potent-industry study  %s → %s\n",
		from.Format("2006-01-02"), to.Format("2006-01-02"))
	if industryFilter != "" {
		fmt.Fprintf(out, "Industry filter: %s\n", industryFilter)
		fmt.Fprintln(out, "Run totals remain full-universe.")
	}
	if len(runs) == 0 {
		fmt.Fprintln(out, "No completed potent-industry runs.")
		return nil
	}

	groupsByRun := make(map[string][]store.IndustryRotationGroup)
	for _, group := range groups {
		if industryFilter != "" &&
			!industryReportLabelMatches(group.Industry, industryFilter) {
			continue
		}
		groupsByRun[group.RunID] = append(groupsByRun[group.RunID], group)
	}
	membersByGroup := make(map[string][]store.IndustryRotationMember)
	for _, member := range members {
		key := industryReportGroupKey(member.RunID, member.GroupType, member.GroupKey)
		membersByGroup[key] = append(membersByGroup[key], member)
	}
	transitionsByRun := make(map[string][]store.IndustryRotationTransition)
	for _, transition := range transitions {
		if industryFilter != "" &&
			!industryReportLabelMatches(transition.GroupKey, industryFilter) {
			continue
		}
		transitionsByRun[transition.RunID] = append(
			transitionsByRun[transition.RunID], transition,
		)
	}

	fmt.Fprintf(out, "%-12s %7s %7s %7s %7s %7s %7s %9s %9s\n",
		"SESSION", "GROUPS", "SCORED", "POTENT", "ADDED", "REMOVED",
		"PERSIST", "G-COVER", "M-COVER")
	fmt.Fprintln(out, strings.Repeat("─", 87))
	for _, run := range runs {
		fmt.Fprintf(out, "%-12s %7d %7d %7d %7d %7d %7d %8.2f%% %8.2f%%\n",
			run.SessionDate.Format("2006-01-02"), run.EligibleGroupCount,
			run.EvaluatedGroupCount, run.PotentGroupCount, run.AddedGroupCount,
			run.RemovedGroupCount, run.PersistentGroupCount,
			run.GroupCoverage*100, run.MemberCoverage*100)
		for _, group := range groupsByRun[run.ID] {
			fmt.Fprintf(out,
				"  %-28s %-14s %-13s score=%s rank=%s ew5=%s median5=%s advance=%s persist=%d/5 coverage=%s benchmark=%s\n",
				group.Industry, group.Archetype, group.Outcome,
				industryReportFloat(group.PotencyScore, 3),
				industryReportInt(group.Rank),
				industryReportPercent(group.EqualWeightReturn5),
				industryReportPercent(group.MedianMemberReturn5),
				industryReportPercent(group.AdvanceShare),
				group.PersistenceSessions,
				industryReportPercentValue(group.MemberCoverage),
				group.Benchmark,
			)
			if group.FailureStage != "" {
				fmt.Fprintf(out, "    failure: %s — %s\n",
					group.FailureStage, group.FailureReason)
			}
			key := industryReportGroupKey(group.RunID, group.GroupType, group.GroupKey)
			for _, member := range membersByGroup[key] {
				fmt.Fprintf(out,
					"    #%s %-8s score=%s vs-industry=%s vs-market=%s\n",
					industryReportInt(member.MemberRank), member.Ticker,
					industryReportFloat(member.MemberScore, 3),
					industryReportPercent(member.StockVsIndustryRS),
					industryReportPercent(member.StockVsMarketRS),
				)
			}
		}
		for _, transition := range transitionsByRun[run.ID] {
			fmt.Fprintf(out, "    transition: %-10s %s (%s)\n",
				transition.Event, transition.GroupKey, transition.Reason)
		}
	}
	return nil
}

func industryReportGroupKey(runID, groupType, groupKey string) string {
	return runID + "\x00" + groupType + "\x00" + groupKey
}

func industryReportLabelMatches(left, right string) bool {
	normalize := func(value string) string {
		return strings.ToUpper(strings.Join(strings.Fields(value), " "))
	}
	return normalize(left) == normalize(right)
}

func industryReportFloat(value *float64, precision int) string {
	if value == nil {
		return "—"
	}
	return fmt.Sprintf("%.*f", precision, *value)
}

func industryReportPercent(value *float64) string {
	if value == nil {
		return "—"
	}
	return fmt.Sprintf("%.2f%%", *value*100)
}

func industryReportPercentValue(value float64) string {
	return fmt.Sprintf("%.2f%%", value*100)
}

func industryReportInt(value *int) string {
	if value == nil {
		return "—"
	}
	return fmt.Sprintf("%d", *value)
}
