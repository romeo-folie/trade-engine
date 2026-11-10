package jev

import (
	"fmt"
	"math"
	"strings"

	"github.com/romeo-folie/trade-engine/internal/analysis"
)

// Version 2 adds the short side (issue #599). It is used only when short
// selling is enabled; otherwise v1 (long-only) runs unchanged, with the same
// questions, cost, and versions.
const (
	QuestionSetVersionV2 = "jev-questions/v2"
	PolicyVersionV2      = "jev-policy/v2"
)

// Short-side question names.
const (
	QShortThesis  = "short_thesis_support"
	QShortAdverse = "material_evidence_against_short"
	QShortLens    = "short_lens"
	QShortTiming  = "short_entry_timing"
	QCover        = "cover"
)

// Cover options.
const (
	CoverNow  = "cover_now"
	CoverHold = "hold_short"
)

// Short-side reason codes.
const (
	ReasonConflicting       = "conflicting_directions"
	ReasonShortEntry        = "short_entry_qualified"
	ReasonCoverSignal       = "cover_signal"
	ReasonHoldShort         = "short_hold"
	ReasonCoverInsufficient = "cover_insufficient_evidence"
)

// Options selects the question set and policy version.
type Options struct {
	// Shorts enables the v2 two-sided question set and policy.
	Shorts bool
}

// Versions returns the question-set and policy versions for opts.
func (o Options) Versions() (questionSet, policy string) {
	if o.Shorts {
		return QuestionSetVersionV2, PolicyVersionV2
	}
	return QuestionSetVersion, PolicyVersion
}

// shortLensDescriptions are the bearish preconditions for each lens.
// pullback_strength describes a long setup and is never a short lens.
var shortLensDescriptions = map[string]string{
	"trend_following": "An established downtrend: price below falling EMAs, ADX showing trend strength, lower highs and lower lows.",
	"mean_reversion":  "A ranging market where price is stretched far above its mean (Bollinger upper extreme, overbought RSI) and likely to revert down.",
	"momentum":        "Strong recent downside price and volume acceleration that is still being confirmed, without capitulation or excessive extension.",
	"news_sentiment":  "A fresh (under ~24h) credible negative catalyst that price and volume are confirming and have not yet fully absorbed.",
}

// ShortLenses returns the admissible lenses that can describe a short.
func ShortLenses(req analysis.AnalysisRequest) []string {
	var out []string
	for _, l := range AdmissibleLenses(req) {
		if _, ok := shortLensDescriptions[l]; ok {
			out = append(out, l)
		}
	}
	return out
}

// HoldsShort reports whether the portfolio holds a short in the ticker.
func HoldsShort(req analysis.AnalysisRequest) bool {
	for _, p := range req.Portfolio.OpenPositions {
		if strings.EqualFold(p.Ticker, req.Ticker) && strings.EqualFold(p.Side, "short") && p.Quantity != 0 {
			return true
		}
	}
	return false
}

// NeedsCallWith reports whether a request needs a Jev call under opts. An
// exit-only row with no position never does; with shorts enabled a held short
// does (to decide on covering).
func NeedsCallWith(req analysis.AnalysisRequest, opts Options) bool {
	if opts.Shorts && HoldsShort(req) {
		return true
	}
	return NeedsCall(req)
}

