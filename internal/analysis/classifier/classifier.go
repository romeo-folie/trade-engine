// Package classifier implements the two-stage analysis gate described in
// the cost-reduction PRD §7.2.
//
// Layer 1 (rule_based.go): zero-token, deterministic threshold evaluation.
// Layer 2 (llm_classifier.go): minimal LLM prompt for ambiguous cases.
package classifier

import "context"

// Decision represents the classifier outcome.
type Decision string

const (
	// DecisionSkip signals that LLM analysis should be skipped for this tick.
	DecisionSkip Decision = "SKIP"
	// DecisionAnalyze signals that LLM analysis should proceed.
	DecisionAnalyze Decision = "ANALYZE"
)

// Result is the classifier output.
type Result struct {
	Decision   Decision
	ReasonCode string
	Deltas     map[string]float64
}

// TickState carries the per-ticker indicator snapshot passed to the classifier.
type TickState struct {
	Ticker        string
	Timeframe     string
	Strategy      string
	RSIDelta      float64 // abs(current RSI - last RSI)
	EMAGapDelta   float64 // abs(current EMAGapPct - last EMAGapPct)
	PriceMovesPct float64 // abs((current - last) / last * 100)
	NewsHash      string  // current news digest hash
	LastNewsHash  string  // prior-tick news digest hash
	LastDecision  string  // BUY | SELL | HOLD | ""
	ElapsedMin    float64 // minutes since last analysis
	// HoldCooldown is deprecated: L1 now sources cooldown from ClassifierConfig.HoldCooldownMin.
	// Field retained for API compatibility; it is no longer read by RuleBased.Classify.
	HoldCooldown float64
}

// Classifier evaluates a tick state and returns a routing decision.
type Classifier interface {
	Classify(ctx context.Context, state TickState) (Result, error)
}
