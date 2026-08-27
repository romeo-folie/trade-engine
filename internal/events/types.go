// Package events provides a publish-subscribe event bus for the engine pipeline.
// All engine pipeline stages emit typed events; consumers (TUI, Telegram, logging)
// subscribe to specific event types and receive them via buffered channels.
package events

import (
	"encoding/json"
	"time"
)

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
	TickStarted            Type = "tick.started"
	FeedbackFlagCreated    Type = "feedback.flag_created"
	ORBStateChanged        Type = "orb.state_changed"
	ORBCandidatesReady     Type = "orb.candidates_ready"
	ORBRangeLocked         Type = "orb.range_locked"
	ORBTradeUpdate         Type = "orb.trade_update"

	ScanStarted      Type = "scanner.started"
	ScanCompleted    Type = "scanner.completed"
	ScannerItemAdded Type = "scanner.item_added"
	// ScannerItemRediscovered is emitted only after a committed scanner source
	// lease receives materially new evidence. Retried or duplicate evidence does
	// not emit this event.
	ScannerItemRediscovered Type = "scanner.item_rediscovered"
	// ScannerPromotionOutcome is emitted after a scanner source's selected
	// promotion attempt has committed or reached a terminal admission outcome.
	// Unlike the legacy scanner lifecycle events, it is shared by post-session
	// producers such as the scheduled-earnings feeder.
	ScannerPromotionOutcome Type = "scanner.promotion_outcome"
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
	SourceNode string  `json:"source_node,omitempty"`
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
	SignalID      string
	Ticker        string
	Action        string
	Confidence    float64
	SignalPrice   float64
	StopLoss      float64
	TakeProfit    float64
	Mode          string // auto | paper | manual
	SourceNode    string `json:"source_node,omitempty"`
	EntrySetup    string `json:"entry_setup,omitempty"`
	SetupEvidence string `json:"setup_evidence,omitempty"`
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
	SignalID      string
	Ticker        string
	Action        string
	Confidence    float64
	ExpiresIn     time.Duration
	EntrySetup    string
	SetupEvidence string
}

// RiskCheckCompletedPayload is emitted after all risk checks run for a signal.
type RiskCheckCompletedPayload struct {
	Ticker string            `json:"ticker"`
	Passed bool              `json:"passed"`
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
	TradeID       string  `json:"trade_id"`
	SignalID      string  `json:"signal_id"`
	Ticker        string  `json:"ticker"`
	Direction     string  `json:"direction"` // buy | sell
	EntryPrice    float64 `json:"entry_price"`
	Quantity      float64 `json:"quantity"`
	Strategy      string  `json:"strategy"`
	EntrySetup    string  `json:"entry_setup,omitempty"`
	SetupEvidence string  `json:"setup_evidence,omitempty"`
	// SourceNode names the pipeline node the execution came out of, for
	// observability routing. Empty means the default risk-controls path.
	// Set to "approvals" when the signal was released from the pending queue —
	// a manual approval, or a first-tick deferral picked up by
	// processDeferredSignals — so the dashboard animates the path the signal
	// actually took instead of always attributing it to risk controls.
	SourceNode string `json:"source_node,omitempty"`
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
	Reason     string
	Evidence   json.RawMessage
	FilledAt   *time.Time
}

// PositionUpdatedPayload is emitted after position monitoring refreshes state.
type PositionUpdatedPayload struct {
	Ticker       string
	Quantity     float64
	CurrentPrice float64
	UnrealizedPL float64
}