// QuestionsWith builds the question set for opts. With shorts disabled it is
// exactly the v1 set.
func QuestionsWith(req analysis.AnalysisRequest, opts Options) map[string]Question {
	if !opts.Shorts {
		return Questions(req)
	}
	if !NeedsCallWith(req, opts) {
		return map[string]Question{}
	}
	if HoldsShort(req) {
		// Built directly, not derived from Questions: for an exit-only row
		// Questions returns an empty set, which would send a blank regime
		// question and leave the short unmanaged.
		return map[string]Question{
			QRegime: regimeQuestion(req),
			QCover: {
				Type: TypeChoice,
				Instructions: "The portfolio holds a short position in " + req.Ticker +
					". Based only on the supplied state, should that short be covered (bought back) now?",
				Criteria: map[string]string{
					CoverNow:        "The bearish thesis is invalidated or materially weakened; cover now.",
					CoverHold:       "The bearish thesis remains intact; keep the short.",
					OptInsufficient: "The supplied state is not sufficient to judge.",
				},
			},
		}
	}
	qs := Questions(req) // v1: exit+regime when long is held, else the long entry set
	if HoldsLong(req) || ExitOnly(req) {
		// A held long can only be exited (no flips); an exit-only row never
		// opens a position on either side.
		return qs
	}
	qs[QShortThesis] = Question{
		Type:         TypeChoice,
		Instructions: "How strongly does the supplied evidence support opening a new SHORT position in " + req.Ticker + " (profiting if the price falls)?",
		Criteria: map[string]string{
			ThesisStrong:    "Multiple independent pieces of evidence clearly support a short thesis.",
			ThesisModerate:  "The evidence supports a short thesis with some caveats.",
			ThesisWeak:      "Some bearish evidence, but it is thin or contradicted.",
			ThesisNone:      "The evidence does not support a short thesis.",
			OptInsufficient: "The supplied state is not sufficient to judge.",
		},
	}
	qs[QShortAdverse] = Question{
		Type: TypeNoul,
		Instructions: "Using only the supplied state, is there material evidence AGAINST shorting " + req.Ticker +
			" now (for example a positive catalyst, squeeze risk from a sharp recent decline or heavy short interest, an imminent earnings release, or strong support)?",
		Criteria: map[string]string{
			"true":  "Material evidence against a short is present in the supplied state.",
			"false": "No material evidence against a short is present in the supplied state.",
		},
	}
	lensOptions := map[string]string{
		OptNone:         "No candidate lens fits a short in the current regime and evidence.",
		OptInsufficient: "The supplied state is not sufficient to judge which lens fits a short.",
	}
	for _, l := range ShortLenses(req) {
		lensOptions[l] = shortLensDescriptions[l]
	}
	qs[QShortLens] = Question{
		Type: TypeChoice,
		Instructions: "For a possible SHORT in " + req.Ticker + ", which candidate lens does the current regime and evidence actually support? " +
			"Choose by fit, not by which yields a trade.",
		Criteria: lensOptions,
	}
	qs[QShortTiming] = Question{
		Type:         TypeChoice,
		Instructions: "Is now a suitable time to open a SHORT in " + req.Ticker + ", given how far price has already fallen, recent move, and catalyst freshness?",
		Criteria: map[string]string{
			TimingEnterNow:  "Shorting now offers a reasonable, defined-risk entry.",
			TimingWait:      "A concrete blocker (already collapsed or over-extended to the downside, an imminent event, an unconfirmed move) makes waiting better.",
			OptInsufficient: "The supplied state is not sufficient to judge timing.",
		},
	}
	return qs
}

