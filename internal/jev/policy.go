package jev

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/romeo-folie/trade-engine/internal/analysis"
)

// PolicyVersion identifies the composition rules below. Bump it with any
// change to thresholds' meaning, ordering, or plan derivation; it is recorded
// on every snapshot so decisions under different policies stay distinct.
const PolicyVersion = "jev-policy/v1"

// PolicyConfig holds the policy's tunable thresholds. Values are calibrated
// on this project's data; the defaults are conservative starting points, not
// validated probabilities of profitable trades.
type PolicyConfig struct {
	// MinThesisSupport is the minimum P(strong) + P(moderate) thesis support.
	MinThesisSupport float64
	// MaxAdverse is the highest tolerated P(material adverse evidence).
	MaxAdverse float64
	// MinLensProbability is the minimum probability of the chosen lens.
	MinLensProbability float64
	// MinTimingProbability is the minimum P(enter_now) to enter immediately.
	MinTimingProbability float64
	// ExitThreshold is the P(exit_now) at which a held long is exited.
	ExitThreshold float64
	// StopATRMultiple places the stop this many ATRs below entry.
	StopATRMultiple float64
	// TargetRMultiple places the target this many risk units above entry; the
	// engine's minimum risk/reward is applied as a floor.
	TargetRMultiple float64
	// SizePct is the incremental BUY size as a percent of the sizing base.
	SizePct float64
}

// DefaultPolicyConfig returns the v1 starting thresholds.
func DefaultPolicyConfig() PolicyConfig {
	return PolicyConfig{
		MinThesisSupport: 0.60, MaxAdverse: 0.40, MinLensProbability: 0.50,
		MinTimingProbability: 0.55, ExitThreshold: 0.60,
		StopATRMultiple: 2.0, TargetRMultiple: 2.0, SizePct: 2.0,
	}
}

// Validate rejects thresholds that would make the policy incoherent.
func (c PolicyConfig) Validate() error {
	for name, v := range map[string]float64{
		"min_thesis_support": c.MinThesisSupport, "max_adverse": c.MaxAdverse,
		"min_lens_probability": c.MinLensProbability, "min_timing_probability": c.MinTimingProbability,
		"exit_threshold": c.ExitThreshold,
	} {
		if !finiteUnit(v) {
			return fmt.Errorf("jev policy: %s must be within [0,1], got %v", name, v)
		}
	}
	if !(c.StopATRMultiple > 0) || !(c.TargetRMultiple > 0) || !(c.SizePct > 0) || math.IsInf(c.SizePct, 0) {
		return fmt.Errorf("jev policy: stop_atr_multiple, target_r_multiple and size_pct must be positive")
	}
	return nil
}

// Reason codes recorded in the policy trace.
const (
	ReasonInsufficient     = "insufficient_evidence"
	ReasonAdverse          = "adverse_evidence"
	ReasonThesis           = "thesis_unsupported"
	ReasonNoLens           = "no_fitting_lens"
	ReasonLensLow          = "lens_low_confidence"
	ReasonTimingWait       = "timing_wait"
	ReasonTimingLow        = "timing_low_confidence"
	ReasonPlanUnavailable  = "plan_unavailable"
	ReasonEntry            = "entry_qualified"
	ReasonExit             = "exit_signal"
	ReasonHoldPosition     = "position_hold"
	ReasonExitInsufficient = "exit_insufficient_evidence"
)

// Trace is the auditable record of how answers became a decision. It holds
// measured values and reason codes only — Jev produces no narrative, and the
// policy does not invent one.
type Trace struct {
	PolicyVersion      string             `json:"policy_version"`
	QuestionSetVersion string             `json:"question_set_version"`
	Held               bool               `json:"held"`
	ReasonCodes        []string           `json:"reason_codes"`
	Values             map[string]float64 `json:"values"`
	SelectedLens       string             `json:"selected_lens,omitempty"`
}

