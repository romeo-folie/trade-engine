package orb

import (
	"strings"

	"github.com/romeo-folie/trade-engine/internal/analysis"
)

// defaultScanConfidenceThreshold is the minimum aggregated conviction for the equities
// ORB scan to treat a gap candidate as tradeable. Mirrors the stock signal gate (0.68).
const defaultScanConfidenceThreshold = 0.68

// effectiveScanThreshold resolves the configured scan confidence threshold, falling
// back to defaultScanConfidenceThreshold when unset (<=0).
func effectiveScanThreshold(configured float64) float64 {
	if configured > 0 {
		return configured
	}
	return defaultScanConfidenceThreshold
}

// scanConfidenceThreshold is the effective min conviction for the equities ORB scan.
func (s *ORBScheduler) scanConfidenceThreshold() float64 {
	return effectiveScanThreshold(s.cfg.ScanConfidenceThreshold)
}

// isTradeAction reports whether an ORB/stock action string means "enter". The ORB prompt
// emits "trade"; the standard stock schema emits "BUY" (used on the parse fallback).
func isTradeAction(action string) bool {
	return strings.EqualFold(action, "trade") || strings.EqualFold(action, "BUY")
}

// aggregateORBScore combines the raw per-analyst ORB verdicts into one scoring result
// WITHOUT an arbiter, so the equities gap scan keeps selecting candidates with however
// many analysts are reachable (even one) and never depends on the arbiter being up.
//
// Rule (mirrors the stock ensemble's winning-action logic, made confidence-based):
//   - Parse each SUCCESSFUL analyst's {action, confidence}; failed analysts are ignored.
//   - The winning action is "trade" when trade-voters are at least half of the
//     successful analysts (ties → trade), otherwise "skip".
//   - When trade wins, conviction is the MEAN confidence of the trade-voters (skips do
//     not dilute it), and the result is emitted as "trade" only if that mean meets
//     confidenceThreshold — the confidence gate. Below it, the candidate is "skip" but
//     keeps the (sub-threshold) conviction so ranking/telemetry can see how close it was.
//
// Metadata (catalyst, reasoning, risks) is carried from the most confident trade-voter
// (or the first successful result when skipping) so the surfaced rationale is representative.
func aggregateORBScore(results []analysis.AnalystResult, confidenceThreshold float64) ORBAnalysisResult {
	var (
		tradeConfSum float64
		tradeVotes   int
		total        int
		best         ORBAnalysisResult
		bestConf     float64
		haveBest     bool
		firstParsed  ORBAnalysisResult
		haveFirst    bool
	)
	for _, r := range results {
		if r.Err != nil {
			continue // failed analyst — excluded from both numerator and denominator
		}
		total++
		orb := parseORBResult(r.Meta.RawResponse, r.Response)
		if !haveFirst {
			firstParsed, haveFirst = orb, true
		}
		if isTradeAction(orb.Action) {
			tradeVotes++
			tradeConfSum += orb.Conviction
			if !haveBest || orb.Conviction > bestConf {
				best, bestConf, haveBest = orb, orb.Conviction, true
			}
		}
	}
	if total == 0 {
		return ORBAnalysisResult{Action: "skip"} // no successful analysts
	}

	// Skip won the vote (trade-voters below half) → no entry.
	if tradeVotes == 0 || tradeVotes*2 < total {
		out := firstParsed
		out.Action = "skip"
		return out
	}

	// Trade won the vote. Gate its mean confidence.
	meanConf := tradeConfSum / float64(tradeVotes)
	out := best
	out.Conviction = meanConf
	if meanConf >= confidenceThreshold {
		out.Action = "trade"
	} else {
		out.Action = "skip" // won the vote but not confident enough
	}
	return out
}
