package web

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	appconfig "github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/events"
	"github.com/romeo-folie/trade-engine/internal/logger"
	"github.com/romeo-folie/trade-engine/internal/portfolio"
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
	Type      string           `json:"type"` // always "snapshot"
	Timestamp time.Time        `json:"timestamp"`
	Nodes     []NodeState      `json:"nodes"`
	Topology  PipelineTopology `json:"topology"`
}

// NodeState carries a single pipeline node's current observable state.
type NodeState struct {
	ID           string         `json:"id"`
	Label        string         `json:"label"`
	Kind         string         `json:"kind"` // visual card type
	EventCount   int64          `json:"event_count"`
	LastEventAt  *time.Time     `json:"last_event_at,omitempty"`
	AvgLatencyMs float64        `json:"avg_latency_ms,omitempty"`
	Meta         map[string]any `json:"meta,omitempty"`
	Stage        int            `json:"stage"`
}

// PipelineTopology describes the configured workflow independently from its
// live activity. Events update node state; only a new snapshot revision should
// add, remove, or reposition nodes.
type PipelineTopology struct {
	Revision     string         `json:"revision"`
	AnalysisMode string         `json:"analysis_mode"` // single | ensemble | specialist
	Edges        []PipelineEdge `json:"edges"`
}

// PipelineEdge is a configured connection in the workflow.
type PipelineEdge struct {
	ID     string `json:"id"`
	Source string `json:"source"`
	Target string `json:"target"`
	Kind   string `json:"kind"` // primary | conditional | bypass
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
		return "news_api", "news_enricher", "NewsFetched"
	case events.NewsEnriched:
		return "news_enricher", "indicator_engine", "NewsEnriched"
	case events.IndicatorsComputed:
		return "indicator_engine", "indicator_engine", "IndicatorsComputed"
	case events.AnalysisCompleted:
		return "llm_analysis", "consensus", "AnalysisCompleted"
	case events.AnalystVotesPublished:
		return "llm_analysis", "llm_analysis", "AnalystVotesPublished"
	case events.ArbiterInvoked:
		return "consensus", "arbiter_node", "ArbiterInvoked"
	case events.AnalysisStarted:
		// Target is dynamically determined from payload in toWebEvents.
		return "indicator_engine", "analyst-set", "AnalysisStarted"

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
		return "risk_manager", "risk_manager", "RiskCheckCompleted"

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
		return "", "", "ErrorOccurred"
	case events.TickStarted:
		return "", "", "TickStarted"

	// ── ORB ──────────────────────────────────────────────────────────────
	case events.ORBStateChanged:
		return "", "", "ORBStateChanged"
	case events.ORBCandidatesReady:
		return "", "", "ORBCandidatesReady"
	case events.ORBRangeLocked:
		return "", "", "ORBRangeLocked"
	case events.ORBTradeUpdate:
		return "", "", "ORBTradeUpdate"

	default:
		return "", "", ""
	}
}

// ── Snapshot builder ──────────────────────────────────────────────────────────

// buildSnapshot returns the current pipeline state for a newly connected client.
// It queries store functions to populate live data where available.
func buildSnapshot(ctx context.Context, cfg *appconfig.Config, tracker *portfolio.Tracker) *SnapshotEvent {
	now := time.Now().UTC()
	nodes, topology := pipelineTopology(cfg)

	// Attach live portfolio figures to the portfolio node's Meta.
	if !store.IsConnected() {
		return &SnapshotEvent{
			Type:      "snapshot",
			Timestamp: now,
			Nodes:     nodes,
			Topology:  topology,
		}
	}
	snap, err := store.GetLatestPortfolioSnapshot(ctx)
	if err != nil {
		logger.Warn("buildSnapshot: portfolio snapshot query failed", "err", err)
	}
	if snap != nil {
		// Include day_pnl (broker-reported equity delta) only when the tracker has a
		// non-zero value. Omitting the key when unavailable lets the frontend fall
		// through to realized_pnl_today rather than masking it with a spurious 0.
		meta := map[string]any{
			"total_value":        snap.TotalValue,
			"cash":               snap.Cash,
			"invested":           snap.Invested,
			"unrealized_pnl":     snap.UnrealizedPnL,
			"realized_pnl_today": snap.RealizedPnLToday,
			"open_positions":     snap.OpenPositions,
		}
		if tracker != nil {
			if dpnl := tracker.CurrentState().DayPnL; dpnl != 0 {
				meta["day_pnl"] = dpnl
			}
		}
		for i := range nodes {
			if nodes[i].ID == "portfolio" {
				nodes[i].Meta = meta
				break
			}
		}
	}

	return &SnapshotEvent{
		Type:      "snapshot",
		Timestamp: now,
		Nodes:     nodes,
		Topology:  topology,
	}
}

