// Package analysis provides the LLM analysis engine for TradeEngine.
// It defines the ModelProvider interface, request/response types, and
// the model registry that wires configured providers into the engine loop.
//
// PRD §8 — Analysis Engine.
package analysis

import (
	"context"
	"time"

	"github.com/romeo-folie/trade-engine/internal/analysis/prompts"
	"github.com/romeo-folie/trade-engine/internal/strategy/antichase"
	"github.com/romeo-folie/trade-engine/internal/strategy/pullback"
	"github.com/romeo-folie/trade-engine/internal/tradeprofile"
)

// TimeframeRole and PrimaryThesisContext are aliases for the prompt-safe
// workflow contract. Keeping them available from analysis makes request
// construction ergonomic without duplicating a DTO across providers.
type TimeframeRole = prompts.TimeframeRole
type PrimaryThesisContext = prompts.PrimaryThesisContext

const (
	TimeframeRolePrimaryThesis     = prompts.TimeframeRolePrimaryThesis
	TimeframeRoleEntryConfirmation = prompts.TimeframeRoleEntryConfirmation
)

// Action represents a trade recommendation from a model.
type Action string

const (
	ActionBuy   Action = "BUY"
	ActionSell  Action = "SELL"
	ActionHold  Action = "HOLD"
	ActionWait  Action = "WAIT"  // real catalyst, poor entry timing — re-evaluate next tick
	ActionTrade Action = "TRADE" // ORB mode: candidate passes the gate
	ActionSkip  Action = "SKIP"  // ORB mode: candidate filtered out
	ActionAdd   Action = "ADD"   // scanner mode: candidate should be added to the watchlist
)

// AnalysisRequest carries everything a model needs to produce a recommendation.
type AnalysisRequest struct {
	Ticker string
	// Strategy is the configured PRIMARY label (watchlist strategies[0]). It is
	// retained for logging, persistence, and the history-window cap; it no
	// longer decides how the analyst frames the setup.
	Strategy string
	// Strategies is the ticker's admissible lens set — the full watchlist
	// strategies column. Prompts offer these as candidates and the analyst
	// selects among them (issue #509), rather than being assigned Strategy as
	// an identity to advocate for. Empty falls back to Strategy.
	Strategies []string
	Timeframe  string
	// TimeframeRole says whether this request establishes a thesis or confirms
	// entry timing for a completed primary-thesis request. The zero value is the
	// primary thesis role, preserving existing callers.
	TimeframeRole TimeframeRole
	// PrimaryThesisContext is supplied only to an entry-confirmation pass. It
	// fixes the selected lens and records the first-pass evidence; confirmation
	// cannot issue an independent exit decision.
	PrimaryThesisContext *PrimaryThesisContext
	// TimeframeContexts carries supplemental market evidence for the same
	// analyst decision. It replaces the former follow-up LLM passes: the
	// primary timeframe still anchors the request, while every configured
	// supplemental timeframe is considered by the very same ensemble.
	TimeframeContexts []TimeframeContext

	// DiscoveryContext records why scanner promotion found this ticker. It is
	// prompt context only: source labels must never become execution lenses.
	DiscoveryContext *tradeprofile.DiscoveryContext
	// ExecutionProfile is the scanner-owned, bounded analytical menu. When it
	// is valid it supersedes legacy Strategies for prompt lens selection; the
	// latter remains intact for engine compatibility and entry-gate tags.
	ExecutionProfile *tradeprofile.ExecutionProfile
	// PullbackContext is the evaluator-owned daily evidence supplied only when
	// pullback_strength is an admissible lens. It informs lens selection but
	// cannot override the deterministic execution gate.
	PullbackContext *pullback.PromptContext

	// Market data — recent candles, most recent first.
	RecentCandles []CandleSnapshot

	// Technical indicators computed over the primary timeframe.
	Indicators IndicatorSnapshot

	// Latest news for this ticker (last 48h), newest first.
	News []NewsSnippet

	// Portfolio context — filled by Phase 4; placeholder for now.
	Portfolio PortfolioContext

	// CustomSystemPrompt and CustomUserMessage bypass the stock prompt builder
	// when set. If either is non-empty the adapter must use them directly.
	CustomSystemPrompt string
	CustomUserMessage  string

	// KnowledgeContext is a pre-formatted knowledge-graph context block for
	// injection into the stock prompt's KNOWLEDGE GRAPH section.
	// Empty string causes the prompt to emit a "not available" placeholder.
	KnowledgeContext string

	// SimilarAnalyses is a list of semantically similar past setups to inject
	// into the SIMILAR PAST ANALYSES section.  Populated from pgvector search
	// when embeddings.similar_analyses_enabled = true.
	// Empty slice causes the prompt to emit a "not available" placeholder.
	SimilarAnalyses []SimilarSetup

	// PerformanceContext is a pre-formatted performance feedback block for
	// injection into the stock prompt's PERFORMANCE CONTEXT section.
	// Empty string causes the prompt to emit a "not available" placeholder.
	PerformanceContext string

	// MomentumPlay signals that this ticker was added by the scanner as a
	// short-term momentum or catalyst play. Providers swap in a momentum-focused
	// system prompt that asks "is the entry still valid?" rather than the
	// standard swing-trade framing.
	MomentumPlay bool
}

