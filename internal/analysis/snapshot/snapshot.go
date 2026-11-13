// Package snapshot builds immutable decision snapshots (issue #589): the
// canonical inputs an analysis consumed, their identities, the sanitized
// effective configuration, and the per-stage model calls. It is pure: it never
// reads the store, the clock, or live portfolio state, so building a snapshot
// cannot change a decision.
package snapshot

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"runtime/debug"
	"slices"
	"time"

	"github.com/romeo-folie/trade-engine/internal/analysis"
	"github.com/romeo-folie/trade-engine/internal/analysis/prompts"
	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/store"
	"github.com/romeo-folie/trade-engine/internal/strategy/pullback"
	"github.com/romeo-folie/trade-engine/internal/tradeprofile"
)

// SchemaVersion is the snapshot contract version. Bump it when Inputs,
// FinalOutput, or stage-call semantics change incompatibly.
const SchemaVersion = 1

// Workflows recorded on a snapshot.
const (
	WorkflowSingle     = "single"
	WorkflowEnsemble   = "ensemble"
	WorkflowSpecialist = "specialist"
	WorkflowJev        = "jev"
)

// Inputs is everything prompt-visible or resolution-relevant that an analysis
// consumed, exactly as supplied. Field order and tags are part of the
// contract: input_hash is computed over this encoding.
type Inputs struct {
	Ticker               string                         `json:"ticker"`
	Timeframe            string                         `json:"timeframe"`
	TimeframeRole        prompts.TimeframeRole          `json:"timeframe_role,omitempty"`
	Strategy             string                         `json:"strategy"`
	Strategies           []string                       `json:"strategies"`
	MomentumPlay         bool                           `json:"momentum_play"`
	DecisionCutoff       time.Time                      `json:"decision_cutoff"`
	RiskPolicy           prompts.RiskPolicy             `json:"risk_policy"`
	RecentCandles        []analysis.CandleSnapshot      `json:"recent_candles"`
	Indicators           analysis.IndicatorSnapshot     `json:"indicators"`
	TimeframeContexts    []analysis.TimeframeContext    `json:"timeframe_contexts"`
	News                 []analysis.NewsSnippet         `json:"news"`
	Portfolio            analysis.PortfolioContext      `json:"portfolio"`
	KnowledgeContext     string                         `json:"knowledge_context"`
	PerformanceContext   string                         `json:"performance_context"`
	SimilarAnalyses      []analysis.SimilarSetup        `json:"similar_analyses"`
	DiscoveryContext     *tradeprofile.DiscoveryContext `json:"discovery_context,omitempty"`
	ExecutionProfile     *tradeprofile.ExecutionProfile `json:"execution_profile,omitempty"`
	PullbackContext      *pullback.PromptContext        `json:"pullback_context,omitempty"`
	PrimaryThesisContext *prompts.PrimaryThesisContext  `json:"primary_thesis_context,omitempty"`
}

// MissingBlock records an optional input that was absent or failed. Replay
// treats it as missing; it never re-fetches.
type MissingBlock struct {
	Block  string `json:"block"`
	Reason string `json:"reason"`
}

// Request restores frozen inputs for offline evaluation. It deliberately does
// not fetch missing blocks or use the current operator configuration.
func (in Inputs) Request() analysis.AnalysisRequest {
	return analysis.AnalysisRequest{
		Ticker: in.Ticker, Timeframe: in.Timeframe, TimeframeRole: in.TimeframeRole,
		Strategy: in.Strategy, Strategies: in.Strategies, MomentumPlay: in.MomentumPlay,
		DecisionCutoff: in.DecisionCutoff, RiskPolicy: in.RiskPolicy,
		RecentCandles: in.RecentCandles, Indicators: in.Indicators, TimeframeContexts: in.TimeframeContexts,
		News: in.News, Portfolio: in.Portfolio, KnowledgeContext: in.KnowledgeContext,
		PerformanceContext: in.PerformanceContext, SimilarAnalyses: in.SimilarAnalyses,
		DiscoveryContext: in.DiscoveryContext, ExecutionProfile: in.ExecutionProfile,
		PullbackContext: in.PullbackContext, PrimaryThesisContext: in.PrimaryThesisContext,
	}
}

