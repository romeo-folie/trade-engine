package classifier

import (
	"context"

	"github.com/romeo-folie/trade-engine/internal/logger"
)

// Gate is the composed two-stage classifier gate. It runs Layer 1 first; if
// Layer 2 (LLM) is enabled and Layer 1 returns an ambiguous result (anything
// below a hard-trigger threshold), it consults Layer 2.
//
// Mode behaviour:
//   - "disabled": always returns DecisionAnalyze without consulting either layer.
//   - "shadow":   classifies but never blocks — logs WOULD_SKIP / WOULD_ANALYZE.
//   - "active":   classifier result gates analysis.
type Gate struct {
	mode   string
	layer1 *RuleBased
	layer2 *LLMClassifier // nil when LLM fallback is disabled
}

// NewGate builds the two-stage gate. layer2 may be nil.
func NewGate(mode string, layer1 *RuleBased, layer2 *LLMClassifier) *Gate {
	return &Gate{mode: mode, layer1: layer1, layer2: layer2}
}

// Classify runs the gate and returns whether analysis should proceed.
// Returns (proceed bool, res Result, err error). err is always nil — errors
// are absorbed internally (fail-open). When proceed is true, analysis runs;
// when false, the tick is skipped. res always contains the classifier result.
func (g *Gate) Classify(ctx context.Context, s TickState) (proceed bool, res Result, err error) {
	if g.mode == "disabled" {
		return true, Result{Decision: DecisionAnalyze, ReasonCode: "CLASSIFIER_DISABLED"}, nil
	}

	res, err = g.layer1.Classify(ctx, s)
	if err != nil {
		logger.Warn("classifier L1 error — failing open", "ticker", s.Ticker, "err", err.Error())
		return true, Result{Decision: DecisionAnalyze, ReasonCode: "L1_ERROR_FAIL_OPEN"}, nil
	}

	// If Layer 1 says SKIP, optionally consult Layer 2 for ambiguous cases.
	// Currently Layer 1 is decisive (no ambiguous state). Layer 2 fires only
	// when llm_fallback_enabled and Layer 1 says skip without hard triggers.
	if res.Decision == DecisionSkip && g.layer2 != nil && res.ReasonCode == "NO_MATERIAL_CHANGE" {
		l2res, l2err := g.layer2.Classify(ctx, s)
		if l2err == nil {
			res = l2res
		}
	}

	if g.mode == "shadow" {
		if res.Decision == DecisionSkip {
			logger.Info("classifier shadow: WOULD_SKIP",
				"ticker", s.Ticker, "reason", res.ReasonCode, "deltas", res.Deltas)
		} else {
			logger.Info("classifier shadow: WOULD_ANALYZE",
				"ticker", s.Ticker, "reason", res.ReasonCode)
		}
		// Shadow mode never blocks.
		return true, res, nil
	}

	// active mode
	return res.Decision == DecisionAnalyze, res, nil
}
