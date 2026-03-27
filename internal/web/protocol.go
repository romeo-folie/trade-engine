package web

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	appconfig "github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/events"
	"github.com/romeo-folie/trade-engine/internal/logger"
	"github.com/romeo-folie/trade-engine/internal/store"
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
		return "alpaca", "indicator_engine", "DataFetched"
	case events.NewsFetched:
		return "news_api", "indicator_engine", "NewsFetched"
	case events.IndicatorsComputed:
		return "indicator_engine", "prompt_assembly", "IndicatorsComputed"
	case events.AnalysisCompleted:
		return "llm_analysis", "consensus", "AnalysisCompleted"
	case events.AnalystVotesPublished:
		return "llm_analysis", "llm_analysis", "AnalystVotesPublished"
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
func buildSnapshot(ctx context.Context, cfg *appconfig.Config) *SnapshotEvent {
	now := time.Now().UTC()
	nodes := pipelineNodes(cfg)

	// Attach live portfolio figures to the portfolio node's Meta.
	if !store.IsConnected() {
		return &SnapshotEvent{
			Type:      "snapshot",
			Timestamp: now,
			Nodes:     nodes,
		}
	}
	snap, err := store.GetLatestPortfolioSnapshot(ctx)
	if err != nil {
		logger.Warn("buildSnapshot: portfolio snapshot query failed", "err", err)
	}
	if snap != nil {
		for i := range nodes {
			if nodes[i].ID == "portfolio" {
				nodes[i].Meta = map[string]any{
					"total_value":       snap.TotalValue,
					"cash":              snap.Cash,
					"invested":          snap.Invested,
					"unrealized_pnl":    snap.UnrealizedPnL,
					"realized_pnl_today": snap.RealizedPnLToday,
					"open_positions":    snap.OpenPositions,
				}
				break
			}
		}
	}

	return &SnapshotEvent{
		Type:      "snapshot",
		Timestamp: now,
		Nodes:     nodes,
	}
}

// pipelineNodes returns the node topology, seeding analyst cards and news label
// from the engine config when available.
func pipelineNodes(cfg *appconfig.Config) []NodeState {
	// News feed label — derive from configured provider.
	newsLabel := "News (Finnhub/Benzinga)" // fallback
	if cfg != nil && cfg.Markets.News.Provider != "" {
		switch strings.ToLower(cfg.Markets.News.Provider) {
		case "finnhub":
			newsLabel = "Finnhub News"
		case "benzinga":
			newsLabel = "Benzinga News"
		default:
			newsLabel = strings.ToUpper(cfg.Markets.News.Provider[:1]) +
				cfg.Markets.News.Provider[1:] + " News"
		}
	}
	// Arbiter node config
	arbiterLabel := "-"
	var arbiterMeta map[string]any
	if cfg != nil {
		var arbCfg appconfig.ModelConfig
		// Strictly pull from registry key "arbiter" as per review feedback.
		// Role fallbacks are handled by the engine's analyzer; UI specifically
		// expects the registry model for this stage's label.
		if cfg.Models.Registry != nil {
			if entry, ok := cfg.Models.Registry["arbiter"]; ok {
				arbCfg = entry.ModelConfig
			}
		}

		if arbCfg.Model != "" {
			arbiterLabel = arbCfg.Model
			arbiterMeta = map[string]any{
				"provider": arbCfg.Provider,
				"model":    arbCfg.Model,
			}
		}
	}

	// Analyst nodes — one per configured analyst, falling back to a single placeholder.
	var analystNodes []NodeState
	if cfg != nil && len(cfg.Models.Roles.Analysts) > 0 {
		for _, roleKey := range cfg.Models.Roles.Analysts {
			entry, ok := cfg.Models.Registry[roleKey]
			if !ok {
				continue
			}
			analystNodes = append(analystNodes, NodeState{
				ID:    "analyst-" + roleKey,
				Label: entry.Model,
				Kind:  "processing",
				Meta: map[string]any{
					"provider":     entry.Provider,
					"registry_key": roleKey,
				},
			})
		}
	}
	if len(analystNodes) == 0 {
		analystNodes = []NodeState{
			{ID: "llm_analysis", Label: "LLM Analysis", Kind: "processing"},
		}
	}

	nodes := []NodeState{
		// ── Source nodes ──────────────────────────────────────────────────
		{ID: "alpaca", Label: "Alpaca", Kind: "source"},
		{ID: "news_api", Label: newsLabel, Kind: "source"},

		// ── Processing nodes ──────────────────────────────────────────────
		{ID: "data_enrichment", Label: "Data Enrichment", Kind: "processing"},
		{ID: "indicator_engine", Label: "Indicator Computation", Kind: "processing"},
		{ID: "prompt_assembly", Label: "Prompt Assembly", Kind: "processing"},
		// llm_analysis is kept as a stable routing node for AnalysisCompleted / AnalystVotesPublished
		// events (mapEventEdge targets this ID). It does not render as an analyst card — the
		// per-analyst cards (analyst-<roleKey>) are rendered from analystNodes below.
		{ID: "llm_analysis", Label: "LLM Analysis", Kind: "processing"},
	}
	nodes = append(nodes, analystNodes...)
	nodes = append(nodes,
		NodeState{ID: "consensus", Label: "Consensus Resolution", Kind: "processing"},
		NodeState{
			ID:    "arbiter_node",
			Label: arbiterLabel,
			Kind:  "processing",
			Meta:  arbiterMeta,
		},
		NodeState{ID: "risk_manager", Label: "Risk Checks", Kind: "processing"},

		// ── Output nodes ─────────────────────────────────────────────────
		NodeState{ID: "executor", Label: "Executed Trades", Kind: "output"},
		NodeState{ID: "blocked_signals", Label: "Blocked Signals", Kind: "output"},
		NodeState{ID: "approvals", Label: "Queued Approvals", Kind: "output"},
		NodeState{ID: "portfolio", Label: "Portfolio", Kind: "output"},

		// ── System nodes ─────────────────────────────────────────────────
		NodeState{ID: "engine", Label: "Engine Core", Kind: "processing"},
		NodeState{ID: "error_bus", Label: "Error Bus", Kind: "output"},
	)
	return nodes
}