// Canonicalize applies the declared canonical order to order-insensitive
// request slices. It runs before both prompt rendering and hashing, so the
// prompt and the hash always agree. Bars and timeframe contexts are already
// ordered (newest first; configured timeframe order) and lens menus keep
// their meaningful profile order.
func Canonicalize(req *analysis.AnalysisRequest) {
	// Clone before sorting: callers may still hold the original slices.
	req.News = slices.Clone(req.News)
	req.Portfolio.OpenPositions = slices.Clone(req.Portfolio.OpenPositions)
	slices.SortStableFunc(req.News, func(a, b analysis.NewsSnippet) int {
		if c := b.PublishedAt.Compare(a.PublishedAt); c != 0 {
			return c
		}
		if c := cmp.Compare(a.ExternalID, b.ExternalID); c != 0 {
			return c
		}
		return cmp.Compare(a.Headline, b.Headline)
	})
	slices.SortStableFunc(req.Portfolio.OpenPositions, func(a, b analysis.PositionSummary) int {
		if c := cmp.Compare(a.Ticker, b.Ticker); c != 0 {
			return c
		}
		return cmp.Compare(a.Side, b.Side)
	})
}

// InputsFromRequest projects a fully built request onto the snapshot inputs.
// Custom prompt overrides are not inputs: snapshots are taken of standard
// analysis requests only.
func InputsFromRequest(req analysis.AnalysisRequest) Inputs {
	return Inputs{
		Ticker:               req.Ticker,
		Timeframe:            req.Timeframe,
		TimeframeRole:        req.TimeframeRole,
		Strategy:             req.Strategy,
		Strategies:           req.Strategies,
		MomentumPlay:         req.MomentumPlay,
		DecisionCutoff:       req.DecisionCutoff.UTC(),
		RiskPolicy:           req.RiskPolicy,
		RecentCandles:        req.RecentCandles,
		Indicators:           req.Indicators,
		TimeframeContexts:    req.TimeframeContexts,
		News:                 req.News,
		Portfolio:            req.Portfolio,
		KnowledgeContext:     req.KnowledgeContext,
		PerformanceContext:   req.PerformanceContext,
		SimilarAnalyses:      req.SimilarAnalyses,
		DiscoveryContext:     req.DiscoveryContext,
		ExecutionProfile:     req.ExecutionProfile,
		PullbackContext:      req.PullbackContext,
		PrimaryThesisContext: req.PrimaryThesisContext,
	}
}

// EncodeInputs returns the canonical encoding of inputs and its digest. Every
// time.Time is normalised to UTC first: values scanned from the store carry
// the process's local zone, and the same instant must hash identically on
// every host. A JSON round trip provides the deep copy, so the caller's
// request is never modified.
func EncodeInputs(in Inputs) (json.RawMessage, string, error) {
	raw, err := json.Marshal(in)
	if err != nil {
		return nil, "", err
	}
	var normalised Inputs
	if err := json.Unmarshal(raw, &normalised); err != nil {
		return nil, "", fmt.Errorf("snapshot inputs do not round-trip: %w", err)
	}
	utcTimes(reflect.ValueOf(&normalised).Elem())
	return Encode(normalised)
}

var timeType = reflect.TypeOf(time.Time{})

// utcTimes rewrites every reachable, settable time.Time to UTC in place.
func utcTimes(v reflect.Value) {
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		if !v.IsNil() {
			utcTimes(v.Elem())
		}
	case reflect.Struct:
		if v.Type() == timeType {
			if v.CanSet() {
				v.Set(reflect.ValueOf(v.Interface().(time.Time).UTC()))
			}
			return
		}
		for i := 0; i < v.NumField(); i++ {
			if f := v.Field(i); f.CanSet() {
				utcTimes(f)
			}
		}
	case reflect.Slice, reflect.Array:
		for i := 0; i < v.Len(); i++ {
			utcTimes(v.Index(i))
		}
	case reflect.Map:
		for _, key := range v.MapKeys() {
			elem := reflect.New(v.Type().Elem()).Elem()
			elem.Set(v.MapIndex(key))
			utcTimes(elem)
			v.SetMapIndex(key, elem)
		}
	}
}

