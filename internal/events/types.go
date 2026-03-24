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
	PortfolioSnapshotTaken Type = "portfolio.snapshot"
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
	Ticker     string
	AnalysisID string
	Action     string // BUY | SELL | HOLD
	Confidence float64
	Reasoning  string
	Provider   string
	Model      string
	LatencyMs  int64
	FastPathed bool `json:"fast_pathed"` // true = unanimous analysts, arbiter skipped
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
	Ticker string
	Passed bool
	Checks []RiskCheckResult
}

// RiskCheckResult is a single check outcome within RiskCheckCompletedPayload.
type RiskCheckResult struct {
	Name   string
	Passed bool
	Reason string
}

// TradeExecutedPayload is emitted when a trade is opened via the executor.
type TradeExecutedPayload struct {
	TradeID    string
	SignalID   string
	Ticker     string
	Direction  string // buy | sell
	EntryPrice float64
	Quantity   float64
	Strategy   string
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
	TotalValue    float64
	Cash          float64
	Invested      float64
	UnrealizedPnL float64
	OpenPositions int
}

// ErrorPayload is emitted when a non-fatal error occurs in the pipeline.
type ErrorPayload struct {
	Component string // "data", "analysis", "execution", "risk", etc.
	Ticker    string
	Message   string
	Err       error
}
