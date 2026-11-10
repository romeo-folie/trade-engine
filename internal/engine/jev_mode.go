package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/romeo-folie/trade-engine/internal/analysis"
	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/jev"
	"github.com/romeo-folie/trade-engine/internal/logger"
)

// Jev decision mode (issue #590). In authoritative mode Jev is the sole
// decision maker: no analysts or arbiter run, and its typed answers are
// composed by the versioned jev policy into the response that flows through
// the unchanged engine path (final lens audit, persistence, risk checks,
// entry gates, execution). In shadow mode the configured analyst workflow
// decides and executes; Jev runs concurrently and is only recorded.

// jevRole marks Jev results in AnalyzeResult.AnalystResults; snapshots map
// it to the "jev" stage and the web topology to the Jev node.
const jevRole = "jev"

// jevSourceNode is the web topology node for authoritative Jev decisions.
const jevSourceNode = "jev"

// errJevShadowGrace marks a shadow call abandoned after the baseline finished.
var errJevShadowGrace = errors.New("jev shadow evaluation exceeded its grace period after the baseline decision")

// newJevEvaluator builds the evaluator from config. MinConfidence defaults to
// the risk manager's confidence threshold so policy BUYs are risk-acceptable.
func newJevEvaluator(cfg *config.Config) (*jev.Evaluator, error) {
	jc := cfg.Analysis.Jev
	client, err := jev.NewClient(jev.ClientConfig{
		BaseURL: jc.BaseURL, APIKey: jc.APIKey, Model: jc.Model,
		Timeout: time.Duration(jc.TimeoutSec) * time.Second, MaxRetries: jc.MaxRetries,
	})
	if err != nil {
		return nil, err
	}
	policy := jevPolicyConfig(cfg)
	if err := policy.Validate(); err != nil {
		return nil, err
	}
	opts := jevOptions(cfg)
	if opts.Shorts {
		if err := policy.ValidateShorts(); err != nil {
			return nil, err
		}
	}
	return &jev.Evaluator{Client: client, Policy: policy, MinRiskReward: cfg.Risk.MinRiskReward, Options: opts}, nil
}

func jevPolicyConfig(cfg *config.Config) jev.PolicyConfig {
	p := cfg.Analysis.Jev.Policy
	minConf := p.MinConfidence
	if minConf == 0 {
		minConf = cfg.Risk.ConfidenceThreshold
	}
	return jev.PolicyConfig{
		MinThesisSupport: p.MinThesisSupport, MaxAdverse: p.MaxAdverse,
		MinLensProbability: p.MinLensProbability, MinTimingProbability: p.MinTimingProbability,
		ExitThreshold: p.ExitThreshold, StopATRMultiple: p.StopATRMultiple,
		TargetRMultiple: p.TargetRMultiple, SizePct: p.SizePct, MinConfidence: minConf,
		ShortMinThesisSupport: p.ShortMinThesisSupport, ShortMaxAdverse: p.ShortMaxAdverse,
		CoverThreshold: p.CoverThreshold, ShortSizePct: cfg.Risk.Shorts.SizePct,
	}
}

// jevOptions selects the two-sided v2 question set and policy when short
// selling is enabled (#599); otherwise v1 runs unchanged.
func jevOptions(cfg *config.Config) jev.Options {
	return jev.Options{Shorts: cfg.Risk.Shorts.Enabled}
}

// checkJevStartup refuses to run a Jev configuration that cannot be honoured:
// a missing evaluator, or authoritative Jev against a live account without
// the explicit allow_live opt-in. It runs after runtime overrides, which can
// switch paper/live at startup.
func (e *Engine) checkJevStartup() error {
	if !e.cfg.Analysis.JevEnabled() {
		return nil
	}
	if e.jevEvaluator == nil {
		return fmt.Errorf("analysis.decision_mode=jev but the Jev evaluator is unavailable: %v", e.jevInitErr)
	}
	stocks := e.cfg.Markets.Stocks
	if e.cfg.Analysis.JevAuthoritative() && stocks.Enabled && !stocks.Paper && !e.cfg.Analysis.Jev.AllowLive {
		return errors.New("analysis.jev.authority=authoritative on a live account requires analysis.jev.allow_live=true")
	}
	return nil
}

type jevAudit struct {
	Trace   jev.Trace             `json:"trace"`
	Answers map[string]jev.Answer `json:"answers,omitempty"`
	// Shadow-only: the decision the engine actually executed, for paired
	// comparison on identical inputs.
	ExecutedAction analysis.Action `json:"executed_action,omitempty"`
	Agrees         *bool           `json:"agrees,omitempty"`
}