// Encode returns the canonical encoding and its SHA-256 hex digest.
func Encode(v any) (json.RawMessage, string, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, "", err
	}
	return raw, digest(raw), nil
}

func digest(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// DecisionHash identifies a decision under a specific policy: identical
// inputs under a different resolution policy, prompt template, or effective
// configuration produce a different decision hash.
func DecisionHash(schemaVersion int, inputHash, policyVersion, promptVersion, configHash string) string {
	return digest([]byte(fmt.Sprintf("%d\x00%s\x00%s\x00%s\x00%s",
		schemaVersion, inputHash, policyVersion, promptVersion, configHash)))
}

// CodeRevision returns the VCS revision embedded by the Go toolchain, with a
// "+dirty" suffix for modified trees, or "unknown" (tests, non-VCS builds).
func CodeRevision() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	var rev, modified string
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			modified = s.Value
		}
	}
	if rev == "" {
		return "unknown"
	}
	if modified == "true" {
		rev += "+dirty"
	}
	return rev
}

// Workflow names the configured analysis topology.
func Workflow(cfg *config.Config) string {
	if cfg.Analysis.JevAuthoritative() {
		return WorkflowJev
	}
	if cfg.Analysis.SpecialistModeEnabled {
		return WorkflowSpecialist
	}
	if cfg.Models.Consensus.Enabled && len(cfg.Models.AnalystConfigs()) > 1 {
		return WorkflowEnsemble
	}
	return WorkflowSingle
}

// ─── Effective configuration (allowlist) ─────────────────────────────────────

// ModelSettings is the allowlisted, credential-free view of one model entry.
type ModelSettings struct {
	Provider               string  `json:"provider"`
	Model                  string  `json:"model"`
	MaxTokens              int     `json:"max_tokens"`
	Temperature            float64 `json:"temperature"`
	TimeoutSec             int     `json:"timeout_sec"`
	ThinkingBudget         int     `json:"thinking_budget,omitempty"`
	ThinkingLevel          string  `json:"thinking_level,omitempty"`
	ThinkingOutputHeadroom int     `json:"thinking_output_headroom,omitempty"`
	ReasoningEffort        string  `json:"reasoning_effort,omitempty"`
	RequestsPerMinute      int     `json:"requests_per_minute,omitempty"`
	Fallback               string  `json:"fallback,omitempty"`
}

func modelSettings(m config.ModelConfig, fallback string) ModelSettings {
	return ModelSettings{
		Provider: m.Provider, Model: m.Model, MaxTokens: m.MaxTokens, Temperature: m.Temperature,
		TimeoutSec: m.TimeoutSec, ThinkingBudget: m.ThinkingBudget, ThinkingLevel: m.ThinkingLevel,
		ThinkingOutputHeadroom: m.ThinkingOutputHeadroom, ReasoningEffort: m.ReasoningEffort,
		RequestsPerMinute: m.RequestsPerMinute, Fallback: fallback,
	}
}