// pipelineNodes is retained for callers that only need the configured nodes.
func pipelineNodes(cfg *appconfig.Config) []NodeState {
	nodes, _ := pipelineTopology(cfg)
	return nodes
}

// pipelineTopology returns only the nodes and edges that can participate in
// the configured execution path. It intentionally does not expose the old
// fixed superset, which made single-model and specialist runs look like a
// three-model ensemble with an arbiter.
func pipelineTopology(cfg *appconfig.Config) ([]NodeState, PipelineTopology) {
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
	nodes := []NodeState{
		{ID: "news_api", Label: newsLabel, Kind: "source", Stage: 0},
		{ID: "alpaca", Label: "Alpaca Feed", Kind: "source", Stage: 1},
		{ID: "news_enricher", Label: "News Enricher", Kind: "enricher", Stage: 1},
		{ID: "indicator_engine", Label: "Indicator Engine", Kind: "arbiter", Stage: 2},
	}

	edges := []PipelineEdge{
		{ID: "e-news-news-enricher", Source: "news_api", Target: "news_enricher", Kind: "primary"},
		{ID: "e-news-enricher-indicators", Source: "news_enricher", Target: "indicator_engine", Kind: "primary"},
		{ID: "e-alpaca-indicators", Source: "alpaca", Target: "indicator_engine", Kind: "primary"},
	}

	mode := "single"
	revisionParts := []string{mode}
	riskStage := 4

	if cfg != nil && cfg.Analysis.SpecialistModeEnabled {
		mode = "specialist"
		revisionParts = []string{mode}
		directionalRoles := []string{"fundamental", "technical", "sentiment"}
		for _, role := range directionalRoles {
			key := cfg.Analysis.SpecialistRoles[role]
			mc, err := cfg.Models.Resolve(key)
			if key == "" || err != nil {
				continue
			}
			id := "specialist-" + strings.ReplaceAll(role, "_", "-")
			nodes = append(nodes, NodeState{
				ID: id, Label: specialistNodeLabel(role), Kind: "analyst", Stage: 3,
				Meta: map[string]any{"provider": mc.Provider, "model": mc.Model, "role": role, "registry_key": key},
			})
			edges = append(edges, PipelineEdge{ID: "e-indicators-" + id, Source: "indicator_engine", Target: id, Kind: "primary"})
			revisionParts = append(revisionParts, id+":"+key)
		}

		if key := cfg.Analysis.SpecialistRoles["risk_manager"]; key != "" {
			if mc, err := cfg.Models.Resolve(key); err == nil {
				nodes = append(nodes, NodeState{
					ID: "specialist-risk-manager", Label: "Risk Specialist", Kind: "risk_specialist", Stage: 4,
					Meta: map[string]any{"provider": mc.Provider, "model": mc.Model, "role": "risk_manager", "registry_key": key},
				})
				for _, n := range nodes {
					if n.Kind == "analyst" {
						edges = append(edges, PipelineEdge{
							ID: "e-" + n.ID + "-specialist-risk-manager", Source: n.ID, Target: "specialist-risk-manager", Kind: "primary",
						})
					}
				}
				revisionParts = append(revisionParts, "specialist-risk-manager:"+key)
			}
		}
		riskStage = 5
	} else {
		var analystNodes []NodeState
		if cfg != nil {
			roleAnalysts := cfg.Models.Roles.Analysts
			configs := cfg.Models.AnalystConfigs()
			for i, ac := range configs {
				var roleKey string
				if i < len(roleAnalysts) {
					roleKey = roleAnalysts[i]
				}
				analystNodes = append(analystNodes, NodeState{
					ID: cfg.Models.AnalystNodeID(i), Label: ac.Model, Kind: "analyst", Stage: 3,
					Meta: map[string]any{"provider": ac.Provider, "model": ac.Model, "registry_key": roleKey},
				})
			}
		}
		if len(analystNodes) == 0 {
			analystNodes = []NodeState{{ID: "llm_analysis", Label: "LLM Analysis", Kind: "analyst", Stage: 3}}
		}

		ensemble := cfg != nil && cfg.Models.Consensus.Enabled && len(analystNodes) > 1
		if !ensemble {
			analystNodes = analystNodes[:1]
		} else {
			mode = "ensemble"
		}
		revisionParts = []string{mode}
		nodes = append(nodes, analystNodes...)
		for _, analyst := range analystNodes {
			edges = append(edges, PipelineEdge{
				ID: "e-indicators-" + analyst.ID, Source: "indicator_engine", Target: analyst.ID, Kind: "primary",
			})
			revisionParts = append(revisionParts, analyst.ID)
		}

		if ensemble {
			nodes = append(nodes, NodeState{ID: "consensus", Label: "Consensus", Kind: "synthesis", Stage: 4})
			for _, analyst := range analystNodes {
				edges = append(edges, PipelineEdge{
					ID: "e-" + analyst.ID + "-consensus", Source: analyst.ID, Target: "consensus", Kind: "primary",
				})
			}
			// Adaptive depth can collapse an otherwise configured ensemble to
			// its first analyst for a cycle. Keep that runtime bypass routable.
			edges = append(edges, PipelineEdge{
				ID:     "e-" + analystNodes[0].ID + "-risk-controls",
				Source: analystNodes[0].ID, Target: "risk_manager", Kind: "bypass",
			})
			riskStage = 6
			arbCfg := cfg.Models.ArbiterConfig()
			if arbCfg.Model != "" {
				nodes = append(nodes, NodeState{
					ID: "arbiter_node", Label: "Ensemble Arbiter", Kind: "arbiter_final", Stage: 5,
					Meta: map[string]any{"provider": arbCfg.Provider, "model": arbCfg.Model},
				})
				edges = append(edges,
					PipelineEdge{ID: "e-consensus-risk-controls", Source: "consensus", Target: "risk_manager", Kind: "bypass"},
					PipelineEdge{ID: "e-consensus-arbiter", Source: "consensus", Target: "arbiter_node", Kind: "conditional"},
					PipelineEdge{ID: "e-arbiter-risk-controls", Source: "arbiter_node", Target: "risk_manager", Kind: "conditional"},
				)
				revisionParts = append(revisionParts, "arbiter:"+arbCfg.Provider+"/"+arbCfg.Model)
			} else {
				edges = append(edges, PipelineEdge{ID: "e-consensus-risk-controls", Source: "consensus", Target: "risk_manager", Kind: "primary"})
				riskStage = 5
			}
		} else {
			edges = append(edges, PipelineEdge{
				ID: "e-" + analystNodes[0].ID + "-risk-controls", Source: analystNodes[0].ID, Target: "risk_manager", Kind: "primary",
			})
		}
	}

	// Risk evaluation has two immediate outcomes, and they are not equivalent:
	//
	//   blocked_signals — terminal. The signal is dropped here.
	//   approvals       — a gate, not an end state. Engine.ApproveSignal
	//                     ("approves a pending signal and executes it") feeds
	//                     the approved signal straight into the executor.
	//
	// Both used to sit alongside the executor at the same depth with no edge
	// leaving the approval queue, which drew manual approval as a dead end
	// equal to Blocked. The queue now sits one stage ahead of the executor and
	// keeps its outgoing edge; Blocked remains the leaf.
	queueLabel, queueCaption, queueActiveLabel := queueNodeLabel(cfg)
	nodes = append(nodes,
		NodeState{ID: "risk_manager", Label: "Risk Controls", Kind: "synthesis", Stage: riskStage},
		NodeState{ID: "blocked_signals", Label: "Blocked", Kind: "outcome", Stage: riskStage + 1},
		NodeState{
			ID: "approvals", Label: queueLabel, Kind: "outcome", Stage: riskStage + 1,
			Meta: map[string]any{"caption": queueCaption, "active_label": queueActiveLabel},
		},
		NodeState{ID: "executor", Label: "Executor", Kind: "execution", Stage: riskStage + 2},
		NodeState{ID: "portfolio", Label: "Portfolio", Kind: "execution", Stage: riskStage + 3},
	)
	// The direct risk → executor edge exists only where risk approval alone can
	// execute. In manual mode every cleared signal is parked pending and reaches
	// the executor solely through Engine.ApproveSignal, so drawing a direct edge
	// would show a path that mode cannot take. In auto/paper the direct edge is
	// the dominant path and the queue holds only first-tick-deferred BUYs, which
	// processDeferredSignals later releases into the executor — both are real.
	if !isManualMode(cfg) {
		edges = append(edges,
			PipelineEdge{ID: "e-risk-controls-executor", Source: "risk_manager", Target: "executor", Kind: "primary"},
		)
	}
	edges = append(edges,
		PipelineEdge{ID: "e-risk-controls-blocked", Source: "risk_manager", Target: "blocked_signals", Kind: "conditional"},
		PipelineEdge{ID: "e-risk-controls-approvals", Source: "risk_manager", Target: "approvals", Kind: queueEntryKind(cfg)},
		PipelineEdge{ID: "e-approvals-executor", Source: "approvals", Target: "executor", Kind: queueEntryKind(cfg)},
		PipelineEdge{ID: "e-executor-portfolio", Source: "executor", Target: "portfolio", Kind: "primary"},
	)

	if mode == "specialist" {
		hasRiskSpecialist := false
		for _, n := range nodes {
			if n.ID == "specialist-risk-manager" {
				hasRiskSpecialist = true
				break
			}
		}
		if hasRiskSpecialist {
			edges = append(edges, PipelineEdge{
				ID: "e-specialist-risk-manager-risk-controls", Source: "specialist-risk-manager", Target: "risk_manager", Kind: "primary",
			})
		}
	}

	return nodes, PipelineTopology{
		Revision:     strings.Join(revisionParts, "|"),
		AnalysisMode: mode,
		Edges:        edges,
	}
}

