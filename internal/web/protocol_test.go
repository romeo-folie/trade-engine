package web

import (
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/events"
)

func topologyTestConfig() *config.Config {
	return &config.Config{
		Models: config.ModelsConfig{
			Registry: config.ModelRegistry{
				"cheap": {ModelConfig: config.ModelConfig{Provider: "deepseek", Model: "cheap-model"}},
				"mid":   {ModelConfig: config.ModelConfig{Provider: "openai", Model: "mid-model"}},
				"front": {ModelConfig: config.ModelConfig{Provider: "anthropic", Model: "front-model"}},
				"arb":   {ModelConfig: config.ModelConfig{Provider: "openai", Model: "arbiter-model"}},
			},
			Roles: config.ModelRoles{
				Analysts: []string{"cheap", "mid", "front"},
				Arbiter:  "arb",
			},
		},
	}
}

func TestPipelineTopologySingleModelCollapsesEnsemble(t *testing.T) {
	cfg := topologyTestConfig()
	cfg.Models.Consensus.Enabled = false

	nodes, topology := pipelineTopology(cfg)
	if topology.AnalysisMode != "single" {
		t.Fatalf("mode = %q, want single", topology.AnalysisMode)
	}
	assertNodeIDs(t, nodes,
		"news_api", "alpaca", "news_enricher", "indicator_engine",
		"analyst-cheap", "risk_manager", "executor", "portfolio",
	)
	assertMissingNode(t, nodes, "analyst-mid")
	assertMissingNode(t, nodes, "consensus")
	assertMissingNode(t, nodes, "arbiter_node")
	assertEdge(t, topology.Edges, "analyst-cheap", "risk_manager", "primary")
}

func TestPipelineTopologyEnsembleKeepsConditionalArbiter(t *testing.T) {
	cfg := topologyTestConfig()
	cfg.Models.Consensus.Enabled = true

	nodes, topology := pipelineTopology(cfg)
	if topology.AnalysisMode != "ensemble" {
		t.Fatalf("mode = %q, want ensemble", topology.AnalysisMode)
	}
	for _, id := range []string{"analyst-cheap", "analyst-mid", "analyst-front", "consensus", "arbiter_node"} {
		assertHasNode(t, nodes, id)
	}
	assertEdge(t, topology.Edges, "consensus", "risk_manager", "bypass")
	assertEdge(t, topology.Edges, "consensus", "arbiter_node", "conditional")
	assertEdge(t, topology.Edges, "arbiter_node", "risk_manager", "conditional")
}

func TestPipelineTopologySpecialistUsesMandatoryRiskSpecialist(t *testing.T) {
	cfg := topologyTestConfig()
	cfg.Analysis.SpecialistModeEnabled = true
	cfg.Analysis.SpecialistRoles = map[string]string{
		"fundamental":  "front",
		"technical":    "mid",
		"sentiment":    "cheap",
		"risk_manager": "arb",
	}

	nodes, topology := pipelineTopology(cfg)
	if topology.AnalysisMode != "specialist" {
		t.Fatalf("mode = %q, want specialist", topology.AnalysisMode)
	}
	assertMissingNode(t, nodes, "consensus")
	assertMissingNode(t, nodes, "arbiter_node")
	for _, id := range []string{
		"specialist-fundamental", "specialist-technical",
		"specialist-sentiment", "specialist-risk-manager", "risk_manager",
	} {
		assertHasNode(t, nodes, id)
	}
	for _, source := range []string{"specialist-fundamental", "specialist-technical", "specialist-sentiment"} {
		assertEdge(t, topology.Edges, source, "specialist-risk-manager", "primary")
	}
	assertEdge(t, topology.Edges, "specialist-risk-manager", "risk_manager", "primary")
}

func TestToWebEventsSpecialistRiskReviewFansInFromDirectionalRoles(t *testing.T) {
	sources := []string{"specialist-fundamental", "specialist-technical", "specialist-sentiment"}
	msgs := toWebEvents(events.Event{
		Type: events.AnalysisStarted, Timestamp: time.Now(),
		Payload: events.AnalysisStartedPayload{
			Ticker: "AAPL", SelectedAnalysts: []string{"specialist-risk-manager"}, SourceNodes: sources,
		},
	})
	if len(msgs) != len(sources) {
		t.Fatalf("message count = %d, want %d", len(msgs), len(sources))
	}
	for i, msg := range msgs {
		if msg.Source != sources[i] || msg.Target != "specialist-risk-manager" {
			t.Fatalf("message[%d] = %s -> %s, want %s -> specialist-risk-manager",
				i, msg.Source, msg.Target, sources[i])
		}
	}
}

func TestToWebEventsSpecialistVotesRouteIntoRiskControls(t *testing.T) {
	msgs := toWebEvents(events.Event{
		Type: events.AnalystVotesPublished, Timestamp: time.Now(),
		Payload: events.AnalystVotesPublishedPayload{
			Ticker: "AAPL",
			Votes: []events.AnalystVotePayload{
				{Model: "fund-model", Role: "fundamental"},
				{Model: "risk-model", Role: "risk_manager"},
			},
		},
	})
	if len(msgs) != 1 {
		t.Fatalf("message count = %d, want 1", len(msgs))
	}
	if msgs[0].Type != "SpecialistReviewCompleted" {
		t.Fatalf("message type = %q, want SpecialistReviewCompleted", msgs[0].Type)
	}
	if msgs[0].Source != "specialist-risk-manager" || msgs[0].Target != "risk_manager" {
		t.Fatalf("message = %s -> %s, want specialist-risk-manager -> risk_manager",
			msgs[0].Source, msgs[0].Target)
	}
}

func assertNodeIDs(t *testing.T, nodes []NodeState, want ...string) {
	t.Helper()
	if len(nodes) != len(want) {
		t.Fatalf("node count = %d, want %d: %+v", len(nodes), len(want), nodes)
	}
	for i := range want {
		if nodes[i].ID != want[i] {
			t.Fatalf("nodes[%d] = %q, want %q", i, nodes[i].ID, want[i])
		}
	}
}

func assertHasNode(t *testing.T, nodes []NodeState, id string) {
	t.Helper()
	for _, node := range nodes {
		if node.ID == id {
			return
		}
	}
	t.Fatalf("missing node %q", id)
}

func assertMissingNode(t *testing.T, nodes []NodeState, id string) {
	t.Helper()
	for _, node := range nodes {
		if node.ID == id {
			t.Fatalf("unexpected node %q", id)
		}
	}
}

func assertEdge(t *testing.T, edges []PipelineEdge, source, target, kind string) {
	t.Helper()
	for _, edge := range edges {
		if edge.Source == source && edge.Target == target && edge.Kind == kind {
			return
		}
	}
	t.Fatalf("missing %s edge %s -> %s: %+v", kind, source, target, edges)
}