// EffectiveConfig is the sanitized configuration that can influence a
// decision. It is an explicit allowlist: operator credentials, endpoints,
// broker accounts, and notification targets are never copied.
type EffectiveConfig struct {
	Workflow                 string                     `json:"workflow"`
	Analysts                 []ModelSettings            `json:"analysts"`
	Arbiter                  *ModelSettings             `json:"arbiter,omitempty"`
	Registry                 map[string]ModelSettings   `json:"registry,omitempty"`
	SpecialistRoles          map[string]string          `json:"specialist_roles,omitempty"`
	ConsensusEnabled         bool                       `json:"consensus_enabled"`
	FastPathConfidence       float64                    `json:"fast_path_confidence"`
	FastPathMaxSpread        float64                    `json:"fast_path_max_spread"`
	AdaptiveDepth            config.AdaptiveDepthConfig `json:"adaptive_depth"`
	MaxCandlesPerPrompt      int                        `json:"max_candles_per_prompt"`
	MaxNewsArticlesPerPrompt int                        `json:"max_news_articles_per_prompt"`
	HistoryWindowByStrategy  map[string]int             `json:"history_window_by_strategy,omitempty"`
	SimilarAnalysesEnabled   bool                       `json:"similar_analyses_enabled"`
	SimilarAnalysesThreshold float64                    `json:"similar_analyses_threshold"`
	SimilarAnalysesLimit     int                        `json:"similar_analyses_limit"`
	EmbeddingModel           string                     `json:"embedding_model,omitempty"`
	FeedbackPromptEnrichment bool                       `json:"feedback_prompt_enrichment"`
	// DecisionMode, JevAuthority, JevModel and JevPolicy identify the Jev
	// decision mode (issue #590); credentials and endpoints are excluded.
	DecisionMode string                  `json:"decision_mode,omitempty"`
	JevAuthority string                  `json:"jev_authority,omitempty"`
	JevModel     string                  `json:"jev_model,omitempty"`
	JevPolicy    *config.JevPolicyConfig `json:"jev_policy,omitempty"`
	// JevMinRiskReward is the engine risk/reward floor applied to Jev's
	// deterministic target (max(target_r_multiple, floor)).
	JevMinRiskReward float64 `json:"jev_min_risk_reward,omitempty"`
	// Shorts is the short-selling configuration when enabled (#599); it
	// switches Jev to its two-sided v2 questions and policy.
	Shorts     *config.ShortsConfig    `json:"shorts,omitempty"`
	EntryGates config.EntryGatesConfig `json:"entry_gates"`
}

// NewEffectiveConfig projects the operator configuration onto the allowlist.
func NewEffectiveConfig(cfg *config.Config) EffectiveConfig {
	out := EffectiveConfig{
		Workflow:                 Workflow(cfg),
		ConsensusEnabled:         cfg.Models.Consensus.Enabled,
		FastPathConfidence:       cfg.Models.Consensus.FastPathConfidence,
		FastPathMaxSpread:        cfg.Models.Consensus.FastPathMaxSpread,
		AdaptiveDepth:            cfg.Analysis.AdaptiveDepth,
		MaxCandlesPerPrompt:      cfg.Analysis.MaxCandlesPerPrompt,
		MaxNewsArticlesPerPrompt: cfg.Analysis.MaxNewsArticlesPerPrompt,
		HistoryWindowByStrategy:  cfg.Analysis.HistoryWindowByStrategy,
		SimilarAnalysesEnabled:   cfg.Embeddings.SimilarAnalysesEnabled,
		SimilarAnalysesThreshold: cfg.Embeddings.SimilarAnalysesThreshold,
		SimilarAnalysesLimit:     cfg.Embeddings.SimilarAnalysesLimit,
		FeedbackPromptEnrichment: cfg.Feedback.Enabled && cfg.Feedback.PromptEnrichment,
		EntryGates:               cfg.Risk.EntryGates,
	}
	if cfg.Embeddings.SimilarAnalysesEnabled {
		out.EmbeddingModel = cfg.Embeddings.Provider + "/" + cfg.Embeddings.Model
	}
	if cfg.Analysis.JevEnabled() {
		policy := cfg.Analysis.Jev.Policy
		if policy.MinConfidence == 0 {
			policy.MinConfidence = cfg.Risk.ConfidenceThreshold // as the engine applies it
		}
		out.DecisionMode, out.JevAuthority, out.JevModel, out.JevPolicy = "jev", cfg.Analysis.Jev.Authority, cfg.Analysis.Jev.Model, &policy
		out.JevMinRiskReward = cfg.Risk.MinRiskReward
	}
	if cfg.Risk.Shorts.Enabled {
		shorts := cfg.Risk.Shorts
		out.Shorts = &shorts
	}
	for _, m := range cfg.Models.AnalystConfigs() {
		out.Analysts = append(out.Analysts, modelSettings(m, ""))
	}
	if arb := cfg.Models.ArbiterConfig(); arb.Provider != "" {
		s := modelSettings(arb, "")
		out.Arbiter = &s
	}
	if cfg.Analysis.SpecialistModeEnabled {
		out.SpecialistRoles = cfg.Analysis.SpecialistRoles
	}
	if len(cfg.Models.Registry) > 0 {
		out.Registry = make(map[string]ModelSettings, len(cfg.Models.Registry))
		for key, entry := range cfg.Models.Registry {
			out.Registry[key] = modelSettings(entry.ModelConfig, entry.Fallback)
		}
	}
	return out
}

