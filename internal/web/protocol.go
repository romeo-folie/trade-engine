package web

import (
	"context"
	"encoding/json"
	"time"

	"github.com/romeo-folie/trade-engine/internal/events"
)

// ── Wire types ────────────────────────────────────────────────────────────────

// WebEvent is the canonical JSON message sent to browser clients over WebSocket.
//
//	{
//	  "type":      "TradeExecuted",
//	  "timestamp": "2026-03-14T03:00:00Z",
//	  "source":    "risk_manager",
//	  "target":    "executor",
//	  "payload":   { "ticker": "AAPL", ... }
//	}
type WebEvent struct {
	Type      string          `json:"type"`
	Timestamp time.Time       `json:"timestamp"`
	Source    string          `json:"source"`
	Target    string          `json:"target"`
	Payload   json.RawMessage `json:"payload,omitempty"`
}

// SnapshotEvent is the first message sent to a new WebSocket client.
// It carries the current state of all observable pipeline nodes so the UI
// renders immediately without waiting for a live tick.
type SnapshotEvent struct {
	Type      string          `json:"type"` // always "snapshot"
	Timestamp time.Time       `json:"timestamp"`
	Nodes     []NodeState     `json:"nodes"`
}

// NodeState carries a single pipeline node's current observable state.
type NodeState struct {
	ID           string         `json:"id"`
	Label        string         `json:"label"`
	Kind         string         `json:"kind"`   // source | processing | output
	EventCount   int64          `json:"event_count"`
	LastEventAt  *time.Time     `json:"last_event_at,omitempty"`
	AvgLatencyMs float64        `json:"avg_latency_ms,omitempty"`
	Meta         map[string]any `json:"meta,omitempty"`
}

// ── Edge mapping ──────────────────────────────────────────────────────────────

// mapEventEdge returns the (source, target, eventType) for a given internal
// event type. The eventType returned is what the browser sees; returning "" 
// means the event is not forwarded to clients.
func mapEventEdge(t events.Type) (source, target, eventType string) {
	switch t {
	// ── Data pipeline ────────────────────────────────────────────────────
	case events.DataFetched:
		return "data_enrichment", "indicator_engine", "DataFetched"
	case events.IndicatorsComputed:
		return "indicator_engine", "prompt_assembly", "IndicatorsComputed"
	case events.AnalysisCompleted:
		return "llm_analysis", "consensus", "AnalysisCompleted"
	case events.ArbiterInvoked:
		return "consensus", "arbiter_node", "ArbiterInvoked"

	// ── Signal flow ──────────────────────────────────────────────────────
	case events.SignalGenerated:
		return "consensus", "risk_manager", "SignalGenerated"
	case events.SignalBlocked:
		return "risk_manager", "blocked_signals", "SignalBlocked"
	case events.SignalExpired:
		return "risk_manager", "blocked_signals", "SignalExpired"
	case events.SignalQueued:
		return "risk_manager", "approvals", "SignalQueued"
	case events.RiskCheckCompleted:
		return "risk_manager", "executor", "RiskCheckCompleted"

	// ── Execution & portfolio ────────────────────────────────────────────
	case events.TradeExecuted:
		return "risk_manager", "executor", "TradeExecuted"
	case events.TradeSimulated:
		return "risk_manager", "executor", "TradeSimulated"
	case events.TradeClosed:
		return "executor", "portfolio", "TradeClosed"
	case events.PositionUpdated:
		return "executor", "portfolio", "PositionUpdated"
	case events.PortfolioSnapshotTaken:
		return "portfolio", "portfolio", "PortfolioSnapshot"

	// ── System ───────────────────────────────────────────────────────────
	case events.ErrorOccurred:
		return "engine", "error_bus", "ErrorOccurred"

	default:
		return "", "", ""
	}
}

// ── Snapshot builder ──────────────────────────────────────────────────────────

// buildSnapshot returns the current pipeline state for a newly connected client.
// It queries store functions to populate live data where available.
func buildSnapshot(_ context.Context) *SnapshotEvent {
	now := time.Now().UTC()
	return &SnapshotEvent{
		Type:      "snapshot",
		Timestamp: now,
		Nodes:     pipelineNodes(),
	}
}

// pipelineNodes returns the static node topology.  Live metrics (event counts,
// latency) are populated by the frontend from streamed events; the snapshot
// carries the baseline structure so the graph can render immediately.
func pipelineNodes() []NodeState {
	return []NodeState{
		// ── Source nodes ──────────────────────────────────────────────────
		{ID: "alpaca", Label: "Alpaca", Kind: "source"},
		{ID: "news_api", Label: "News (Finnhub/Benzinga)", Kind: "source"},

		// ── Processing nodes ──────────────────────────────────────────────
		{ID: "data_enrichment", Label: "Data Enrichment", Kind: "processing"},
		{ID: "indicator_engine", Label: "Indicator Computation", Kind: "processing"},
		{ID: "prompt_assembly", Label: "Prompt Assembly", Kind: "processing"},
		{ID: "llm_analysis", Label: "LLM Analysis", Kind: "processing"},
		{ID: "consensus", Label: "Consensus Resolution", Kind: "processing"},
		{ID: "arbiter_node", Label: "Arbiter", Kind: "processing"},
		{ID: "risk_manager", Label: "Risk Checks", Kind: "processing"},

		// ── Output nodes ─────────────────────────────────────────────────
		{ID: "executor", Label: "Executed Trades", Kind: "output"},
		{ID: "blocked_signals", Label: "Blocked Signals", Kind: "output"},
		{ID: "approvals", Label: "Queued Approvals", Kind: "output"},
		{ID: "portfolio", Label: "Portfolio", Kind: "output"},

		// ── System nodes ─────────────────────────────────────────────────
		{ID: "engine", Label: "Engine Core", Kind: "processing"},
		{ID: "error_bus", Label: "Error Bus", Kind: "output"},
	}
}