// ResolveWith composes answers under opts. With shorts disabled it is exactly
// Resolve (v1).
func ResolveWith(req analysis.AnalysisRequest, answers map[string]Answer, cfg PolicyConfig, minRiskReward float64, opts Options) (analysis.AnalysisResponse, Trace, error) {
	if !opts.Shorts {
		return Resolve(req, answers, cfg, minRiskReward)
	}
	qsVersion, policyVersion := opts.Versions()
	trace := Trace{PolicyVersion: policyVersion, QuestionSetVersion: qsVersion, Held: HoldsLong(req) || HoldsShort(req), Values: map[string]float64{}}
	if err := cfg.Validate(); err != nil {
		return analysis.AnalysisResponse{}, trace, err
	}
	if err := cfg.ValidateShorts(); err != nil {
		return analysis.AnalysisResponse{}, trace, err
	}
	if math.IsNaN(minRiskReward) || math.IsInf(minRiskReward, 0) || minRiskReward < 0 {
		return analysis.AnalysisResponse{}, trace, fmt.Errorf("jev policy: min risk/reward must be finite and non-negative, got %v", minRiskReward)
	}
	if HoldsLong(req) && HoldsShort(req) {
		// Both sides at once is a reconciliation problem, never a decision.
		return analysis.AnalysisResponse{}, trace, fmt.Errorf("jev policy: %s holds both a long and a short position", req.Ticker)
	}
	if !NeedsCallWith(req, opts) {
		return relabel(Resolve(req, answers, cfg, minRiskReward))(trace)
	}
	if err := ValidateAnswers(QuestionsWith(req, opts), answers); err != nil {
		return analysis.AnalysisResponse{}, trace, err
	}
	regime := answers[QRegime].Choice
	switch {
	case HoldsShort(req):
		return resolveCover(answers, cfg, regime, trace)
	case HoldsLong(req):
		// A held long is only ever exited or held (no flips): the v1 rule.
		resp, held, err := resolveHeld(answers, cfg, regime, trace)
		resp.Reasoning = reasoning(held)
		return resp, held, err
	}

	longResp, longTrace, err := resolveEntry(req, answers, cfg, minRiskReward, regime, *answers[QAdverse].Noul,
		Trace{Values: map[string]float64{"adverse": *answers[QAdverse].Noul}})
	if err != nil {
		return analysis.AnalysisResponse{}, trace, err
	}
	shortResp, shortTrace := resolveShortEntry(req, answers, cfg, minRiskReward, regime)

	longQ, shortQ := longResp.Action == analysis.ActionBuy, shortResp.Action == analysis.ActionShort
	longWait, shortWait := longResp.Action == analysis.ActionWait, shortResp.Action == analysis.ActionWait
	mergeBoth := func() {
		for _, c := range longTrace.ReasonCodes {
			trace.ReasonCodes = append(trace.ReasonCodes, "long:"+c)
		}
		for _, c := range shortTrace.ReasonCodes {
			trace.ReasonCodes = append(trace.ReasonCodes, "short:"+c)
		}
		for k, v := range longTrace.Values {
			trace.Values["long_"+k] = v
		}
		for k, v := range shortTrace.Values {
			trace.Values["short_"+k] = v
		}
	}
	mergeBoth()
	switch {
	case (longQ || longWait) && (shortQ || shortWait):
		// Both directions are plausible: the evidence contradicts itself.
		trace.ReasonCodes = append(trace.ReasonCodes, ReasonConflicting)
		resp := analysis.AnalysisResponse{Action: analysis.ActionHold, Regime: regime, SelectedStrategy: OptNone,
			DecisiveReason: "Jev supported both a long and a short setup; contradictory evidence is not traded."}
		resp.Reasoning = reasoning(trace)
		return resp, trace, nil
	case longQ || longWait:
		trace.SelectedLens = longTrace.SelectedLens
		longResp.Reasoning = reasoning(trace)
		return longResp, trace, nil
	case shortQ || shortWait:
		trace.SelectedLens = shortTrace.SelectedLens
		shortResp.Reasoning = reasoning(trace)
		return shortResp, trace, nil
	default:
		resp := analysis.AnalysisResponse{Action: analysis.ActionHold, Regime: regime, SelectedStrategy: OptNone,
			Confidence:     math.Max(longResp.Confidence, shortResp.Confidence),
			DecisiveReason: "Neither a long nor a short setup qualified: " + longResp.DecisiveReason + " / " + shortResp.DecisiveReason}
		resp.Reasoning = reasoning(trace)
		return resp, trace, nil
	}
}

// relabel re-stamps a v1 resolution's trace with the v2 versions.
func relabel(resp analysis.AnalysisResponse, t Trace, err error) func(Trace) (analysis.AnalysisResponse, Trace, error) {
	return func(base Trace) (analysis.AnalysisResponse, Trace, error) {
		t.PolicyVersion, t.QuestionSetVersion = base.PolicyVersion, base.QuestionSetVersion
		resp.Reasoning = reasoning(t)
		return resp, t, err
	}
}