// LensSet returns the admissible lens set to offer the analyst, falling back to
// the configured primary when Strategies is unset. Callers that construct an
// AnalysisRequest without the watchlist list (tests, one-off tooling) therefore
// keep single-lens behaviour instead of silently dropping to defaultLens.
func (r AnalysisRequest) LensSet() []string {
	if r.ExecutionProfile != nil {
		if profile, err := r.ExecutionProfile.Canonicalize(); err == nil {
			return append([]string(nil), profile.AdmissibleLenses...)
		}
	}
	if len(r.Strategies) > 0 {
		return r.Strategies
	}
	if r.Strategy != "" {
		return []string{r.Strategy}
	}
	return nil
}

// SimilarSetup describes a past analysis that resembles the current one,
// used for prompt injection.  Mirrors store.SimilarAnalysis without importing
// the store package into the analysis layer.
type SimilarSetup struct {
	Ticker          string
	Action          string
	Confidence      float64
	Reasoning       string
	CreatedAt       string
	TradeOutcomePct *float64
}

// AnalysisResponse is the structured output the model must return.
// PRD §8.2 — exact schema.
type AnalysisResponse struct {
	Action     Action  `json:"action"`
	Confidence float64 `json:"confidence"`
	SizePct    float64 `json:"size_pct"`
	StopLoss   float64 `json:"stop_loss"`
	TakeProfit float64 `json:"take_profit"`
	Reasoning  string  `json:"reasoning"`
	// DecisiveReason is the one-sentence fact that determined the final action.
	// For WAIT it must name the concrete blocker, so the persisted outcome is
	// intelligible even when every directional analyst favoured BUY.
	DecisiveReason string `json:"decisive_reason"`

	// Regime is the analyst's classification of current conditions
	// ("trending" | "ranging" | "transitional"). Observation-only.
	Regime string `json:"regime"`
	// SelectedStrategy is the candidate lens the analyst chose, or "none" when
	// no admissible lens fits the regime (a regime-mismatch HOLD).
	//
	// This is the auditable output of the candidate-lens contract (issue #509):
	// comparing it against the CONFIGURED strategy detects mislabelled
	// watchlist rows, and it is the only way to attribute a trade to the lens
	// actually applied. NOT persisted yet — phase 1 runs in shadow so the
	// divergence rate can justify the migration before it is built.
	SelectedStrategy string `json:"selected_strategy"`
	// RejectedStrategies maps each candidate lens the analyst did not select to
	// its one-clause rejection reason. Its presence is the guard against
	// lens-shopping: a menu of lenses is also a menu of justifications, so the
	// analyst must say why the others do not fit.
	RejectedStrategies map[string]string `json:"rejected_strategies"`
}

// TimeframeContext is supplemental evidence supplied to one unified analyst
// decision. Role is descriptive (for example "entry_timing"), never a second
// decision authority.
type TimeframeContext struct {
	Timeframe     string
	Role          string
	RecentCandles []CandleSnapshot
	Indicators    IndicatorSnapshot
}

// ModelCallMeta records operational metadata for a single model invocation.
type ModelCallMeta struct {
	Provider      string
	Model         string
	PromptHash    string
	RawPrompt     string
	RawResponse   string
	InputTokens   int
	OutputTokens  int
	LatencyMs     int64
	IndicatorJSON []byte // JSONB snapshot
}

