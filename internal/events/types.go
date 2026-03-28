// Package events provides a publish-subscribe event bus for the engine pipeline.
// All engine pipeline stages emit typed events; consumers (TUI, Telegram, logging)
// subscribe to specific event types and receive them via buffered channels.
package events

import "time"

// Type identifies the kind of event.
type Type string

const (
	DataFetched            Type = "data.fetched"
	IndicatorsComputed     Type = "indicators.computed"
	AnalysisCompleted      Type = "analysis.completed"
	ArbiterInvoked         Type = "arbiter.invoked"
	SignalGenerated        Type = "signal.generated"
	SignalBlocked          Type = "signal.blocked"
	SignalExpired          Type = "signal.expired"
	SignalQueued           Type = "signal.queued"
	RiskCheckCompleted     Type = "risk.check_completed"
	TradeExecuted          Type = "trade.executed"
	TradeSimulated         Type = "trade.simulated"
	TradeClosed            Type = "trade.closed"
	PositionUpdated        Type = "position.updated"
	NewsFetched            Type = "news.fetched"
	AnalystVotesPublished  Type = "analysis.analyst_votes"
	PortfolioSnapshotTaken Type = "portfolio.snapshot"
	AnalysisStarted        Type = "analysis.started"
	NewsEnriched           Type = "news.enriched"
	ErrorOccurred          Type = "error"
)

// Event is the envelope delivered to subscribers.
type Event struct {
	Type      Type
	Timestamp time.Time
	Payload   any // one of the typed payloads below
}

// ─── Payloads ─────────────────────────────────────────────────────────────────

// DataFetchedPayload is emitted after candle data is fetched and stored.
type DataFetchedPayload struct {
	Ticker    string
	Timeframe string
	Count     int
}

// IndicatorsComputedPayload is emitted after indicator calculation.
type IndicatorsComputedPayload struct {
	Ticker string
	RSI    float64
	EMA20  float64
	MACD   float64
	ADX    float64
}

// AnalysisCompletedPayload is emitted after an LLM analysis completes.
type AnalysisCompletedPayload struct {
	Ticker     string  `json:"ticker"`
	AnalysisID string  `json:"analysis_id"`
	Action     string  `json:"action"` // BUY | SELL | HOLD
	Confidence float64 `json:"confidence"`
	Reasoning  string  `json:"reasoning"`
	Provider   string  `json:"provider"`
	Model      string  `json:"model"`
	LatencyMs  int64   `json:"latency_ms"`
	FastPathed bool    `json:"fast_pathed"` // true = unanimous analysts, arbiter skipped
}

// ArbiterInvokedPayload is emitted when the arbiter resolves analyst dissent.
type ArbiterInvokedPayload struct {
	Ticker         string  `json:"ticker"`
	Action         string  `json:"action"`
	Confidence     float64 `json:"confidence"`
	DissentSummary string  `json:"dissent_summary"`
}

// SignalGeneratedPayload is emitted when a new signal is created (passed risk).
type SignalGeneratedPayload struct {
	SignalID    string
	Ticker      string
	Action      string
	Confidence  float64
	SignalPrice float64
	StopLoss    float64
	TakeProfit  float64
	Mode        string // auto | paper | manual
}

// SignalBlockedPayload is emitted when a signal fails risk checks.
type SignalBlockedPayload struct {
	Ticker      string
	Action      string
	Confidence  float64
	BlockReason string
	FailedCheck string
}

// SignalExpiredPayload is emitted when a pending signal expires or goes stale.
type SignalExpiredPayload struct {
	SignalID string
	Ticker   string
	Reason   string // "time limit" | "price moved X%"
}

// SignalQueuedPayload is emitted in manual mode when a signal is queued for approval.
type SignalQueuedPayload struct {
	SignalID   string
	Ticker     string
	Action     string
	Confidence float64
	ExpiresIn  time.Duration
}

// RiskCheckCompletedPayload is emitted after all risk checks run for a signal.
type RiskCheckCompletedPayload struct {
	Ticker string           `json:"ticker"`
	Passed bool             `json:"passed"`
	Checks []RiskCheckResult `json:"checks"`
}

// RiskCheckResult is a single check outcome within RiskCheckCompletedPayload.
type RiskCheckResult struct {
	Name   string `json:"name"`
	Passed bool   `json:"passed"`
	Reason string `json:"reason"`
}

// TradeExecutedPayload is emitted when a trade is opened via the executor.
type TradeExecutedPayload struct {
	TradeID    string  `json:"trade_id"`
	SignalID   string  `json:"signal_id"`
	Ticker     string  `json:"ticker"`
	Direction  string  `json:"direction"` // buy | sell
	EntryPrice float64 `json:"entry_price"`
	Quantity   float64 `json:"quantity"`
	Strategy   string  `json:"strategy"`
}

// TradeSimulatedPayload is emitted for paper trades (same data as executed).
type TradeSimulatedPayload = TradeExecutedPayload

// TradeClosedPayload is emitted when a trade is closed (SL/TP/manual).
type TradeClosedPayload struct {
	TradeID    string
	Ticker     string
	Direction  string
	EntryPrice float64
	ExitPrice  float64
	PnL        float64
	PnLPct     float64
	Reason     string // "stop-loss hit" | "take-profit hit" | "manual"
}

// PositionUpdatedPayload is emitted after position monitoring refreshes state.
type PositionUpdatedPayload struct {
	Ticker       string
	Quantity     float64
	CurrentPrice float64
	UnrealizedPL float64
}

// PortfolioSnapshotPayload is emitted after a portfolio snapshot is written.
type PortfolioSnapshotPayload struct {
	TotalValue     float64 `json:"total_value"`
	Cash           float64 `json:"cash"`
	Invested       float64 `json:"invested"`
	UnrealizedPnL  float64 `json:"unrealized_pnl"`
	RealizedPnLDay float64 `json:"realized_pnl_today"`
	OpenPositions  int     `json:"open_positions"`
}

// NewsFetchedPayload is emitted after news articles are fetched and stored for a ticker.
type NewsFetchedPayload struct {
	Ticker string `json:"ticker"`
	Count  int    `json:"count"`
}

// AnalystVotePayload describes one analyst's individual vote in an ensemble cycle.
type AnalystVotePayload struct {
	Provider   string  `json:"provider"`
	Model      string  `json:"model"`
	Action     string  `json:"action"`    // BUY | SELL | HOLD | ""
	Confidence float64 `json:"confidence"`
	Skipped    bool    `json:"skipped"`   // true = analyst errored or wasn't invoked
	LatencyMs  int64   `json:"latency_ms"`
}

// AnalystVotesPublishedPayload is emitted after per-analyst results are collected.
type AnalystVotesPublishedPayload struct {
	Ticker string               `json:"ticker"`
	Votes  []AnalystVotePayload `json:"votes"`
}

// AnalysisStartedPayload is emitted when the analyzer begins the fan-out.
type AnalysisStartedPayload struct {
	Ticker           string   `json:"ticker"`
	Depth            int      `json:"depth"`
	SelectedAnalysts []string `json:"selected_analysts"` // e.g. ["analyst-cheap", "analyst-mid"]
}

// NewsEnrichedPayload is emitted after the news enricher LLM extracts sentiment.
type NewsEnrichedPayload struct {
	Ticker string `json:"ticker"`
	Count  int    `json:"count"`
}

// ErrorPayload is emitted when a non-fatal error occurs in the pipeline.
type ErrorPayload struct {
	Component string // "data", "analysis", "execution", "risk", etc.
	Ticker    string
	Message   string
	Err       error
}