func resolveCover(answers map[string]Answer, cfg PolicyConfig, regime string, trace Trace) (analysis.AnalysisResponse, Trace, error) {
	cover := answers[QCover]
	p := cover.Probabilities[CoverNow]
	trace.Values["p_cover"] = p
	resp := analysis.AnalysisResponse{Regime: regime, SelectedStrategy: OptNone, Confidence: round3(p)}
	switch {
	case cover.Choice == OptInsufficient:
		trace.ReasonCodes = append(trace.ReasonCodes, ReasonCoverInsufficient)
		resp.Action = analysis.ActionHold
		resp.DecisiveReason = "Jev found the state insufficient to judge a cover; the short is held under its protective stop."
	case p >= cfg.CoverThreshold:
		trace.ReasonCodes = append(trace.ReasonCodes, ReasonCoverSignal)
		resp.Action = analysis.ActionCover
		resp.DecisiveReason = fmt.Sprintf("Jev cover probability %.2f met the %.2f cover threshold.", p, cfg.CoverThreshold)
	default:
		trace.ReasonCodes = append(trace.ReasonCodes, ReasonHoldShort)
		resp.Action = analysis.ActionHold
		resp.DecisiveReason = fmt.Sprintf("Jev cover probability %.2f is below the %.2f cover threshold; the short is held.", p, cfg.CoverThreshold)
	}
	resp.Reasoning = reasoning(trace)
	return resp, trace, nil
}

