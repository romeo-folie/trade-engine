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
	EntryGates               config.EntryGatesConfig    `json:"entry_gates"`
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
)

// ArbiterExtra is the arbiter-only audit carried on its stage call.
type ArbiterExtra struct {
	PolicyVersion  string                 `json:"policy_version"`
	Ordering       []analysis.ArbiterSlot `json:"ordering"`
	DissentSummary string                 `json:"dissent_summary,omitempty"`
	FastPathed     bool                   `json:"fast_pathed"`
}

// StageCalls maps an analyzer result onto stage-call rows. Partial results
// from a failed analysis are included: their raw responses are evidence.
func StageCalls(result analysis.AnalyzeResult) ([]store.DecisionStageCall, error) {
	calls := make([]store.DecisionStageCall, 0, len(result.AnalystResults)+1)
	for i, r := range result.AnalystResults {
		stage := StageAnalyst
		if r.Role != "" {
			stage = StageSpecialist
		}
		call, err := stageCall(stage, i, r.Role, r.Provider, r.Model, r.Meta, r.Response, r.Err, r.LatencyMs)
		if err != nil {
			return nil, err
		}
		calls = append(calls, call)
	}
	if len(result.AnalystResults) == 0 && result.Meta.Provider != "" {
		call, err := stageCall(StageAnalyst, 0, "", result.Meta.Provider, result.Meta.Model,
			result.Meta, result.Response, nil, result.Meta.LatencyMs)
		if err != nil {
			return nil, err
		}
		calls = append(calls, call)
	}
	if arb := result.Arbiter; arb != nil {
		call, err := stageCall(StageArbiter, 0, "", arb.Meta.Provider, arb.Meta.Model,
			arb.Meta, arb.Final, nil, arb.Meta.LatencyMs)
		if err != nil {
			return nil, err
		}
		extra, err := json.Marshal(ArbiterExtra{
			PolicyVersion: arb.PolicyVersion, Ordering: arb.Ordering,
			DissentSummary: arb.DissentSummary, FastPathed: arb.FastPathed,
		})
		if err != nil {
			return nil, err
		}
		call.Extra = extra
		calls = append(calls, call)
	}
	return calls, nil
}

func stageCall(stage string, ordinal int, role, provider, model string, meta analysis.ModelCallMeta,
	resp analysis.AnalysisResponse, callErr error, latencyMs int64) (store.DecisionStageCall, error) {
	if provider == "" {
		provider = meta.Provider
	}
	if model == "" {
		model = meta.Model
	}
	call := store.DecisionStageCall{
		Stage: stage, Ordinal: ordinal, Role: role, Provider: provider, RequestedModel: model,
		ResolvedModel: meta.ResolvedModel, RawPrompt: meta.RawPrompt, RawResponse: meta.RawResponse,
		InputTokens: meta.InputTokens, OutputTokens: meta.OutputTokens, LatencyMs: latencyMs,
	}
	if len(meta.RequestParams) > 0 {
		params, err := json.Marshal(meta.RequestParams)
		if err != nil {
			return call, err
		}
		call.RequestParams = params
	}
	if callErr != nil {
		call.Error = callErr.Error()
		return call, nil
	}
	parsed, err := json.Marshal(resp)
	if err != nil {
		return call, err
	}
	call.ParsedResponse = parsed
	return call, nil
}

// FinalOutput is the engine-resolved decision handed to signal generation:
// after aggregation, final lens validation, supplemental-evidence blocking,
// and decision-policy reasoning. Recorded-answer replay must reproduce it.
type FinalOutput struct {
	ResolutionPolicyVersion string                        `json:"resolution_policy_version"`
	Response                analysis.AnalysisResponse     `json:"response"`
	LensAudit               analysis.LensSelectionAudit   `json:"lens_audit"`
	DecisionPolicy          *analysis.DecisionPolicyAudit `json:"decision_policy,omitempty"`
	FastPathed              bool                          `json:"fast_pathed"`
	Arbitrated              bool                          `json:"arbitrated"`
	SupplementalBlocked     bool                          `json:"supplemental_blocked"`
}
