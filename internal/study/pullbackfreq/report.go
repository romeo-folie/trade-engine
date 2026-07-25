package pullbackfreq

import (
	"encoding/csv"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

func Report(result Result) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Pullback frequency study\n\n")
	fmt.Fprintf(&b, "Cohort tickers:       %d\n", result.CohortSize)
	fmt.Fprintf(&b, "Observed sessions:    %d\n", result.ObservedSessions)
	fmt.Fprintf(&b, "Liquidity qualified:  %d\n", result.LiquidityQualified)
	fmt.Fprintf(&b, "Momentum qualified:   %d\n", result.MomentumQualified)
	fmt.Fprintf(&b, "Selected candidate-days: %d\n", result.SelectedDays)
	fmt.Fprintf(&b, "Eligible setup days:  %d\n", result.EligibleDays)
	fmt.Fprintf(&b, "Distinct episodes:    %d\n", result.Episodes)
	if result.SelectedDays > 0 {
		fmt.Fprintf(&b, "Setup-day rate:       %.3f%% of selected candidate-days\n",
			float64(result.EligibleDays)/float64(result.SelectedDays)*100)
	}
	if result.ObservedSessions > 0 {
		fmt.Fprintf(&b, "Episode rate:         %.3f per 1,000 observed symbol-days\n",
			float64(result.Episodes)/float64(result.ObservedSessions)*1000)
	}

	fmt.Fprintln(&b, "\nFailure funnel:")
	type pair struct {
		key string
		n   int
	}
	blocks := make([]pair, 0, len(result.Blocks))
	for key, n := range result.Blocks {
		blocks = append(blocks, pair{key, n})
	}
	sort.Slice(blocks, func(i, j int) bool {
		if blocks[i].n == blocks[j].n {
			return blocks[i].key < blocks[j].key
		}
		return blocks[i].n > blocks[j].n
	})
	for _, block := range blocks {
		fmt.Fprintf(&b, "  %-22s %d\n", block.key, block.n)
	}
	if len(result.LoadErrors) > 0 {
		fmt.Fprintf(&b, "\nCandle-load failures: %d\n", len(result.LoadErrors))
	}
	return b.String()
}

func CSV(result Result) string {
	var b strings.Builder
	w := csv.NewWriter(&b)
	_ = w.Write([]string{
		"ticker", "date", "return_1m_pct", "return_3m_pct", "return_6m_pct",
		"support", "stop_loss", "stop_width_pct",
	})
	for _, row := range result.EpisodeRows {
		_ = w.Write([]string{
			row.Ticker,
			row.Date.Format("2006-01-02"),
			strconv.FormatFloat(row.Return1M, 'f', 4, 64),
			strconv.FormatFloat(row.Return3M, 'f', 4, 64),
			strconv.FormatFloat(row.Return6M, 'f', 4, 64),
			strconv.FormatFloat(row.Support, 'f', 4, 64),
			strconv.FormatFloat(row.StopLoss, 'f', 4, 64),
			strconv.FormatFloat(row.StopWidthPct, 'f', 4, 64),
		})
	}
	w.Flush()
	return b.String()
}
