import {
  ReactFlow,
  Background,
  BackgroundVariant,
  type Node,
  type Edge,
} from '@xyflow/react';
import '@xyflow/react/dist/style.css';
import { useCallback, useMemo } from 'react';
import type { NodeState, WebEvent } from '../types/protocol';
import { particleColor } from '../types/protocol';
import type { NodeMetrics } from '../hooks/useEngineSocket';
import { PipelineNode } from './PipelineNode';
import { AnimatedEdge, GlowDefs } from './AnimatedEdge';
import type { ParticleInfo } from './AnimatedEdge';
import { colors } from '../styles/tokens';

// ── Layout constants ──────────────────────────────────────────────────────────
const COL = { 
  source: 0, 
  processing: 320, // Arbiter / Processing
  analysis: 680,   // Analysts
  synthesis: 1040, // Synthesis / Risk
  output: 1360     // Execution / Portfolio
};
const ROW_GAP = 160;

// Helper to determine column position based on node properties
function getNodePosition(node: NodeState, index: number): { x: number, y: number } {
  if (node.kind === 'source') return { x: COL.source, y: index * ROW_GAP + 100 };
  
  // Specific assignments based on ID to match hub-and-spoke
  if (node.id === 'data_enrichment' || node.id === 'indicator_engine' || node.id === 'prompt_assembly') {
    return { x: COL.processing, y: 180 }; // Stacked or single
  }
  
  if (node.id.includes('analysis')) {
    // If we have multiple analyses, we could spread them. For now, assume one or few.
    return { x: COL.analysis, y: index * ROW_GAP + 60 };
  }
  
  if (node.id === 'consensus' || node.id === 'risk_manager') {
    return { x: COL.synthesis, y: 180 };
  }
  
  if (node.id === 'executor' || node.id === 'portfolio') {
    return { x: COL.output, y: 180 };
  }

  // Fallback
  const x = node.kind === 'output' ? COL.output : COL.processing;
  return { x, y: index * ROW_GAP + 60 };
}

interface PipelineGraphProps {
  nodes: NodeState[];
  events: WebEvent[];
  nodeMetrics: Record<string, NodeMetrics>;
  onNodeClick: (nodeId: string) => void;
}

const nodeTypes = { pipeline: PipelineNode };
const edgeTypes = { animated: AnimatedEdge };

export function PipelineGraph({ nodes, events, nodeMetrics, onNodeClick }: PipelineGraphProps) {
  // Assign positions per column.
  const flowNodes: Node[] = useMemo(() => {
    return nodes.map((n, i) => ({
      id: n.id,
      type: 'pipeline',
      position: getNodePosition(n, i),
      data: {
        nodeState: n,
        isActive: isRecentlyActive(n, events),
        metrics: nodeMetrics[n.id],
      },
    }));
  }, [nodes, events, nodeMetrics]);

  // ... [keep flowEdges logic as is for now, just ensure colors come from tokens]
  const flowEdges: Edge[] = useMemo(() => {
    // Collect recent particles per edge.
    const edgeParticles: Record<string, ParticleInfo[]> = {};
    const recentEvents = events.slice(0, 30);

    for (const ev of recentEvents) {
      const edgeKey = `${ev.source}-${ev.target}`;
      if (!edgeParticles[edgeKey]) edgeParticles[edgeKey] = [];
      if (edgeParticles[edgeKey].length < 4) {
        edgeParticles[edgeKey].push({
          id: `${ev.type}-${ev.timestamp}-${edgeParticles[edgeKey].length}`,
          color: particleColor(ev.type),
          timestamp: new Date(ev.timestamp).getTime(),
        });
      }
    }

    const activeEdges = new Set(recentEvents.map(e => `${e.source}-${e.target}`));

    return STATIC_EDGES.map(e => {
      const key = `${e.source}-${e.target}`;
      const isActive = activeEdges.has(key);
      return {
        ...e,
        type: 'animated' as const,
        animated: false,
        data: {
          active: isActive,
          particles: edgeParticles[key] ?? [],
        },
      };
    });
  }, [events]);

  const handleNodeClick = useCallback(
    (_: React.MouseEvent, node: Node) => onNodeClick(node.id),
    [onNodeClick]
  );

  return (
    <div style={{ height: '100%', background: colors.canvas }}>
      <GlowDefs />
      <ReactFlow
        nodes={flowNodes}
        edges={flowEdges}
        nodeTypes={nodeTypes}
        edgeTypes={edgeTypes}
        onNodeClick={handleNodeClick}
        fitView
        fitViewOptions={{ padding: 0.15 }}
        proOptions={{ hideAttribution: true }}
        colorMode="dark"
      >
        <Background 
          variant={BackgroundVariant.Dots} 
          color={colors.borderMuted} 
          gap={20} 
          size={1}
          style={{ opacity: 0.2 }}
        />
      </ReactFlow>
    </div>
  );
}

// ── Static edge topology (mirrors mapEventEdge in protocol.go) ────────────────
const STATIC_EDGES: Edge[] = [
  // Sources → Data Enrichment
  { id: 'alpaca-enrich', source: 'alpaca', target: 'data_enrichment' },
  { id: 'polymarket-enrich', source: 'polymarket', target: 'data_enrichment' },
  { id: 'kalshi-enrich', source: 'kalshi', target: 'data_enrichment' },
  { id: 'news-enrich', source: 'news_api', target: 'data_enrichment' },
  // Processing chain
  { id: 'enrich-indicators', source: 'data_enrichment', target: 'indicator_engine' },
  { id: 'indicators-prompt', source: 'indicator_engine', target: 'prompt_assembly' },
  { id: 'prompt-llm', source: 'prompt_assembly', target: 'llm_analysis' },
  { id: 'llm-consensus', source: 'llm_analysis', target: 'consensus' },
  { id: 'consensus-risk', source: 'consensus', target: 'risk_manager' },
  // Risk → outputs
  { id: 'risk-executor', source: 'risk_manager', target: 'executor' },
  { id: 'risk-blocked', source: 'risk_manager', target: 'blocked_signals' },
  { id: 'risk-approvals', source: 'risk_manager', target: 'approvals' },
  // Executor → portfolio
  { id: 'executor-portfolio', source: 'executor', target: 'portfolio' },
  // portfolio self-loop: Go emits PortfolioSnapshot events as portfolio→portfolio
  { id: 'portfolio-portfolio', source: 'portfolio', target: 'portfolio' },
];

function isRecentlyActive(node: NodeState, events: WebEvent[]): boolean {
  return events.slice(0, 15).some(e => e.source === node.id || e.target === node.id);
}
