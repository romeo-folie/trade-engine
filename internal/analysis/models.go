// Package analysis provides the LLM analysis engine for TradeEngine.
// It defines the ModelProvider interface, request/response types, and
// the model registry that wires configured providers into the engine loop.
//
// PRD §8 — Analysis Engine.
package analysis

import (
	"context"
	"time"
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
	Ticker    string
	Strategy  string
	Timeframe string

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
	OpenPositions []PositionSummary
}

// PositionSummary is a condensed view of an open position.
type PositionSummary struct {
	Ticker       string
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
}