// ModelProvider is the interface every LLM adapter must implement.
// PRD §18 — model provider interface.
type ModelProvider interface {
	// Analyze sends a structured analysis request and returns the model's
	// recommendation plus operational metadata.
	Analyze(ctx context.Context, req AnalysisRequest) (AnalysisResponse, ModelCallMeta, error)

	// ProviderName returns the provider identifier (e.g. "anthropic").
	ProviderName() string

	// ModelName returns the specific model (e.g. "claude-sonnet-4-5").
	ModelName() string

	// IsAvailable returns true if the adapter is fully configured and ready.
	IsAvailable() bool
}

// ─── Snapshot types (decoupled from data/store packages) ─────────────────────

// CandleSnapshot is a lightweight OHLCV record for prompt construction.
type CandleSnapshot struct {
	Timestamp time.Time
	Open      float64
	High      float64
	Low       float64
	Close     float64
	Volume    int64
	VWAP      float64
}

// IndicatorSnapshot mirrors indicators.IndicatorSet but lives here to keep
// analysis independent of the indicators package at the interface boundary.
type IndicatorSnapshot struct {
	EMA20  float64
	EMA50  float64
	EMA200 float64
	SMA20  float64
	SMA50  float64
	SMA200 float64

	RSI float64

	MACDLine      float64
	MACDSignal    float64
	MACDHistogram float64

	BBUpper  float64
	BBMiddle float64
	BBLower  float64

	ADX float64
	ATR float64
	OBV float64

	StochK float64
	StochD float64

	VWAP     float64
	VolSMA20 float64
	VolRatio float64 // current volume / VolSMA20 — computed by engine at callsite
	// TimingSnapshot is a deterministic, daily entry-quality context. It is
	// deliberately distinct from the short prompt candle window.
	TimingSnapshot *antichase.Snapshot
}

// NewsSnippet is a compressed news item for prompt inclusion.
type NewsSnippet struct {
	Headline    string
	Source      string
	PublishedAt time.Time
	Sentiment   string // bullish | bearish | neutral
}

// PortfolioContext carries current portfolio state for the prompt.
// Placeholder in Phase 3 — populated by Phase 4.
type PortfolioContext struct {
	CashAvailable float64
	// CashKnown distinguishes a real balance (including a fully-deployed $0)
	// from an unpopulated tracker: with it false the prompt renders an
	// explicit "not provided" marker instead of a false zero (Codex #392 P2).
	CashKnown     bool
	OpenPositions []PositionSummary
}

// PositionSummary is a condensed view of an open position.
// Side is "long"/"short" when the broker reports it (empty means unknown,
// rendered as long — the stock pipeline is long-only by the #306 guard).
type PositionSummary struct {
	Ticker       string
	Side         string
	Quantity     float64
	AvgCostBasis float64
	CurrentValue float64
	UnrealizedPL float64
}

// ─── Phase 12 types ───────────────────────────────────────────────────────────

// AnalystResult holds the outcome of one analyst model call (Phase 12).
//
// Role names the review angle in specialist mode ("fundamental", "technical",
// "sentiment", "risk_manager"); it is empty for the default generalist ensemble.
// It is persisted as part of ensemble_results so outcomes can later be sliced by
// angle — previously the role survived only as a "[role]" text prefix inside the
// reasoning, which was not queryable.
type AnalystResult struct {
	Provider  string
	Model     string
	Role      string
	Response  AnalysisResponse
	Meta      ModelCallMeta
	Err       error
	LatencyMs int64
}

// AnalyzeResult is the richer return type for Analyzer.AnalyzeFull.
// In single-analyst mode AnalystResults has one entry and Arbiter is nil.
type AnalyzeResult struct {
	Response       AnalysisResponse
	Meta           ModelCallMeta
	AnalystResults []AnalystResult
	Arbiter        *ArbiterResult // nil when fast-pathed or single-model
	FastPathed     bool
	DecisionPolicy *DecisionPolicyAudit
}

// DecisionPolicyAudit records deterministic topology constraints applied after
// a model response. In specialist mode it prevents the risk reviewer from
// originating a new BUY without support from the directional electorate.
type DecisionPolicyAudit struct {
	Version          int               `json:"version"`
	Mode             string            `json:"mode"`
	DirectionalVotes map[string]Action `json:"directional_votes"`
	RiskAction       Action            `json:"risk_action"`
	BuyVotes         int               `json:"buy_votes"`
	RequiredBuyVotes int               `json:"required_buy_votes"`
	OriginalAction   Action            `json:"original_action"`
	ResolvedAction   Action            `json:"resolved_action"`
	Applied          bool              `json:"applied"`
	Reason           string            `json:"reason,omitempty"`
}
