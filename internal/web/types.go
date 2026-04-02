package web

import (
	"encoding/json"

	"github.com/romeo-folie/trade-engine/internal/analysis"
)

// APIAnalystResult is the frontend-friendly projection of an AnalystResult.
type APIAnalystResult struct {
	Name        string  `json:"name"`
	Verdict     string  `json:"verdict"`
	Confidence  float64 `json:"confidence"`
	Description string  `json:"description"`
	Weight      float64 `json:"weight"`
	Active      bool    `json:"active"`
	Model       string  `json:"model"`
}

// APIEnsembleResult is a summary of the ensemble verdict.
type APIEnsembleResult struct {
	Verdict    string  `json:"verdict"`
	Confidence float64 `json:"confidence"`
}

// analystNames maps index → human-readable analyst label.
// The order matches the analyst slots wired in internal/analysis/ensemble.go.
// If the analyst roster changes (new analysts added, order reordered), update
// both this slice and ensemble.go in the same commit — they are coupled.
// Long-term fix: have AnalystResult carry its own Name field so this mapping
// is unnecessary (tracked as a follow-up task).
var analystNames = []string{"Standard Analysis", "High-Depth Analysis", "Deep-Reasoning Analysis", "Macro Analysis"}

func analystName(idx int) string {
	if idx < len(analystNames) {
		return analystNames[idx]
	}
	return "Analyst"
}

// mapVerdict converts BUY/SELL/HOLD → LONG/SHORT/NEUTRAL.
func mapVerdict(action analysis.Action) string {
	switch action {
	case analysis.ActionBuy:
		return "LONG"
	case analysis.ActionSell:
		return "SHORT"
	default:
		return "NEUTRAL"
	}
}

// truncate limits a string to maxLen runes, appending "…" if truncated.
func truncate(s string, maxLen int) string {
	runes := []rune(s)
	if len(runes) <= maxLen {
		return s
	}
	return string(runes[:maxLen-1]) + "…"
}

// mapAnalystResults converts raw ensemble_results JSON into API types.
// Returns nil slices when raw is nil or empty.
func mapAnalystResults(raw []byte) ([]APIAnalystResult, *APIEnsembleResult) {
	if len(raw) == 0 {
		return nil, nil
	}

	var results []analysis.AnalystResult
	if err := json.Unmarshal(raw, &results); err != nil {
		return nil, nil
	}
	if len(results) == 0 {
		return nil, nil
	}

	n := len(results)
	// Equal-weight assumption: all analysts contribute equally to the ensemble.
	// When Phase 12 weighted ensembles land, replace this with per-analyst weights
	// stored in the EnsembleRecord (internal/store/analyses.go EnsembleRecord.Weights).
	weight := 1.0 / float64(n)
	analysts := make([]APIAnalystResult, n)

	for i, r := range results {
		model := r.Meta.Model
		if model == "" {
			model = r.Model
		}
		if model == "" {
			model = r.Provider
		}

		// After JSON round-trip, Err is always nil (error interface
		// doesn't unmarshal). Detect inactive analysts by empty Action.
		active := r.Response.Action != ""

		analysts[i] = APIAnalystResult{
			Name:        analystName(i),
			Verdict:     mapVerdict(r.Response.Action),
			Confidence:  r.Response.Confidence,
			Description: r.Response.Reasoning,
			Weight:      weight,
			Active:      active,
			Model:       model,
		}
	}

	// Derive ensemble: majority vote on verdict, weighted average confidence.
	votes := map[string]int{}
	var confSum float64
	var activeCount int
	for _, a := range analysts {
		if a.Active {
			votes[a.Verdict]++
			confSum += a.Confidence
			activeCount++
		}
	}

	ensembleVerdict := "NEUTRAL"
	maxVotes := 0
	for _, v := range []string{"LONG", "SHORT", "NEUTRAL"} {
		if votes[v] > maxVotes {
			maxVotes = votes[v]
			ensembleVerdict = v
		}
	}
	// Tie-break: if LONG and SHORT have equal non-zero votes, return NEUTRAL
	if votes["LONG"] > 0 && votes["LONG"] == votes["SHORT"] {
		ensembleVerdict = "NEUTRAL"
	}

	var ensembleConf float64
	if activeCount > 0 {
		ensembleConf = confSum / float64(activeCount)
	}

	ensemble := &APIEnsembleResult{
		Verdict:    ensembleVerdict,
		Confidence: ensembleConf,
	}

	return analysts, ensemble
}