// Resolve composes validated answers into an engine response. Answers must
// already satisfy ValidateAnswers for Questions(req); Resolve re-validates and
// returns ErrMalformed rather than guessing. minRiskReward is the engine's
// configured floor (0 when unset).
func Resolve(req analysis.AnalysisRequest, answers map[string]Answer, cfg PolicyConfig, minRiskReward float64) (analysis.AnalysisResponse, Trace, error) {
	trace := Trace{PolicyVersion: PolicyVersion, QuestionSetVersion: QuestionSetVersion,
		Held: HoldsLong(req), Values: map[string]float64{}}
	if err := cfg.Validate(); err != nil {
		return analysis.AnalysisResponse{}, trace, err
	}
	if err := ValidateAnswers(Questions(req), answers); err != nil {
		return analysis.AnalysisResponse{}, trace, err
	}
	regime := answers[QRegime].Choice
	adverse := *answers[QAdverse].Noul
	trace.Values["adverse"] = adverse

	if trace.Held {
		return resolveHeld(answers, cfg, regime, adverse, trace)
	}
	return resolveEntry(req, answers, cfg, minRiskReward, regime, adverse, trace)
}

func resolveHeld(answers map[string]Answer, cfg PolicyConfig, regime string, adverse float64, trace Trace) (analysis.AnalysisResponse, Trace, error) {
	exit := answers[QExit]
	pExit := exit.Probabilities[ExitNow]
	trace.Values["p_exit"] = pExit
	resp := analysis.AnalysisResponse{Regime: regime, SelectedStrategy: OptNone, Confidence: round3(pExit)}
	switch {
	case exit.Choice == OptInsufficient:
		trace.ReasonCodes = append(trace.ReasonCodes, ReasonExitInsufficient)
		resp.Action = analysis.ActionHold
		resp.DecisiveReason = "Jev found the state insufficient to judge an exit; the position is held under existing protective orders."
	case pExit >= cfg.ExitThreshold:
		trace.ReasonCodes = append(trace.ReasonCodes, ReasonExit)
		resp.Action = analysis.ActionSell
		resp.DecisiveReason = fmt.Sprintf("Jev exit probability %.2f met the %.2f exit threshold.", pExit, cfg.ExitThreshold)
	default:
		trace.ReasonCodes = append(trace.ReasonCodes, ReasonHoldPosition)
		resp.Action = analysis.ActionHold
		resp.DecisiveReason = fmt.Sprintf("Jev exit probability %.2f is below the %.2f exit threshold; the thesis is held.", pExit, cfg.ExitThreshold)
	}
	resp.Reasoning = reasoning(trace)
	return resp, trace, nil
}

