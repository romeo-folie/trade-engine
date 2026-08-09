package currencystrengthresearch

import (
	"encoding/json"
	"fmt"
	"io"
)

func WriteJSON(w io.Writer, report Report) error {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(report)
}

func WriteText(w io.Writer, report Report) error {
	if _, err := fmt.Fprintf(w, "currency-strength rolling-origin research · %s -> %s · %d candidates · %d pairs\n\n", report.From.Format("2006-01-02"), report.To.Format("2006-01-02"), report.CandidateCount, len(report.Universe)); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(w, "BASELINE FOLDS"); err != nil {
		return err
	}
	for _, fold := range report.Folds {
		if _, err := fmt.Fprintf(w, "%s  select %s..%s  test %s..%s\n  winner %s  selection_score=%.4f selection_trades=%d selection_net=%+.2f\n", fold.Fold.ID, fold.Fold.SelectionFrom.Format("2006-01-02"), fold.Fold.SelectionTo.Format("2006-01-02"), fold.Fold.TestFrom.Format("2006-01-02"), fold.Fold.TestTo.Format("2006-01-02"), fold.Selection.Param.ID(), fold.Selection.Score, fold.Selection.SelectionMetric.Trades, fold.Selection.SelectionMetric.NetPnL); err != nil {
			return err
		}
		for _, cost := range fold.Test {
			if _, err := fmt.Fprintf(w, "  %.1fx: trades=%d win=%.1f%% pf=%s net=%+.2f avg=%+.2f\n", cost.Multiplier, cost.Metrics.Trades, cost.Metrics.WinRate*100, metricPF(cost.Metrics), cost.Metrics.NetPnL, cost.Metrics.AvgPnL); err != nil {
				return err
			}
		}
	}
	if _, err := fmt.Fprintln(w, "\nPAIR ABLATION VERDICTS"); err != nil {
		return err
	}
	for _, pair := range report.Pairs {
		if _, err := fmt.Fprintf(w, "%-8s %s\n", pair.Pair, pair.Verdict); err != nil {
			return err
		}
		for _, evidence := range pair.Evidence {
			if _, err := fmt.Fprintf(w, "  %-9s %-10s both=%d/%d (require >=%d) affected=%d delta_1x=%+.2f delta_2x=%+.2f qualifies=%t", evidence.Scope, evidence.Policy, evidence.ImprovedBoth, report.Criteria.ExpectedFolds, report.Criteria.RequiredImprovedFolds, evidence.AffectedDays, evidence.AggregateDelta1X, evidence.AggregateDelta2X, evidence.Qualifies); err != nil {
				return err
			}
			if len(evidence.Reasons) > 0 {
				if _, err := fmt.Fprintf(w, " · %v", evidence.Reasons); err != nil {
					return err
				}
			}
			if _, err := fmt.Fprintln(w); err != nil {
				return err
			}
		}
	}
	return nil
}

func metricPF(metrics Metrics) string {
	if metrics.ProfitFactorInfinite {
		return "inf"
	}
	if metrics.ProfitFactor == nil {
		return "-"
	}
	return fmt.Sprintf("%.2f", *metrics.ProfitFactor)
}