// PortfolioSnapshotPayload is emitted after a portfolio snapshot is written.
//
// OpenTradeRows is the count of trade rows with status='open' (DB), companion
// to OpenPositions which counts distinct (ticker, direction) pairs. Exposing
// both lets the canvas portfolio card surface divergence in real time without
// being clobbered by the next WebSocket tick. Pointer + omitempty: nil values
// are omitted entirely from the JSON payload (the key is absent rather than
// encoded as null) so subscribers must treat absence as "unknown" — distinct
// from a known zero. The frontend hides the divergence indicator in that case.
type PortfolioSnapshotPayload struct {
	Available              bool    `json:"available"`
	TotalValue             float64 `json:"total_value"`
	Cash                   float64 `json:"cash"`
	Invested               float64 `json:"invested"`
	UnrealizedPnL          float64 `json:"unrealized_pnl"`
	RealizedPnLToday       float64 `json:"realized_pnl_today"` // realized P&L from closed trades today
	DayPnL                 float64 `json:"day_pnl"`            // net equity change since last session close (broker-reported)
	OpenPositions          int     `json:"open_positions"`
	PositionStatsAvailable bool    `json:"position_stats_available"`
	OpenTradeRows          *int    `json:"open_trade_rows,omitempty"`
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
	Role       string  `json:"role,omitempty"`
	Action     string  `json:"action"` // BUY | SELL | HOLD | ""
	Confidence float64 `json:"confidence"`
	Skipped    bool    `json:"skipped"` // true = analyst errored or wasn't invoked
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
	SourceNodes      []string `json:"source_nodes,omitempty"`
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

// TickStartedPayload is emitted at the very start of each engine tick.
// Frontend uses this as a cycle boundary to reset pipeline node card state.
type TickStartedPayload struct{}

// FeedbackFlagCreatedPayload is emitted when a new feedback flag is raised.
type FeedbackFlagCreatedPayload struct {
	FlagID   string `json:"flag_id"`
	Scope    string `json:"scope"`
	ScopeKey string `json:"scope_key"`
	Severity string `json:"severity"`
	Message  string `json:"message"`
}

// ORBStateChangedPayload is emitted when the ORB scheduler transitions to a new state.
type ORBStateChangedPayload struct {
	State      string `json:"state"`
	SessionID  string `json:"session_id,omitempty"`
	Date       string `json:"date,omitempty"`
	AssetClass string `json:"asset_class,omitempty"`
}

// ORBCandidatesReadyPayload is emitted when ORB candidates are selected and persisted.
type ORBCandidatesReadyPayload struct {
	SessionID  string   `json:"session_id"`
	Candidates any      `json:"candidates"` // []ScoredCandidate serialized
	Selected   []string `json:"selected"`
}

// ORBRangeLockedPayload is emitted when an opening range is locked for a ticker.
type ORBRangeLockedPayload struct {
	Ticker       string  `json:"ticker"`
	RangeHigh    float64 `json:"range_high"`
	RangeLow     float64 `json:"range_low"`
	RangeMinutes int     `json:"range_minutes"`
}

// ORBTradeUpdatePayload is emitted when an ORB trade is placed or its status changes.
type ORBTradeUpdatePayload struct {
	OrderID   string   `json:"order_id"`
	Ticker    string   `json:"ticker"`
	Side      string   `json:"side"`
	Status    string   `json:"status"`
	FillPrice float64  `json:"fill_price,omitempty"`
	PnL       *float64 `json:"pnl,omitempty"`
}

// ScanStartedPayload is emitted when the market scanner begins a scan cycle.
// ScanID links the terminal event and any scanner watchlist events in the same
// cycle, allowing consumers to form a compact, ordered summary.
type ScanStartedPayload struct {
	ScanID string `json:"scan_id"`
}

// ScanCompletedPayload is emitted when a scan cycle finishes.
type ScanCompletedPayload struct {
	ScanID              string `json:"scan_id"`
	CandidatesEvaluated int    `json:"candidates_evaluated"`
	ItemsAdded          int    `json:"items_added"`
	ItemsExpired        int    `json:"items_expired"`
	DurationMs          int64  `json:"duration_ms"`
	Failed              bool   `json:"failed"`
}

// ScannerItemAddedPayload is emitted when a scanner candidate is added to the watchlist.
type ScannerItemAddedPayload struct {
	ScanID                      string   `json:"scan_id"`
	Ticker                      string   `json:"ticker"`
	Conviction                  float64  `json:"conviction"`
	SetupType                   string   `json:"setup_type"`
	Strategy                    string   `json:"strategy"`
	Timeframes                  []string `json:"timeframes"`
	Reasoning                   string   `json:"reasoning"`
	ExpiresAt                   string   `json:"expires_at"`              // RFC3339
	Sources                     []string `json:"sources,omitempty"`       // additive scanner discovery-source tags
	GraphContext                string   `json:"graph_context,omitempty"` // why a graph neighbour was surfaced
	WeeklyLeaderEvidence        string   `json:"weekly_leader_evidence,omitempty"`
	PotentIndustryEvidence      string   `json:"potent_industry_evidence,omitempty"`
	PremarketEpisodicEvidence   string   `json:"premarket_episodic_evidence,omitempty"`
	PoliticalDisclosureEvidence string   `json:"political_disclosure_evidence,omitempty"`
	ResearchBucket              string   `json:"research_bucket,omitempty"`
	DeterministicPriority       float64  `json:"deterministic_priority,omitempty"`
	AdmissionReason             string   `json:"admission_reason,omitempty"`
}

// ScannerSourceLeaseChangePayload identifies one source whose committed lease
// was renewed by genuinely new evidence. The timestamps and evidence are
// source-owned, so a multi-source ticker can report only the source that
// actually changed.
type ScannerSourceLeaseChangePayload struct {
	Source      string          `json:"source"`
	QualifiedAt string          `json:"qualified_at"`
	ObservedAt  string          `json:"observed_at"`
	EffectiveAt string          `json:"effective_at"`
	ExpiresAt   string          `json:"expires_at"`
	Evidence    json.RawMessage `json:"evidence,omitempty"`
}

// ScannerItemRediscoveredPayload is emitted after a committed active-watchlist
// source-lease refresh. ChangedSources never includes unchanged/retried leases.
type ScannerItemRediscoveredPayload struct {
	ScanID         string                            `json:"scan_id"`
	Ticker         string                            `json:"ticker"`
	ChangedSources []ScannerSourceLeaseChangePayload `json:"changed_sources"`
}

// ScannerPromotionSourcePayload carries source-local ranking and deterministic
// evidence. Multiple source entries may describe one merged ticker attempt.
type ScannerPromotionSourcePayload struct {
	Source   string `json:"source"`
	Rank     int    `json:"rank"`
	Evidence string `json:"evidence,omitempty"`
}

// ScannerPromotionOutcomePayload describes a committed scanner-promotion
// outcome. Outcome is admitted, rediscovered, or not_admitted; ReasonCode is
// stable operator-facing classification such as queue_capacity,
// active_duplicate, or persistence_error.
type ScannerPromotionOutcomePayload struct {
	RunID       string                          `json:"run_id,omitempty"`
	Ticker      string                          `json:"ticker"`
	Outcome     string                          `json:"outcome"`
	Sources     []ScannerPromotionSourcePayload `json:"sources"`
	QueueBucket string                          `json:"queue_bucket,omitempty"`
	ReasonCode  string                          `json:"reason_code,omitempty"`
	Reason      string                          `json:"reason,omitempty"`
}