func resolveEntry(req analysis.AnalysisRequest, answers map[string]Answer, cfg PolicyConfig, minRiskReward float64,
	regime string, adverse float64, trace Trace) (analysis.AnalysisResponse, Trace, error) {
	thesis, lens, timing := answers[QThesis], answers[QLens], answers[QTiming]
	support := thesis.Probabilities[ThesisStrong] + thesis.Probabilities[ThesisModerate]
	trace.Values["thesis_support"] = support
	trace.Values["p_enter_now"] = timing.Probabilities[TimingEnterNow]
	confidence := round3(support * (1 - adverse))
	resp := analysis.AnalysisResponse{Regime: regime, SelectedStrategy: OptNone, Confidence: confidence}

	hold := func(code, reason string) (analysis.AnalysisResponse, Trace, error) {
		trace.ReasonCodes = append(trace.ReasonCodes, code)
		resp.Action, resp.DecisiveReason = analysis.ActionHold, reason
		resp.Reasoning = reasoning(trace)
		return resp, trace, nil
	}

	// Order matters and is part of the versioned policy: abstentions first,
	// then disqualifying evidence, then lens fit, then timing, then the plan.
	if thesis.Choice == OptInsufficient || lens.Choice == OptInsufficient || timing.Choice == OptInsufficient {
		return hold(ReasonInsufficient, "Jev found the state insufficient to judge a new entry.")
	}
	if adverse > cfg.MaxAdverse {
		return hold(ReasonAdverse, fmt.Sprintf("Jev adverse-evidence probability %.2f exceeds the %.2f limit.", adverse, cfg.MaxAdverse))
	}
	if support < cfg.MinThesisSupport {
		return hold(ReasonThesis, fmt.Sprintf("Jev thesis support %.2f is below the %.2f minimum.", support, cfg.MinThesisSupport))
	}
	if lens.Choice == OptNone {
		return hold(ReasonNoLens, "Jev found no candidate lens that fits the current regime.")
	}
	pLens := lens.Probabilities[lens.Choice]
	trace.Values["p_lens"] = pLens
	if pLens < cfg.MinLensProbability {
		return hold(ReasonLensLow, fmt.Sprintf("Jev lens %s probability %.2f is below the %.2f minimum.", lens.Choice, pLens, cfg.MinLensProbability))
	}
	trace.SelectedLens = lens.Choice
	resp.SelectedStrategy = lens.Choice
	resp.RejectedStrategies = rejectedLenses(AdmissibleLenses(req), lens)

	wait := func(code, reason string) (analysis.AnalysisResponse, Trace, error) {
		trace.ReasonCodes = append(trace.ReasonCodes, code)
		resp.Action, resp.DecisiveReason = analysis.ActionWait, reason
		resp.Reasoning = reasoning(trace)
		return resp, trace, nil
	}
	if timing.Choice == TimingWait {
		return wait(ReasonTimingWait, "Jev judged that waiting for a better entry is preferable to entering now.")
	}
	if p := timing.Probabilities[TimingEnterNow]; p < cfg.MinTimingProbability {
		return wait(ReasonTimingLow, fmt.Sprintf("Jev enter-now probability %.2f is below the %.2f minimum.", p, cfg.MinTimingProbability))
	}

	entry, atr := entryPrice(req), req.Indicators.ATR
	if !(entry > 0) || !(atr > 0) || math.IsInf(entry, 0) || math.IsInf(atr, 0) {
		return hold(ReasonPlanUnavailable, "A defined-risk plan could not be derived: entry price or ATR is unavailable.")
	}
	stop := entry - cfg.StopATRMultiple*atr
	if !(stop > 0) {
		return hold(ReasonPlanUnavailable, "A defined-risk plan could not be derived: the ATR stop falls at or below zero.")
	}
	r := math.Max(cfg.TargetRMultiple, minRiskReward)
	target := entry + r*(entry-stop)
	trace.Values["entry"], trace.Values["atr"], trace.Values["risk_reward"] = entry, atr, r

	trace.ReasonCodes = append(trace.ReasonCodes, ReasonEntry)
	resp.Action = analysis.ActionBuy
	resp.SizePct = cfg.SizePct
	resp.StopLoss = round4(stop)
	resp.TakeProfit = round4(target)
	resp.DecisiveReason = fmt.Sprintf("Jev supported a %s entry: thesis support %.2f, adverse evidence %.2f, enter-now %.2f.",
		lens.Choice, support, adverse, timing.Probabilities[TimingEnterNow])
	resp.Reasoning = reasoning(trace)
	return resp, trace, nil
}

// entryPrice is the latest close, preferring the entry-timing timeframe when
// supplied (matching the arbiter's signal price), else the primary timeframe.
func entryPrice(req analysis.AnalysisRequest) float64 {
	for _, c := range req.TimeframeContexts {
		if c.Role == "entry_timing" && len(c.RecentCandles) > 0 {
			return c.RecentCandles[0].Close
		}
	}
	if len(req.RecentCandles) > 0 {
		return req.RecentCandles[0].Close
	}
	return 0
}

// rejectedLenses states, for every admissible lens not chosen, the measured
// reason: its Jev probability against the chosen lens's. This satisfies the
// engine's rejection-coverage audit with facts, not invented prose.
func rejectedLenses(admissible []string, lens Answer) map[string]string {
	out := map[string]string{}
	for _, l := range admissible {
		if l == lens.Choice {
			continue
		}
		out[l] = fmt.Sprintf("Jev lens probability %.2f vs %.2f for %s", lens.Probabilities[l], lens.Probabilities[lens.Choice], lens.Choice)
	}
	return out
}

func reasoning(t Trace) string {
	keys := make([]string, 0, len(t.Values))
	for k := range t.Values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%.4g", k, t.Values[k]))
	}
	return fmt.Sprintf("[%s] %s; %s", t.PolicyVersion, strings.Join(t.ReasonCodes, ","), strings.Join(parts, " "))
}

func round3(v float64) float64 { return math.Round(v*1000) / 1000 }
func round4(v float64) float64 { return math.Round(v*10000) / 10000 }