// ─── Stage calls and final output ────────────────────────────────────────────

// Stage names recorded on decision_stage_calls.
const (
	StageAnalyst    = "analyst"
	StageSpecialist = "specialist"
	StageArbiter    = "arbiter"
	// StageJev is an authoritative Jev decision call (issue #590).
	StageJev = "jev"
	// StageJevShadow is a shadow Jev call recorded beside the baseline.
	StageJevShadow = "jev_shadow"
)

// JevRole is the AnalystResult.Role the engine gives Jev results.
const JevRole = "jev"

// ArbiterExtra is the arbiter-only audit carried on its stage call.
type ArbiterExtra struct {
	PolicyVersion  string                 `json:"policy_version"`
	Ordering       []analysis.ArbiterSlot `json:"ordering"`
	DissentSummary string                 `json:"dissent_summary,omitempty"`
	FastPathed     bool                   `json:"fast_pathed"`
}

// RetryExtra marks a stage-call row that records a retried transport attempt.
type RetryExtra struct {
	Kind       string `json:"kind"` // "transport_retry"
	StatusCode int    `json:"status_code,omitempty"`
}

// StageCalls maps an analyzer result onto stage-call rows. Partial results
// from a failed analysis are included: their raw responses are evidence.
//
// Every invocation gets its own row. Within one (stage, ordinal) the attempt
// number increases in call order: superseded calls to other models (a
// specialist's failed primary before its fallback) come first, each followed
// by its retried transport attempts; the final call is the highest attempt.
func StageCalls(result analysis.AnalyzeResult, shadows ...analysis.AnalystResult) ([]store.DecisionStageCall, error) {
	var calls []store.DecisionStageCall
	add := func(stage string, ordinal int, role, provider, model string, meta analysis.ModelCallMeta,
		resp *analysis.AnalysisResponse, errText string, latencyMs int64, extra any) error {
		rows, err := attemptRows(stage, ordinal, role, provider, model, meta, resp, errText, latencyMs, extra)
		calls = append(calls, rows...)
		return err
	}
	errText := func(err error) string {
		if err == nil {
			return ""
		}
		return err.Error()
	}
	for i, r := range result.AnalystResults {
		stage := StageAnalyst
		switch {
		case r.Role == JevRole:
			stage = StageJev
		case r.Role != "":
			stage = StageSpecialist
		}
		resp := &r.Response
		if r.Err != nil {
			resp = nil
		}
		if err := add(stage, i, r.Role, r.Provider, r.Model, r.Meta, resp, errText(r.Err), r.LatencyMs, rawExtra(r.Extra)); err != nil {
			return nil, err
		}
	}
	for i, r := range shadows {
		resp := &r.Response
		if r.Err != nil {
			resp = nil
		}
		if err := add(StageJevShadow, i, r.Role, r.Provider, r.Model, r.Meta, resp, errText(r.Err), r.LatencyMs, rawExtra(r.Extra)); err != nil {
			return nil, err
		}
	}
	if len(result.AnalystResults) == 0 && result.Meta.Provider != "" {
		if err := add(StageAnalyst, 0, "", result.Meta.Provider, result.Meta.Model, result.Meta,
			&result.Response, "", result.Meta.LatencyMs, nil); err != nil {
			return nil, err
		}
	}
	if arb := result.Arbiter; arb != nil {
		extra := ArbiterExtra{PolicyVersion: arb.PolicyVersion, Ordering: arb.Ordering,
			DissentSummary: arb.DissentSummary, FastPathed: arb.FastPathed}
		if err := add(StageArbiter, 0, "", arb.Meta.Provider, arb.Meta.Model, arb.Meta,
			&arb.Final, "", arb.Meta.LatencyMs, extra); err != nil {
			return nil, err
		}
	}
	if failed := result.FailedArbiter; failed != nil {
		extra := ArbiterExtra{PolicyVersion: failed.PolicyVersion, Ordering: failed.Ordering}
		if err := add(StageArbiter, 0, "", failed.Provider, failed.Model, failed.Meta,
			nil, errText(failed.Err), failed.Meta.LatencyMs, extra); err != nil {
			return nil, err
		}
	}
	return calls, nil
}

