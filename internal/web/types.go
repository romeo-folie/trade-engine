package web

import (
	"encoding/json"

	"github.com/romeo-folie/trade-engine/internal/analysis"
)

// APIAnalystResult is the frontend-friendly projection of an AnalystResult.
type APIAnalystResult struct {
	Name        string  `json:"name"`
	Role        string  `json:"role,omitempty"`
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

// mapVerdict converts an engine Action into the dashboard's verdict vocabulary.
//
// Directional actions are renamed to the position they imply (BUY→LONG,
// SELL→SHORT). Every other action passes through unchanged.
//
// This used to be a `default:` that returned NEUTRAL for everything that was
// not BUY or SELL, which erased five distinct decisions:
//
//	HOLD  — drop this ticker; genuine disengagement
//	WAIT  — real catalyst, poor entry timing; re-evaluate next tick
//	TRADE — ORB candidate passed the gate
//	SKIP  — ORB candidate was filtered out
//	ADD   — scanner wants the ticker on the watchlist
//
// HOLD and WAIT are opposite intentions, and rendering a passing ORB candidate
// (TRADE) identically to a rejected one (SKIP) was actively misleading.
//
// NEUTRAL is now reserved for an action the dashboard genuinely cannot
// classify — in practice the empty action left by an analyst that errored.
func mapVerdict(action analysis.Action) string {
	switch action {
	case analysis.ActionBuy:
		return "LONG"
	case analysis.ActionSell:
		return "SHORT"
	case analysis.ActionHold, analysis.ActionWait, analysis.ActionTrade,
		analysis.ActionSkip, analysis.ActionAdd:
		return string(action)
	default:
		return "NEUTRAL"
	}
}

// deriveEnsembleVerdict picks the majority verdict across active analysts.
//
// Ties resolve in the same order internal/analysis/ensemble.go uses, so this
// summary agrees with the decision the engine actually reached: HOLD (genuine
// disengagement) wins outright, opposed LONG/SHORT ties fall to HOLD rather
// than inventing a direction, a directional verdict beats WAIT, and SKIP beats
// TRADE so ORB stays conservative.
//
// Resolving against a fixed precedence list rather than ranging over the votes
// map also keeps the outcome deterministic — Go randomises map iteration order,
// so a tie would otherwise resolve differently from one request to the next.
func deriveEnsembleVerdict(votes map[string]int) string {
	maxVotes := 0
	for _, n := range votes {
		if n > maxVotes {
			maxVotes = n
		}
	}
	if maxVotes == 0 {
		// No active analysts — there is no opinion to summarise.
		return "NEUTRAL"
	}

	leads := func(v string) bool { return votes[v] == maxVotes }
	switch {
	case leads("HOLD"):
		return "HOLD"
	case leads("LONG") && leads("SHORT"):
		return "HOLD"
	case leads("LONG"):
		return "LONG"
	case leads("SHORT"):
		return "SHORT"
	case leads("SKIP"):
		return "SKIP"
	case leads("TRADE"):
		return "TRADE"
	case leads("WAIT"):
		return "WAIT"
	case leads("ADD"):
		return "ADD"
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

// analystResultJSON is a JSON-safe mirror of analysis.AnalystResult.
// The Err field is intentionally omitted: encoding/json serialises a non-nil
// error as {} (no exported fields on the concrete type), then fails to
// deserialise {} back into the error interface — killing the whole slice
// decode. Active analysts are detected via empty Response.Action instead.
type analystResultJSON struct {
	Provider  string                    `json:"Provider"`
	Model     string                    `json:"Model"`
	Role      string                    `json:"Role"`
	Response  analysis.AnalysisResponse `json:"Response"`
	Meta      analysis.ModelCallMeta    `json:"Meta"`
	LatencyMs int64                     `json:"LatencyMs"`
}

// mapAnalystResults converts raw ensemble_results JSON into API types.
// Returns nil slices when raw is nil or empty.
func mapAnalystResults(raw []byte) ([]APIAnalystResult, *APIEnsembleResult) {
	if len(raw) == 0 {
		return nil, nil
	}

	var results []analystResultJSON
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

		active := r.Response.Action != ""

		name := analystName(i)
		if r.Role != "" {
			name = specialistRoleName(r.Role)
		}

		analysts[i] = APIAnalystResult{
			Name:        name,
			Role:        r.Role,
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

	// Tallies every verdict the analysts actually produced. The previous version
	// only ever counted LONG/SHORT/NEUTRAL, so once mapVerdict stopped folding
	// HOLD and WAIT into NEUTRAL those votes would have gone uncounted entirely.
	ensembleVerdict := deriveEnsembleVerdict(votes)

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

func specialistRoleName(role string) string {
	switch role {
	case "fundamental":
		return "Fundamental"
	case "technical":
		return "Technical"
	case "sentiment":
		return "Sentiment"
	case "risk_manager":
		return "Risk Manager"
	default:
		return role
	}
}
