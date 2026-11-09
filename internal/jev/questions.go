package jev

import (
	"strings"

	"github.com/romeo-folie/trade-engine/internal/analysis"
	"github.com/romeo-folie/trade-engine/internal/analysis/prompts"
	"github.com/romeo-folie/trade-engine/internal/analysis/snapshot"
)

// QuestionSetVersion identifies the questions, options, and instructions
// below. Bump it with any change Jev can observe; it is recorded on every
// decision snapshot alongside the policy version.
const QuestionSetVersion = "jev-questions/v1"

// Question names (stable keys in requests, answers, and audits).
const (
	QThesis  = "thesis_support"
	QAdverse = "material_adverse_evidence"
	QLens    = "lens"
	QTiming  = "entry_timing"
	QRegime  = "regime"
	QExit    = "exit"
)

// Shared option names.
const (
	OptInsufficient = "insufficient_evidence"
	OptNone         = "none"

	ThesisStrong   = "strong_support"
	ThesisModerate = "moderate_support"
	ThesisWeak     = "weak_support"
	ThesisNone     = "no_support"

	TimingEnterNow = "enter_now"
	TimingWait     = "wait_for_better_entry"

	ExitNow  = "exit_now"
	ExitHold = "hold_position"
)

// lensDescriptions are the decision preconditions for each analytical lens.
var lensDescriptions = map[string]string{
	"trend_following":   "An established directional trend: price above rising EMAs, ADX showing trend strength, higher highs and higher lows.",
	"mean_reversion":    "A ranging market where price is stretched from its mean (Bollinger extremes, oversold RSI) and likely to revert.",
	"momentum":          "Strong recent price and volume acceleration that is still being confirmed, without exhaustion or excessive extension.",
	"news_sentiment":    "A fresh (under ~24h) credible catalyst that price and volume are confirming and have not yet fully absorbed.",
	"pullback_strength": "A controlled pullback within a strong daily uptrend, holding named support, per the deterministic pullback evaluation in the state.",
}

func lensDescription(lens string) string {
	if d, ok := lensDescriptions[lens]; ok {
		return d
	}
	return "The " + strings.ReplaceAll(lens, "_", " ") + " lens fits the current regime and evidence."
}

// HoldsLong reports whether the request's portfolio holds a long position in
// its ticker. Held tickers are asked exit questions; others entry questions.
func HoldsLong(req analysis.AnalysisRequest) bool {
	for _, p := range req.Portfolio.OpenPositions {
		if strings.EqualFold(p.Ticker, req.Ticker) && p.Quantity > 0 && (p.Side == "" || strings.EqualFold(p.Side, "long")) {
			return true
		}
	}
	return false
}

// AdmissibleLenses returns the request's lens menu, filtered to analytical
// lenses, in profile order.
func AdmissibleLenses(req analysis.AnalysisRequest) []string {
	return prompts.Lenses(req.LensSet())
}

// Questions builds the v1 question set for a request. Each question is
// atomic and evaluated independently by Jev; the policy composes them.
func Questions(req analysis.AnalysisRequest) map[string]Question {
	qs := map[string]Question{
		QAdverse: {
			Type: TypeNoul,
			Instructions: "Using only the supplied state, is there material adverse evidence for holding a long position in " +
				req.Ticker + " now (for example a negative catalyst, a broken trend or support, guidance cuts, or regulatory risk)?",
			Criteria: map[string]string{
				"true":  "Material adverse evidence is present in the supplied state.",
				"false": "No material adverse evidence is present in the supplied state.",
			},
		},
		QRegime: {
			Type:         TypeChoice,
			Instructions: "Classify the current market regime for " + req.Ticker + " from the indicators and price action in the state.",
			Criteria: map[string]string{
				"trending":     "A sustained directional move with trend-strength confirmation.",
				"ranging":      "Sideways price action between identifiable support and resistance.",
				"transitional": "The regime is changing or mixed; neither trending nor ranging clearly applies.",
			},
		},
	}
	if HoldsLong(req) {
		qs[QExit] = Question{
			Type: TypeChoice,
			Instructions: "The portfolio already holds a long position in " + req.Ticker +
				". Based only on the supplied state, should that position be exited now?",
			Criteria: map[string]string{
				ExitNow:         "The long thesis is invalidated or materially weakened; exit now.",
				ExitHold:        "The long thesis remains intact; keep holding.",
				OptInsufficient: "The supplied state is not sufficient to judge.",
			},
		}
		return qs
	}

	qs[QThesis] = Question{
		Type:         TypeChoice,
		Instructions: "How strongly does the supplied evidence support opening a new long position in " + req.Ticker + "?",
		Criteria: map[string]string{
			ThesisStrong:    "Multiple independent pieces of evidence clearly support a long thesis.",
			ThesisModerate:  "The evidence supports a long thesis with some caveats.",
			ThesisWeak:      "Some supporting evidence, but it is thin or contradicted.",
			ThesisNone:      "The evidence does not support a long thesis.",
			OptInsufficient: "The supplied state is not sufficient to judge.",
		},
	}
	lensOptions := map[string]string{
		OptNone:         "No candidate lens fits the current regime and evidence.",
		OptInsufficient: "The supplied state is not sufficient to judge which lens fits.",
	}
	for _, lens := range AdmissibleLenses(req) {
		lensOptions[lens] = lensDescription(lens)
	}
	qs[QLens] = Question{
		Type: TypeChoice,
		Instructions: "Which candidate lens does the current regime and evidence for " + req.Ticker +
			" actually support? Choose by fit, not by which yields a trade.",
		Criteria: lensOptions,
	}
	qs[QTiming] = Question{
		Type:         TypeChoice,
		Instructions: "Is now a suitable time to enter a long position in " + req.Ticker + ", given extension, recent move, and catalyst freshness in the state?",
		Criteria: map[string]string{
			TimingEnterNow:  "Entry now offers a reasonable, defined-risk entry.",
			TimingWait:      "A concrete blocker (over-extension, an imminent event, unconfirmed move) makes waiting better.",
			OptInsufficient: "The supplied state is not sufficient to judge timing.",
		},
	}
	return qs
}

// State is the System One state for a request: the canonical snapshot
// inputs (exactly what was frozen for the decision) under a short framing.
type State struct {
	Task   string          `json:"task"`
	Inputs snapshot.Inputs `json:"inputs"`
}

// BuildState frames the frozen request for Jev.
func BuildState(req analysis.AnalysisRequest) State {
	return State{
		Task: "Evaluate a long-only equity trading decision for " + req.Ticker +
			" using only this point-in-time data. decision_cutoff is the evaluation time; news ages are relative to it. " +
			"recent_candles are newest first.",
		Inputs: snapshot.InputsFromRequest(req),
	}
}