// executionSourceNode reads the pipeline node an execution was released from,
// as stamped by Engine.executeSignal. Returns "" for anything that is not a
// trade payload or that executed straight off risk approval, leaving the
// caller's default attribution in place.
func executionSourceNode(payload any) string {
	p, ok := payload.(events.TradeExecutedPayload)
	if !ok {
		return ""
	}
	return p.SourceNode
}

// isManualMode reports whether every risk-cleared signal must be approved by a
// human before it can execute. Engine.generateSignal routes "auto" and "paper"
// identically (execute unless first-tick-deferred); only "manual" parks every
// signal pending.
func isManualMode(cfg *appconfig.Config) bool {
	return cfg != nil && strings.EqualFold(strings.TrimSpace(cfg.Engine.Mode), "manual")
}

// queueEntryKind describes how signals reach — and leave — the pending queue.
// In manual mode that is the only route to the executor, so it is the primary
// path; elsewhere it is the conditional first-tick deferral branch.
func queueEntryKind(cfg *appconfig.Config) string {
	if isManualMode(cfg) {
		return "primary"
	}
	return "conditional"
}

// queueNodeLabel names the pending queue for what it actually holds, and gives
// the card the wording for its populated state. Outside manual mode nothing is
// awaiting human review — the queue holds first-tick deferred BUYs that
// processDeferredSignals releases on the next tick. Resolving this server-side
// keeps the mode rule in one place rather than restating it in the client.
func queueNodeLabel(cfg *appconfig.Config) (label, caption, activeLabel string) {
	if isManualMode(cfg) {
		return "Approval Queue", "MANUAL REVIEW", "WAITING APPROVAL"
	}
	return "Pending Queue", "DEFERRED ENTRY", "AWAITING RELEASE"
}

func specialistNodeLabel(role string) string {
	switch role {
	case "fundamental":
		return "Fundamental"
	case "technical":
		return "Technical"
	case "sentiment":
		return "Sentiment"
	default:
		return role
	}
}
