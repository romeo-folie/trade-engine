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

// ── Layout constants ──────────────────────────────────────────────────────────
const COL = { source: 0, processing: 400, output: 820 };
const ROW_GAP = 110;

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
    const byKind: Record<string, NodeState[]> = { source: [], processing: [], output: [] };
    for (const n of nodes) byKind[n.kind]?.push(n);

    const result: Node[] = [];
    for (const [kind, kindNodes] of Object.entries(byKind)) {
      kindNodes.forEach((n, i) => {
        result.push({
          id: n.id,
          type: 'pipeline',
          position: { x: COL[kind as keyof typeof COL], y: i * ROW_GAP + 60 },
          data: {
            nodeState: n,
            isActive: isRecentlyActive(n, events),
            metrics: nodeMetrics[n.id],
          },
        });
      });
    }
    return result;
  }, [nodes, events, nodeMetrics]);

  // Build edges with particle data from recent events.
  const flowEdges: Edge[] = useMemo(() => {
    // Collect recent particles per edge.
    const edgeParticles: Record<string, ParticleInfo[]> = {};
    const edgeColors: Record<string, string> = {};
    const recentEvents = events.slice(0, 30);

    for (const ev of recentEvents) {
      const edgeKey = `${ev.source}-${ev.target}`;
      if (!edgeParticles[edgeKey]) edgeParticles[edgeKey] = [];
      // Limit particles per edge to avoid visual clutter.
      if (edgeParticles[edgeKey].length < 4) {
        edgeParticles[edgeKey].push({
          id: `${ev.type}-${ev.timestamp}-${edgeParticles[edgeKey].length}`,
          color: particleColor(ev.type),
          timestamp: new Date(ev.timestamp).getTime(),
        });
      }
      // Edge color follows the most recent event type.
      if (!edgeColors[edgeKey]) {
        edgeColors[edgeKey] = particleColor(ev.type);
      }
    }

    const activeEdges = new Set(recentEvents.map(e => `${e.source}-${e.target}`));

    return STATIC_EDGES.map(e => {
      const key = `${e.source}-${e.target}`;
      const isActive = activeEdges.has(key);
      const color = edgeColors[key] ?? (isActive ? '#00f5ff' : '#2d2b45');
      return {
        ...e,
        type: 'animated' as const,
        animated: false, // we handle animation via particles
        data: {
          active: isActive,
          particles: edgeParticles[key] ?? [],
          edgeColor: color,
        },
      };
    });
  }, [events]);

  const handleNodeClick = useCallback(
    (_: React.MouseEvent, node: Node) => onNodeClick(node.id),
    [onNodeClick]
  );

  return (
    <div style={{ height: '100%', background: '#07060f' }}>
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
        <Background variant={BackgroundVariant.Dots} color="#1e1b35" gap={24} />
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
