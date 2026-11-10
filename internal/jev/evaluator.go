package jev

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/romeo-folie/trade-engine/internal/analysis"
)

// Decision is one Jev decision: the engine-compatible response, the policy
// trace, the raw typed answers, and the call's metadata.
type Decision struct {
	Response analysis.AnalysisResponse
	Trace    Trace
	Answers  map[string]Answer
	Meta     analysis.ModelCallMeta
}

// Evaluator asks Jev the v1 question set about a frozen request and resolves
// the answers with the v1 policy.
type Evaluator struct {
	Client        *Client
	Policy        PolicyConfig
	MinRiskReward float64
	// Options selects v1 (long-only) or v2 (two-sided, shorts enabled).
	Options Options
}

// Evaluate returns a decision, or an error that still carries the attempted
// call's metadata (and, for malformed answers, the raw response). Callers must
// treat any error as an analysis failure: there is no fallback model.
func (e *Evaluator) Evaluate(ctx context.Context, req analysis.AnalysisRequest) (Decision, error) {
	if !NeedsCallWith(req, e.Options) {
		// Exit-only row with no position: the only valid outcome is HOLD, so
		// no call (and no cost) is made. Meta stays empty: there was no call.
		out, trace, err := ResolveWith(req, nil, e.Policy, e.MinRiskReward, e.Options)
		return Decision{Response: out, Trace: trace}, err
	}
	questions := QuestionsWith(req, e.Options)
	questionSet, policy := e.Options.Versions()
	resp, meta, err := e.Client.Evaluate(ctx, BuildStateWith(req, e.Options), questions, questionSet)
	// The trace carries the versions the call was made under even when it
	// fails before the policy runs, so failed calls are attributable.
	d := Decision{Meta: meta, Answers: resp.Answers, Trace: Trace{
		PolicyVersion: policy, QuestionSetVersion: questionSet,
		Held: HoldsLong(req) || HoldsShort(req), Values: map[string]float64{},
	}}
	if err != nil {
		return d, err
	}
	out, trace, err := ResolveWith(req, resp.Answers, e.Policy, e.MinRiskReward, e.Options)
	d.Trace = trace
	if err != nil {
		return d, fmt.Errorf("jev resolve: %w", err)
	}
	d.Response = out
	return d, nil
}

// TraceJSON encodes a trace for audit storage.
func TraceJSON(t Trace) (json.RawMessage, error) { return json.Marshal(t) }