func (e *Engine) jevResult(d jev.Decision, err error, executed *analysis.AnalysisResponse) analysis.AnalystResult {
	audit := jevAudit{Trace: d.Trace, Answers: d.Answers}
	if audit.Trace.PolicyVersion == "" {
		// A failed call never reached the policy; still record which policy
		// and question set it was asked under.
		questionSet, policy := jevOptions(e.cfg).Versions()
		audit.Trace.PolicyVersion, audit.Trace.QuestionSetVersion = policy, questionSet
	}
	if executed != nil {
		audit.ExecutedAction = executed.Action
		if err == nil {
			agrees := d.Response.Action == executed.Action
			audit.Agrees = &agrees
		}
	}
	extra, _ := json.Marshal(audit)
	model := d.Meta.Model
	if model == "" {
		model = e.cfg.Analysis.Jev.Model
	}
	return analysis.AnalystResult{
		Provider: jev.ProviderName, Model: model, Role: jevRole,
		Response: d.Response, Meta: d.Meta, Err: err, LatencyMs: d.Meta.LatencyMs, Extra: extra,
	}
}

// analyzeWithJev is the authoritative path: Jev alone decides. Errors are
// analysis failures (no fallback model); protective exits are unaffected.
func (e *Engine) analyzeWithJev(ctx context.Context, req analysis.AnalysisRequest) (analysis.AnalyzeResult, *jev.Trace, error) {
	d, err := e.jevEvaluator.Evaluate(ctx, req)
	result := analysis.AnalyzeResult{Response: d.Response, Meta: d.Meta}
	if d.Meta.Provider != "" || err != nil {
		// A call was made (or attempted): record it as the decision's stage.
		result.AnalystResults = []analysis.AnalystResult{e.jevResult(d, err, nil)}
	}
	if err != nil {
		return result, nil, err
	}
	return result, &d.Trace, nil
}

// jevShadow is a running shadow evaluation.
type jevShadow struct {
	done   chan struct{}
	cancel context.CancelFunc
	d      jev.Decision
	err    error
}

// startJevShadow launches Jev concurrently with the baseline analysis. Its
// context is detached from the baseline's result but not from shutdown.
func (e *Engine) startJevShadow(ctx context.Context, req analysis.AnalysisRequest) *jevShadow {
	if !e.cfg.Analysis.JevShadow() || e.jevEvaluator == nil {
		return nil
	}
	sctx, cancel := context.WithCancel(ctx)
	s := &jevShadow{done: make(chan struct{}), cancel: cancel}
	go func() {
		defer close(s.done)
		s.d, s.err = e.jevEvaluator.Evaluate(sctx, req)
	}()
	return s
}

// collect waits for the shadow result for at most the configured grace after
// the baseline finished, then records it against the executed decision
// (nil when the baseline failed). A shadow call never changes the decision.
func (s *jevShadow) collect(e *Engine, executed *analysis.AnalysisResponse) *analysis.AnalystResult {
	if s == nil {
		return nil
	}
	grace := time.Duration(e.cfg.Analysis.Jev.ShadowGraceMs) * time.Millisecond
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case <-s.done:
	case <-timer.C:
		s.cancel()
		<-s.done // Evaluate returns promptly once cancelled.
		// It may have finished successfully just as the timer fired; only a
		// call that actually failed is reported as exceeding the grace.
		if s.err != nil {
			s.err = fmt.Errorf("%w: %w", errJevShadowGrace, s.err)
		}
	}
	s.cancel()
	if s.err != nil {
		logger.Warn("jev shadow evaluation failed", "err", s.err)
	}
	r := e.jevResult(s.d, s.err, executed)
	if m := r.Meta; m.Provider != "" && (m.InputTokens > 0 || m.OutputTokens > 0) {
		e.costTracker.Record(m.Provider, m.Model, m.InputTokens, m.OutputTokens)
	}
	return &r
}

// results is collect as a slice for snapshot completion (empty when no
// shadow was started).
func (s *jevShadow) results(e *Engine, executed *analysis.AnalysisResponse) []analysis.AnalystResult {
	if r := s.collect(e, executed); r != nil {
		return []analysis.AnalystResult{*r}
	}
	return nil
}

// decisionMakerAvailable reports whether the configured decision maker can
// run: the Jev evaluator in authoritative mode, otherwise the primary analyst.
func (e *Engine) decisionMakerAvailable() bool {
	if e.cfg.Analysis.JevAuthoritative() {
		return e.jevEvaluator != nil
	}
	if e.analyzer == nil {
		return false
	}
	primary := e.analyzer.Primary()
	return primary != nil && primary.IsAvailable()
}