// attemptRows flattens one stage's call history into numbered rows.
func attemptRows(stage string, ordinal int, role, provider, model string, meta analysis.ModelCallMeta,
	resp *analysis.AnalysisResponse, errText string, latencyMs int64, extra any) ([]store.DecisionStageCall, error) {
	var rows []store.DecisionStageCall
	var emit func(meta analysis.ModelCallMeta, provider, model string, resp *analysis.AnalysisResponse,
		errText string, latencyMs int64, extra any) error
	emit = func(meta analysis.ModelCallMeta, provider, model string, resp *analysis.AnalysisResponse,
		errText string, latencyMs int64, extra any) error {
		for _, prior := range meta.PriorAttempts {
			if err := emit(prior.Meta, prior.Meta.Provider, prior.Meta.Model, nil,
				prior.Error, prior.Meta.LatencyMs, nil); err != nil {
				return err
			}
		}
		if provider == "" {
			provider = meta.Provider
		}
		if model == "" {
			model = meta.Model
		}
		params, err := marshalOptional(meta.RequestParams)
		if err != nil {
			return err
		}
		for _, retry := range meta.Retries {
			retryExtra, err := json.Marshal(RetryExtra{Kind: "transport_retry", StatusCode: retry.StatusCode})
			if err != nil {
				return err
			}
			// The prompt is identical to the final attempt's; it is stored once
			// there rather than duplicated on every retry row.
			rows = append(rows, store.DecisionStageCall{
				Stage: stage, Ordinal: ordinal, Attempt: len(rows), Role: role,
				Provider: provider, RequestedModel: model, RequestParams: params,
				Error: retry.Error, LatencyMs: retry.LatencyMs, Extra: retryExtra,
			})
		}
		row := store.DecisionStageCall{
			Stage: stage, Ordinal: ordinal, Attempt: len(rows), Role: role,
			Provider: provider, RequestedModel: model, ResolvedModel: meta.ResolvedModel,
			RequestParams: params, RawPrompt: meta.RawPrompt, RawResponse: meta.RawResponse,
			Error: errText, InputTokens: meta.InputTokens, OutputTokens: meta.OutputTokens, LatencyMs: latencyMs,
		}
		if errText == "" && resp != nil {
			if row.ParsedResponse, err = json.Marshal(resp); err != nil {
				return err
			}
		}
		if extra != nil {
			if row.Extra, err = json.Marshal(extra); err != nil {
				return err
			}
		}
		rows = append(rows, row)
		return nil
	}
	err := emit(meta, provider, model, resp, errText, latencyMs, extra)
	return rows, err
}

// rawExtra passes pre-encoded audit JSON through attemptRows unchanged.
func rawExtra(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	return raw
}

func marshalOptional(v map[string]any) (json.RawMessage, error) {
	if len(v) == 0 {
		return nil, nil
	}
	return json.Marshal(v)
}

// FinalOutput is the engine-resolved decision handed to signal generation:
// after aggregation, final lens validation, supplemental-evidence blocking,
// and decision-policy reasoning. Recorded-answer replay must reproduce it.
type FinalOutput struct {
	ResolutionPolicyVersion string `json:"resolution_policy_version"`
	// Evaluator names who decided: the workflow (single/ensemble/specialist)
	// or "jev" for an authoritative Jev decision.
	Evaluator string `json:"evaluator,omitempty"`
	// PolicyTrace is the evaluator's resolution trace (Jev reason codes and
	// measured values) when one exists.
	PolicyTrace         json.RawMessage               `json:"policy_trace,omitempty"`
	Response            analysis.AnalysisResponse     `json:"response"`
	LensAudit           analysis.LensSelectionAudit   `json:"lens_audit"`
	DecisionPolicy      *analysis.DecisionPolicyAudit `json:"decision_policy,omitempty"`
	FastPathed          bool                          `json:"fast_pathed"`
	Arbitrated          bool                          `json:"arbitrated"`
	SupplementalBlocked bool                          `json:"supplemental_blocked"`
}