// resolveShortEntry mirrors resolveEntry for a short, with its own
// thresholds and an inverted plan (stop above entry, target below).
func resolveShortEntry(req analysis.AnalysisRequest, answers map[string]Answer, cfg PolicyConfig, minRiskReward float64, regime string) (analysis.AnalysisResponse, Trace) {
	thesis, lens, timing := answers[QShortThesis], answers[QShortLens], answers[QShortTiming]
	adverse := *answers[QShortAdverse].Noul
	support := math.Min(1, thesis.Probabilities[ThesisStrong]+thesis.Probabilities[ThesisModerate])
	trace := Trace{Values: map[string]float64{"adverse": adverse, "thesis_support": support, "p_enter_now": timing.Probabilities[TimingEnterNow]}}
	confidence := round3(math.Min(1, math.Max(0, support*(1-adverse))))
	resp := analysis.AnalysisResponse{Regime: regime, SelectedStrategy: OptNone, Confidence: confidence}
	stop := func(action analysis.Action, code, reason string) (analysis.AnalysisResponse, Trace) {
		trace.ReasonCodes = append(trace.ReasonCodes, code)
		resp.Action, resp.DecisiveReason = action, reason
		return resp, trace
	}
	if thesis.Choice == OptInsufficient || lens.Choice == OptInsufficient || timing.Choice == OptInsufficient {
		return stop(analysis.ActionHold, ReasonInsufficient, "Jev found the state insufficient to judge a short.")
	}
	if adverse > cfg.ShortMaxAdverse {
		return stop(analysis.ActionHold, ReasonAdverse, fmt.Sprintf("Jev evidence-against-short probability %.2f exceeds the %.2f limit.", adverse, cfg.ShortMaxAdverse))
	}
	if support < cfg.ShortMinThesisSupport {
		return stop(analysis.ActionHold, ReasonThesis, fmt.Sprintf("Jev short thesis support %.2f is below the %.2f minimum.", support, cfg.ShortMinThesisSupport))
	}
	if lens.Choice == OptNone {
		return stop(analysis.ActionHold, ReasonNoLens, "Jev found no candidate lens that fits a short.")
	}
	pLens := lens.Probabilities[lens.Choice]
	trace.Values["p_lens"] = pLens
	if pLens < cfg.MinLensProbability {
		return stop(analysis.ActionHold, ReasonLensLow, fmt.Sprintf("Jev short lens %s probability %.2f is below the %.2f minimum.", lens.Choice, pLens, cfg.MinLensProbability))
	}
	trace.SelectedLens = lens.Choice
	resp.SelectedStrategy = lens.Choice
	resp.RejectedStrategies = shortRejectedLenses(AdmissibleLenses(req), lens)
	if timing.Choice == TimingWait {
		return stop(analysis.ActionWait, ReasonTimingWait, "Jev judged that waiting for a better short entry is preferable.")
	}
	if p := timing.Probabilities[TimingEnterNow]; p < cfg.MinTimingProbability {
		return stop(analysis.ActionWait, ReasonTimingLow, fmt.Sprintf("Jev short enter-now probability %.2f is below the %.2f minimum.", p, cfg.MinTimingProbability))
	}
	if confidence < cfg.MinConfidence {
		resp.SelectedStrategy, resp.RejectedStrategies, trace.SelectedLens = OptNone, nil, ""
		return stop(analysis.ActionHold, ReasonLowConfidence, fmt.Sprintf("Jev short confidence %.3f is below the %.2f minimum.", confidence, cfg.MinConfidence))
	}
	entry, atr := entryPrice(req), req.Indicators.ATR
	if !(entry > 0) || !(atr > 0) || math.IsInf(entry, 0) || math.IsInf(atr, 0) {
		resp.SelectedStrategy, resp.RejectedStrategies, trace.SelectedLens = OptNone, nil, ""
		return stop(analysis.ActionHold, ReasonPlanUnavailable, "A defined-risk short plan could not be derived: entry price or ATR is unavailable.")
	}
	stopPrice := ceil4(entry + cfg.StopATRMultiple*atr)
	r := math.Max(cfg.TargetRMultiple, minRiskReward)
	target := shortBracketTarget(entry, stopPrice, r)
	if !(stopPrice > entry) || !(target > 0) || math.IsInf(stopPrice, 0) {
		resp.SelectedStrategy, resp.RejectedStrategies, trace.SelectedLens = OptNone, nil, ""
		return stop(analysis.ActionHold, ReasonPlanUnavailable, "A defined-risk short plan could not be derived: the target falls at or below zero.")
	}
	trace.Values["entry"], trace.Values["atr"], trace.Values["risk_reward"] = entry, atr, r
	trace.ReasonCodes = append(trace.ReasonCodes, ReasonShortEntry)
	resp.Action = analysis.ActionShort
	resp.SizePct = cfg.ShortSizePct
	resp.StopLoss, resp.TakeProfit = stopPrice, target
	resp.DecisiveReason = fmt.Sprintf("Jev supported a %s short: thesis support %.2f, evidence against %.2f, enter-now %.2f.",
		lens.Choice, support, adverse, timing.Probabilities[TimingEnterNow])
	return resp, trace
}

// shortBracketTarget returns the largest 4-decimal target at or below
// entry - r*(stop-entry) whose realised reward/risk, computed from the stored
// prices as the risk manager does, is at least r.
func shortBracketTarget(entry, stop, r float64) float64 {
	risk := stop - entry
	target := math.Floor((entry-r*risk)*10000) / 10000
	for i := 0; i < 4 && (entry-target)/risk < r; i++ {
		target = round4(target - 0.0001)
	}
	return target
}

func ceil4(v float64) float64 { return math.Ceil(v*10000) / 10000 }

// shortRejectedLenses rejects every admissible lens other than the chosen
// short lens, including long-only lenses, so the engine's rejection-coverage
// audit is satisfied with measured facts.
func shortRejectedLenses(admissible []string, lens Answer) map[string]string {
	out := map[string]string{}
	for _, l := range admissible {
		if l == lens.Choice {
			continue
		}
		if _, ok := shortLensDescriptions[l]; !ok {
			out[l] = "not a short lens"
			continue
		}
		out[l] = fmt.Sprintf("Jev short lens probability %.2f vs %.2f for %s", lens.Probabilities[l], lens.Probabilities[lens.Choice], lens.Choice)
	}
	return out
}
